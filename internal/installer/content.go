package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentfile/internal/fsnode"
	"agentfile/internal/profile"
	"agentfile/internal/render"
	"agentfile/internal/state"
)

func contentAction(cur, want *fsnode.Node, restore bool) Action {
	switch {
	case fsnode.ContentFingerprint(cur) == fsnode.ContentFingerprint(want):
		return Unchanged
	case want.Type == fsnode.Absent:
		return Remove
	case cur.Type == fsnode.Absent && restore:
		return Recreate
	case cur.Type == fsnode.Absent:
		return Create
	}
	return UpdateInPlace
}

// skillNote names the unmanaged entries beside a managed SKILL.md.
func skillNote(path string) string {
	if filepath.Base(path) != skillFile {
		return ""
	}
	dir := filepath.Dir(path)
	if fsnode.CheckPlainDir(dir) != nil {
		return ""
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var others []string
	for _, e := range ents {
		if e.Name() != skillFile {
			others = append(others, e.Name())
		}
	}
	if len(others) == 0 {
		return ""
	}
	return fmt.Sprintf("Only %s is managed; other entries in %s stay untouched: %s", skillFile, filepath.Base(dir), strings.Join(others, ", "))
}

// contentItem fills in an item's current state in content-only mode.
func (e Env) contentItem(it *Item) {
	it.Current = &fsnode.Node{Type: fsnode.Absent}
	if err := e.checkParents(it.Agent); err != nil {
		it.Blocked = err.Error()
	} else if cur, err := fsnode.CaptureContent(it.Path); err != nil {
		it.Blocked = err.Error()
	} else {
		it.Current = cur
		it.Note = skillNote(it.Path)
	}
}

func (e Env) planContentInstall(st *state.Store, p profile.Profile, arts []render.Artifact) (*Plan, error) {
	installed := st.LastInstalled()
	plan := &Plan{Kind: "apply", Profile: p}
	for _, a := range arts {
		path := filepath.Join(e.AgentHome(a.Agent), filepath.FromSlash(a.Path))
		data := a.Data
		if a.IsDir() {
			if len(a.Files) != 1 || a.Files[skillFile] == nil {
				return nil, fmt.Errorf("%s: content-only mode manages only %s in a named skill directory", a.Path, skillFile)
			}
			path = filepath.Join(path, skillFile)
			data = a.Files[skillFile]
		}
		it := Item{Agent: a.Agent, Path: path, Want: &fsnode.Node{Type: fsnode.File, Data: data}}
		e.contentItem(&it)
		it.Action = contentAction(it.Current, it.Want, false)
		if it.Blocked == "" {
			it.Status = e.targetStatus(path, installed)
		}
		it.Conflict = it.Current.Type != fsnode.Absent && it.Action != Unchanged
		it.Replace = !it.Conflict
		plan.Items = append(plan.Items, it)
	}
	return plan, nil
}

func (e Env) planContentRestore(st *state.Store, m *state.Manifest) (*Plan, error) {
	id := m.ID
	entries, dirAgents := map[string]profile.Agent{}, map[string]profile.Agent{}
	for _, a := range []profile.Agent{profile.Claude, profile.Codex} {
		for _, p := range e.ManagedPaths(a) {
			entries[p] = a
		}
		for _, d := range e.managedDirs(a) {
			dirAgents[d] = a
		}
	}
	for _, d := range m.CreatedDirs {
		if _, ok := dirAgents[d]; !ok {
			return nil, fmt.Errorf("snapshot %s names a directory outside the managed homes: %s", id, d)
		}
	}
	removed := false
	for _, saved := range m.RemovedDirStates {
		if _, ok := dirAgents[saved.Path]; !ok {
			return nil, fmt.Errorf("snapshot %s has invalid removed parent %s", id, saved.Path)
		}
		removed = removed || saved.Removed
	}
	for _, en := range m.Entries {
		if _, ok := entries[en.Path]; !ok || en.ParentDir || en.Mode != state.ModeContent {
			return nil, fmt.Errorf("snapshot %s names a path outside the managed set: %s", id, en.Path)
		}
	}
	if len(m.Entries) == 0 && !removed && len(m.CreatedDirs) == 0 {
		return nil, fmt.Errorf("snapshot %s has no managed entries", id)
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
		it := Item{Agent: dirAgents[saved.Path], Path: saved.Path, Want: saved.Node, ParentRestore: true, Replace: true}
		it.Current = &fsnode.Node{Type: fsnode.Absent}
		if cur, err := fsnode.CaptureContentDir(it.Path); err != nil {
			it.Blocked = err.Error()
			it.Action = CreateParent
		} else if cur.Type == fsnode.Absent {
			it.Action = CreateParent
			it.Status = "removed by that operation"
		} else {
			it.Current = cur
			it.Action = Unchanged
			it.Status = "parent already exists"
		}
		plan.Items = append(plan.Items, it)
	}
	for i, en := range m.Entries {
		it := Item{Agent: entries[en.Path], Path: en.Path, Want: origs[i]}
		e.contentItem(&it)
		it.Action = contentAction(it.Current, it.Want, true)
		changed := fsnode.ContentFingerprint(it.Current) != en.InstalledFP
		it.Status = "unchanged since that operation"
		if changed {
			it.Status = "changed since that operation"
		}
		if it.Current.Type == fsnode.Absent && changed {
			it.Status = "missing since that operation"
		}
		it.Conflict = changed && it.Action != Unchanged
		it.Replace = !it.Conflict
		plan.Items = append(plan.Items, it)
	}
	for _, d := range m.CreatedDirs {
		it := Item{Agent: dirAgents[d], Path: d, Want: &fsnode.Node{Type: fsnode.Absent}, ParentCleanup: true, Replace: true}
		it.Current = &fsnode.Node{Type: fsnode.Absent}
		if cur, err := fsnode.CaptureContentDir(d); err != nil {
			it.Blocked = err.Error()
		} else {
			it.Current = cur
		}
		it.Action = Unchanged
		if it.Current.Type != fsnode.Absent {
			it.Action = RemoveParent
			it.Status = "removed only if empty"
		}
		plan.Items = append(plan.Items, it)
	}
	return plan, nil
}

// contentChanges returns content-only file changes and every parent
// directory, parents first, that the chosen items need.
func (e Env) contentChanges(p *Plan) ([]state.Change, []string) {
	var cs []state.Change
	need := map[string]bool{}
	for _, it := range p.Items {
		if !it.Proceeds() || it.ParentCleanup {
			continue
		}
		if it.ParentRestore {
			e.addParents(need, it.Agent, it.Path)
			continue
		}
		cs = append(cs, state.Change{Path: it.Path, Want: it.Want, Expect: fsnode.ContentFingerprint(it.Current),
			ExpectID: it.Current.FileID, Mode: state.ModeContent})
		if it.Want.Type != fsnode.Absent {
			e.addParents(need, it.Agent, filepath.Dir(it.Path))
		}
	}
	var dirs []string
	for d := range need {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return cs, dirs
}

// addParents adds dir and its ancestors up to the agent home.
func (e Env) addParents(need map[string]bool, a profile.Agent, dir string) {
	home := e.AgentHome(a)
	for {
		need[dir] = true
		if dir == home {
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(dir, home) {
			return
		}
		dir = parent
	}
}
