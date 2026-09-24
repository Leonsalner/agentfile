package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentfile/internal/fsnode"
)

func contentFile(data string) *fsnode.Node {
	return &fsnode.Node{Type: fsnode.File, Data: []byte(data)}
}

func captureContent(t *testing.T, path string) *fsnode.Node {
	t.Helper()
	n, err := fsnode.CaptureContent(path)
	must(t, err)
	return n
}

func contentChange(t *testing.T, path string, want *fsnode.Node) Change {
	t.Helper()
	cur := captureContent(t, path)
	return Change{Path: path, Want: want, Expect: fsnode.ContentFingerprint(cur), ExpectID: cur.FileID, Mode: ModeContent}
}

func contentMeta(kind string) Meta { return Meta{Kind: kind, Mode: ModeContent} }

func contentRestore(t *testing.T, s *Store, m *Manifest) *Manifest {
	t.Helper()
	origs, err := s.Originals(m)
	must(t, err)
	var cs []Change
	for i, e := range m.Entries {
		cs = append(cs, contentChange(t, e.Path, origs[i]))
	}
	meta := contentMeta("restore")
	meta.CleanupDirs = m.CreatedDirs
	rm, err := s.Execute(cs, meta)
	must(t, err)
	return rm
}

// crashAfter runs a content apply of path to "managed\n" that crashes at stage.
func crashAfter(t *testing.T, s *Store, path, stage string, during func() error) {
	t.Helper()
	testHook = func(st string, _ int) error {
		if st != stage {
			return nil
		}
		if during != nil {
			if err := during(); err != nil {
				return err
			}
		}
		return errCrash
	}
	defer func() { testHook = nil }()
	_, err := s.Execute([]Change{contentChange(t, path, contentFile("managed\n"))}, contentMeta("apply"))
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected crash at %s, got %v", stage, err)
	}
	must(t, s.lock())
}

func TestContentApplyAndRestoreInPlace(t *testing.T) {
	s, home := newStore(t)
	existing, fresh := filepath.Join(home, "RULES.md"), filepath.Join(home, "SKILL.md")
	must(t, os.WriteFile(existing, []byte("user rules\n"), 0o640))
	before, err := os.Stat(existing)
	must(t, err)
	m, err := s.Execute([]Change{contentChange(t, existing, contentFile("managed rules\n")), contentChange(t, fresh, contentFile("managed skill\n"))}, contentMeta("apply"))
	must(t, err)
	if m == nil || m.Mode != ModeContent || len(m.Entries) != 2 {
		t.Fatalf("expected a content snapshot: %+v", m)
	}
	for _, e := range m.Entries {
		if e.Mode != ModeContent || e.InstalledFP != fsnode.ContentFingerprint(captureContent(t, e.Path)) {
			t.Errorf("%s: entry not recorded as content-only: %+v", e.Path, e)
		}
	}
	after, err := os.Stat(existing)
	must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("existing file was replaced, not updated in place")
	}
	if s.Pending() || s.Locked() {
		t.Fatal("journal or lock left behind")
	}
	contentRestore(t, s, m)
	if b, _ := os.ReadFile(existing); string(b) != "user rules\n" {
		t.Fatalf("original bytes not restored: %q", b)
	}
	restored, err := os.Stat(existing)
	must(t, err)
	if !os.SameFile(before, restored) {
		t.Fatal("restore replaced the existing file")
	}
	if _, err := os.Lstat(fresh); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("created file not removed on restore")
	}
}

func TestContentNoOpWritesNothing(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	must(t, os.WriteFile(p, []byte("same\n"), 0o644))
	m, err := s.Execute([]Change{contentChange(t, p, contentFile("same\n"))}, contentMeta("apply"))
	if err != nil || m != nil {
		t.Fatalf("no-op: m=%v err=%v", m, err)
	}
	if ents, _ := os.ReadDir(s.backupsDir()); len(ents) != 0 || s.Pending() {
		t.Error("no-op wrote a backup or journal")
	}
}

