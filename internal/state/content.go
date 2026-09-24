package state

// The content-only engine backs Windows installs. It manages the bytes of
// individual files: an existing file is updated in place through a handle
// whose identity and contents are rechecked, a missing one is created
// exclusively, and a created one is removed only while it still matches.
// In-place writes are not atomic, so recovery never overwrites bytes that
// differ from the backup without an explicit per-target choice.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentfile/internal/fsnode"
)

// RecoveryItem is a content-only target an interrupted operation left
// different from its verified backup.
type RecoveryItem struct {
	Path    string
	Current *fsnode.Node // bytes found now; Absent if missing
	Backup  *fsnode.Node // verified bytes from before the operation
	Token   string       // pass to RecoverWith to restore Backup over Current, or to keep Current
}

func recoveryToken(n *fsnode.Node) string {
	return fsnode.ContentFingerprint(n) + "/" + n.FileID
}

func checkContentDirs(dirs []string) error {
	for _, dir := range dirs {
		if err := fsnode.CheckPlainDir(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func contentChanged(err error) error {
	if errors.Is(err, fsnode.ErrContentChanged) {
		return fmt.Errorf("%w: %v", ErrChanged, err)
	}
	return err
}

func (s *Store) executeContent(changes []Change, meta Meta) (*Manifest, error) {
	if s.Pending() {
		return nil, ErrRecoveryNeeded
	}
	if err := s.lock(); err != nil {
		return nil, err
	}
	defer s.unlock()

	checks := append(append(append([]string{}, meta.CheckDirs...), meta.ParentDirs...), meta.CleanupDirs...)
	if err := checkContentDirs(checks); err != nil {
		return nil, err
	}
	var todo []Change
	var originals []*fsnode.Node
	for _, c := range changes {
		if c.Mode != ModeContent || c.ParentDir || c.ParentMetaOnly {
			return nil, fmt.Errorf("%s: a content-only transaction accepts only content-only file changes", c.Path)
		}
		if c.Want == nil || (c.Want.Type != fsnode.File && c.Want.Type != fsnode.Absent) {
			return nil, fmt.Errorf("%s: content-only mode writes only regular files", c.Path)
		}
		cur, err := fsnode.CaptureContent(c.Path)
		if err != nil {
			return nil, err
		}
		fp := fsnode.ContentFingerprint(cur)
		if fp != c.Expect || cur.FileID != c.ExpectID {
			return nil, fmt.Errorf("%w: %s", ErrChanged, c.Path)
		}
		if fp == fsnode.ContentFingerprint(c.Want) {
			continue
		}
		todo = append(todo, c)
		originals = append(originals, cur)
	}
	missingParents := false
	for _, d := range meta.ParentDirs {
		if _, err := os.Lstat(d); errors.Is(err, fs.ErrNotExist) {
			missingParents = true
		}
	}
	if len(todo) == 0 && len(meta.CleanupDirs) == 0 && !missingParents {
		return nil, nil
	}

	m, err := s.writeContentSnapshot(todo, originals, meta)
	if err != nil {
		return nil, err
	}
	j := &journal{Schema: schema, Snapshot: m.ID, Kind: meta.Kind, Mode: ModeContent, CheckDirs: checks}
	for i, c := range todo {
		j.Steps = append(j.Steps, step{Path: c.Path, Status: "pending", WantFP: fsnode.ContentFingerprint(c.Want),
			Mode: ModeContent, FileID: originals[i].FileID})
	}
	for i := len(meta.CleanupDirs) - 1; i >= 0; i-- {
		j.Cleanup = append(j.Cleanup, cleanupStep{Path: meta.CleanupDirs[i], Status: "pending"})
	}
	if err := writeJSON(s.journalPath(), j); err != nil {
		os.RemoveAll(filepath.Join(s.backupsDir(), m.ID))
		return nil, err
	}

	if err := s.runContent(j, m, todo, meta.ParentDirs); err != nil {
		if errors.Is(err, errCrash) {
			return nil, err
		}
		if rerr := s.rollbackContent(j, m, nil, nil, true); rerr != nil {
			return nil, fmt.Errorf("%w; rollback also failed (%v): the journal and snapshot %s are kept, choose Recover", err, rerr, m.ID)
		}
		return nil, fmt.Errorf("%w (all changes were rolled back)", err)
	}
	if len(todo) == 0 && len(m.RemovedDirs) == 0 && len(m.CreatedDirs) == 0 {
		if err := os.RemoveAll(filepath.Join(s.backupsDir(), m.ID)); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return m, nil
}

func (s *Store) writeContentSnapshot(todo []Change, originals []*fsnode.Node, meta Meta) (*Manifest, error) {
	m := &Manifest{
		Schema: schema, ID: newSnapshotID(time.Now()), Created: time.Now().UTC(),
		Kind: meta.Kind, State: "prepared", OS: hostOS, Mode: ModeContent, Agents: meta.Agents,
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
			Path: c.Path, OriginalType: originals[i].Type, OriginalFP: fsnode.ContentFingerprint(originals[i]),
			OriginalFileID: originals[i].FileID, Mode: ModeContent,
			Artifact: rel, ArtifactSHA256: hex.EncodeToString(sum[:]), Size: int64(len(b)),
		})
	}
	for _, d := range meta.CleanupDirs {
		n, err := fsnode.CaptureContentDir(d)
		if err != nil {
			return fail(err)
		}
		if n.Type == fsnode.Absent {
			continue
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

// applyContent brings path from cur to want, rechecking cur on the opened
// file. It returns the identity of the file left at path, if any.
func applyContent(path string, cur, want *fsnode.Node) (string, error) {
	switch {
	case want.Type == fsnode.Absent && cur.Type == fsnode.Absent:
		return "", nil
	case want.Type == fsnode.Absent:
		return "", fsnode.RemoveContent(path, cur.FileID, fsnode.ContentFingerprint(cur))
	case cur.Type == fsnode.Absent:
		return fsnode.CreateContent(path, want.Data)
	}
	return fsnode.WriteContent(path, cur.FileID, fsnode.ContentFingerprint(cur), want.Data)
}

func (s *Store) runContent(j *journal, m *Manifest, todo []Change, parents []string) error {
	if err := checkContentDirs(j.CheckDirs); err != nil {
		return err
	}
	if err := s.createParents(j, parents); err != nil {
		return err
	}
	for i, c := range todo {
		if err := hook("step", i); err != nil {
			return err
		}
		if err := checkContentDirs(j.CheckDirs); err != nil {
			return err
		}
		if _, err := s.original(m, i); err != nil {
			return fmt.Errorf("snapshot did not verify before writing %s: %w", c.Path, err)
		}
		cur, err := fsnode.CaptureContent(c.Path)
		if err != nil {
			return err
		}
		if fsnode.ContentFingerprint(cur) != m.Entries[i].OriginalFP || cur.FileID != m.Entries[i].OriginalFileID {
			return fmt.Errorf("%w: %s", ErrChanged, c.Path)
		}
		j.Steps[i].Status = "started"
		if err := writeJSON(s.journalPath(), j); err != nil {
			return err
		}
		if err := hook("started", i); err != nil {
			return err
		}
		id, err := applyContent(c.Path, cur, c.Want)
		if errors.Is(err, fsnode.ErrContentChanged) {
			// Refused on the opened file before any byte changed.
			j.Steps[i].Status = "pending"
			if werr := writeJSON(s.journalPath(), j); werr != nil {
				return werr
			}
		}
		if err != nil {
			return contentChanged(err)
		}
		if err := hook("written", i); err != nil {
			return err
		}
		j.Steps[i].FileID = id
		j.Steps[i].Status = "done"
		if err := writeJSON(s.journalPath(), j); err != nil {
			return err
		}
	}
	for i := range j.Cleanup {
		d := j.Cleanup[i].Path
		n, err := fsnode.CaptureContentDir(d)
		if err != nil {
			return err
		}
		if n.Type == fsnode.Absent || findDirState(m.RemovedDirStates, d) == nil {
			j.Cleanup[i].Status = "skipped"
		} else {
			j.Cleanup[i].Status = "started"
			if err := writeJSON(s.journalPath(), j); err != nil {
				return err
			}
			err = os.Remove(d)
			switch {
			case err == nil:
				j.Cleanup[i].Status = "done"
				m.RemovedDirs = append(m.RemovedDirs, d)
				findDirState(m.RemovedDirStates, d).Removed = true
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
	if err := hook("commit", len(todo)); err != nil {
		return err
	}
	for i, c := range todo {
		got, err := fsnode.CaptureContent(c.Path)
		if err != nil {
			return err
		}
		fp := fsnode.ContentFingerprint(got)
		if fp != j.Steps[i].WantFP || got.FileID != j.Steps[i].FileID {
			return fmt.Errorf("%s did not verify after writing", c.Path)
		}
		m.Entries[i].InstalledFP = fp
		m.Entries[i].InstalledFileID = got.FileID
	}
	m.CreatedDirs = j.CreatedDirs
	for _, d := range j.CreatedDirs {
		n, err := fsnode.CaptureContentDir(d)
		if err != nil {
			return err
		}
		if n.Type != fsnode.Dir {
			return fmt.Errorf("created parent %s disappeared", d)
		}
		m.CreatedDirStates = append(m.CreatedDirStates, DirState{Path: d, Node: n, MetaFP: fsnode.RestoreFingerprint(n)})
	}
	m.State = "committed"
	if err := s.saveManifest(m); err != nil {
		return err
	}
	return os.Remove(s.journalPath())
}

// validContentJournal checks that j belongs to content snapshot m.
func validContentJournal(j *journal, m *Manifest) error {
	if j.Schema != schema || j.Snapshot != m.ID || j.Kind != m.Kind || j.Mode != ModeContent ||
		m.Mode != ModeContent || len(j.Steps) != len(m.Entries) {
		return fmt.Errorf("journal does not match snapshot %s", m.ID)
	}
	for i, st := range j.Steps {
		if st.Path != m.Entries[i].Path || st.Mode != ModeContent || st.WantFP == "" {
			return fmt.Errorf("journal step %d does not match snapshot %s", i, m.ID)
		}
		if st.Status != "pending" && st.Status != "started" && st.Status != "done" {
			return fmt.Errorf("invalid journal status %q for %s", st.Status, st.Path)
		}
	}
	return nil
}

// RecoveryReview lists, without changing anything, each content-only
// target that an interrupted operation left different from its backup.
// Exact-mode journals have no per-target choices and return none.
func (s *Store) RecoveryReview() ([]RecoveryItem, error) {
	j, m, err := s.pendingJournal()
	if err != nil || j == nil || j.Mode != ModeContent {
		return nil, err
	}
	if err := validContentJournal(j, m); err != nil {
		return nil, err
	}
	originals, err := s.Originals(m)
	if err != nil {
		return nil, err
	}
	var out []RecoveryItem
	for i, st := range j.Steps {
		if st.Status == "pending" {
			continue
		}
		cur, err := fsnode.CaptureContent(st.Path)
		if err != nil {
			return nil, err
		}
		if fsnode.ContentFingerprint(cur) != m.Entries[i].OriginalFP {
			out = append(out, RecoveryItem{Path: st.Path, Current: cur, Backup: originals[i], Token: recoveryToken(cur)})
		}
	}
	return out, nil
}

// rollbackContent restores the original bytes of every started step. A
// target that already matches its backup needs no write. Any other target
// is restored only if approved names its current token, or, when trustOwn
// is set during an in-process failure, if it still holds exactly the bytes
// and file this operation wrote. A target whose current token is in keep is
// left as it is. Otherwise nothing is changed and the journal and snapshot
// are kept. If anything is kept, the snapshot stays available to Restore.
func (s *Store) rollbackContent(j *journal, m *Manifest, approved, keep map[string]string, trustOwn bool) error {
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
	if err := validContentJournal(j, m); err != nil {
		return err
	}
	if err := checkContentDirs(j.CheckDirs); err != nil {
		return err
	}
	originals, err := s.Originals(m)
	if err != nil {
		return err
	}
	if err := VerifyDirStates(m); err != nil {
		return err
	}
	var restores, keeps []restoreStep
	var conflicts []string
	for i := len(j.Steps) - 1; i >= 0; i-- {
		st := j.Steps[i]
		if st.Status == "pending" {
			continue
		}
		cur, err := fsnode.CaptureContent(st.Path)
		if err != nil {
			return err
		}
		fp := fsnode.ContentFingerprint(cur)
		if fp == m.Entries[i].OriginalFP {
			continue
		}
		if approved[st.Path] != "" && keep[st.Path] != "" {
			return fmt.Errorf("%s: choose either restore or keep, not both", st.Path)
		}
		if keep[st.Path] != "" && keep[st.Path] == recoveryToken(cur) {
			keeps = append(keeps, restoreStep{i, cur})
			continue
		}
		own := trustOwn && st.Status == "done" && fp == st.WantFP && cur.FileID == st.FileID
		chosen := approved[st.Path] != "" && approved[st.Path] == recoveryToken(cur)
		if !own && !chosen {
			conflicts = append(conflicts, st.Path)
			continue
		}
		restores = append(restores, restoreStep{i, cur})
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w: %s differ from the backup after interruption; review each and choose whether to restore its backed-up bytes",
			ErrRecoveryConflict, strings.Join(conflicts, ", "))
	}
	for i := len(j.Cleanup) - 1; i >= 0; i-- {
		st := j.Cleanup[i]
		if st.Status != "started" && st.Status != "done" {
			continue
		}
		original := findDirState(m.RemovedDirStates, st.Path)
		if original == nil {
			return fmt.Errorf("journal cleanup %s has no directory snapshot", st.Path)
		}
		if _, err := os.Lstat(st.Path); errors.Is(err, fs.ErrNotExist) {
			if st.Status == "started" && !original.Removed {
				return fmt.Errorf("%w: cannot prove whether cleanup removed %s", ErrRecoveryConflict, st.Path)
			}
			if err := os.Mkdir(st.Path, 0o755); err != nil {
				return err
			}
			if err := hook("rollback-dir", i); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if err := fsnode.CheckPlainDir(st.Path); err != nil {
			return fmt.Errorf("%w: %v", ErrRecoveryConflict, err)
		}
	}
	for _, r := range restores {
		if _, err := applyContent(j.Steps[r.i].Path, r.cur, originals[r.i]); err != nil {
			if errors.Is(err, fsnode.ErrContentChanged) {
				return fmt.Errorf("%w: %v", ErrRecoveryConflict, err)
			}
			return err
		}
	}
	removeEmptyDirs(j.CreatedDirs)
	m.State = "rolled-back"
	if len(keeps) > 0 {
		if err := keptSnapshot(m, j, keeps); err != nil {
			return err
		}
	}
	if err := s.saveManifest(m); err != nil {
		return err
	}
	return os.Remove(s.journalPath())
}

// restoreStep is a journal step index and the target's current state.
type restoreStep struct {
	i   int
	cur *fsnode.Node
}

// keptSnapshot commits m as a backup of the state recovery left: kept files
// are recorded as installed, and parents this operation created that still
// hold them can be removed by a later Restore. Restored and untouched files
// have no installed state.
func keptSnapshot(m *Manifest, j *journal, keeps []restoreStep) error {
	for i := range m.Entries {
		m.Entries[i].InstalledFP, m.Entries[i].InstalledFileID = "", ""
	}
	for _, k := range keeps {
		m.Entries[k.i].InstalledFP = fsnode.ContentFingerprint(k.cur)
		m.Entries[k.i].InstalledFileID = k.cur.FileID
	}
	m.CreatedDirs, m.CreatedDirStates, m.RemovedDirs = nil, nil, nil
	for i := range m.RemovedDirStates {
		m.RemovedDirStates[i].Removed = false
	}
	for _, d := range j.CreatedDirs {
		n, err := fsnode.CaptureContentDir(d)
		if err != nil {
			return err
		}
		if n.Type == fsnode.Dir {
			m.CreatedDirs = append(m.CreatedDirs, d)
			m.CreatedDirStates = append(m.CreatedDirStates, DirState{Path: d, Node: n, MetaFP: fsnode.RestoreFingerprint(n)})
		}
	}
	m.State = "committed"
	return nil
}
