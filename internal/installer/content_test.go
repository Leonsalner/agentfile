package installer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agentfile/internal/fsnode"
	"agentfile/internal/profile"
	"agentfile/internal/state"
)

// newContentFixture plans for Windows on any host. File safety still comes
// from the host's fsnode implementation, so these tests also run natively.
func newContentFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	local := filepath.Join(home, "AppData", "Local")
	must(t, os.MkdirAll(local, 0o700))
	env := Env{
		Home: home, GOOS: "windows",
		Getenv:   func(k string) string { return map[string]string{"LOCALAPPDATA": local}[k] },
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	}
	root, err := state.DefaultRoot("windows", home, env.Getenv)
	must(t, err)
	st, err := state.Open(root)
	must(t, err)
	return &fixture{env: env, store: st, home: home}
}

// markWindows lets a host that is not Windows restore a snapshot it made
// while planning for Windows.
func markWindows(t *testing.T, f *fixture, id string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	path := filepath.Join(f.store.Dir, "backups", id, "manifest.json")
	b, err := os.ReadFile(path)
	must(t, err)
	b = []byte(strings.Replace(string(b), `"os": "`+runtime.GOOS+`"`, `"os": "windows"`, 1))
	must(t, os.WriteFile(path, b, 0o600))
}

var claudeOnly = profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}}

func contentSkill(f *fixture, name string) string {
	return filepath.Join(f.home, ".claude", "skills", name, "SKILL.md")
}

// existingSkill creates a named skill directory with a user SKILL.md and an
// unrelated extra file.
func existingSkill(t *testing.T, f *fixture) (skill, extra string) {
	t.Helper()
	skill = contentSkill(f, "routing")
	extra = filepath.Join(filepath.Dir(skill), "notes.txt")
	must(t, os.MkdirAll(filepath.Dir(skill), 0o755))
	must(t, os.WriteFile(skill, []byte("user skill\n"), 0o644))
	must(t, os.WriteFile(extra, []byte("user notes\n"), 0o644))
	return skill, extra
}

func findItem(t *testing.T, p *Plan, path string) *Item {
	t.Helper()
	for i := range p.Items {
		if p.Items[i].Path == path {
			return &p.Items[i]
		}
	}
	t.Fatalf("%s missing from the preview", path)
	return nil
}

func TestWindowsPreviewManagesOnlySkillFile(t *testing.T) {
	f := newContentFixture(t)
	skill, extra := existingSkill(t, f)
	homeBefore := tree(t, f.home)
	plan, err := PlanInstall(f.env, f.store, claudeOnly)
	must(t, err)
	want := map[string]bool{filepath.Join(f.home, ".claude", "CLAUDE.md"): true}
	for _, name := range []string{"routing", "handoff", "consult"} {
		want[contentSkill(f, name)] = true
	}
	if len(plan.Items) != len(want) {
		t.Fatalf("preview lists %d paths, want %d", len(plan.Items), len(want))
	}
	for _, it := range plan.Items {
		if !want[it.Path] {
			t.Errorf("unexpected managed path %s", it.Path)
		}
		if it.Path == extra || it.Path == filepath.Dir(skill) {
			t.Errorf("preview manages %s", it.Path)
		}
		_ = Diff(it)
	}
	it := findItem(t, plan, skill)
	if it.Action != UpdateInPlace || !it.Conflict || it.Replace {
		t.Fatalf("existing SKILL.md should be a kept in-place update: %+v", it)
	}
	if !strings.Contains(it.Note, "notes.txt") {
		t.Errorf("preview does not show the untouched extra file: %q", it.Note)
	}
	if got := findItem(t, plan, contentSkill(f, "consult")).Action; got != Create {
		t.Errorf("missing SKILL.md action %q, want %q", got, Create)
	}
	if tree(t, f.home) != homeBefore {
		t.Fatal("preview changed the home")
	}
	if got := f.env.ManagedPaths(profile.Claude); len(got) != 4 || got[1] != contentSkill(f, "routing") {
		t.Errorf("managed paths %v", got)
	}
}

