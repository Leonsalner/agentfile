// Package installer discovers the V1 targets, builds the preview for an
// install or restore, and executes only the changes the user chose.
package installer

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentfile/internal/diff"
	"agentfile/internal/fsnode"
	"agentfile/internal/profile"
	"agentfile/internal/render"
	"agentfile/internal/state"
)

// Env is the machine the installer runs against; tests supply a temporary home.
type Env struct {
	Home     string
	GOOS     string
	Getenv   func(string) string
	LookPath func(string) (string, error)
}

// AgentHome is the only directory V1 writes into for each agent.
func (e Env) AgentHome(a profile.Agent) string {
	if a == profile.Claude {
		return filepath.Join(e.Home, ".claude")
	}
	return filepath.Join(e.Home, ".codex")
}

func binaryName(a profile.Agent) string { return string(a) }

func instructionFile(a profile.Agent) string {
	if a == profile.Claude {
		return "CLAUDE.md"
	}
	return "AGENTS.md"
}

// contentOnly reports whether managed files are updated in place and only
// their bytes are backed up and restored. Windows cannot prove an exact
// restore of security descriptors, so it manages each file's contents,
// including only SKILL.md inside a named skill directory.
func (e Env) contentOnly() bool { return e.GOOS == "windows" }

const skillFile = "SKILL.md"

// ManagedPaths lists every path V1 may write for an agent.
func (e Env) ManagedPaths(a profile.Agent) []string {
	home := e.AgentHome(a)
	out := []string{filepath.Join(home, instructionFile(a))}
	for _, s := range render.SkillNames {
		if e.contentOnly() {
			out = append(out, filepath.Join(home, "skills", s, skillFile))
		} else {
			out = append(out, filepath.Join(home, "skills", s))
		}
	}
	return out
}

// managedDirs are the parent directories V1 may create for an agent. In
// content-only mode the named skill directories are parents too.
func (e Env) managedDirs(a profile.Agent) []string {
	out := []string{e.AgentHome(a), filepath.Join(e.AgentHome(a), "skills")}
	if e.contentOnly() {
		for _, s := range render.SkillNames {
			out = append(out, filepath.Join(e.AgentHome(a), "skills", s))
		}
	}
	return out
}

// checkParents refuses agent homes or skills directories that are symlinks or
// not directories, so writes cannot be redirected outside the named homes.
func (e Env) checkParents(a profile.Agent) error {
	fi, err := os.Lstat(e.Home)
	if err != nil || fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("home directory %s is not usable", e.Home)
	}
	for _, d := range e.managedDirs(a) {
		if e.contentOnly() {
			if err := fsnode.CheckPlainDir(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		fi, err := os.Lstat(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; agentfile will not write through it", d)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s exists but is not a directory", d)
		}
	}
	return nil
}

// TargetState is one managed path's status in discovery.
type TargetState struct {
	Path   string
	Status string
}

// AgentState is what discovery reports for one agent.
type AgentState struct {
	Agent    profile.Agent
	Binary   string // empty when not found on PATH
	Home     string
	Problem  string // non-empty when the home cannot be written safely
	Warnings []string
	Targets  []TargetState
}

// Discover inspects only the named homes and managed entries.
func Discover(e Env, st *state.Store) []AgentState {
	installed := map[string]string{}
	if st != nil {
		installed = st.LastInstalled()
	}
	var out []AgentState
	for _, a := range []profile.Agent{profile.Claude, profile.Codex} {
		as := AgentState{Agent: a, Home: e.AgentHome(a)}
		if p, err := e.LookPath(binaryName(a)); err == nil {
			as.Binary = p
		} else {
			as.Warnings = append(as.Warnings, fmt.Sprintf("`%s` was not found on PATH; you can still install for it.", binaryName(a)))
		}
		as.Warnings = append(as.Warnings, e.overrideWarnings(a)...)
		if err := e.checkParents(a); err != nil {
			as.Problem = err.Error()
		}
		for _, p := range e.ManagedPaths(a) {
			status := "not inspected: unsafe parent"
			if as.Problem == "" {
				status = e.targetStatus(p, installed)
			}
			as.Targets = append(as.Targets, TargetState{Path: p, Status: status})
		}
		out = append(out, as)
	}
	return out
}

