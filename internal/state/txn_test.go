package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"agentfile/internal/fsnode"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	home := t.TempDir()
	root, err := DefaultRoot(runtime.GOOS, home, func(string) string {
		if runtime.GOOS == "windows" {
			return home
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, home
}

func file(data string) *fsnode.Node {
	return &fsnode.Node{Type: fsnode.File, Mode: 0o644, Data: []byte(data)}
}

func skillDir(data string) *fsnode.Node {
	return &fsnode.Node{Type: fsnode.Dir, Mode: 0o755, Children: map[string]*fsnode.Node{"SKILL.md": file(data)}}
}

func capture(t *testing.T, path string) *fsnode.Node {
	t.Helper()
	n, err := fsnode.Capture(path)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func fp(t *testing.T, path string) string { return fsnode.Fingerprint(capture(t, path)) }

func change(t *testing.T, path string, want *fsnode.Node) Change {
	return Change{Path: path, Want: want, Expect: fp(t, path)}
}

// setup creates a home with a customized file, a real skill directory, a
// symlinked skill, and an absent entry.
func setup(t *testing.T, home string) (paths []string, before map[string]*fsnode.Node) {
	t.Helper()
	agent := filepath.Join(home, ".agent")
	skills := filepath.Join(agent, "skills")
	must(t, os.MkdirAll(filepath.Join(skills, "handoff"), 0o755))
	must(t, os.WriteFile(filepath.Join(agent, "RULES.md"), []byte("user rules\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(skills, "handoff", "SKILL.md"), []byte("user skill\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(skills, "handoff", "notes.txt"), []byte("extra\n"), 0o640))
	old := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, os.Chtimes(filepath.Join(agent, "RULES.md"), old, old))
	if runtime.GOOS != "windows" {
		must(t, os.Symlink("../../elsewhere/routing", filepath.Join(skills, "routing")))
	}
	must(t, os.WriteFile(filepath.Join(skills, "unrelated"), []byte("keep me\n"), 0o644))
	paths = []string{
		filepath.Join(agent, "RULES.md"),
		filepath.Join(skills, "handoff"),
		filepath.Join(skills, "routing"),
		filepath.Join(skills, "consult"),
	}
	before = map[string]*fsnode.Node{}
	for _, p := range paths {
		before[p] = capture(t, p)
	}
	return paths, before
}

// exactRecovery skips tests that recover an exact-mode journal: Windows
// refuses those and recovers only content-only journals.
func exactRecovery(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows recovers only content-only journals; see content_test.go")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func applyAll(t *testing.T, s *Store, paths []string) *Manifest {
	t.Helper()
	var cs []Change
	for i, p := range paths {
		want := skillDir("managed " + p)
		if i == 0 {
			want = file("managed rules\n")
		}
		cs = append(cs, change(t, p, want))
	}
	m, err := s.Execute(cs, Meta{Kind: "apply", Agents: []string{"test"}, ContentVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func assertRestored(t *testing.T, before map[string]*fsnode.Node) {
	t.Helper()
	for p, want := range before {
		got := capture(t, p)
		if fsnode.Fingerprint(got) != fsnode.Fingerprint(want) {
			t.Errorf("%s: not restored (got %s, want %s)", p, got.Type, want.Type)
		}
		if want.Type == fsnode.File && !got.ModTime.Equal(want.ModTime) {
			t.Errorf("%s: mtime %v, want %v", p, got.ModTime, want.ModTime)
		}
	}
}

func restoreFrom(t *testing.T, s *Store, m *Manifest) *Manifest {
	t.Helper()
	origs, err := s.Originals(m)
	if err != nil {
		t.Fatal(err)
	}
	var cs []Change
	for i, e := range m.Entries {
		cs = append(cs, change(t, e.Path, origs[i]))
	}
	rm, err := s.Execute(cs, Meta{Kind: "restore", RestoredFrom: m.ID, CleanupDirs: m.CreatedDirs})
	if err != nil {
		t.Fatal(err)
	}
	return rm
}

func TestApplyThenRestoreIsExact(t *testing.T) {
	s, home := newStore(t)
	paths, before := setup(t, home)
	m := applyAll(t, s, paths)
	if m == nil || m.State != "committed" {
		t.Fatal("expected committed manifest")
	}
	for _, e := range m.Entries {
		if e.InstalledFP != fp(t, e.Path) {
			t.Errorf("%s: installed fingerprint not recorded", e.Path)
		}
	}
	if s.Pending() || s.Locked() {
		t.Fatal("journal or lock left behind")
	}
	restoreFrom(t, s, m)
	assertRestored(t, before)
	if b, _ := os.ReadFile(filepath.Join(home, ".agent", "skills", "unrelated")); string(b) != "keep me\n" {
		t.Error("unrelated entry touched")
	}
}

func TestNoOpWritesNothing(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	must(t, os.WriteFile(p, []byte("same\n"), 0o644))
	m, err := s.Execute([]Change{change(t, p, file("same\n"))}, Meta{Kind: "apply"})
	if err != nil || m != nil {
		t.Fatalf("no-op: m=%v err=%v", m, err)
	}
	if list, _ := s.List(); len(list) != 0 {
		t.Error("no-op created a backup")
	}
	ents, _ := os.ReadDir(s.backupsDir())
	if len(ents) != 0 {
		t.Error("no-op wrote into backups")
	}
}

func TestChangedAfterPreviewAborts(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	must(t, os.WriteFile(p, []byte("v1\n"), 0o644))
	c := change(t, p, file("managed\n"))
	must(t, os.WriteFile(p, []byte("edited after preview\n"), 0o644))
	_, err := s.Execute([]Change{c}, Meta{Kind: "apply"})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("want ErrChanged, got %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "edited after preview\n" {
		t.Error("edited file was overwritten")
	}
	if ents, _ := os.ReadDir(s.backupsDir()); len(ents) != 0 {
		t.Error("aborted apply left a snapshot")
	}
}

func TestChangedAfterSnapshotBeforeReplacementIsPreserved(t *testing.T) {
	s, home := newStore(t)
	first, later := filepath.Join(home, "first.md"), filepath.Join(home, "later.md")
	must(t, os.WriteFile(first, []byte("first original\n"), 0o644))
	must(t, os.WriteFile(later, []byte("later original\n"), 0o644))
	changes := []Change{change(t, first, file("first managed\n")), change(t, later, file("later managed\n"))}
	testHook = func(stage string, i int) error {
		if stage == "step" && i == 1 {
			return os.WriteFile(later, []byte("edited during apply\n"), 0o644)
		}
		return nil
	}
	defer func() { testHook = nil }()
	_, err := s.Execute(changes, Meta{Kind: "apply"})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("want ErrChanged, got %v", err)
	}
	if b, _ := os.ReadFile(first); string(b) != "first original\n" {
		t.Errorf("earlier step was not rolled back: %q", b)
	}
	if b, _ := os.ReadFile(later); string(b) != "edited during apply\n" {
		t.Errorf("concurrent edit was overwritten: %q", b)
	}
}

func TestParentSwappedToSymlinkAfterSnapshotIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link requires privileges on Windows")
	}
	s, home := newStore(t)
	parent := filepath.Join(home, ".agent")
	must(t, os.Mkdir(parent, 0o755))
	path := filepath.Join(parent, "RULES.md")
	must(t, os.WriteFile(path, []byte("original\n"), 0o644))
	outside := t.TempDir()
	outsidePath := filepath.Join(outside, "RULES.md")
	must(t, os.WriteFile(outsidePath, []byte("original\n"), 0o644))
	testHook = func(stage string, i int) error {
		if stage == "step" && i == 0 {
			if err := os.Rename(parent, parent+"-held"); err != nil {
				return err
			}
			return os.Symlink(outside, parent)
		}
		return nil
	}
	defer func() { testHook = nil }()
	_, err := s.Execute([]Change{change(t, path, file("managed\n"))}, Meta{Kind: "apply", ParentDirs: []string{parent}})
	if err == nil {
		t.Fatal("symlinked parent was accepted")
	}
	if b, _ := os.ReadFile(outsidePath); string(b) != "original\n" {
		t.Errorf("write escaped the managed parent: %q", b)
	}
}

func TestFailedParentCreationDoesNotRemoveConcurrentFile(t *testing.T) {
	s, home := newStore(t)
	parent := filepath.Join(home, ".agent")
	path := filepath.Join(parent, "RULES.md")
	testHook = func(stage string, i int) error {
		if stage == "parent" && i == 0 {
			return os.WriteFile(parent, []byte("other process"), 0o600)
		}
		return nil
	}
	defer func() { testHook = nil }()
	_, err := s.Execute([]Change{change(t, path, file("managed\n"))}, Meta{Kind: "apply", ParentDirs: []string{parent}})
	if err == nil {
		t.Fatal("expected parent creation to fail")
	}
	if b, readErr := os.ReadFile(parent); readErr != nil || string(b) != "other process" {
		t.Fatalf("concurrent file was removed: data %q, error %v", b, readErr)
	}
	if s.Pending() || s.Locked() {
		t.Fatal("failed parent creation left a recovery journal for a directory it did not create")
	}
}

func TestFailedParentCreationKeepsConcurrentDirectory(t *testing.T) {
	s, home := newStore(t)
	parent := filepath.Join(home, ".agent")
	path := filepath.Join(parent, "RULES.md")
	testHook = func(stage string, i int) error {
		if stage == "parent" && i == 0 {
			return os.Mkdir(parent, 0o700)
		}
		return nil
	}
	defer func() { testHook = nil }()
	_, err := s.Execute([]Change{change(t, path, file("managed\n"))}, Meta{Kind: "apply", ParentDirs: []string{parent}})
	if err == nil || errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("expected a plain parent creation failure, got %v", err)
	}
	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		t.Fatalf("concurrent directory was removed: %v", err)
	}
	if s.Pending() || s.Locked() {
		t.Fatal("failed parent creation left a recovery journal for a directory it did not create")
	}
}

func TestCrashBetweenParentCreationAndJournalNeedsResolution(t *testing.T) {
	exactRecovery(t)
	s, home := newStore(t)
	parent := filepath.Join(home, ".agent")
	path := filepath.Join(parent, "RULES.md")
	testHook = func(stage string, _ int) error {
		if stage == "parent-created" {
			return errCrash
		}
		return nil
	}
	_, err := s.Execute([]Change{change(t, path, file("managed\n"))}, Meta{Kind: "apply", ParentDirs: []string{parent}})
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected crash after mkdir, got %v", err)
	}
	must(t, s.lock())
	if err := s.Recover(); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("ambiguous parent creation should require resolution, got %v", err)
	}
	if _, err := os.Lstat(parent); err != nil || !s.Pending() {
		t.Fatal("ambiguous parent or recovery journal was lost")
	}
	must(t, os.Remove(parent))
	if err := s.Recover(); err != nil || s.Pending() || s.Locked() {
		t.Fatalf("recovery did not finish after resolving the parent: %v", err)
	}
}