func TestContentModeMarkerRequired(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	must(t, os.WriteFile(p, []byte("v1\n"), 0o644))
	c := contentChange(t, p, contentFile("v2\n"))
	c.Mode = ""
	if _, err := s.Execute([]Change{c}, contentMeta("apply")); err == nil {
		t.Fatal("exact change accepted in a content-only transaction")
	}
	c.Mode = ModeContent
	if _, err := s.Execute([]Change{c}, Meta{Kind: "apply"}); err == nil {
		t.Fatal("content change accepted in an exact transaction")
	}
	if b, _ := os.ReadFile(p); string(b) != "v1\n" {
		t.Fatal("refused transaction wrote the target")
	}
}

func TestContentChangedAfterPreviewAborts(t *testing.T) {
	for _, edit := range []string{"content", "identity"} {
		s, home := newStore(t)
		p := filepath.Join(home, "f.md")
		must(t, os.WriteFile(p, []byte("v1\n"), 0o644))
		c := contentChange(t, p, contentFile("managed\n"))
		if edit == "content" {
			must(t, os.WriteFile(p, []byte("edited\n"), 0o644))
		} else {
			must(t, os.WriteFile(p+".new", []byte("v1\n"), 0o644))
			must(t, os.Rename(p+".new", p))
		}
		want, _ := os.ReadFile(p)
		if _, err := s.Execute([]Change{c}, contentMeta("apply")); !errors.Is(err, ErrChanged) {
			t.Fatalf("%s: want ErrChanged, got %v", edit, err)
		}
		if b, _ := os.ReadFile(p); string(b) != string(want) {
			t.Fatalf("%s: target overwritten", edit)
		}
		if ents, _ := os.ReadDir(s.backupsDir()); len(ents) != 0 || s.Pending() {
			t.Fatalf("%s: refused apply left a snapshot or journal", edit)
		}
	}
}

func TestContentChangedBeforeOpenedWriteRollsBack(t *testing.T) {
	s, home := newStore(t)
	first, later := filepath.Join(home, "first.md"), filepath.Join(home, "later.md")
	must(t, os.WriteFile(first, []byte("first\n"), 0o644))
	must(t, os.WriteFile(later, []byte("later\n"), 0o644))
	changes := []Change{contentChange(t, first, contentFile("first managed\n")), contentChange(t, later, contentFile("later managed\n"))}
	testHook = func(stage string, i int) error {
		if stage == "started" && i == 1 {
			return os.WriteFile(later, []byte("edited during apply\n"), 0o644)
		}
		return nil
	}
	defer func() { testHook = nil }()
	if _, err := s.Execute(changes, contentMeta("apply")); !errors.Is(err, ErrChanged) {
		t.Fatalf("want ErrChanged, got %v", err)
	}
	if b, _ := os.ReadFile(first); string(b) != "first\n" {
		t.Errorf("earlier step not rolled back: %q", b)
	}
	if b, _ := os.ReadFile(later); string(b) != "edited during apply\n" {
		t.Errorf("concurrent edit overwritten: %q", b)
	}
	if s.Pending() {
		t.Error("rolled-back apply left a journal")
	}
	if list, _ := s.List(); len(list) != 0 {
		t.Error("rolled-back apply left a committed backup")
	}
}