func (e Env) overrideWarnings(a profile.Agent) []string {
	v := "CLAUDE_CONFIG_DIR"
	if a == profile.Codex {
		v = "CODEX_HOME"
	}
	val := e.Getenv(v)
	if val == "" || filepath.Clean(val) == e.AgentHome(a) {
		return nil
	}
	return []string{fmt.Sprintf("%s is set to %s; %s reads from there, but agentfile only writes %s.", v, val, profile.ClientName(a), e.AgentHome(a))}
}

// capture reads a managed path the way this mode backs it up.
func (e Env) capture(path string) (*fsnode.Node, error) {
	if e.contentOnly() {
		return fsnode.CaptureContent(path)
	}
	return fsnode.Capture(path)
}

func (e Env) fingerprint(n *fsnode.Node) string {
	if e.contentOnly() {
		return fsnode.ContentFingerprint(n)
	}
	return fsnode.Fingerprint(n)
}

func (e Env) targetStatus(path string, installed map[string]string) string {
	n, err := e.capture(path)
	if err != nil {
		return "cannot be backed up: " + err.Error()
	}
	if n.Type == fsnode.Absent {
		return "absent"
	}
	fp := e.fingerprint(n)
	switch last, ok := installed[path]; {
	case ok && last == fp:
		return "installed by agentfile"
	case ok:
		return "customized since agentfile installed it"
	}
	return "present, not installed by agentfile"
}

// Action names what a preview item will do.
type Action string

const (
	Unchanged         Action = "unchanged"
	Create            Action = "create"
	ReplaceFile       Action = "replace file"
	ReplaceSymlink    Action = "replace symlink"
	ReplaceDir        Action = "replace named skill directory"
	Remove            Action = "remove"
	RemoveParent      Action = "remove empty parent directory"
	RestoreParentMeta Action = "restore parent metadata"
	// Content-only (Windows) actions.
	UpdateInPlace Action = "update file contents"
	Recreate      Action = "restore missing file"
	CreateParent  Action = "create parent directory"
)

// Item is one exact path in a preview.
type Item struct {
	Agent          profile.Agent
	Path           string
	Action         Action
	Current        *fsnode.Node
	Want           *fsnode.Node
	Status         string // relation to what agentfile last wrote
	Conflict       bool   // an existing differing entry: needs its own choice
	Replace        bool   // chosen; always true for non-conflicts
	Blocked        string // why this path cannot be changed; never replaced
	ParentCleanup  bool
	ParentRestore  bool
	ParentMetaOnly bool
	Baseline       *fsnode.Node // parent metadata after the recorded operation
	Note           string       // extra context shown in the preview
}

// Proceeds reports whether the item will be written.
func (it Item) Proceeds() bool {
	return it.Action != Unchanged && it.Blocked == "" && (!it.Conflict || it.Replace)
}

// Plan is a complete preview.
type Plan struct {
	Kind     string // "apply" or "restore"
	Profile  profile.Profile
	Snapshot *state.Manifest // restore only
	Items    []Item
}

func fileNode(b []byte) *fsnode.Node { return &fsnode.Node{Type: fsnode.File, Mode: 0o644, Data: b} }

func artifactNode(a render.Artifact) *fsnode.Node {
	if !a.IsDir() {
		return fsnode.Canonical(fileNode(a.Data))
	}
	n := &fsnode.Node{Type: fsnode.Dir, Mode: 0o755, Children: map[string]*fsnode.Node{}}
	for name, b := range a.Files {
		n.Children[name] = fileNode(b)
	}
	return fsnode.Canonical(n)
}