func TestFailureMidwayRollsBack(t *testing.T) {
	s, home := newStore(t)
	paths, before := setup(t, home)
	for fail := 0; fail < len(paths); fail++ {
		testHook = func(stage string, i int) error {
			if stage == "step" && i == fail {
				return errors.New("injected")
			}
			return nil
		}
		var cs []Change
		for _, p := range paths {
			cs = append(cs, change(t, p, skillDir("managed")))
		}
		_, err := s.Execute(cs, Meta{Kind: "apply"})
		testHook = nil
		if err == nil {
			t.Fatal("expected injected failure")
		}
		assertRestored(t, before)
		if s.Pending() || s.Locked() {
			t.Fatal("rollback left journal or lock")
		}
		assertNoTemps(t, filepath.Join(home, ".agent"))
	}
}

func TestCrashThenRecover(t *testing.T) {
	exactRecovery(t)
	for _, stage := range []string{"step", "commit"} {
		s, home := newStore(t)
		paths, before := setup(t, home)
		// Parent dirs created by the crashed apply must be removed on recovery.
		fresh := filepath.Join(home, ".fresh")
		freshSkills := filepath.Join(fresh, "skills")
		paths = append(paths, filepath.Join(freshSkills, "routing"))
		before[paths[len(paths)-1]] = &fsnode.Node{Type: fsnode.Absent}
		testHook = func(st string, i int) error {
			if st == stage && (stage == "commit" || i == 2) {
				return errCrash
			}
			return nil
		}
		var cs []Change
		for _, p := range paths {
			cs = append(cs, Change{Path: p, Want: skillDir("managed"), Expect: fsnode.Fingerprint(before[p])})
		}
		_, err := s.Execute(cs, Meta{Kind: "apply", ParentDirs: []string{fresh, freshSkills}})
		testHook = nil
		if !errors.Is(err, errCrash) {
			t.Fatalf("%s: expected simulated crash, got %v", stage, err)
		}
		s.lock() // a real crash would also leave the lock behind
		if !s.Pending() {
			t.Fatalf("%s: crash should leave a journal", stage)
		}
		if _, err := s.Execute(nil, Meta{Kind: "apply"}); !errors.Is(err, ErrRecoveryNeeded) {
			t.Fatalf("%s: expected ErrRecoveryNeeded, got %v", stage, err)
		}
		if err := s.Recover(); err != nil {
			t.Fatalf("%s: recover: %v", stage, err)
		}
		assertRestored(t, before)
		if _, err := os.Lstat(fresh); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: created parent dir not removed", stage)
		}
		if s.Pending() || s.Locked() {
			t.Fatalf("%s: recovery left journal or lock", stage)
		}
		if list, _ := s.List(); len(list) != 0 {
			t.Errorf("%s: rolled-back snapshot listed as restorable", stage)
		}
	}
}