func TestContentCreatedBeforeExclusiveCreateAborts(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	c := contentChange(t, p, contentFile("managed\n"))
	testHook = func(stage string, _ int) error {
		if stage == "started" {
			return os.WriteFile(p, []byte("created during apply\n"), 0o644)
		}
		return nil
	}
	defer func() { testHook = nil }()
	if _, err := s.Execute([]Change{c}, contentMeta("apply")); !errors.Is(err, ErrChanged) {
		t.Fatalf("want ErrChanged, got %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "created during apply\n" {
		t.Errorf("concurrent file overwritten: %q", b)
	}
	if s.Pending() {
		t.Error("refused create left a journal needing recovery")
	}
	if list, _ := s.List(); len(list) != 0 {
		t.Error("refused create left a committed backup")
	}
}

func TestContentSnapshotRecheckedBeforeEachStep(t *testing.T) {
	s, home := newStore(t)
	first, later := filepath.Join(home, "first.md"), filepath.Join(home, "later.md")
	must(t, os.WriteFile(first, []byte("first\n"), 0o644))
	must(t, os.WriteFile(later, []byte("later\n"), 0o644))
	changes := []Change{contentChange(t, first, contentFile("first managed\n")), contentChange(t, later, contentFile("later managed\n"))}
	testHook = func(stage string, i int) error {
		if stage == "step" && i == 1 {
			ents, _ := os.ReadDir(s.backupsDir())
			return os.WriteFile(filepath.Join(s.backupsDir(), ents[0].Name(), "entries", "1.json"), []byte("{}"), 0o600)
		}
		return nil
	}
	defer func() { testHook = nil }()
	_, err := s.Execute(changes, contentMeta("apply"))
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt snapshot was not detected before writing: %v", err)
	}
	if b, _ := os.ReadFile(later); string(b) != "later\n" {
		t.Fatal("target written from an unverified snapshot")
	}
}

func TestContentCrashBeforeWriteRecoversWithoutChoice(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	must(t, os.WriteFile(p, []byte("original\n"), 0o644))
	crashAfter(t, s, p, "started", nil)
	items, err := s.RecoveryReview()
	if err != nil || len(items) != 0 {
		t.Fatalf("unchanged target needs no choice: %v %v", items, err)
	}
	must(t, s.Recover())
	if s.Pending() || s.Locked() {
		t.Fatal("recovery left journal or lock")
	}
}

func TestContentCrashAfterWriteNeedsExplicitChoice(t *testing.T) {
	for _, stage := range []string{"written", "partial"} {
		s, home := newStore(t)
		p := filepath.Join(home, "f.md")
		must(t, os.WriteFile(p, []byte("original contents\n"), 0o644))
		before, err := os.Stat(p)
		must(t, err)
		if stage == "partial" {
			crashAfter(t, s, p, "started", func() error {
				f, err := os.OpenFile(p, os.O_WRONLY, 0)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = f.WriteAt([]byte("mana"), 0)
				return err
			})
		} else {
			crashAfter(t, s, p, stage, nil)
		}
		crashed, _ := os.ReadFile(p)
		if err := s.Recover(); !errors.Is(err, ErrRecoveryConflict) {
			t.Fatalf("%s: recovery restored without a choice: %v", stage, err)
		}
		if b, _ := os.ReadFile(p); string(b) != string(crashed) || !s.Pending() || !s.Locked() {
			t.Fatalf("%s: conflict changed the target or cleared the journal", stage)
		}
		if _, err := s.Execute(nil, contentMeta("apply")); !errors.Is(err, ErrRecoveryNeeded) {
			t.Fatalf("%s: other operations not blocked: %v", stage, err)
		}
		items, err := s.RecoveryReview()
		must(t, err)
		if len(items) != 1 || items[0].Path != p || string(items[0].Backup.Data) != "original contents\n" ||
			string(items[0].Current.Data) != string(crashed) || items[0].Token == "" {
			t.Fatalf("%s: review %+v", stage, items)
		}
		must(t, s.RecoverWith(map[string]string{p: items[0].Token}, nil))
		if b, _ := os.ReadFile(p); string(b) != "original contents\n" {
			t.Fatalf("%s: confirmed recovery did not restore bytes: %q", stage, b)
		}
		after, err := os.Stat(p)
		must(t, err)
		if !os.SameFile(before, after) {
			t.Fatalf("%s: recovery replaced the file", stage)
		}
		if s.Pending() || s.Locked() {
			t.Fatalf("%s: recovery left journal or lock", stage)
		}
	}
}

