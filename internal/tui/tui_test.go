package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"agentfile/internal/installer"
	"agentfile/internal/profile"
	"agentfile/internal/state"
)

func newModel(t *testing.T) (Model, string) {
	t.Helper()
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "LOCALAPPDATA" {
			return filepath.Join(home, "AppData", "Local")
		}
		return ""
	}
	if runtime.GOOS == "windows" {
		os.MkdirAll(filepath.Join(home, "AppData", "Local"), 0o700)
	}
	env := installer.Env{Home: home, GOOS: runtime.GOOS, Getenv: getenv,
		LookPath: func(string) (string, error) { return "", errors.New("missing") }}
	root, err := state.DefaultRoot(runtime.GOOS, home, getenv)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return New(env, st), home
}

func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "space":
			msg = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
		default:
			msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
		}
		next, _ := m.Update(msg)
		m = next.(Model)
	}
	return m
}

var modelNames = regexp.MustCompile(`\b(Opus|Sonnet|Sol|Luna|Astra|Fable|gpt-)`)

func TestInstallFlowNeverAsksForModels(t *testing.T) {
	m, home := newModel(t)
	if !strings.Contains(m.render(), "not on PATH") {
		t.Error("missing binary not reported")
	}
	// Install → select both → Claude Max 5x, no credits → ChatGPT Plus, credits.
	m = press(t, m, "enter")
	for _, stage := range []string{"agents"} {
		if modelNames.MatchString(m.render()) {
			t.Fatalf("%s screen mentions a model", stage)
		}
	}
	m = press(t, m, "space", "down", "space", "enter")
	if m.scr != scrPlan || m.asking != "claude" || modelNames.MatchString(m.render()) {
		t.Fatalf("expected Claude plan question without models, got screen %d", m.scr)
	}
	m = press(t, m, "down", "enter")
	if m.scr != scrCredits || modelNames.MatchString(m.render()) {
		t.Fatal("expected credits question without models")
	}
	m = press(t, m, "enter") // default "No"
	if m.scr != scrPlan || m.asking != "codex" {
		t.Fatal("expected Codex plan question")
	}
	m = press(t, m, "enter", "up", "enter") // Plus, Yes
	if m.scr != scrPreview {
		t.Fatalf("expected preview, got %d: %s", m.scr, m.result)
	}
	if m.prof.Claude.Plan != "max-5x" || m.prof.Claude.Credits || m.prof.Codex.Plan != "plus" || !m.prof.Codex.Credits {
		t.Fatalf("profile not captured: %+v %+v", m.prof.Claude, m.prof.Codex)
	}
	view := m.render()
	for _, p := range []string{"CLAUDE.md", "AGENTS.md", "routing", "handoff", "consult"} {
		if !strings.Contains(view, p) {
			t.Errorf("preview missing %s", p)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview wrote to the home")
	}
	m = press(t, m, "enter") // diff of first item
	if m.scr != scrDetail || !strings.Contains(strings.Join(m.detail, "\n"), "+# CLAUDE.md") {
		t.Fatal("detail view missing diff")
	}
	m = press(t, m, "esc", "a")
	if m.scr != scrConfirm {
		t.Fatal("expected confirmation")
	}
	m = press(t, m, "y")
	if m.scr != scrResult || !strings.Contains(m.result, "Installed 8 entries") {
		t.Fatalf("install result: %s", m.result)
	}
	b, err := os.ReadFile(filepath.Join(home, ".codex", "skills", "routing", "SKILL.md"))
	if err != nil || !strings.Contains(string(b), "paid extra usage as already enabled") {
		t.Fatal("routing skill missing or credits not applied")
	}
	m = press(t, m, "enter")
	if !strings.Contains(m.render(), "installed by agentfile") {
		t.Error("home screen does not show installed status")
	}
}

func TestConflictToggleAndRestoreFlow(t *testing.T) {
	m, home := newModel(t)
	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	os.MkdirAll(filepath.Dir(claudeMD), 0o755)
	os.WriteFile(claudeMD, []byte("mine\n"), 0o644)

	m = press(t, m, "enter", "space", "enter", "enter", "enter") // Claude, Pro, no credits
	if m.scr != scrPreview || !strings.Contains(m.render(), "[keep]") {
		t.Fatalf("conflict not shown as kept: %s", m.render())
	}
	m = press(t, m, "a", "y")
	if b, _ := os.ReadFile(claudeMD); string(b) != "mine\n" {
		t.Fatal("kept file overwritten")
	}
	m = press(t, m, "enter", "enter", "space", "enter", "enter", "enter")
	m = press(t, m, "space")
	if !strings.Contains(m.render(), "[repl]") {
		t.Fatal("toggle did not select replacement")
	}
	m = press(t, m, "a", "y")
	if b, _ := os.ReadFile(claudeMD); !strings.HasPrefix(string(b), "# CLAUDE.md") {
		t.Fatal("replacement not written")
	}

	// Restore the newest backup: the original file returns.
	m = press(t, m, "enter", "down", "enter")
	if m.scr != scrBackups || len(m.backups) != 2 || !strings.Contains(m.render(), "apply") {
		t.Fatalf("backups screen wrong: %s", m.render())
	}
	m = press(t, m, "enter")
	if m.scr != scrPreview || m.plan.Kind != "restore" {
		t.Fatal("expected restore preview")
	}
	m = press(t, m, "a", "y")
	if b, _ := os.ReadFile(claudeMD); string(b) != "mine\n" {
		t.Fatalf("restore did not return the original: %s", m.result)
	}
}

func TestNoOpApply(t *testing.T) {
	m, _ := newModel(t)
	m = press(t, m, "enter", "space", "enter", "enter", "enter", "a", "y", "enter")
	m = press(t, m, "enter", "space", "enter", "enter", "enter", "a")
	if m.scr != scrResult || !strings.Contains(m.result, "No backup was made") {
		t.Fatalf("no-op apply: %s", m.result)
	}
}

func TestRecoverMenuWhenLocked(t *testing.T) {
	m, _ := newModel(t)
	os.MkdirAll(m.store.Dir, 0o700)
	os.WriteFile(filepath.Join(m.store.Dir, "lock"), []byte("pid 1\n"), 0o600)
	m.goHome()
	if m.menu[0] != menuRecover {
		t.Fatalf("menu %v", m.menu)
	}
	m = press(t, m, "enter", "y")
	if m.scr != scrResult || !strings.Contains(m.result, "Recovery finished") || m.store.Locked() {
		t.Fatalf("recover: %s", m.result)
	}
}

func TestRestorePreviewListsCreatedParentCleanup(t *testing.T) {
	m, home := newModel(t)
	p := profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}}
	apply, err := installer.PlanInstall(m.env, m.store, p)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := installer.Execute(m.env, m.store, apply)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := installer.PlanRestore(m.env, m.store, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.plan, m.scr = restore, scrPreview
	view := m.render()
	for _, path := range []string{filepath.Join(home, ".claude"), filepath.Join(home, ".claude", "skills")} {
		found := false
		for _, line := range strings.Split(view, "\n") {
			found = found || strings.Contains(line, shortPath(home, path)) && strings.Contains(line, "remove empty parent directory")
		}
		if !found {
			t.Errorf("restore preview omitted created parent %s", path)
		}
	}
	m.execute()
	restored := fmt.Sprintf("Restored %d entries", len(snapshot.Entries)+len(snapshot.CreatedDirs))
	if !strings.Contains(m.result, restored) || !strings.Contains(m.result, filepath.Join(home, ".claude", "skills")) {
		t.Fatalf("restore result omitted removed parent directories: %s", m.result)
	}
}