func TestRecoverRefusesEditMadeAfterCrash(t *testing.T) {
	exactRecovery(t)
	s, home := newStore(t)
	path := filepath.Join(home, "RULES.md")
	must(t, os.WriteFile(path, []byte("original\n"), 0o644))
	testHook = func(stage string, _ int) error {
		if stage == "commit" {
			return errCrash
		}
		return nil
	}
	_, err := s.Execute([]Change{change(t, path, file("managed\n"))}, Meta{Kind: "apply"})
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected simulated crash, got %v", err)
	}
	must(t, s.lock())
	must(t, os.WriteFile(path, []byte("edited after crash\n"), 0o644))
	if err := s.Recover(); err == nil {
		t.Fatal("recovery overwrote an edit made after the crash")
	}
	if b, _ := os.ReadFile(path); string(b) != "edited after crash\n" {
		t.Errorf("post-crash edit was lost: %q", b)
	}
	if !s.Pending() || !s.Locked() {
		t.Error("recovery conflict cleared the journal or lock")
	}
}

func TestRecoverRefusesMetadataEditMadeAfterCrash(t *testing.T) {
	exactRecovery(t)
	s, home := newStore(t)
	path := filepath.Join(home, "RULES.md")
	must(t, os.WriteFile(path, []byte("original\n"), 0o644))
	testHook = func(stage string, _ int) error {
		if stage == "commit" {
			return errCrash
		}
		return nil
	}
	_, err := s.Execute([]Change{change(t, path, file("managed\n"))}, Meta{Kind: "apply"})
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected simulated crash, got %v", err)
	}
	must(t, s.lock())
	edited := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, os.Chtimes(path, edited, edited))
	if err := s.Recover(); err == nil {
		t.Fatal("recovery overwrote a metadata edit made after the crash")
	}
	fi, err := os.Stat(path)
	must(t, err)
	if !fi.ModTime().Equal(edited) || !s.Pending() {
		t.Fatal("metadata edit or recovery journal was lost")
	}
}