func TestWindowsApplyAndRestoreUpdateInPlace(t *testing.T) {
	f := newContentFixture(t)
	skill, extra := existingSkill(t, f)
	before, err := os.Stat(skill)
	must(t, err)
	m := install(t, f, claudeOnly, replaceAll)
	if m == nil || m.Mode != state.ModeContent || len(m.Entries) != 4 {
		t.Fatalf("expected a content-only snapshot of 4 files: %+v", m)
	}
	after, err := os.Stat(skill)
	must(t, err)
	if b, _ := os.ReadFile(skill); !strings.Contains(string(b), "name: routing") {
		t.Fatalf("SKILL.md not updated: %q", b)
	}
	if !os.SameFile(before, after) {
		t.Fatal("existing SKILL.md was replaced instead of updated in place")
	}
	if b, _ := os.ReadFile(extra); string(b) != "user notes\n" {
		t.Fatal("extra file in the named skill directory changed")
	}
	if again := install(t, f, claudeOnly, replaceAll); again != nil {
		t.Fatal("identical content-only apply was not a no-op")
	}
	markWindows(t, f, m.ID)
	restore(t, f, m.ID, nil)
	restored, err := os.Stat(skill)
	must(t, err)
	if b, _ := os.ReadFile(skill); string(b) != "user skill\n" {
		t.Fatalf("original bytes not restored: %q", b)
	}
	if !os.SameFile(before, restored) {
		t.Fatal("restore replaced the existing SKILL.md")
	}
	if b, _ := os.ReadFile(extra); string(b) != "user notes\n" {
		t.Fatal("restore changed the extra file")
	}
	for _, name := range []string{"handoff", "consult"} {
		if _, err := os.Lstat(filepath.Dir(contentSkill(f, name))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("named skill directory created for %s was not removed", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(f.home, ".claude", "CLAUDE.md")); !errors.Is(err, os.ErrNotExist) {
		t.Error("created CLAUDE.md was not removed")
	}
}

func TestWindowsUnexpectedPathTypesBlocked(t *testing.T) {
	f := newContentFixture(t)
	dirSkill := contentSkill(f, "routing")
	must(t, os.MkdirAll(dirSkill, 0o755))
	linked := contentSkill(f, "handoff")
	must(t, os.MkdirAll(filepath.Dir(linked), 0o755))
	must(t, os.WriteFile(linked, []byte("shared\n"), 0o644))
	must(t, os.Link(linked, filepath.Join(f.home, "second-name")))
	plan, err := PlanInstall(f.env, f.store, claudeOnly)
	must(t, err)
	for _, p := range []string{dirSkill, linked} {
		if it := findItem(t, plan, p); it.Blocked == "" || it.Proceeds() {
			t.Errorf("%s was not blocked: %+v", p, it)
		}
	}
	for i := range plan.Items {
		plan.Items[i].Replace = true
	}
	_, err = Execute(f.env, f.store, plan)
	must(t, err)
	if b, _ := os.ReadFile(linked); string(b) != "shared\n" {
		t.Fatal("hard-linked SKILL.md was written")
	}
}

func TestWindowsNamedSkillFileBlocksAgent(t *testing.T) {
	f := newContentFixture(t)
	named := filepath.Dir(contentSkill(f, "routing"))
	must(t, os.MkdirAll(filepath.Dir(named), 0o755))
	must(t, os.WriteFile(named, []byte("not a directory"), 0o644))
	plan, err := PlanInstall(f.env, f.store, claudeOnly)
	must(t, err)
	for _, it := range plan.Items {
		if it.Blocked == "" {
			t.Errorf("%s was not blocked by a non-directory named skill path", it.Path)
		}
	}
	if Discover(f.env, f.store)[0].Problem == "" {
		t.Error("discovery did not report the unsafe named skill path")
	}
}

func TestWindowsSymlinkedSkillFileBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link requires privileges on Windows")
	}
	f := newContentFixture(t)
	outside := filepath.Join(t.TempDir(), "SKILL.md")
	must(t, os.WriteFile(outside, []byte("elsewhere\n"), 0o644))
	skill := contentSkill(f, "routing")
	must(t, os.MkdirAll(filepath.Dir(skill), 0o755))
	must(t, os.Symlink(outside, skill))
	plan, err := PlanInstall(f.env, f.store, claudeOnly)
	must(t, err)
	if it := findItem(t, plan, skill); it.Blocked == "" {
		t.Fatalf("symlinked SKILL.md was not blocked: %+v", it)
	}
}