func TestContentRecoverDoesNotOverwriteLaterEdit(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	must(t, os.WriteFile(p, []byte("original\n"), 0o644))
	crashAfter(t, s, p, "written", nil)
	items, err := s.RecoveryReview()
	must(t, err)
	must(t, os.WriteFile(p, []byte("edited after review\n"), 0o644))
	if err := s.RecoverWith(map[string]string{p: items[0].Token}, nil); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("stale confirmation accepted: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "edited after review\n" || !s.Pending() {
		t.Fatal("edit after review was overwritten or the journal cleared")
	}
	snap := filepath.Join(s.backupsDir())
	if ents, _ := os.ReadDir(snap); len(ents) != 1 {
		t.Fatal("verified backup was removed before resolution")
	}
}

func TestContentRecoverKeepsChosenFiles(t *testing.T) {
	s, home := newStore(t)
	kept, restored := filepath.Join(home, "kept.md"), filepath.Join(home, "restored.md")
	must(t, os.WriteFile(kept, []byte("kept original\n"), 0o644))
	must(t, os.WriteFile(restored, []byte("restored original\n"), 0o644))
	changes := []Change{contentChange(t, kept, contentFile("managed\n")), contentChange(t, restored, contentFile("managed\n"))}
	testHook = func(stage string, i int) error {
		if stage == "written" && i == 1 {
			return errCrash
		}
		return nil
	}
	_, err := s.Execute(changes, contentMeta("apply"))
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected crash, got %v", err)
	}
	must(t, s.lock())
	must(t, os.WriteFile(kept, []byte("my edit after the crash\n"), 0o644))
	items, err := s.RecoveryReview()
	must(t, err)
	if len(items) != 2 {
		t.Fatalf("review %+v", items)
	}
	tokens := map[string]string{}
	for _, it := range items {
		tokens[it.Path] = it.Token
	}
	if err := s.RecoverWith(map[string]string{kept: tokens[kept]}, map[string]string{kept: tokens[kept]}); err == nil || !s.Pending() {
		t.Fatalf("restore and keep for one file accepted: %v", err)
	}
	must(t, os.WriteFile(kept, []byte("edited after review\n"), 0o644))
	if err := s.RecoverWith(map[string]string{restored: tokens[restored]}, map[string]string{kept: tokens[kept]}); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("stale keep accepted: %v", err)
	}
	if b, _ := os.ReadFile(restored); string(b) != "managed\n" || !s.Pending() {
		t.Fatal("refused recovery changed a file or cleared the journal")
	}
	items, err = s.RecoveryReview()
	must(t, err)
	for _, it := range items {
		tokens[it.Path] = it.Token
	}
	must(t, s.RecoverWith(map[string]string{restored: tokens[restored]}, map[string]string{kept: tokens[kept]}))
	if b, _ := os.ReadFile(kept); string(b) != "edited after review\n" {
		t.Fatalf("kept file changed: %q", b)
	}
	if b, _ := os.ReadFile(restored); string(b) != "restored original\n" {
		t.Fatalf("chosen file not restored: %q", b)
	}
	if s.Pending() || s.Locked() {
		t.Fatal("recovery left journal or lock")
	}
	list, err := s.List()
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("backup with kept files is not listed for Restore: %d", len(list))
	}
	if got := s.LastInstalled(); got[restored] != "" || got[kept] != fsnode.ContentFingerprint(contentFile("edited after review\n")) {
		t.Fatalf("installed state %v", got)
	}
	rm := contentRestore(t, s, list[0])
	if rm == nil {
		t.Fatal("restoring the kept backup changed nothing")
	}
	if b, _ := os.ReadFile(kept); string(b) != "kept original\n" {
		t.Fatalf("later Restore did not bring back the original: %q", b)
	}
}