func TestRecoverRefusesDeletionAfterStartedStep(t *testing.T) {
	exactRecovery(t)
	s, home := newStore(t)
	path := filepath.Join(home, "RULES.md")
	must(t, os.WriteFile(path, []byte("original\n"), 0o644))
	testHook = func(stage string, _ int) error {
		if stage == "started" {
			return errCrash
		}
		return nil
	}
	_, err := s.Execute([]Change{change(t, path, file("managed\n"))}, Meta{Kind: "apply"})
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected crash after journaling the started step, got %v", err)
	}
	must(t, s.lock())
	must(t, os.Remove(path))
	if err := s.Recover(); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("expected recovery conflict after deletion, got %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) || !s.Pending() {
		t.Fatal("recovery recreated a user-deleted path or cleared its journal")
	}
}

func TestRecoverRefusesMetadataEditBeforeStepCompletion(t *testing.T) {
	exactRecovery(t)
	s, home := newStore(t)
	path := filepath.Join(home, "RULES.md")
	must(t, os.WriteFile(path, []byte("original\n"), 0o644))
	testHook = func(stage string, _ int) error {
		if stage == "replaced" {
			return errCrash
		}
		return nil
	}
	_, err := s.Execute([]Change{change(t, path, file("managed\n"))}, Meta{Kind: "apply"})
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected crash before step completion was journaled, got %v", err)
	}
	must(t, s.lock())
	edited := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, os.Chtimes(path, edited, edited))
	if err := s.Recover(); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("expected recovery conflict after metadata edit, got %v", err)
	}
	fi, err := os.Stat(path)
	must(t, err)
	if !fi.ModTime().Equal(edited) || !s.Pending() {
		t.Fatal("recovery lost the metadata edit or cleared its journal")
	}
}