func actionFor(cur, want *fsnode.Node, exact bool) Action {
	equal := fsnode.Fingerprint(cur) == fsnode.Fingerprint(want)
	if exact {
		equal = fsnode.RestoreFingerprint(cur) == fsnode.RestoreFingerprint(want)
	}
	switch {
	case equal:
		return Unchanged
	case want.Type == fsnode.Absent:
		return Remove
	case cur.Type == fsnode.Absent:
		return Create
	case cur.Type == fsnode.Symlink:
		return ReplaceSymlink
	case cur.Type == fsnode.Dir:
		return ReplaceDir
	}
	return ReplaceFile
}

// PlanInstall previews installing the rendered content for p.
func PlanInstall(e Env, st *state.Store, p profile.Profile) (*Plan, error) {
	arts, err := render.Render(p, e.GOOS)
	if err != nil {
		return nil, err
	}
	if e.contentOnly() {
		return e.planContentInstall(st, p, arts)
	}
	installed := st.LastInstalled()
	plan := &Plan{Kind: "apply", Profile: p}
	for _, a := range arts {
		path := filepath.Join(e.AgentHome(a.Agent), filepath.FromSlash(a.Path))
		it := Item{Agent: a.Agent, Path: path, Want: artifactNode(a)}
		if err := e.checkParents(a.Agent); err != nil {
			it.Blocked = err.Error()
			it.Current = &fsnode.Node{Type: fsnode.Absent}
		} else if cur, err := fsnode.Capture(path); err != nil {
			it.Blocked = err.Error()
			it.Current = &fsnode.Node{Type: fsnode.Absent}
		} else {
			it.Current = cur
		}
		it.Action = actionFor(it.Current, it.Want, false)
		if it.Blocked == "" {
			it.Status = e.targetStatus(path, installed)
		}
		it.Conflict = it.Current.Type != fsnode.Absent && it.Action != Unchanged
		it.Replace = !it.Conflict
		plan.Items = append(plan.Items, it)
	}
	return plan, nil
}

