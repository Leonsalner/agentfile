package installer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agentfile/internal/fsnode"
	"agentfile/internal/profile"
	"agentfile/internal/state"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	env   Env
	store *state.Store
	home  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	env := Env{
		Home: home, GOOS: runtime.GOOS,
		Getenv: func(k string) string {
			return map[string]string{"LOCALAPPDATA": filepath.Join(home, "AppData", "Local")}[k]
		},
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	}
	if runtime.GOOS == "windows" {
		must(t, os.MkdirAll(filepath.Join(home, "AppData", "Local"), 0o700))
	}
	root, err := state.DefaultRoot(runtime.GOOS, home, env.Getenv)
	must(t, err)
	st, err := state.Open(root)
	must(t, err)
	return &fixture{env: env, store: st, home: home}
}

func tree(t *testing.T, path string) string {
	t.Helper()
	n, err := fsnode.Capture(path)
	must(t, err)
	return fsnode.Fingerprint(n)
}

var both = profile.Profile{Claude: &profile.AgentProfile{Plan: "max-5x"}, Codex: &profile.AgentProfile{Plan: "plus"}}

func install(t *testing.T, f *fixture, p profile.Profile, choose func(*Item)) *state.Manifest {
	t.Helper()
	plan, err := PlanInstall(f.env, f.store, p)
	must(t, err)
	for i := range plan.Items {
		if choose != nil && plan.Items[i].Conflict {
			choose(&plan.Items[i])
		}
	}
	m, err := Execute(f.env, f.store, plan)
	must(t, err)
	return m
}

func restore(t *testing.T, f *fixture, id string, choose func(*Item)) *state.Manifest {
	t.Helper()
	plan, err := PlanRestore(f.env, f.store, id)
	must(t, err)
	for i := range plan.Items {
		if choose != nil && plan.Items[i].Conflict {
			choose(&plan.Items[i])
		}
	}
	m, err := Execute(f.env, f.store, plan)
	must(t, err)
	return m
}

func replaceAll(it *Item) { it.Replace = true }

// exactOnly skips tests of exact metadata restore, which Windows does not
// promise: it manages file contents only.
func exactOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows manages file contents only; see content_test.go")
	}
}

func TestFreshInstallAndRestoreToAbsent(t *testing.T) {
	f := newFixture(t)
	stateRel, _ := filepath.Rel(f.home, f.store.Dir)
	stateTop := strings.Split(stateRel, string(filepath.Separator))[0]
	before := treeWithout(t, f.home, stateTop)
	m := install(t, f, both, nil)
	if m == nil || len(m.Entries) != 8 {
		t.Fatalf("expected 8 entries, got %+v", m)
	}
	for _, a := range []profile.Agent{profile.Claude, profile.Codex} {
		for _, p := range f.env.ManagedPaths(a) {
			if _, err := os.Lstat(p); err != nil {
				t.Errorf("missing %s", p)
			}
		}
	}
	if b, _ := os.ReadFile(filepath.Join(f.home, ".codex", "skills", "routing", "SKILL.md")); !strings.Contains(string(b), "name: routing") {
		t.Error("skill not installed as a copy")
	}
	restore(t, f, m.ID, nil)
	for _, d := range []string{".claude", ".codex"} {
		if _, err := os.Lstat(filepath.Join(f.home, d)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("created %s not removed on restore", d)
		}
	}
	if treeWithout(t, f.home, stateTop) != before {
		t.Error("home differs from pre-install state outside the state root")
	}
}

// treeWithout fingerprints home minus one top-level entry (the state root).
func treeWithout(t *testing.T, home, skip string) string {
	t.Helper()
	n, err := fsnode.Capture(home)
	must(t, err)
	delete(n.Children, skip)
	return fsnode.Fingerprint(n)
}

func TestPreviewWritesNothing(t *testing.T) {
	f := newFixture(t)
	must(t, os.MkdirAll(filepath.Join(f.home, ".claude"), 0o755))
	must(t, os.WriteFile(filepath.Join(f.home, ".claude", "CLAUDE.md"), []byte("mine\n"), 0o644))
	homeBefore, stateBefore := tree(t, f.home), tree(t, f.store.Dir)
	plan, err := PlanInstall(f.env, f.store, both)
	must(t, err)
	for _, it := range plan.Items {
		_ = Diff(it)
	}
	Discover(f.env, f.store)
	if tree(t, f.home) != homeBefore || tree(t, f.store.Dir) != stateBefore {
		t.Fatal("preview or discovery changed targets or backup storage")
	}
	var paths []string
	for _, it := range plan.Items {
		paths = append(paths, it.Path)
	}
	if len(paths) != 8 {
		t.Fatalf("preview lists %d paths, want 8", len(paths))
	}
}