func TestRecoverAfterApplyReplacedBeforeStepCompletion(t *testing.T) {
	exactRecovery(t)
	for _, want := range []*fsnode.Node{file("managed\n"), skillDir("managed\n")} {
		s, home := newStore(t)
		path := filepath.Join(home, "RULES.md")
		must(t, os.WriteFile(path, []byte("original\n"), 0o644))
		before := capture(t, path)
		testHook = func(stage string, _ int) error {
			if stage == "replaced" {
				return errCrash
			}
			return nil
		}
		_, err := s.Execute([]Change{change(t, path, want)}, Meta{Kind: "apply"})
		testHook = nil
		if !errors.Is(err, errCrash) {
			t.Fatalf("expected crash before step completion was journaled, got %v", err)
		}
		must(t, s.lock())
		if err := s.Recover(); err != nil {
			t.Fatalf("%s: recovery rejected its own unedited write: %v", want.Type, err)
		}
		assertRestored(t, map[string]*fsnode.Node{path: before})
	}
}

func TestRecoverRecreatesParentsRemovedDuringRestore(t *testing.T) {
	exactRecovery(t)
	s, home := newStore(t)
	parent := filepath.Join(home, ".agent")
	skills := filepath.Join(parent, "skills")
	must(t, os.MkdirAll(skills, 0o755))
	path := filepath.Join(skills, "routing")
	must(t, os.WriteFile(path, []byte("managed\n"), 0o644))
	must(t, os.Chmod(skills, 0o700))
	originalTime := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
	must(t, os.Chtimes(skills, originalTime, originalTime))
	testHook = func(stage string, _ int) error {
		if stage == "after-cleanup" {
			return errCrash
		}
		return nil
	}
	_, err := s.Execute([]Change{change(t, path, &fsnode.Node{Type: fsnode.Absent})},
		Meta{Kind: "restore", CleanupDirs: []string{parent, skills}, CheckDirs: []string{parent, skills}})
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected simulated crash after directory cleanup, got %v", err)
	}
	if _, err := os.Lstat(parent); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("restore did not remove empty created parents before the crash")
	}
	must(t, s.lock())
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "managed\n" {
		t.Fatalf("recovery did not restore the target and its parents: %q, %v", b, err)
	}
	fi, err := os.Stat(skills)
	must(t, err)
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Errorf("recovered parent mode %o, want 0700", fi.Mode().Perm())
	}
	if !fi.ModTime().Equal(originalTime) {
		t.Errorf("recovered parent mtime %v, want %v", fi.ModTime(), originalTime)
	}
}