func TestWindowsReplacedAfterPreviewRefused(t *testing.T) {
	for _, edit := range []string{"content", "identity"} {
		f := newContentFixture(t)
		skill, _ := existingSkill(t, f)
		plan, err := PlanInstall(f.env, f.store, claudeOnly)
		must(t, err)
		findItem(t, plan, skill).Replace = true
		if edit == "content" {
			must(t, os.WriteFile(skill, []byte("edited after preview\n"), 0o644))
		} else {
			other := skill + ".new"
			must(t, os.WriteFile(other, []byte("user skill\n"), 0o644))
			must(t, os.Rename(other, skill))
		}
		want, _ := os.ReadFile(skill)
		if _, err := Execute(f.env, f.store, plan); !errors.Is(err, state.ErrChanged) {
			t.Fatalf("%s: want ErrChanged, got %v", edit, err)
		}
		if b, _ := os.ReadFile(skill); string(b) != string(want) {
			t.Fatalf("%s: target bytes changed", edit)
		}
		if list, _ := f.store.List(); len(list) != 0 || f.store.Pending() {
			t.Fatalf("%s: refused attempt left a committed backup or journal", edit)
		}
	}
}

func TestWindowsRestoreOfDeletedFileNeedsRecreationChoice(t *testing.T) {
	f := newContentFixture(t)
	skill, _ := existingSkill(t, f)
	m := install(t, f, claudeOnly, replaceAll)
	markWindows(t, f, m.ID)
	must(t, os.Remove(skill))
	plan, err := PlanRestore(f.env, f.store, m.ID)
	must(t, err)
	it := findItem(t, plan, skill)
	if it.Action != Recreate || !it.Conflict || it.Replace {
		t.Fatalf("missing file should be a kept recreation choice: %+v", it)
	}
	_, err = Execute(f.env, f.store, plan)
	must(t, err)
	if _, err := os.Lstat(skill); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("declined recreation wrote the file")
	}
	plan, err = PlanRestore(f.env, f.store, m.ID)
	must(t, err)
	findItem(t, plan, skill).Replace = true
	_, err = Execute(f.env, f.store, plan)
	must(t, err)
	if b, _ := os.ReadFile(skill); string(b) != "user skill\n" {
		t.Fatalf("chosen recreation did not restore bytes: %q", b)
	}
}

func TestWindowsRestoreOfEditedCreatedFileNeedsChoice(t *testing.T) {
	f := newContentFixture(t)
	m := install(t, f, claudeOnly, nil)
	markWindows(t, f, m.ID)
	claudeMD := filepath.Join(f.home, ".claude", "CLAUDE.md")
	must(t, os.WriteFile(claudeMD, []byte("edited after install\n"), 0o644))
	plan, err := PlanRestore(f.env, f.store, m.ID)
	must(t, err)
	if it := findItem(t, plan, claudeMD); it.Action != Remove || !it.Conflict || it.Replace {
		t.Fatalf("edited created file should be a kept removal choice: %+v", it)
	}
	_, err = Execute(f.env, f.store, plan)
	must(t, err)
	if b, _ := os.ReadFile(claudeMD); string(b) != "edited after install\n" {
		t.Fatal("edited file removed without a choice")
	}
	if _, err := os.Lstat(contentSkill(f, "routing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unedited created SKILL.md was not removed")
	}
}