// PlanRestore previews returning each path in snapshot id to its state
// before that operation. Paths changed since the operation are conflicts.
func PlanRestore(e Env, st *state.Store, id string) (*Plan, error) {
	m, err := st.Load(id)
	if err != nil {
		return nil, err
	}
	if m.State != "committed" {
		return nil, fmt.Errorf("snapshot %s is not committed", id)
	}
	if m.OS != e.GOOS {
		return nil, fmt.Errorf("snapshot %s was made on %s, not %s", id, m.OS, e.GOOS)
	}
	if e.contentOnly() && m.Mode != state.ModeContent {
		return nil, fmt.Errorf("snapshot %s was made by an earlier exact-restore build; this Windows build restores only content-only snapshots and changed nothing. The snapshot is kept at %s",
			id, filepath.Join(st.Dir, "backups", id))
	}
	if !e.contentOnly() && m.Mode != "" {
		return nil, fmt.Errorf("snapshot %s is a %s snapshot, which this platform does not restore", id, m.Mode)
	}
	if err := state.VerifyDirStates(m); err != nil {
		return nil, err
	}
	if e.contentOnly() {
		return e.planContentRestore(st, m)
	}
	hasRemovedParent := false
	for _, saved := range m.RemovedDirStates {
		hasRemovedParent = hasRemovedParent || saved.Removed
	}
	if len(m.Entries) == 0 && !hasRemovedParent {
		return nil, fmt.Errorf("snapshot %s has no managed entries", id)
	}
	entries, dirs := map[string]profile.Agent{}, map[string]bool{}
	dirAgents := map[string]profile.Agent{}
	for _, a := range []profile.Agent{profile.Claude, profile.Codex} {
		for _, p := range e.ManagedPaths(a) {
			entries[p] = a
		}
		for _, d := range e.managedDirs(a) {
			dirs[d] = true
			dirAgents[d] = a
		}
	}
	for _, d := range m.CreatedDirs {
		if !dirs[d] {
			return nil, fmt.Errorf("snapshot %s names a directory outside the managed homes: %s", id, d)
		}
	}
	for _, saved := range m.RemovedDirStates {
		if !dirs[saved.Path] || saved.Node == nil || saved.Node.Type != fsnode.Dir || len(saved.Node.Children) != 0 {
			return nil, fmt.Errorf("snapshot %s has invalid removed parent %s", id, saved.Path)
		}
	}
	for _, en := range m.Entries {
		if en.ParentDir {
			original := fsnode.Absent
			if en.ParentMetaOnly {
				original = fsnode.Dir
			}
			if m.Kind != "restore" || !dirs[en.Path] || en.OriginalType != original {
				return nil, fmt.Errorf("snapshot %s names an invalid parent path: %s", id, en.Path)
			}
		} else if _, ok := entries[en.Path]; !ok {
			return nil, fmt.Errorf("snapshot %s names a path outside the managed set: %s", id, en.Path)
		}
	}
	origs, err := st.Originals(m)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Kind: "restore", Snapshot: m}
	for _, saved := range m.RemovedDirStates {
		if !saved.Removed {
			continue
		}
		it := Item{Agent: dirAgents[saved.Path], Path: saved.Path, Want: saved.Node, ParentRestore: true}
		if _, err := os.Lstat(it.Path); errors.Is(err, fs.ErrNotExist) {
			it.Current = &fsnode.Node{Type: fsnode.Absent}
			it.Action = Create
			it.Replace = true
			it.Status = "removed by that operation"
		} else if err != nil {
			it.Current = &fsnode.Node{Type: fsnode.Absent}
			it.Blocked = err.Error()
			it.Action = Create
		} else if cur, err := fsnode.CaptureShallowDir(it.Path); err != nil {
			it.Current = &fsnode.Node{Type: fsnode.Absent}
			it.Blocked = err.Error()
			it.Action = Create
		} else {
			it.Current = cur
			if fsnode.RestoreFingerprint(cur) == fsnode.RestoreFingerprint(it.Want) {
				it.Action = Unchanged
				it.Status = "parent already matches"
			} else {
				it.Action = RestoreParentMeta
				it.ParentMetaOnly = true
				it.Conflict = true
				it.Status = "parent metadata differs"
			}
		}
		plan.Items = append(plan.Items, it)
	}
	for i, en := range m.Entries {
		if en.ParentMetaOnly {
			it := Item{Agent: dirAgents[en.Path], Path: en.Path, Want: origs[i], ParentRestore: true, ParentMetaOnly: true}
			it.Current = &fsnode.Node{Type: fsnode.Absent}
			it.Action = RestoreParentMeta
			if cur, err := fsnode.CaptureShallowDir(en.Path); errors.Is(err, fs.ErrNotExist) {
				it.Blocked = "parent directory no longer exists"
			} else if err != nil {
				it.Blocked = err.Error()
			} else {
				it.Current = cur
				if fsnode.RestoreFingerprint(cur) == fsnode.RestoreFingerprint(it.Want) {
					it.Action = Unchanged
				}
			}
			changed := en.InstalledMetaFP == "" || fsnode.RestoreFingerprint(it.Current) != en.InstalledMetaFP
			it.Status = "unchanged since that operation"
			if changed {
				it.Status = "changed since that operation"
			}
			it.Conflict = changed && it.Action != Unchanged
			it.Replace = !it.Conflict
			plan.Items = append(plan.Items, it)
			continue
		}
		if en.ParentDir {
			it := Item{Agent: dirAgents[en.Path], Path: en.Path, Want: origs[i], ParentCleanup: true}
			if _, err := os.Lstat(en.Path); errors.Is(err, fs.ErrNotExist) {
				it.Current = &fsnode.Node{Type: fsnode.Absent}
			} else if err != nil {
				it.Blocked = err.Error()
				it.Current = &fsnode.Node{Type: fsnode.Absent}
			} else if cur, err := fsnode.CaptureShallowDir(en.Path); err != nil {
				it.Blocked = err.Error()
				it.Current = &fsnode.Node{Type: fsnode.Absent}
			} else {
				it.Current = cur
			}
			if it.Current.Type == fsnode.Absent {
				it.Action = Unchanged
			} else {
				it.Action = RemoveParent
			}
			changed := en.InstalledMetaFP == "" || fsnode.RestoreFingerprint(it.Current) != en.InstalledMetaFP
			it.Status = "unchanged since that operation"
			if changed {
				it.Status = "changed since that operation"
			}
			it.Conflict = changed && it.Action != Unchanged
			it.Replace = !it.Conflict
			plan.Items = append(plan.Items, it)
			continue
		}
		a := entries[en.Path]
		it := Item{Agent: a, Path: en.Path, Want: origs[i]}
		if err := e.checkParents(a); err != nil {
			it.Blocked = err.Error()
			it.Current = &fsnode.Node{Type: fsnode.Absent}
		} else if cur, err := fsnode.Capture(en.Path); err != nil {
			it.Blocked = err.Error()
			it.Current = &fsnode.Node{Type: fsnode.Absent}
		} else {
			it.Current = cur
		}
		it.Action = actionFor(it.Current, it.Want, true)
		changed := en.InstalledMetaFP == "" || fsnode.RestoreFingerprint(it.Current) != en.InstalledMetaFP
		it.Status = "unchanged since that operation"
		if changed {
			it.Status = "changed since that operation"
		}
		it.Conflict = changed && it.Action != Unchanged
		it.Replace = !it.Conflict
		plan.Items = append(plan.Items, it)
	}
	for _, d := range m.CreatedDirs {
		it := Item{Agent: dirAgents[d], Path: d, Want: &fsnode.Node{Type: fsnode.Absent}, ParentCleanup: true}
		for _, saved := range m.CreatedDirStates {
			if saved.Path == d {
				it.Baseline = saved.Node
				break
			}
		}
		if it.Baseline == nil {
			it.Blocked = "created parent metadata was not recorded in this snapshot"
		}
		if _, err := os.Lstat(d); errors.Is(err, fs.ErrNotExist) {
			it.Current = &fsnode.Node{Type: fsnode.Absent}
		} else if err != nil {
			it.Blocked = err.Error()
			it.Current = &fsnode.Node{Type: fsnode.Absent}
		} else if cur, err := fsnode.CaptureShallowDir(d); err != nil {
			it.Blocked = err.Error()
			it.Current = &fsnode.Node{Type: fsnode.Absent}
		} else {
			it.Current = cur
		}
		if it.Current.Type == fsnode.Absent {
			it.Action = Unchanged
		} else {
			it.Action = RemoveParent
		}
		changed := it.Baseline == nil || fsnode.RestoreFingerprint(it.Current) != fsnode.RestoreFingerprint(it.Baseline)
		it.Status = "unchanged since that operation"
		if changed {
			it.Status = "changed since that operation"
		}
		it.Conflict = changed && it.Action != Unchanged
		it.Replace = !it.Conflict
		plan.Items = append(plan.Items, it)
	}
	return plan, nil
}