func TestCustomizedInstructionsKeptByDefault(t *testing.T) {
	f := newFixture(t)
	claudeMD := filepath.Join(f.home, ".claude", "CLAUDE.md")
	must(t, os.MkdirAll(filepath.Dir(claudeMD), 0o755))
	must(t, os.WriteFile(claudeMD, []byte("my own rules\n"), 0o600))
	plan, err := PlanInstall(f.env, f.store, both)
	must(t, err)
	var it Item
	for _, i := range plan.Items {
		if i.Path == claudeMD {
			it = i
		}
	}
	wantAction := ReplaceFile
	if runtime.GOOS == "windows" {
		wantAction = UpdateInPlace
	}
	if !it.Conflict || it.Replace || it.Action != wantAction {
		t.Fatalf("customized file should be a kept-by-default conflict: %+v", it)
	}
	if d := Diff(it); !strings.Contains(d, "-my own rules") || !strings.Contains(d, "+# CLAUDE.md") {
		t.Errorf("diff missing content:\n%.400s", d)
	}
	m, err := Execute(f.env, f.store, plan)
	must(t, err)
	if b, _ := os.ReadFile(claudeMD); string(b) != "my own rules\n" {
		t.Fatal("kept file was overwritten")
	}
	for _, e := range m.Entries {
		if e.Path == claudeMD {
			t.Fatal("kept file was backed up as changed")
		}
	}
	// Now choose replacement explicitly; the original is backed up and restorable.
	m2 := install(t, f, both, replaceAll)
	if b, _ := os.ReadFile(claudeMD); !strings.HasPrefix(string(b), "# CLAUDE.md") {
		t.Fatal("replacement not applied")
	}
	restore(t, f, m2.ID, nil)
	b, _ := os.ReadFile(claudeMD)
	fi, _ := os.Stat(claudeMD)
	if string(b) != "my own rules\n" {
		t.Fatal("original not restored")
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o not restored", fi.Mode().Perm())
	}
}

func TestSymlinkAndRealSkillDirRestored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks are rejected on Windows")
	}
	f := newFixture(t)
	skills := filepath.Join(f.home, ".claude", "skills")
	must(t, os.MkdirAll(filepath.Join(skills, "handoff"), 0o755))
	must(t, os.WriteFile(filepath.Join(skills, "handoff", "SKILL.md"), []byte("old\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(skills, "handoff", "script.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(t, os.Symlink("/some/checkout/skills/routing", filepath.Join(skills, "routing")))
	must(t, os.MkdirAll(filepath.Join(skills, "other-skill"), 0o755))
	must(t, os.WriteFile(filepath.Join(f.home, ".claude", "settings.json"), []byte("{}"), 0o600))
	unrelated := []string{filepath.Join(skills, "other-skill"), filepath.Join(f.home, ".claude", "settings.json")}
	fps := map[string]string{}
	for _, p := range append(unrelated, filepath.Join(skills, "handoff"), filepath.Join(skills, "routing")) {
		fps[p] = tree(t, p)
	}
	plan, err := PlanInstall(f.env, f.store, profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}})
	must(t, err)
	actions := map[string]Action{}
	for _, it := range plan.Items {
		actions[filepath.Base(it.Path)] = it.Action
	}
	if actions["routing"] != ReplaceSymlink || actions["handoff"] != ReplaceDir || actions["consult"] != Create {
		t.Fatalf("unexpected actions %v", actions)
	}
	for i := range plan.Items {
		plan.Items[i].Replace = true
	}
	for _, it := range plan.Items {
		if filepath.Base(it.Path) == "handoff" && !strings.Contains(Diff(it), "script.sh") {
			t.Error("non-Markdown entry missing from tree summary")
		}
	}
	m, err := Execute(f.env, f.store, plan)
	must(t, err)
	if fi, _ := os.Lstat(filepath.Join(skills, "routing")); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("symlink not replaced by a copy")
	}
	for _, p := range unrelated {
		if tree(t, p) != fps[p] {
			t.Errorf("unrelated %s changed", p)
		}
	}
	restore(t, f, m.ID, nil)
	for p, want := range fps {
		if tree(t, p) != want {
			t.Errorf("%s not restored exactly", p)
		}
	}
	if target, _ := os.Readlink(filepath.Join(skills, "routing")); target != "/some/checkout/skills/routing" {
		t.Errorf("symlink target %q not restored", target)
	}
	if _, err := os.Lstat(filepath.Join(skills, "consult")); !errors.Is(err, os.ErrNotExist) {
		t.Error("originally absent skill not removed")
	}
}

func TestSymlinkedParentBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	f := newFixture(t)
	real := filepath.Join(f.home, "dotfiles", "claude")
	must(t, os.MkdirAll(real, 0o755))
	must(t, os.Symlink(real, filepath.Join(f.home, ".claude")))
	plan, err := PlanInstall(f.env, f.store, both)
	must(t, err)
	for _, it := range plan.Items {
		if it.Agent == profile.Claude && (it.Blocked == "" || it.Proceeds()) {
			t.Fatalf("symlinked parent not blocked: %+v", it.Path)
		}
	}
	for i := range plan.Items {
		plan.Items[i].Replace = true
	}
	_, err = Execute(f.env, f.store, plan)
	must(t, err)
	if ents, _ := os.ReadDir(real); len(ents) != 0 {
		t.Fatal("wrote through symlinked parent")
	}
	states := Discover(f.env, f.store)
	if states[0].Problem == "" {
		t.Error("discovery did not report the symlinked home")
	}
}

func TestRepeatedNoOps(t *testing.T) {
	f := newFixture(t)
	m := install(t, f, both, nil)
	if again := install(t, f, both, replaceAll); again != nil {
		t.Fatal("second identical apply was not a no-op")
	}
	restore(t, f, m.ID, nil)
	if again := restore(t, f, m.ID, replaceAll); again != nil {
		t.Fatal("second identical restore was not a no-op")
	}
	list, _ := f.store.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 snapshots (apply, restore), got %d", len(list))
	}
}

func TestRestoreAfterUserEditNeedsConfirmation(t *testing.T) {
	f := newFixture(t)
	m := install(t, f, profile.Profile{Codex: &profile.AgentProfile{Plan: "plus"}}, nil)
	agents := filepath.Join(f.home, ".codex", "AGENTS.md")
	must(t, os.WriteFile(agents, []byte("edited after install\n"), 0o644))
	if s := Discover(f.env, f.store)[1].Targets[0].Status; s != "customized since agentfile installed it" {
		t.Errorf("discovery status %q", s)
	}
	restore(t, f, m.ID, nil) // decline the conflict
	if b, _ := os.ReadFile(agents); string(b) != "edited after install\n" {
		t.Fatal("edited file restored without confirmation")
	}
	if _, err := os.Lstat(filepath.Join(f.home, ".codex")); err != nil {
		t.Fatal("non-empty created dir was removed")
	}
	restore(t, f, m.ID, replaceAll)
	if _, err := os.Lstat(filepath.Join(f.home, ".codex")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("confirmed restore did not return to the absent state")
	}
}

func TestMetadataOnlyEditAfterInstallNeedsRestoreChoice(t *testing.T) {
	exactOnly(t)
	f := newFixture(t)
	p := profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}}
	m := install(t, f, p, nil)
	path := filepath.Join(f.home, ".claude", "CLAUDE.md")
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, os.Chtimes(path, old, old))
	plan, err := PlanRestore(f.env, f.store, m.ID)
	must(t, err)
	for _, item := range plan.Items {
		if item.Path == path {
			if !item.Conflict || item.Replace || item.Action != Remove {
				t.Fatalf("metadata-only edit was not kept by default: %+v", item)
			}
			return
		}
	}
	t.Fatal("managed file missing from restore preview")
}

func TestChangedCreatedParentIsKeptByDefault(t *testing.T) {
	exactOnly(t)
	f := newFixture(t)
	p := profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}}
	m := install(t, f, p, nil)
	dir := filepath.Join(f.home, ".claude", "skills")
	if runtime.GOOS != "windows" {
		must(t, os.Chmod(dir, 0o700))
	} else {
		old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		must(t, os.Chtimes(dir, old, old))
	}
	plan, err := PlanRestore(f.env, f.store, m.ID)
	must(t, err)
	for _, item := range plan.Items {
		if item.Path == dir {
			if !item.ParentCleanup || !item.Conflict || item.Replace {
				t.Fatalf("changed created parent was not kept by default: %+v", item)
			}
			if runtime.GOOS != "windows" && !strings.Contains(Diff(item), "mode 0700 → 0755") {
				t.Fatalf("parent preview did not explain changed mode: %s", Diff(item))
			}
			_, err := Execute(f.env, f.store, plan)
			must(t, err)
			fi, err := os.Stat(dir)
			must(t, err)
			if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
				t.Fatalf("kept parent mode changed to %o", fi.Mode().Perm())
			}
			return
		}
	}
	t.Fatal("created parent missing from restore preview")
}