func TestRecoverAfterChildCreationChangedParentMtime(t *testing.T) {
	exactRecovery(t)
	s, home := newStore(t)
	parent := filepath.Join(home, ".agent")
	child := filepath.Join(parent, "skills")
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	want := &fsnode.Node{Type: fsnode.Dir, Mode: 0o755, ModTime: old, Children: map[string]*fsnode.Node{}}
	absent := &fsnode.Node{Type: fsnode.Absent}
	changes := []Change{
		{Path: parent, Want: want, Expect: fsnode.Fingerprint(absent), ExpectMeta: fsnode.RestoreFingerprint(absent), ParentDir: true},
		{Path: child, Want: want, Expect: fsnode.Fingerprint(absent), ExpectMeta: fsnode.RestoreFingerprint(absent), ParentDir: true},
	}
	testHook = func(stage string, i int) error {
		if stage == "replaced" && i == 1 {
			return errCrash
		}
		return nil
	}
	_, err := s.Execute(changes, Meta{Kind: "restore", CheckDirs: []string{home, parent, child}})
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected crash after child creation, got %v", err)
	}
	must(t, s.lock())
	if err := s.Recover(); err != nil {
		t.Fatalf("recovery rejected its own parent mtime change: %v", err)
	}
	if _, err := os.Lstat(parent); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("recovery did not remove created parent")
	}
}

func TestRecoverRetryAfterRecreatingRemovedParent(t *testing.T) {
	exactRecovery(t)
	s, home := newStore(t)
	parent := filepath.Join(home, ".agent")
	must(t, os.Mkdir(parent, 0o755))
	path := filepath.Join(parent, "RULES.md")
	must(t, os.WriteFile(path, []byte("managed\n"), 0o644))
	testHook = func(stage string, _ int) error {
		if stage == "after-cleanup" {
			return errCrash
		}
		return nil
	}
	_, err := s.Execute([]Change{change(t, path, &fsnode.Node{Type: fsnode.Absent})},
		Meta{Kind: "restore", CleanupDirs: []string{parent}, CheckDirs: []string{parent}})
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected crash after cleanup, got %v", err)
	}
	must(t, s.lock())
	testHook = func(stage string, _ int) error {
		if stage == "rollback-dir" {
			return errCrash
		}
		return nil
	}
	if err := s.Recover(); !errors.Is(err, errCrash) {
		t.Fatalf("expected interrupted recovery, got %v", err)
	}
	testHook = nil
	if err := s.Recover(); err != nil {
		t.Fatalf("second recovery rejected its own recreated parent: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "managed\n" {
		t.Fatalf("second recovery did not restore target: %q, %v", b, err)
	}
}

func TestCorruptBackupRejected(t *testing.T) {
	s, home := newStore(t)
	paths, _ := setup(t, home)
	m := applyAll(t, s, paths)
	art := filepath.Join(s.backupsDir(), m.ID, "entries", "0.json")
	b, _ := os.ReadFile(art)
	b[len(b)/2] ^= 0xff
	must(t, os.WriteFile(art, b, 0o600))
	if _, err := s.Originals(m); err == nil {
		t.Fatal("corrupt artifact accepted")
	}
	must(t, os.WriteFile(filepath.Join(s.backupsDir(), m.ID, "manifest.json"), []byte("{"), 0o600))
	if _, err := s.Load(m.ID); err == nil {
		t.Fatal("corrupt manifest accepted")
	}
	if _, err := s.Load("../escape"); err == nil {
		t.Fatal("path traversal id accepted")
	}
}

func TestShortArtifactPathRejected(t *testing.T) {
	s, home := newStore(t)
	paths, _ := setup(t, home)
	m := applyAll(t, s, paths)
	m.Entries[0].Artifact = "x"
	if _, err := s.Originals(m); err == nil {
		t.Fatal("short artifact path accepted")
	}
}