// Changes returns the transaction for the chosen items and the parent
// directories they need.
func (e Env) Changes(p *Plan) ([]state.Change, []string) {
	if e.contentOnly() {
		return e.contentChanges(p)
	}
	var cs []state.Change
	need := map[profile.Agent]bool{}
	restoreParents := map[string]bool{}
	for _, it := range p.Items {
		if it.ParentRestore && it.Proceeds() && !it.ParentMetaOnly {
			restoreParents[it.Path] = true
		}
	}
	for _, it := range p.Items {
		if !it.Proceeds() || it.ParentCleanup {
			continue
		}
		c := state.Change{Path: it.Path, Want: it.Want, Expect: fsnode.Fingerprint(it.Current), ParentDir: it.ParentRestore, ParentMetaOnly: it.ParentMetaOnly}
		if p.Kind == "restore" {
			c.ExpectMeta = fsnode.RestoreFingerprint(it.Current)
		}
		cs = append(cs, c)
		if it.Want.Type != fsnode.Absent {
			need[it.Agent] = true
		}
	}
	var dirs []string
	for _, a := range []profile.Agent{profile.Claude, profile.Codex} {
		if need[a] {
			for _, d := range e.managedDirs(a) {
				if !restoreParents[d] {
					dirs = append(dirs, d)
				}
			}
		}
	}
	return cs, dirs
}