func TestCleanupOnlyRestoreCanBeUndone(t *testing.T) {
	exactOnly(t)
	f := newFixture(t)
	p := profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}}
	installed := install(t, f, p, nil)
	for _, target := range f.env.ManagedPaths(profile.Claude) {
		must(t, os.RemoveAll(target))
	}
	parents := f.env.managedDirs(profile.Claude)
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, dir := range parents {
		if runtime.GOOS != "windows" {
			must(t, os.Chmod(dir, 0o700))
		}
		must(t, os.Chtimes(dir, old, old))
	}
	plan, err := PlanRestore(f.env, f.store, installed.ID)
	must(t, err)
	for i := range plan.Items {
		if plan.Items[i].ParentCleanup && plan.Items[i].Conflict {
			plan.Items[i].Replace = true
		}
	}
	cleanupBackup, err := Execute(f.env, f.store, plan)
	must(t, err)
	if cleanupBackup == nil || len(cleanupBackup.RemovedDirs) != len(parents) {
		t.Fatalf("cleanup-only restore did not back up removed parents: %+v", cleanupBackup)
	}
	for _, dir := range parents {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created parent was not removed: %s", dir)
		}
	}
	undo, err := PlanRestore(f.env, f.store, cleanupBackup.ID)
	must(t, err)
	undoBackup, err := Execute(f.env, f.store, undo)
	must(t, err)
	for _, dir := range parents {
		fi, err := os.Stat(dir)
		must(t, err)
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
			t.Errorf("recreated parent mode %o, want 0700", fi.Mode().Perm())
		}
		if !fi.ModTime().Equal(old) {
			t.Errorf("recreated parent mtime %v, want %v", fi.ModTime(), old)
		}
	}
	redo, err := PlanRestore(f.env, f.store, undoBackup.ID)
	must(t, err)
	_, err = Execute(f.env, f.store, redo)
	must(t, err)
	for _, dir := range parents {
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("undo of parent restoration did not remove %s", dir)
		}
	}
}

func TestUndoOfMixedParentRestoreRemovesChildBeforeParent(t *testing.T) {
	f := newFixture(t)
	home := f.env.AgentHome(profile.Claude)
	must(t, os.Mkdir(home, 0o755))
	p := profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}}
	installed := install(t, f, p, nil)
	removed := restore(t, f, installed.ID, nil)
	// The skills parent comes back as a restored entry, the home as a
	// directory created for it.
	must(t, os.Remove(home))
	undo := restore(t, f, removed.ID, nil)
	redo := restore(t, f, undo.ID, nil)
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created home was left behind; removed %v", redo.RemovedDirs)
	}
	restore(t, f, redo.ID, nil)
	if _, err := os.Stat(filepath.Join(home, "skills", "routing", "SKILL.md")); err != nil {
		t.Fatalf("undo did not recreate parents in order: %v", err)
	}
}

func TestCorruptRemovedParentMetadataIsRejected(t *testing.T) {
	f := newFixture(t)
	p := profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}}
	installed := install(t, f, p, nil)
	for _, target := range f.env.ManagedPaths(profile.Claude) {
		must(t, os.RemoveAll(target))
	}
	plan, err := PlanRestore(f.env, f.store, installed.ID)
	must(t, err)
	for i := range plan.Items {
		if plan.Items[i].ParentCleanup && plan.Items[i].Conflict {
			plan.Items[i].Replace = true
		}
	}
	backup, err := Execute(f.env, f.store, plan)
	must(t, err)
	if backup == nil || len(backup.RemovedDirStates) == 0 {
		t.Fatal("expected removed parent metadata")
	}
	backup.RemovedDirStates[0].Node.Mode ^= 0o100
	b, err := json.Marshal(backup)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(f.store.Dir, "backups", backup.ID, "manifest.json"), b, 0o600))
	if _, err := PlanRestore(f.env, f.store, backup.ID); err == nil {
		t.Fatal("corrupt removed parent metadata was accepted")
	}
}

