package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentfile/internal/installer"
	"agentfile/internal/profile"
	"agentfile/internal/state"
)

// newContentModel plans for Windows on any host.
func newContentModel(t *testing.T) (Model, string) {
	t.Helper()
	home := t.TempDir()
	local := filepath.Join(home, "AppData", "Local")
	if err := os.MkdirAll(local, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "LOCALAPPDATA" {
			return local
		}
		return ""
	}
	env := installer.Env{Home: home, GOOS: "windows", Getenv: getenv,
		LookPath: func(string) (string, error) { return "", errors.New("missing") }}
	root, err := state.DefaultRoot("windows", home, getenv)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return New(env, st), home
}

// leaveInterrupted turns a committed content-only apply back into the
// journal and prepared snapshot a crash after its writes would leave.
func leaveInterrupted(t *testing.T, m Model, snap *state.Manifest) {
	t.Helper()
	type step struct {
		Path   string `json:"path"`
		Status string `json:"status"`
		WantFP string `json:"want_fingerprint"`
		Mode   string `json:"mode"`
		FileID string `json:"file_id"`
	}
	j := map[string]any{"schema": 1, "snapshot": snap.ID, "kind": snap.Kind, "mode": state.ModeContent,
		"created_dirs": snap.CreatedDirs}
	var steps []step
	for _, e := range snap.Entries {
		steps = append(steps, step{e.Path, "done", e.InstalledFP, state.ModeContent, e.InstalledFileID})
	}
	j["steps"] = steps
	write := func(path string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(m.store.Dir, "journal.json"), j)
	snap.State = "prepared"
	write(filepath.Join(m.store.Dir, "backups", snap.ID, "manifest.json"), snap)
	if err := os.WriteFile(filepath.Join(m.store.Dir, "lock"), []byte("pid 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestContentRecoveryNeedsPerTargetChoice(t *testing.T) {
	m, home := newContentModel(t)
	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	os.MkdirAll(filepath.Dir(claudeMD), 0o755)
	os.WriteFile(claudeMD, []byte("mine\n"), 0o644)
	plan, err := installer.PlanInstall(m.env, m.store, profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range plan.Items {
		plan.Items[i].Replace = true
	}
	snap, err := installer.Execute(m.env, m.store, plan)
	if err != nil {
		t.Fatal(err)
	}
	leaveInterrupted(t, m, snap)
	m.goHome()
	if m.menu[0] != menuRecover {
		t.Fatalf("menu %v", m.menu)
	}
	m = press(t, m, "enter")
	if m.scr != scrRecover || len(m.recovery) != len(snap.Entries) {
		t.Fatalf("recovery review lists %d targets, want %d: %s", len(m.recovery), len(snap.Entries), m.result)
	}
	view := m.render()
	if !strings.Contains(view, "[ ] Choose restore or keep") || !strings.Contains(view, "CLAUDE.md") || strings.Contains(view, "rolls every path") {
		t.Fatalf("review does not show per-target choices:\n%s", view)
	}
	m = press(t, m, "enter")
	if m.scr != scrDetail || !strings.Contains(strings.Join(m.detail, "\n"), "+mine") {
		t.Fatalf("detail does not compare current and backed-up bytes:\n%s", strings.Join(m.detail, "\n"))
	}
	m = press(t, m, "esc", "y")
	if m.scr != scrRecover || !strings.Contains(m.render(), "Choose restore or keep for every file") {
		t.Fatalf("unselected files must stay in recovery review:\n%s", m.render())
	}
	if b, _ := os.ReadFile(claudeMD); strings.TrimSpace(string(b)) == "mine" || !m.store.Pending() {
		t.Fatal("recovery overwrote a target without a choice or cleared the journal")
	}
	for range m.recovery {
		m = press(t, m, "space", "down")
	}
	if !strings.Contains(m.render(), "[x] Restore saved copy") {
		t.Fatal("choice not shown")
	}
	m = press(t, m, "y")
	if m.scr != scrResult || !strings.Contains(m.result, "Recovery finished") {
		t.Fatalf("confirmed recovery: %s", m.result)
	}
	if b, _ := os.ReadFile(claudeMD); string(b) != "mine\n" {
		t.Fatalf("backed-up bytes not restored: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created files and parents not removed")
	}
	if m.store.Pending() || m.store.Locked() {
		t.Fatal("journal or lock left behind")
	}
}

func TestContentRecoveryCanKeepFiles(t *testing.T) {
	m, home := newContentModel(t)
	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	os.MkdirAll(filepath.Dir(claudeMD), 0o755)
	os.WriteFile(claudeMD, []byte("mine\n"), 0o644)
	plan, err := installer.PlanInstall(m.env, m.store, profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range plan.Items {
		plan.Items[i].Replace = true
	}
	snap, err := installer.Execute(m.env, m.store, plan)
	if err != nil {
		t.Fatal(err)
	}
	installed, _ := os.ReadFile(claudeMD)
	leaveInterrupted(t, m, snap)
	m.goHome()
	m = press(t, m, "enter")
	for i, r := range m.recovery {
		key := "r"
		if r.Path == claudeMD {
			key = "c"
		}
		m.cursor = i
		m = press(t, m, key)
	}
	m.cursor = 0
	first := m.recovery[0].Path
	want := m.choice[first]
	m = press(t, m, "r", "r")
	if m.choice[first] != "" {
		t.Fatal("pressing a choice again does not clear it")
	}
	m = press(t, m, "c", "r")
	if m.choice[first] != choiceRestore {
		t.Fatal("choice cannot be switched from keep to restore")
	}
	if want == choiceKeep {
		m = press(t, m, "c")
	}
	if !strings.Contains(m.render(), "[x] Keep current file") {
		t.Fatalf("keep choice not shown:\n%s", m.render())
	}
	m = press(t, m, "y")
	if m.scr != scrResult || !strings.Contains(m.result, "kept 1") || !strings.Contains(m.result, menuRestore) {
		t.Fatalf("recovery with a kept file: %s", m.result)
	}
	if b, _ := os.ReadFile(claudeMD); string(b) != string(installed) {
		t.Fatalf("kept file changed: %q", b)
	}
	if m.store.Pending() || m.store.Locked() {
		t.Fatal("journal or lock left behind")
	}
	if list, _ := m.store.List(); len(list) != 1 {
		t.Fatalf("saved copies not kept for Restore: %d backups", len(list))
	}
}

func TestContentPreviewShowsInPlaceAndExtraFiles(t *testing.T) {
	m, home := newContentModel(t)
	dir := filepath.Join(home, ".claude", "skills", "routing")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("old\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "helper.py"), []byte("print()\n"), 0o644)
	m = press(t, m, "enter", "space", "enter", "enter", "enter")
	if m.scr != scrPreview {
		t.Fatalf("expected preview: %s", m.result)
	}
	for m.plan.Items[m.cursor].Path != filepath.Join(dir, "SKILL.md") {
		m = press(t, m, "down")
	}
	view := m.render()
	for _, want := range []string{"update file contents", "helper.py", "partly updated"} {
		if !strings.Contains(view, want) {
			t.Errorf("preview missing %q:\n%s", want, view)
		}
	}
}