// Execute runs the chosen changes. A nil manifest with nil error is a no-op.
func Execute(e Env, st *state.Store, p *Plan) (*state.Manifest, error) {
	for _, a := range []profile.Agent{profile.Claude, profile.Codex} {
		for _, it := range p.Items {
			if it.Agent == a && it.Proceeds() {
				if err := e.checkParents(a); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	cs, dirs := e.Changes(p)
	var cleanup []string
	cleanupExpected := map[string]string{}
	for _, it := range p.Items {
		if it.ParentCleanup && it.Proceeds() {
			cleanup = append(cleanup, it.Path)
			cleanupExpected[it.Path] = fsnode.RestoreFingerprint(it.Current)
		}
	}
	if len(cs) == 0 && len(cleanup) == 0 && len(dirs) == 0 {
		return nil, nil
	}
	// Parents sort before their children; state removes them in reverse.
	sort.Strings(cleanup)
	meta := state.Meta{Kind: p.Kind, ParentDirs: dirs, CheckDirs: []string{e.Home}, CleanupDirs: cleanup, CleanupExpected: cleanupExpected}
	if e.contentOnly() {
		meta.Mode = state.ModeContent
	}
	for _, a := range []profile.Agent{profile.Claude, profile.Codex} {
		for _, it := range p.Items {
			if it.Agent == a && it.Proceeds() {
				meta.CheckDirs = append(meta.CheckDirs, e.managedDirs(a)...)
				break
			}
		}
	}
	if p.Kind == "apply" {
		prov, err := render.LoadProvenance()
		if err != nil {
			return nil, err
		}
		for _, a := range p.Profile.Agents() {
			meta.Agents = append(meta.Agents, string(a))
		}
		meta.Profile = p.Profile.Summary()
		meta.ContentVersion = prov.ContentVersion
		meta.UpstreamCommit = prov.UpstreamCommit
	} else {
		meta.Agents = p.Snapshot.Agents
		meta.Profile = p.Snapshot.Profile
		meta.ContentVersion = p.Snapshot.ContentVersion
		meta.RestoredFrom = p.Snapshot.ID
	}
	m, err := st.Execute(cs, meta)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Diff renders the preview detail for one item: unified diffs for Markdown
// and a type/hash summary for everything else.
func Diff(it Item) string {
	var b strings.Builder
	if it.ParentCleanup {
		summarize(&b, filepath.Base(it.Path), it.Current, it.Want)
		if it.Baseline != nil {
			appendMetadataDiff(&b, it.Current, it.Baseline)
		}
		return b.String()
	}
	var walk func(rel string, cur, want *fsnode.Node)
	walk = func(rel string, cur, want *fsnode.Node) {
		if cur == nil {
			cur = &fsnode.Node{Type: fsnode.Absent}
		}
		if want == nil {
			want = &fsnode.Node{Type: fsnode.Absent}
		}
		if fsnode.RestoreFingerprint(cur) == fsnode.RestoreFingerprint(want) {
			return
		}
		if cur.Type == fsnode.Dir || want.Type == fsnode.Dir {
			if cur.Type == fsnode.Dir && want.Type == fsnode.Dir {
				appendMetadataDiff(&b, cur, want)
				names := map[string]bool{}
				for n := range cur.Children {
					names[n] = true
				}
				for n := range want.Children {
					names[n] = true
				}
				var sorted []string
				for n := range names {
					sorted = append(sorted, n)
				}
				sort.Strings(sorted)
				for _, n := range sorted {
					walk(rel+"/"+n, cur.Children[n], want.Children[n])
				}
				return
			}
			summarize(&b, rel, cur, want)
			return
		}
		markdown := strings.HasSuffix(strings.ToLower(rel), ".md")
		if markdown && cur.Type != fsnode.Symlink && want.Type != fsnode.Symlink {
			b.WriteString(diff.Unified(string(cur.Data), string(want.Data), "current "+rel, "new "+rel))
			appendMetadataDiff(&b, cur, want)
			return
		}
		summarize(&b, rel, cur, want)
		appendMetadataDiff(&b, cur, want)
	}
	walk(filepath.Base(it.Path), it.Current, it.Want)
	return b.String()
}

func appendMetadataDiff(b *strings.Builder, cur, want *fsnode.Node) {
	if cur.Type != want.Type || cur.Type == fsnode.Absent {
		return
	}
	if cur.Mode != want.Mode {
		fmt.Fprintf(b, "mode %04o → %04o\n", cur.Mode, want.Mode)
	}
	if !cur.ModTime.Equal(want.ModTime) {
		fmt.Fprintf(b, "mtime %s → %s\n", cur.ModTime.UTC().Format(time.RFC3339Nano), want.ModTime.UTC().Format(time.RFC3339Nano))
	}
	if (cur.Gid == nil) != (want.Gid == nil) || (cur.Gid != nil && want.Gid != nil && *cur.Gid != *want.Gid) {
		fmt.Fprintf(b, "group %s → %s\n", groupLabel(cur.Gid), groupLabel(want.Gid))
	}
	if cur.Attrs != want.Attrs {
		fmt.Fprintf(b, "Windows attributes %#x → %#x\n", cur.Attrs, want.Attrs)
	}
	names := map[string]bool{}
	for name := range cur.Xattrs {
		names[name] = true
	}
	for name := range want.Xattrs {
		names[name] = true
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		old, oldOK := cur.Xattrs[name]
		newVal, newOK := want.Xattrs[name]
		if oldOK != newOK || !bytes.Equal(old, newVal) {
			fmt.Fprintf(b, "xattr %s %s → %s\n", name, xattrLabel(old, oldOK), xattrLabel(newVal, newOK))
		}
	}
}

func groupLabel(gid *int) string {
	if gid == nil {
		return "none"
	}
	return fmt.Sprint(*gid)
}

func xattrLabel(value []byte, present bool) string {
	if !present {
		return "absent"
	}
	sum := sha256.Sum256(value)
	return fmt.Sprintf("sha256:%x (%dB)", sum[:6], len(value))
}

func summarize(b *strings.Builder, rel string, cur, want *fsnode.Node) {
	fmt.Fprintf(b, "=== %s\n", rel)
	for _, side := range []struct {
		label string
		n     *fsnode.Node
	}{{"current", cur}, {"new", want}} {
		if side.n.Type == fsnode.Absent {
			fmt.Fprintf(b, "  %s: absent\n", side.label)
			continue
		}
		fmt.Fprintf(b, "  %s:\n", side.label)
		for _, l := range fsnode.Summary(side.n) {
			fmt.Fprintf(b, "    %s\n", l)
		}
	}
	for name, n := range want.Children {
		if strings.HasSuffix(name, ".md") && n.Type == fsnode.File {
			var old string
			if c, ok := cur.Children[name]; ok && c.Type == fsnode.File {
				old = string(c.Data)
			}
			b.WriteString(diff.Unified(old, string(n.Data), "current "+rel+"/"+name, "new "+rel+"/"+name))
		}
	}
}