func TestUndoCleanupOffersMetadataChoiceForRecreatedParent(t *testing.T) {
	exactOnly(t)
	f := newFixture(t)
	p := profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}}
	installed := install(t, f, p, nil)
	for _, target := range f.env.ManagedPaths(profile.Claude) {
		must(t, os.RemoveAll(target))
	}
	plan, err := PlanRestore(f.env, f.store, installed.ID)
	must(t, err)
	for i := range plan.Items {
		if plan.Items[i].ParentCleanup && plan.Items[i].Conflict {
			plan.Items[i].Replace = true
		}
	}
	backup, err := Execute(f.env, f.store, plan)
	must(t, err)
	parent := f.env.AgentHome(profile.Claude)
	must(t, os.Mkdir(parent, 0o700))
	unrelated := filepath.Join(parent, "unrelated.txt")
	must(t, os.WriteFile(unrelated, []byte("keep"), 0o600))
	undo, err := PlanRestore(f.env, f.store, backup.ID)
	must(t, err)
	for i := range undo.Items {
		item := &undo.Items[i]
		if item.Path != parent {
			continue
		}
		if !item.ParentRestore || !item.Conflict || item.Replace {
			t.Fatalf("existing parent metadata was not offered as a kept conflict: %+v", item)
		}
		item.Replace = true
		before, err := os.Stat(parent)
		must(t, err)
		undoBackup, err := Execute(f.env, f.store, undo)
		must(t, err)
		if b, err := os.ReadFile(unrelated); err != nil || string(b) != "keep" {
			t.Fatalf("unrelated child was changed: %q, %v", b, err)
		}
		redo, err := PlanRestore(f.env, f.store, undoBackup.ID)
		if err != nil {
			t.Fatalf("restore that updated parent metadata cannot be undone: %v", err)
		}
		_, err = Execute(f.env, f.store, redo)
		must(t, err)
		fi, err := os.Stat(parent)
		must(t, err)
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
			t.Errorf("undo left parent mode %o, want 0700", fi.Mode().Perm())
		}
		if !fi.ModTime().Equal(before.ModTime()) {
			t.Errorf("undo left parent mtime %v, want %v", fi.ModTime(), before.ModTime())
		}
		if b, err := os.ReadFile(unrelated); err != nil || string(b) != "keep" {
			t.Fatalf("unrelated child was changed by undo: %q, %v", b, err)
		}
		return
	}
	t.Fatal("recreated parent missing from undo preview")
}

func TestRestorePreviewAndWriteMetadataOnlyDifference(t *testing.T) {
	exactOnly(t)
	f := newFixture(t)
	path := filepath.Join(f.home, ".claude", "CLAUDE.md")
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte("original\n"), 0o644))
	originalTime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, os.Chtimes(path, originalTime, originalTime))
	p := profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}}
	m := install(t, f, p, replaceAll)
	must(t, os.WriteFile(path, []byte("original\n"), 0o644))
	laterTime := time.Date(2022, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, os.Chtimes(path, laterTime, laterTime))
	plan, err := PlanRestore(f.env, f.store, m.ID)
	must(t, err)
	for i := range plan.Items {
		item := &plan.Items[i]
		if item.Path != path {
			continue
		}
		if item.Action != ReplaceFile || !item.Conflict || item.Replace {
			t.Fatalf("metadata-only restore was not a conflict: %+v", item)
		}
		if !strings.Contains(Diff(*item), "mtime") {
			t.Fatal("preview did not explain the metadata difference")
		}
		item.Replace = true
		_, err = Execute(f.env, f.store, plan)
		must(t, err)
		fi, err := os.Stat(path)
		must(t, err)
		if !fi.ModTime().Equal(originalTime) {
			t.Errorf("mtime %v, want %v", fi.ModTime(), originalTime)
		}
		return
	}
	t.Fatal("managed file missing from restore preview")
}

func TestChangedAfterPreviewRefused(t *testing.T) {
	f := newFixture(t)
	plan, err := PlanInstall(f.env, f.store, both)
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(f.home, ".claude"), 0o755))
	must(t, os.WriteFile(filepath.Join(f.home, ".claude", "CLAUDE.md"), []byte("appeared\n"), 0o644))
	if _, err := Execute(f.env, f.store, plan); !errors.Is(err, state.ErrChanged) {
		t.Fatalf("want ErrChanged, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.home, ".claude", "CLAUDE.md")); string(b) != "appeared\n" {
		t.Fatal("file created after preview was overwritten")
	}
}

