package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"agentfile/internal/fsnode"
)

const schema = 1

// ModeContent marks Windows content-only changes, snapshot entries and
// journal steps: existing files are updated in place and only their bytes
// are backed up and restored. An empty mode is the exact whole-entry mode.
const ModeContent = "content"

// Change is one managed path to bring to Want. Expect is the fingerprint
// shown in the preview; the change aborts if the path no longer matches it.
type Change struct {
	Path           string
	Want           *fsnode.Node
	Expect         string
	ExpectMeta     string // set for Restore, including supported metadata
	ParentDir      bool   // shallow parent entry; never capture or remove its children
	ParentMetaOnly bool   // update an existing parent's metadata in place
	Mode           string // ModeContent, or empty for exact mode
	ExpectID       string // content mode: file identity shown in the preview
}

func captureChange(c Change) (*fsnode.Node, error) {
	if !c.ParentDir {
		return fsnode.Capture(c.Path)
	}
	n, err := fsnode.CaptureShallowDir(c.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return &fsnode.Node{Type: fsnode.Absent}, nil
	}
	return n, err
}

// Meta describes the operation for the manifest.
type Meta struct {
	Kind           string // "apply" or "restore"
	Agents         []string
	Profile        string
	ContentVersion string
	UpstreamCommit string
	RestoredFrom   string
	// ParentDirs must exist before the changes; missing ones are created,
	// in order, and recorded so Restore can remove them if left empty.
	ParentDirs []string
	// CheckDirs are parent directories that may never be symlinks or files.
	CheckDirs []string
	// CleanupDirs are installer-created parents to remove if empty after restore.
	CleanupDirs []string
	// CleanupExpected rechecks each chosen parent's metadata from the preview.
	CleanupExpected map[string]string
	// Mode is ModeContent for a content-only transaction, else exact.
	Mode string
}

type DirState struct {
	Path    string       `json:"path"`
	Node    *fsnode.Node `json:"node"`
	MetaFP  string       `json:"metadata_fingerprint"`
	Removed bool         `json:"removed,omitempty"`
}

type Entry struct {
	Path            string      `json:"path"`
	OriginalType    fsnode.Type `json:"original_type"`
	OriginalFP      string      `json:"original_fingerprint"`
	OriginalMetaFP  string      `json:"original_metadata_fingerprint,omitempty"`
	Artifact        string      `json:"artifact"`
	ArtifactSHA256  string      `json:"artifact_sha256"`
	InstalledFP     string      `json:"installed_fingerprint"`
	InstalledMetaFP string      `json:"installed_metadata_fingerprint,omitempty"`
	ParentDir       bool        `json:"parent_dir,omitempty"`
	ParentMetaOnly  bool        `json:"parent_meta_only,omitempty"`
	Size            int64       `json:"size"`
	Mode            string      `json:"mode,omitempty"`
	OriginalFileID  string      `json:"original_file_id,omitempty"`
	InstalledFileID string      `json:"installed_file_id,omitempty"`
}

type Manifest struct {
	Schema           int        `json:"schema"`
	ID               string     `json:"id"`
	Created          time.Time  `json:"created"`
	Kind             string     `json:"kind"`
	State            string     `json:"state"` // prepared, committed, rolled-back
	Mode             string     `json:"mode,omitempty"`
	OS               string     `json:"os"`
	Agents           []string   `json:"agents"`
	Profile          string     `json:"profile"`
	ContentVersion   string     `json:"content_version"`
	UpstreamCommit   string     `json:"upstream_commit,omitempty"`
	RestoredFrom     string     `json:"restored_from,omitempty"`
	Entries          []Entry    `json:"entries"`
	CreatedDirs      []string   `json:"created_dirs,omitempty"`
	RemovedDirs      []string   `json:"removed_dirs,omitempty"`
	CreatedDirStates []DirState `json:"created_dir_states,omitempty"`
	RemovedDirStates []DirState `json:"removed_dir_states,omitempty"`
}

// VerifyDirStates detects corruption in the shallow parent metadata stored
// inline with a snapshot, before Restore or Recover uses it.
func VerifyDirStates(m *Manifest) error {
	for _, states := range [][]DirState{m.CreatedDirStates, m.RemovedDirStates} {
		for _, saved := range states {
			if saved.Node == nil || saved.Node.Type != fsnode.Dir || len(saved.Node.Children) != 0 ||
				saved.MetaFP == "" || fsnode.RestoreFingerprint(saved.Node) != saved.MetaFP {
				return fmt.Errorf("snapshot %s has corrupt parent metadata for %s", m.ID, saved.Path)
			}
		}
	}
	return nil
}