func TestWindowsRestoreCanBeUndone(t *testing.T) {
	f := newContentFixture(t)
	installed := install(t, f, claudeOnly, nil)
	markWindows(t, f, installed.ID)
	removed := restore(t, f, installed.ID, nil)
	home := f.env.AgentHome(profile.Claude)
	if removed == nil || len(removed.RemovedDirs) != 5 {
		t.Fatalf("restore did not remove the created parents: %+v", removed)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created agent home not removed")
	}
	markWindows(t, f, removed.ID)
	plan, err := PlanRestore(f.env, f.store, removed.ID)
	must(t, err)
	if it := findItem(t, plan, contentSkill(f, "routing")); it.Action != Recreate || it.Conflict {
		t.Fatalf("file removed by restore should be recreated without a conflict: %+v", it)
	}
	if it := findItem(t, plan, filepath.Dir(contentSkill(f, "routing"))); it.Action != CreateParent {
		t.Fatalf("removed parent not offered for creation: %+v", it)
	}
	undo, err := Execute(f.env, f.store, plan)
	must(t, err)
	if b, _ := os.ReadFile(contentSkill(f, "routing")); !strings.Contains(string(b), "name: routing") {
		t.Fatal("undo did not recreate SKILL.md")
	}
	markWindows(t, f, undo.ID)
	restore(t, f, undo.ID, nil)
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("redo did not remove the recreated parents")
	}
}

func TestWindowsExactSnapshotRefused(t *testing.T) {
	f := newContentFixture(t)
	existingSkill(t, f)
	m := install(t, f, claudeOnly, replaceAll)
	markWindows(t, f, m.ID)
	// An earlier Windows build wrote the same manifest without mode markers.
	path := filepath.Join(f.store.Dir, "backups", m.ID, "manifest.json")
	var raw map[string]any
	b, err := os.ReadFile(path)
	must(t, err)
	must(t, json.Unmarshal(b, &raw))
	delete(raw, "mode")
	for _, e := range raw["entries"].([]any) {
		delete(e.(map[string]any), "mode")
	}
	b, err = json.Marshal(raw)
	must(t, err)
	must(t, os.WriteFile(path, b, 0o600))
	before := tree(t, f.home)
	if _, err := PlanRestore(f.env, f.store, m.ID); err == nil || !strings.Contains(err.Error(), "exact-restore") {
		t.Fatalf("old Windows exact snapshot was not refused clearly: %v", err)
	}
	if tree(t, f.home) != before {
		t.Fatal("refusal changed the home")
	}
	if _, err := f.store.Load(m.ID); err != nil {
		t.Fatal("refused snapshot was not retained")
	}
}

func TestExactSnapshotStillRestoresOnHost(t *testing.T) {
	f := newFixture(t)
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses content-only snapshots")
	}
	m := install(t, f, claudeOnly, nil)
	if m.Mode != "" {
		t.Fatalf("host snapshot mode %q, want exact", m.Mode)
	}
	restore(t, f, m.ID, nil)
	if _, err := os.Lstat(filepath.Join(f.home, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exact restore did not return to the absent state")
	}
}

func TestContentNodesHaveNoMetadataDiff(t *testing.T) {
	cur := &fsnode.Node{Type: fsnode.File, Data: []byte("a\n")}
	want := &fsnode.Node{Type: fsnode.File, Data: []byte("b\n")}
	d := Diff(Item{Path: "SKILL.md", Current: cur, Want: want, Action: UpdateInPlace})
	if !strings.Contains(d, "-a") || !strings.Contains(d, "+b") || strings.Contains(d, "mode") {
		t.Fatalf("content diff: %s", d)
	}
}