func TestCorruptSnapshotRestoreRefused(t *testing.T) {
	f := newFixture(t)
	m := install(t, f, both, nil)
	art := filepath.Join(f.store.Dir, "backups", m.ID, "entries", "0.json")
	must(t, os.WriteFile(art, []byte(`{"type":"file","data":"dGFtcGVyZWQ="}`), 0o600))
	if _, err := PlanRestore(f.env, f.store, m.ID); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
}

func TestPreparedSnapshotCannotBeRestored(t *testing.T) {
	f := newFixture(t)
	m := install(t, f, both, nil)
	path := filepath.Join(f.store.Dir, "backups", m.ID, "manifest.json")
	b, err := os.ReadFile(path)
	must(t, err)
	b = []byte(strings.Replace(string(b), `"state": "committed"`, `"state": "prepared"`, 1))
	must(t, os.WriteFile(path, b, 0o600))
	if _, err := PlanRestore(f.env, f.store, m.ID); err == nil {
		t.Fatal("uncommitted snapshot was offered for restore")
	}
}

func TestSnapshotFromAnotherOSCannotBeRestored(t *testing.T) {
	f := newFixture(t)
	m := install(t, f, both, nil)
	path := filepath.Join(f.store.Dir, "backups", m.ID, "manifest.json")
	b, err := os.ReadFile(path)
	must(t, err)
	other := "linux"
	if runtime.GOOS == "linux" {
		other = "windows"
	}
	b = []byte(strings.Replace(string(b), `"os": "`+runtime.GOOS+`"`, `"os": "`+other+`"`, 1))
	must(t, os.WriteFile(path, b, 0o600))
	if _, err := PlanRestore(f.env, f.store, m.ID); err == nil {
		t.Fatal("snapshot from another operating system was offered for restore")
	}
}

func TestEmptySnapshotCannotOpenRestorePreview(t *testing.T) {
	f := newFixture(t)
	m := install(t, f, both, nil)
	path := filepath.Join(f.store.Dir, "backups", m.ID, "manifest.json")
	m.Entries = nil
	// Content-only snapshots may hold only created parents to clean up.
	m.CreatedDirs, m.CreatedDirStates = nil, nil
	b, err := json.Marshal(m)
	must(t, err)
	must(t, os.WriteFile(path, b, 0o600))
	if _, err := PlanRestore(f.env, f.store, m.ID); err == nil {
		t.Fatal("empty snapshot opened a restore preview with no selectable rows")
	}
}

func TestTamperedManifestOutsideManagedSetRefused(t *testing.T) {
	f := newFixture(t)
	m := install(t, f, both, nil)
	path := filepath.Join(f.store.Dir, "backups", m.ID, "manifest.json")
	b, _ := os.ReadFile(path)
	target := filepath.Join(f.home, ".claude", "CLAUDE.md")
	b = []byte(strings.Replace(string(b), jsonEscape(target), jsonEscape(filepath.Join(f.home, ".ssh", "config")), 1))
	must(t, os.WriteFile(path, b, 0o600))
	must(t, os.WriteFile(filepath.Join(f.store.Dir, "backups", m.ID, "entries", "0.json"), []byte("corrupt"), 0o600))
	if _, err := PlanRestore(f.env, f.store, m.ID); err == nil || !strings.Contains(err.Error(), "outside the managed set") {
		t.Fatalf("unmanaged path was not rejected before its artifact was read: %v", err)
	}
}

func jsonEscape(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }

func TestDiscoveryWarnings(t *testing.T) {
	f := newFixture(t)
	f.env.Getenv = func(k string) string {
		if k == "CODEX_HOME" {
			return "/elsewhere/codex"
		}
		return ""
	}
	st := Discover(f.env, f.store)
	joined := strings.Join(append(st[0].Warnings, st[1].Warnings...), "\n")
	if !strings.Contains(joined, "`claude` was not found on PATH") || !strings.Contains(joined, "CODEX_HOME is set") {
		t.Errorf("warnings missing:\n%s", joined)
	}
}

func TestDiscoveryDoesNotInspectThroughSymlinkedAgentHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link requires privileges on Windows")
	}
	f := newFixture(t)
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "CLAUDE.md"), []byte("private"), 0o600))
	must(t, os.Symlink(outside, f.env.AgentHome(profile.Claude)))
	got := Discover(f.env, f.store)[0]
	if got.Problem == "" {
		t.Fatal("symlinked agent home was not rejected")
	}
	for _, target := range got.Targets {
		if target.Status != "not inspected: unsafe parent" {
			t.Errorf("%s status revealed a target behind the symlink: %q", target.Path, target.Status)
		}
	}
}