type step struct {
	Path           string `json:"path"`
	Status         string `json:"status"` // pending, started, done
	WantFP         string `json:"want_fingerprint"`
	WantMetaFP     string `json:"want_metadata_fingerprint,omitempty"`
	WantStableFP   string `json:"want_stable_fingerprint,omitempty"`
	ParentDir      bool   `json:"parent_dir,omitempty"`
	ParentMetaOnly bool   `json:"parent_meta_only,omitempty"`
	Mode           string `json:"mode,omitempty"`
	FileID         string `json:"file_id,omitempty"` // content mode: identity the target has before, then after, the write
}

type cleanupStep struct {
	Path   string `json:"path"`
	Status string `json:"status"` // pending, started, done, skipped
}

type journal struct {
	Schema      int           `json:"schema"`
	Snapshot    string        `json:"snapshot"`
	Kind        string        `json:"kind"`
	Steps       []step        `json:"steps"`
	CreatedDirs []string      `json:"created_dirs"`
	CreatingDir string        `json:"creating_dir,omitempty"`
	CheckDirs   []string      `json:"check_dirs,omitempty"`
	Cleanup     []cleanupStep `json:"cleanup,omitempty"`
	Mode        string        `json:"mode,omitempty"`
}

var (
	// ErrChanged means a target changed after the preview; nothing was written.
	ErrChanged = errors.New("target changed after preview")
	// ErrRecoveryConflict means a touched target changed after interruption.
	ErrRecoveryConflict = errors.New("recovery conflict; journal and snapshot retained")
	// ErrRecoveryNeeded means an interrupted operation must be recovered first.
	ErrRecoveryNeeded = errors.New("an interrupted operation must be recovered first")
	// errCrash is returned by testHook to simulate a crash: no rollback runs.
	errCrash = errors.New("simulated crash")
	// testHook, when set, runs before each step and may inject failures.
	testHook func(stage string, i int) error
)

func hook(stage string, i int) error {
	if testHook != nil {
		return testHook(stage, i)
	}
	return nil
}

// Pending reports whether an interrupted operation left a journal.
func (s *Store) Pending() bool {
	_, err := os.Lstat(s.journalPath())
	return err == nil
}