func TestContentRecoverKeepAllClearsJournal(t *testing.T) {
	s, home := newStore(t)
	parent := filepath.Join(home, ".agent")
	p := filepath.Join(parent, "f.md")
	testHook = func(stage string, _ int) error {
		if stage == "written" {
			return errCrash
		}
		return nil
	}
	c := Change{Path: p, Want: contentFile("managed\n"), Expect: fsnode.ContentFingerprint(&fsnode.Node{Type: fsnode.Absent}), Mode: ModeContent}
	meta := contentMeta("apply")
	meta.ParentDirs = []string{parent}
	_, err := s.Execute([]Change{c}, meta)
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected crash, got %v", err)
	}
	must(t, s.lock())
	items, err := s.RecoveryReview()
	must(t, err)
	must(t, s.RecoverWith(nil, map[string]string{p: items[0].Token}))
	if b, _ := os.ReadFile(p); string(b) != "managed\n" || s.Pending() {
		t.Fatalf("kept created file changed or journal kept: %q", b)
	}
	list, err := s.List()
	must(t, err)
	if len(list) != 1 || len(list[0].CreatedDirs) != 1 || list[0].CreatedDirs[0] != parent {
		t.Fatalf("kept backup does not record its created parent: %+v", list)
	}
	contentRestore(t, s, list[0])
	if _, err := os.Lstat(parent); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("restoring the kept backup left the created file or parent")
	}
}

func TestContentCrashAfterCreateNeedsChoiceToRemove(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "f.md")
	crashAfter(t, s, p, "written", nil)
	items, err := s.RecoveryReview()
	must(t, err)
	if len(items) != 1 || items[0].Backup.Type != fsnode.Absent {
		t.Fatalf("created file not listed: %+v", items)
	}
	if err := s.Recover(); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("created file removed without a choice: %v", err)
	}
	must(t, s.RecoverWith(map[string]string{p: items[0].Token}, nil))
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("confirmed recovery did not remove the created file")
	}
}

func TestContentRecoverRemovesCreatedParents(t *testing.T) {
	s, home := newStore(t)
	parent := filepath.Join(home, ".agent")
	p := filepath.Join(parent, "f.md")
	testHook = func(stage string, _ int) error {
		if stage == "started" {
			return errCrash
		}
		return nil
	}
	c := Change{Path: p, Want: contentFile("managed\n"), Expect: fsnode.ContentFingerprint(&fsnode.Node{Type: fsnode.Absent}), Mode: ModeContent}
	meta := contentMeta("apply")
	meta.ParentDirs = []string{parent}
	_, err := s.Execute([]Change{c}, meta)
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected crash, got %v", err)
	}
	must(t, s.lock())
	must(t, s.Recover())
	if _, err := os.Lstat(parent); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("created parent not removed on recovery")
	}
}

func TestWindowsRefusesExactJournal(t *testing.T) {
	s, home := newStore(t)
	p := filepath.Join(home, "RULES.md")
	must(t, os.WriteFile(p, []byte("original\n"), 0o644))
	testHook = func(stage string, _ int) error {
		if stage == "commit" {
			return errCrash
		}
		return nil
	}
	_, err := s.Execute([]Change{change(t, p, file("managed\n"))}, Meta{Kind: "apply"})
	testHook = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("expected crash, got %v", err)
	}
	must(t, s.lock())
	saved := hostOS
	hostOS = "windows"
	defer func() { hostOS = saved }()
	if _, err := s.RecoveryReview(); err == nil || !strings.Contains(err.Error(), "exact-restore") {
		t.Fatalf("review accepted an exact-mode journal on Windows: %v", err)
	}
	if err := s.Recover(); err == nil || !strings.Contains(err.Error(), "exact-restore") {
		t.Fatalf("recovery accepted an exact-mode journal on Windows: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "managed\n" || !s.Pending() {
		t.Fatal("refused recovery changed the target or cleared the journal")
	}
	hostOS = saved
	if saved == "windows" {
		return
	}
	must(t, s.Recover()) // the same journal still recovers on macOS and Linux
	if b, _ := os.ReadFile(p); string(b) != "original\n" {
		t.Fatal("exact-mode recovery regressed")
	}
}