func TestRestoreAfterUserEditNeedsExplicitChange(t *testing.T) {
	s, home := newStore(t)
	paths, _ := setup(t, home)
	m := applyAll(t, s, paths)
	must(t, os.WriteFile(paths[0], []byte("edited after install\n"), 0o644))
	origs, _ := s.Originals(m)
	// A stale expectation (the installed fingerprint) is refused.
	_, err := s.Execute([]Change{{Path: paths[0], Want: origs[0], Expect: m.Entries[0].InstalledFP}}, Meta{Kind: "restore"})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("want ErrChanged, got %v", err)
	}
	// Confirming against the current content snapshots the edit first.
	rm, err := s.Execute([]Change{change(t, paths[0], origs[0])}, Meta{Kind: "restore", RestoredFrom: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	o, _ := s.Originals(rm)
	if string(o[0].Data) != "edited after install\n" {
		t.Error("restore did not back up the user's edit")
	}
}

func TestLockBlocksConcurrentRun(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	must(t, s.lock())
	_, err := s.Execute([]Change{change(t, p, file("x"))}, Meta{Kind: "apply"})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	s.unlock()
}

func TestLastInstalledAndList(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	m1, _ := s.Execute([]Change{change(t, p, file("one"))}, Meta{Kind: "apply"})
	time.Sleep(10 * time.Millisecond)
	m2, _ := s.Execute([]Change{change(t, p, file("two"))}, Meta{Kind: "apply"})
	list, _ := s.List()
	if len(list) != 2 || list[0].ID != m2.ID || list[1].ID != m1.ID {
		t.Fatal("list not newest-first")
	}
	if s.LastInstalled()[p] != fp(t, p) {
		t.Error("LastInstalled does not reflect newest write")
	}
	if s.DiskSize(m1.ID) == 0 {
		t.Error("zero snapshot size")
	}
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && len(d.Name()) > 11 && d.Name()[:11] == ".agentfile-" {
			t.Errorf("temporary left behind: %s", p)
		}
		return nil
	})
}

func TestOpenWritesNothingAndRejectsSymlinkedRoot(t *testing.T) {
	home := t.TempDir()
	root := Root{Base: home, Rel: []string{"state", "agentfile"}}
	s, err := Open(root)
	must(t, err)
	if _, err := os.Lstat(filepath.Join(home, "state")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("Open created directories")
	}
	if list, err := s.List(); err != nil || list != nil {
		t.Fatalf("List on missing root: %v %v", list, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	must(t, os.MkdirAll(filepath.Join(home, "elsewhere"), 0o700))
	must(t, os.Symlink(filepath.Join(home, "elsewhere"), filepath.Join(home, "state")))
	if _, err := Open(root); err == nil {
		t.Fatal("symlinked state component accepted")
	}
}

func TestOpenRejectsSymlinkedStateBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link requires privileges on Windows")
	}
	home := t.TempDir()
	realBase := filepath.Join(home, "real")
	must(t, os.Mkdir(realBase, 0o700))
	link := filepath.Join(home, "state-link")
	must(t, os.Symlink(realBase, link))
	if _, err := Open(Root{Base: link, Rel: []string{"agentfile"}}); err == nil {
		t.Fatal("symlinked state base accepted")
	}
}

func TestStateBaseSwappedAfterOpenIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link requires privileges on Windows")
	}
	home := t.TempDir()
	base := filepath.Join(home, "state")
	outside := filepath.Join(home, "outside")
	must(t, os.Mkdir(base, 0o700))
	must(t, os.Mkdir(outside, 0o700))
	s, err := Open(Root{Base: base, Rel: []string{"agentfile"}})
	must(t, err)
	must(t, os.Rename(base, base+"-held"))
	must(t, os.Symlink(outside, base))
	p := filepath.Join(home, "f.md")
	if _, err := s.Execute([]Change{change(t, p, file("x"))}, Meta{Kind: "apply"}); err == nil {
		t.Fatal("write through a swapped state base was accepted")
	}
	if _, err := os.Lstat(filepath.Join(outside, "agentfile")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("state was written outside its original base")
	}
}

func TestStateDirsAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows relies on the inherited LOCALAPPDATA ACL")
	}
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	if _, err := s.Execute([]Change{change(t, p, file("x"))}, Meta{Kind: "apply"}); err != nil {
		t.Fatal(err)
	}
	comps := s.root.components()
	for _, d := range comps[len(comps)-2:] {
		fi, err := os.Stat(d)
		must(t, err)
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s has mode %o", d, fi.Mode().Perm())
		}
	}
}

func TestSharedStateParentKeepsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX permissions on Windows")
	}
	home := t.TempDir()
	shared := filepath.Join(home, ".local")
	must(t, os.Mkdir(shared, 0o755))
	s, err := Open(Root{Base: home, Rel: []string{".local", "state", "agentfile"}})
	must(t, err)
	p := filepath.Join(home, "f.md")
	if _, err := s.Execute([]Change{change(t, p, file("x"))}, Meta{Kind: "apply"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(shared); fi.Mode().Perm() != 0o755 {
		t.Errorf("shared parent permissions changed to %o", fi.Mode().Perm())
	}
}