// Execute applies changes as one journaled transaction: it rechecks every
// target, writes and verifies a snapshot of only the entries it will change,
// then replaces entries one at a time, rolling back on failure. A no-op
// returns (nil, nil) and writes nothing.
func (s *Store) Execute(changes []Change, meta Meta) (*Manifest, error) {
	if meta.Mode == ModeContent {
		return s.executeContent(changes, meta)
	}
	if meta.Mode != "" {
		return nil, fmt.Errorf("unknown transaction mode %q", meta.Mode)
	}
	for _, c := range changes {
		if c.Mode != "" {
			return nil, fmt.Errorf("%s: %s change in an exact transaction", c.Path, c.Mode)
		}
	}
	if s.Pending() {
		return nil, ErrRecoveryNeeded
	}
	if err := s.lock(); err != nil {
		return nil, err
	}
	defer s.unlock()

	var todo []Change
	var originals []*fsnode.Node
	for _, c := range changes {
		c.Want = fsnode.Canonical(c.Want)
		cur, err := captureChange(c)
		if err != nil {
			return nil, err
		}
		fp := fsnode.Fingerprint(cur)
		if fp != c.Expect {
			return nil, fmt.Errorf("%w: %s", ErrChanged, c.Path)
		}
		metaFP := fsnode.RestoreFingerprint(cur)
		if c.ExpectMeta != "" && metaFP != c.ExpectMeta {
			return nil, fmt.Errorf("%w: %s", ErrChanged, c.Path)
		}
		if fp == fsnode.Fingerprint(c.Want) &&
			(c.ExpectMeta == "" || metaFP == fsnode.RestoreFingerprint(c.Want)) {
			continue
		}
		todo = append(todo, c)
		originals = append(originals, cur)
	}
	if len(todo) == 0 && len(meta.CleanupDirs) == 0 {
		return nil, nil
	}

	m, err := s.writeSnapshot(todo, originals, meta)
	if err != nil {
		return nil, err
	}
	j := &journal{Schema: schema, Snapshot: m.ID, Kind: meta.Kind,
		CheckDirs: append(append(append([]string{}, meta.CheckDirs...), meta.ParentDirs...), meta.CleanupDirs...)}
	for _, c := range todo {
		st := step{Path: c.Path, Status: "pending", WantFP: fsnode.Fingerprint(c.Want), ParentDir: c.ParentDir, ParentMetaOnly: c.ParentMetaOnly}
		if c.ExpectMeta != "" {
			st.WantMetaFP = fsnode.RestoreFingerprint(c.Want)
		}
		if c.ParentDir {
			if c.ParentMetaOnly {
				st.WantStableFP = stableDirMetaFP(c.Want)
			} else {
				st.WantStableFP = createdParentStableFP(c.Want)
			}
		}
		j.Steps = append(j.Steps, st)
	}
	for i := len(meta.CleanupDirs) - 1; i >= 0; i-- {
		j.Cleanup = append(j.Cleanup, cleanupStep{Path: meta.CleanupDirs[i], Status: "pending"})
	}
	if err := writeJSON(s.journalPath(), j); err != nil {
		os.RemoveAll(filepath.Join(s.backupsDir(), m.ID))
		return nil, err
	}

	if err := s.run(j, m, todo, meta.ParentDirs); err != nil {
		if errors.Is(err, errCrash) {
			return nil, err
		}
		if rerr := s.rollback(j, m); rerr != nil {
			return nil, fmt.Errorf("%w; rollback also failed (%v): the journal and snapshot %s are kept, choose Recover", err, rerr, m.ID)
		}
		return nil, fmt.Errorf("%w (all changes were rolled back)", err)
	}
	if len(todo) == 0 && len(m.RemovedDirs) == 0 {
		if err := os.RemoveAll(filepath.Join(s.backupsDir(), m.ID)); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return m, nil
}

func (s *Store) run(j *journal, m *Manifest, todo []Change, parents []string) error {
	if err := checkDirs(j.CheckDirs); err != nil {
		return err
	}
	if err := s.createParents(j, parents); err != nil {
		return err
	}
	for i, c := range todo {
		if err := hook("step", i); err != nil {
			return err
		}
		if err := checkDirs(j.CheckDirs); err != nil {
			return err
		}
		cur, err := captureChange(c)
		if err != nil {
			return err
		}
		if fsnode.Fingerprint(cur) != m.Entries[i].OriginalFP {
			return fmt.Errorf("%w: %s", ErrChanged, c.Path)
		}
		if c.ExpectMeta != "" && fsnode.RestoreFingerprint(cur) != m.Entries[i].OriginalMetaFP {
			return fmt.Errorf("%w: %s", ErrChanged, c.Path)
		}
		j.Steps[i].Status = "started"
		if err := writeJSON(s.journalPath(), j); err != nil {
			return err
		}
		if err := hook("started", i); err != nil {
			return err
		}
		if c.ParentMetaOnly {
			if err := fsnode.ApplyMeta(c.Path, c.Want); err != nil {
				return err
			}
		} else if err := replace(c.Path, c.Want, m.ID, func(tmp string) error {
			if j.Steps[i].WantMetaFP != "" {
				return nil
			}
			// Apply cannot predict written metadata such as mtime, so record
			// the staged entry before the target changes.
			staged, err := captureChange(Change{Path: tmp, ParentDir: c.ParentDir})
			if err != nil {
				return err
			}
			j.Steps[i].WantMetaFP = fsnode.RestoreFingerprint(staged)
			return writeJSON(s.journalPath(), j)
		}); err != nil {
			return err
		}
		if err := hook("replaced", i); err != nil {
			return err
		}
		written, err := captureChange(c)
		if err != nil {
			return err
		}
		j.Steps[i].WantMetaFP = fsnode.RestoreFingerprint(written)
		if c.ParentDir {
			if c.ParentMetaOnly {
				j.Steps[i].WantStableFP = stableDirMetaFP(written)
			} else {
				j.Steps[i].WantStableFP = createdParentStableFP(written)
			}
		}
		j.Steps[i].Status = "done"
		if err := writeJSON(s.journalPath(), j); err != nil {
			return err
		}
	}
	for i := range j.Cleanup {
		d := j.Cleanup[i].Path
		n, err := fsnode.CaptureShallowDir(d)
		if errors.Is(err, fs.ErrNotExist) {
			j.Cleanup[i].Status = "skipped"
		} else if err != nil {
			return err
		} else {
			original := findDirState(m.RemovedDirStates, d)
			if original == nil || stableDirMetaFP(n) != stableDirMetaFP(original.Node) {
				return fmt.Errorf("%w: created parent metadata changed at %s", ErrChanged, d)
			}
			j.Cleanup[i].Status = "started"
			if err := writeJSON(s.journalPath(), j); err != nil {
				return err
			}
			err = os.Remove(d)
			switch {
			case err == nil:
				j.Cleanup[i].Status = "done"
				m.RemovedDirs = append(m.RemovedDirs, d)
				original.Removed = true
			case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrExist), errors.Is(err, syscall.ENOTEMPTY):
				j.Cleanup[i].Status = "skipped"
			default:
				return err
			}
		}
		if err := writeJSON(s.journalPath(), j); err != nil {
			return err
		}
	}
	if err := hook("after-cleanup", len(j.Cleanup)); err != nil {
		return err
	}
	for i := len(todo) - 1; i >= 0; i-- {
		if !todo[i].ParentDir {
			continue
		}
		if err := fsnode.ApplyMeta(todo[i].Path, todo[i].Want); err != nil {
			return err
		}
		got, err := captureChange(todo[i])
		if err != nil {
			return err
		}
		j.Steps[i].WantMetaFP = fsnode.RestoreFingerprint(got)
		if err := writeJSON(s.journalPath(), j); err != nil {
			return err
		}
	}
	if err := hook("commit", len(todo)); err != nil {
		return err
	}
	for i, c := range todo {
		got, err := captureChange(c)
		if err != nil {
			return err
		}
		if fp := fsnode.Fingerprint(got); fp != fsnode.Fingerprint(c.Want) {
			return fmt.Errorf("%s did not verify after writing", c.Path)
		} else {
			m.Entries[i].InstalledFP = fp
		}
		m.Entries[i].InstalledMetaFP = fsnode.RestoreFingerprint(got)
		if c.ExpectMeta != "" && m.Entries[i].InstalledMetaFP != fsnode.RestoreFingerprint(c.Want) {
			return fmt.Errorf("%s metadata did not verify after writing", c.Path)
		}
	}
	m.CreatedDirs = j.CreatedDirs
	for _, d := range j.CreatedDirs {
		n, err := fsnode.CaptureShallowDir(d)
		if err != nil {
			return err
		}
		m.CreatedDirStates = append(m.CreatedDirStates, DirState{Path: d, Node: n, MetaFP: fsnode.RestoreFingerprint(n)})
	}
	m.State = "committed"
	if err := s.saveManifest(m); err != nil {
		return err
	}
	return os.Remove(s.journalPath())
}

// createParents creates missing parents in order, journaling each one
// before and after mkdir so recovery knows which it created.
func (s *Store) createParents(j *journal, parents []string) error {
	for i, d := range parents {
		if _, err := os.Lstat(d); err == nil {
			continue
		}
		j.CreatingDir = d
		if err := writeJSON(s.journalPath(), j); err != nil {
			return err
		}
		if err := hook("parent", i); err != nil {
			return err
		}
		if err := os.Mkdir(d, 0o755); err != nil {
			j.CreatingDir = "" // a failed mkdir created nothing
			return err
		}
		if err := hook("parent-created", i); err != nil {
			return err
		}
		j.CreatedDirs = append(j.CreatedDirs, d)
		j.CreatingDir = ""
		if err := writeJSON(s.journalPath(), j); err != nil {
			return err
		}
	}
	return nil
}

func findDirState(states []DirState, path string) *DirState {
	for i := range states {
		if states[i].Path == path {
			return &states[i]
		}
	}
	return nil
}

func stableDirMetaFP(n *fsnode.Node) string {
	c := *n
	c.ModTime = time.Time{}
	return fsnode.RestoreFingerprint(&c)
}

func createdParentStableFP(n *fsnode.Node) string {
	c := *n
	c.ModTime = time.Time{}
	c.Gid = nil
	c.Attrs = 0
	return fsnode.RestoreFingerprint(&c)
}

// replace brings path to want. Non-directory replacements use one atomic
// rename; directory swaps move the old entry aside first. staged runs after
// the new entry is written beside path and before path changes.
func replace(path string, want *fsnode.Node, txid string, staged func(tmp string) error) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	tmp := filepath.Join(dir, ".agentfile-new-"+txid+"-"+base)
	old := filepath.Join(dir, ".agentfile-old-"+txid+"-"+base)
	if err := fsnode.RemoveAll(tmp); err != nil {
		return err
	}
	if want.Type != fsnode.Absent {
		if err := fsnode.Write(tmp, want); err != nil {
			fsnode.RemoveAll(tmp)
			return err
		}
	}
	if err := staged(tmp); err != nil {
		fsnode.RemoveAll(tmp)
		return err
	}
	fi, err := os.Lstat(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	atomic := exists && !fi.IsDir() && want.Type != fsnode.Absent && want.Type != fsnode.Dir
	switch {
	case atomic || (!exists && want.Type != fsnode.Absent):
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	case exists:
		if err := fsnode.RemoveAll(old); err != nil {
			return err
		}
		if err := os.Rename(path, old); err != nil {
			return err
		}
		if want.Type != fsnode.Absent {
			if err := os.Rename(tmp, path); err != nil {
				return err
			}
		}
		if err := fsnode.RemoveAll(old); err != nil {
			return err
		}
	}
	return fsnode.SyncDir(dir)
}

func (s *Store) writeSnapshot(todo []Change, originals []*fsnode.Node, meta Meta) (*Manifest, error) {
	m := &Manifest{
		Schema: schema, ID: newSnapshotID(time.Now()), Created: time.Now().UTC(),
		Kind: meta.Kind, State: "prepared", OS: hostOS, Agents: meta.Agents,
		Profile: meta.Profile, ContentVersion: meta.ContentVersion,
		UpstreamCommit: meta.UpstreamCommit, RestoredFrom: meta.RestoredFrom,
	}
	dir := filepath.Join(s.backupsDir(), m.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(dir, "entries"), 0o700); err != nil {
		return nil, err
	}
	fail := func(err error) (*Manifest, error) {
		os.RemoveAll(dir)
		return nil, err
	}
	for i, c := range todo {
		b, err := json.Marshal(originals[i])
		if err != nil {
			return fail(err)
		}
		rel := fmt.Sprintf("entries/%d.json", i)
		if err := writeDurable(filepath.Join(dir, filepath.FromSlash(rel)), b); err != nil {
			return fail(err)
		}
		sum := sha256.Sum256(b)
		m.Entries = append(m.Entries, Entry{
			Path: c.Path, OriginalType: originals[i].Type, OriginalFP: fsnode.Fingerprint(originals[i]),
			OriginalMetaFP: fsnode.RestoreFingerprint(originals[i]),
			ParentDir:      c.ParentDir,
			ParentMetaOnly: c.ParentMetaOnly,
			Artifact:       rel, ArtifactSHA256: hex.EncodeToString(sum[:]), Size: int64(len(b)),
		})
	}
	for _, d := range meta.CleanupDirs {
		n, err := fsnode.CaptureShallowDir(d)
		if err != nil {
			return fail(err)
		}
		if expect := meta.CleanupExpected[d]; expect != "" && fsnode.RestoreFingerprint(n) != expect {
			return fail(fmt.Errorf("%w: %s", ErrChanged, d))
		}
		m.RemovedDirStates = append(m.RemovedDirStates, DirState{Path: d, Node: n, MetaFP: fsnode.RestoreFingerprint(n)})
	}
	if err := fsnode.SyncDir(filepath.Join(dir, "entries")); err != nil {
		return fail(err)
	}
	if err := s.saveManifest(m); err != nil {
		return fail(err)
	}
	if err := fsnode.SyncDir(s.backupsDir()); err != nil {
		return fail(err)
	}
	// Verify everything round-trips before any target changes.
	loaded, err := s.Load(m.ID)
	if err != nil {
		return fail(fmt.Errorf("snapshot did not verify: %w", err))
	}
	if _, err := s.Originals(loaded); err != nil {
		return fail(fmt.Errorf("snapshot did not verify: %w", err))
	}
	if err := VerifyDirStates(loaded); err != nil {
		return fail(fmt.Errorf("snapshot did not verify: %w", err))
	}
	return m, nil
}

func (s *Store) saveManifest(m *Manifest) error {
	return writeJSON(filepath.Join(s.backupsDir(), m.ID, "manifest.json"), m)
}

// Load reads a manifest without checking its artifacts.
func (s *Store) Load(id string) (*Manifest, error) {
	if filepath.Base(id) != id || id == "." || id == ".." {
		return nil, fmt.Errorf("invalid snapshot id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(s.backupsDir(), id, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest %s is corrupt: %w", id, err)
	}
	if m.Schema != schema || m.ID != id {
		return nil, fmt.Errorf("manifest %s has unsupported schema or mismatched id", id)
	}
	return &m, nil
}

// Originals verifies every artifact's hash and fingerprint and returns the
// captured original nodes in entry order.
func (s *Store) Originals(m *Manifest) ([]*fsnode.Node, error) {
	var out []*fsnode.Node
	for i := range m.Entries {
		n, err := s.original(m, i)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// original verifies and returns one entry's captured original.
func (s *Store) original(m *Manifest, i int) (*fsnode.Node, error) {
	e := m.Entries[i]
	if e.Artifact != fmt.Sprintf("entries/%d.json", i) {
		return nil, fmt.Errorf("snapshot %s: invalid artifact path %q", m.ID, e.Artifact)
	}
	if e.Mode != m.Mode {
		return nil, fmt.Errorf("snapshot %s: %s has mode %q in a %q snapshot", m.ID, e.Artifact, e.Mode, m.Mode)
	}
	b, err := os.ReadFile(filepath.Join(s.backupsDir(), m.ID, filepath.FromSlash(e.Artifact)))
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", m.ID, err)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != e.ArtifactSHA256 {
		return nil, fmt.Errorf("snapshot %s: %s is corrupt (hash mismatch)", m.ID, e.Artifact)
	}
	var n fsnode.Node
	if err := json.Unmarshal(b, &n); err != nil {
		return nil, fmt.Errorf("snapshot %s: %s is corrupt: %w", m.ID, e.Artifact, err)
	}
	if e.Mode == ModeContent {
		if (n.Type != fsnode.File && n.Type != fsnode.Absent) || fsnode.ContentFingerprint(&n) != e.OriginalFP {
			return nil, fmt.Errorf("snapshot %s: %s does not match its recorded fingerprint", m.ID, e.Artifact)
		}
		return &n, nil
	}
	if fsnode.Fingerprint(&n) != e.OriginalFP {
		return nil, fmt.Errorf("snapshot %s: %s does not match its recorded fingerprint", m.ID, e.Artifact)
	}
	if e.OriginalMetaFP != "" && fsnode.RestoreFingerprint(&n) != e.OriginalMetaFP {
		return nil, fmt.Errorf("snapshot %s: %s metadata does not match its recorded fingerprint", m.ID, e.Artifact)
	}
	return &n, nil
}

// rollback restores the original of every started or completed step and
// removes directories the operation created, if still empty.
func (s *Store) rollback(j *journal, m *Manifest) error {
	if j.CreatingDir != "" {
		created := false
		for _, d := range j.CreatedDirs {
			created = created || d == j.CreatingDir
		}
		if !created {
			fi, err := os.Lstat(j.CreatingDir)
			if err == nil && fi.IsDir() {
				return fmt.Errorf("%w: cannot prove whether agentfile created %s; inspect it before retrying Recover", ErrRecoveryConflict, j.CreatingDir)
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	mutated := len(j.CreatedDirs) > 0
	for _, st := range j.Steps {
		mutated = mutated || st.Status != "pending"
	}
	for _, st := range j.Cleanup {
		mutated = mutated || st.Status == "started" || st.Status == "done"
	}
	if mutated {
		if err := checkDirs(j.CheckDirs); err != nil {
			return err
		}
	}
	if j.Schema != schema || j.Snapshot != m.ID || j.Kind != m.Kind || len(j.Steps) != len(m.Entries) {
		return fmt.Errorf("journal does not match snapshot %s", m.ID)
	}
	originals, err := s.Originals(m)
	if err != nil {
		return err
	}
	if err := VerifyDirStates(m); err != nil {
		return err
	}
	byPath := map[string]*fsnode.Node{}
	for i, e := range m.Entries {
		byPath[e.Path] = originals[i]
		if j.Steps[i].Path != e.Path || j.Steps[i].WantFP == "" {
			return fmt.Errorf("journal step %d does not match snapshot %s", i, m.ID)
		}
	}
	for i, st := range j.Steps {
		if err := checkRollbackTarget(st, m.Entries[i], m.ID); err != nil {
			return err
		}
	}
	recreatedDirs := map[string]*fsnode.Node{}
	for i := len(j.Cleanup) - 1; i >= 0; i-- {
		st := j.Cleanup[i]
		if st.Status != "started" && st.Status != "done" {
			continue
		}
		original := findDirState(m.RemovedDirStates, st.Path)
		if original == nil || original.Node == nil {
			return fmt.Errorf("journal cleanup %s has no directory snapshot", st.Path)
		}
		_, err := os.Lstat(st.Path)
		if errors.Is(err, fs.ErrNotExist) {
			if st.Status == "started" && !original.Removed {
				return fmt.Errorf("%w: cannot prove whether cleanup removed %s", ErrRecoveryConflict, st.Path)
			}
			if err := fsnode.Write(st.Path, original.Node); err != nil {
				return err
			}
			if err := hook("rollback-dir", i); err != nil {
				return err
			}
			recreatedDirs[st.Path] = original.Node
		} else if err != nil {
			return err
		} else if st.Status == "done" {
			current, err := fsnode.CaptureShallowDir(st.Path)
			if err != nil || stableDirMetaFP(current) != stableDirMetaFP(original.Node) {
				return fmt.Errorf("%w: removed parent %s appeared with different metadata", ErrRecoveryConflict, st.Path)
			}
			recreatedDirs[st.Path] = original.Node
		}
	}
	var firstErr error
	for i := len(j.Steps) - 1; i >= 0; i-- {
		st := j.Steps[i]
		if st.Status == "pending" {
			continue
		}
		orig, ok := byPath[st.Path]
		if !ok {
			return fmt.Errorf("journal step %s is missing from snapshot %s", st.Path, m.ID)
		}
		if err := checkRollbackTarget(st, m.Entries[i], m.ID); err != nil {
			return err
		}
		dir, base := filepath.Dir(st.Path), filepath.Base(st.Path)
		fsnode.RemoveAll(filepath.Join(dir, ".agentfile-new-"+m.ID+"-"+base))
		var restoreErr error
		if st.ParentMetaOnly {
			restoreErr = fsnode.ApplyMeta(st.Path, orig)
		} else if st.ParentDir {
			restoreErr = restoreParentEntry(st.Path, orig)
		} else {
			restoreErr = restoreEntry(st.Path, orig, m.ID)
		}
		if restoreErr != nil && firstErr == nil {
			firstErr = restoreErr
		}
		fsnode.RemoveAll(filepath.Join(dir, ".agentfile-old-"+m.ID+"-"+base))
	}
	if firstErr != nil {
		return firstErr
	}
	for _, st := range j.Cleanup {
		if n := recreatedDirs[st.Path]; n != nil {
			if err := fsnode.ApplyMeta(st.Path, n); err != nil {
				return err
			}
		}
	}
	removeEmptyDirs(j.CreatedDirs)
	m.State = "rolled-back"
	if err := s.saveManifest(m); err != nil {
		return err
	}
	return os.Remove(s.journalPath())
}

func restoreParentEntry(path string, orig *fsnode.Node) error {
	if orig.Type != fsnode.Absent {
		return fmt.Errorf("cannot replace an existing parent directory during rollback: %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: parent %s is no longer empty: %v", ErrRecoveryConflict, path, err)
	}
	return fsnode.SyncDir(filepath.Dir(path))
}

func checkRollbackTarget(st step, entry Entry, txid string) error {
	if st.Status == "pending" {
		return nil
	}
	if st.Status != "started" && st.Status != "done" {
		return fmt.Errorf("invalid journal status %q for %s", st.Status, st.Path)
	}
	cur, err := captureChange(Change{Path: st.Path, ParentDir: st.ParentDir})
	if err != nil {
		return err
	}
	fp := fsnode.Fingerprint(cur)
	metaFP := fsnode.RestoreFingerprint(cur)
	if fp == entry.OriginalFP && (entry.OriginalMetaFP == "" || metaFP == entry.OriginalMetaFP) {
		return nil
	}
	if fp == st.WantFP && st.WantMetaFP != "" && metaFP == st.WantMetaFP {
		return nil
	}
	if st.ParentDir && cur.Type == fsnode.Dir && st.WantStableFP != "" {
		if st.ParentMetaOnly && stableDirMetaFP(cur) == st.WantStableFP ||
			!st.ParentMetaOnly && createdParentStableFP(cur) == st.WantStableFP {
			return nil
		}
	}
	if st.Status == "started" && cur.Type == fsnode.Absent {
		old := filepath.Join(filepath.Dir(st.Path), ".agentfile-old-"+txid+"-"+filepath.Base(st.Path))
		aside, err := fsnode.Capture(old)
		if err == nil && aside.Type != fsnode.Absent &&
			fsnode.Fingerprint(aside) == entry.OriginalFP &&
			(entry.OriginalMetaFP == "" || fsnode.RestoreFingerprint(aside) == entry.OriginalMetaFP) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s changed after interruption", ErrRecoveryConflict, st.Path)
}

func checkDirs(dirs []string) error {
	for _, dir := range dirs {
		fi, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%s is not a real directory", dir)
		}
	}
	return nil
}

func restoreEntry(path string, orig *fsnode.Node, txid string) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	if orig.Type == fsnode.Absent {
		if err := fsnode.RemoveAll(path); err != nil {
			return err
		}
		return fsnode.SyncDir(dir)
	}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := filepath.Join(dir, ".agentfile-restore-"+txid+"-"+base)
	aside := filepath.Join(dir, ".agentfile-aside-"+txid+"-"+base)
	fsnode.RemoveAll(tmp)
	if err := fsnode.Write(tmp, orig); err != nil {
		fsnode.RemoveAll(tmp)
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		fsnode.RemoveAll(aside)
		if err := os.Rename(path, aside); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	fsnode.RemoveAll(aside)
	return fsnode.SyncDir(dir)
}

// removeEmptyDirs removes dirs in reverse creation order, skipping any that
// are not empty.
func removeEmptyDirs(dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // fails harmlessly when not empty
	}
}

// Recover rolls back an interrupted operation using its journal and
// snapshot, then clears a stale lock. Only call it when no other agentfile
// process is running. A content-only target that differs from its backup is
// a conflict; see RecoverWith.
func (s *Store) Recover() error { return s.RecoverWith(nil, nil) }

// RecoverWith is Recover that also restores backup bytes over each
// content-only target whose RecoveryReview token is in approved, and leaves
// each one whose token is in keep as it is, after rechecking that the target
// still matches that token.
func (s *Store) RecoverWith(approved, keep map[string]string) error {
	if err := s.ensure(); err != nil {
		return err
	}
	j, m, err := s.pendingJournal()
	if err != nil {
		return err
	}
	if j == nil {
		s.unlock()
		return nil
	}
	switch {
	case j.Mode == ModeContent:
		err = s.rollbackContent(j, m, approved, keep, false)
	case m.Mode != "":
		err = fmt.Errorf("journal does not match snapshot %s", m.ID)
	default:
		err = s.rollback(j, m)
	}
	if err != nil {
		return err
	}
	s.unlock()
	return nil
}

// pendingJournal reads the journal and its snapshot. A nil journal means no
// operation is pending. Exact-mode journals are refused on Windows.
func (s *Store) pendingJournal() (*journal, *Manifest, error) {
	b, err := os.ReadFile(s.journalPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var j journal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, nil, fmt.Errorf("journal is corrupt: %w; it is kept at %s", err, s.journalPath())
	}
	m, err := s.Load(j.Snapshot)
	if err != nil {
		return nil, nil, err
	}
	if hostOS == "windows" && (j.Mode != ModeContent || m.Mode != ModeContent) {
		return nil, nil, fmt.Errorf("the interrupted operation was made by an earlier exact-restore build, which this Windows build does not recover; nothing was changed. Its journal %s and snapshot %s are kept: check the paths recorded in them yourself, then move the journal aside",
			s.journalPath(), filepath.Join(s.backupsDir(), m.ID))
	}
	return &j, m, nil
}

// List returns committed snapshots, newest first.
func (s *Store) List() ([]*Manifest, error) {
	ents, err := os.ReadDir(s.backupsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		m, err := s.Load(e.Name())
		if err != nil || m.State != "committed" {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Created.After(out[k].Created) })
	return out, nil
}

// DiskSize is the on-disk size of a snapshot's files.
func (s *Store) DiskSize(id string) int64 {
	var total int64
	filepath.WalkDir(filepath.Join(s.backupsDir(), id), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// LastInstalled maps each managed path to the fingerprint agentfile most
// recently wrote there.
func (s *Store) LastInstalled() map[string]string {
	out := map[string]string{}
	list, err := s.List()
	if err != nil {
		return out
	}
	for i := len(list) - 1; i >= 0; i-- {
		for _, e := range list[i].Entries {
			if e.InstalledFP != "" { // empty: restored during recovery, never installed
				out[e.Path] = e.InstalledFP
			}
		}
	}
	return out
}
