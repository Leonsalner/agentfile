package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"agentfile/internal/fsnode"
	"agentfile/internal/installer"
	"agentfile/internal/profile"
	"agentfile/internal/state"
)

func resize(m Model, w, h int) Model {
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(Model)
}

// plain is the screen as text, without styling.
func plain(m Model) []string {
	return strings.Split(sgrCodes.ReplaceAllString(m.render(), ""), "\n")
}

// checkFit fails unless a screen takes exactly the terminal's rows, with the
// key line on the last one, and no row is wider than the terminal: the
// renderer cuts off what does not fit.
func checkFit(t *testing.T, name string, m Model) {
	t.Helper()
	lines := plain(m)
	if len(lines) != m.height {
		t.Errorf("%s at %dx%d takes %d rows:\n%s", name, m.width, m.height, len(lines), strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		if cells(l) > m.width {
			t.Errorf("%s at %dx%d: %d cells in row %q", name, m.width, m.height, cells(l), l)
		}
	}
}

// walk drives a both-agents install with one conflicting file through every
// screen, then a restore, and calls visit on each screen.
func walk(t *testing.T, w, h int, visit func(name string, m Model)) {
	t.Helper()
	m, home := newModel(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.goHome()
	m = resize(m, w, h)
	step := func(name string, keys ...string) {
		t.Helper()
		m = press(t, m, keys...)
		visit(name, m)
	}
	visit("home", m)
	step("agents with nothing selected", "enter", "enter")
	step("agents", "space", "down", "space")
	step("plan", "enter")
	step("extra usage", "down", "enter")
	step("preview", "enter", "enter", "enter")
	if m.scr != scrPreview {
		t.Fatalf("expected the preview, got screen %d: %s", m.scr, m.result)
	}
	for i := 1; i < len(m.plan.Items); i++ {
		step(fmt.Sprintf("preview row %d", i), "down")
	}
	step("diff", "enter")
	step("diff paged", "space")
	step("preview with a replacement chosen", "esc", "g", "space")
	step("confirm", "a")
	step("result", "y")
	if !strings.Contains(m.result, "Installed 16 entries") {
		t.Fatalf("install result: %s", m.result)
	}
	step("result scrolled", "down", "down")
	step("home after install", "enter")
	step("backups", "down", "enter")
	step("restore preview", "enter")
	step("backups again", "esc")
	if m.scr != scrBackups {
		t.Fatalf("esc from a restore preview should return to the backups, got screen %d", m.scr)
	}
	m.fail(fmt.Errorf("open %s: permission denied", filepath.Join(home, ".claude", "CLAUDE.md")))
	visit("error", m)
}

func TestEveryScreenFitsTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}, {100, 16}, {60, 20}, {minWidth, minHeight}} {
		walk(t, size[0], size[1], func(name string, m Model) {
			if _, ok := m.layout(); !ok {
				t.Errorf("%s at %dx%d: the screen says the terminal is too small", name, m.width, m.height)
			}
			checkFit(t, name, m)
		})
	}
}

// At the size most terminals open with, no screen may lose its keys, its
// question or the row the cursor is on.
func TestStandardTerminalShowsCursorAndKeys(t *testing.T) {
	walk(t, 80, 24, func(name string, m Model) {
		lines := plain(m)
		last := lines[len(lines)-1]
		if !strings.Contains(last, "esc") && !strings.Contains(last, "quit") && !strings.Contains(last, "y write") {
			t.Errorf("%s: last row is not the key line: %q", name, last)
		}
		view := strings.Join(lines, "\n")
		for scr, question := range map[screen]string{
			scrAgents:  "Which agents should get the instructions and skills?",
			scrPlan:    "plan do you use?",
			scrCredits: "Have you already enabled paid extra usage for",
			scrConfirm: "unchanged by your choice?",
			scrPreview: " preview",
			scrBackups: "Backups  newest first",
		} {
			if m.scr == scr && !strings.Contains(view, question) {
				t.Errorf("%s: question %q is not on the screen:\n%s", name, question, view)
			}
		}
		switch m.scr {
		case scrHome, scrAgents, scrPlan, scrCredits, scrPreview, scrBackups:
			if n := strings.Count(strings.Join(lines, "\n"), "\n  › "); n != 1 {
				t.Errorf("%s: %d cursor rows:\n%s", name, n, strings.Join(lines, "\n"))
			}
		}
		if m.scr == scrPreview {
			marks := false
			for _, it := range m.plan.Items {
				marks = marks || it.Conflict
			}
			want := "› "
			if it := m.plan.Items[m.cursor]; it.Conflict && it.Replace {
				want += "[repl] "
			} else if it.Conflict {
				want += "[keep] "
			} else if marks {
				want += "       "
			}
			want += shortPath(m.env.Home, m.plan.Items[m.cursor].Path)
			if !strings.Contains(strings.Join(lines, "\n"), want) {
				t.Errorf("%s: cursor row %q is not in view:\n%s", name, want, strings.Join(lines, "\n"))
			}
		}
	})
}

func TestWindowKeepsCursorAndCountsHiddenRows(t *testing.T) {
	if got := window(nil, 0, 5); len(got) != 0 {
		t.Fatalf("window of an empty list = %q", got)
	}
	for n := 1; n <= 24; n++ {
		list := make([]string, n)
		for i := range list {
			list[i] = fmt.Sprintf("row %02d", i)
		}
		for page := 1; page <= 14; page++ {
			for cursor := 0; cursor < n; cursor++ {
				got := window(list, cursor, page)
				if len(got) != min(n, page) {
					t.Fatalf("n=%d page=%d cursor=%d: %d rows", n, page, cursor, len(got))
				}
				// Rows in order: an optional "↑ k more", rows first..last, an
				// optional "↓ k more"; the counts are the rows on each side.
				first, last, above, below := -1, -1, 0, 0
				for i, l := range got {
					l = strings.TrimSpace(sgrCodes.ReplaceAllString(l, ""))
					var at int
					switch {
					case strings.HasPrefix(l, "row "):
						fmt.Sscanf(l, "row %d", &at)
						if first < 0 {
							first = at
						} else if at != last+1 {
							t.Fatalf("n=%d page=%d cursor=%d: rows out of order: %q", n, page, cursor, got)
						}
						last = at
					case strings.HasPrefix(l, "↑ ") && i == 0:
						fmt.Sscanf(l, "↑ %d more", &above)
					case strings.HasPrefix(l, "↓ ") && i == len(got)-1:
						fmt.Sscanf(l, "↓ %d more", &below)
					default:
						t.Fatalf("n=%d page=%d cursor=%d: unexpected row %q", n, page, cursor, l)
					}
				}
				if cursor < first || cursor > last {
					t.Fatalf("n=%d page=%d cursor=%d: cursor outside rows %d..%d", n, page, cursor, first, last)
				}
				if page >= 3 && (above != first || below != n-1-last) {
					t.Fatalf("n=%d page=%d cursor=%d: markers say %d above and %d below rows %d..%d", n, page, cursor, above, below, first, last)
				}
			}
		}
	}
}

func TestTextHelpersCountTerminalCells(t *testing.T) {
	for s, want := range map[string]int{"abc": 3, "技能": 4, "e\u0301": 1, "☕": 2, "\x1b[1;36m›\x1b[0m x": 3, "": 0} {
		if got := cells(s); got != want {
			t.Errorf("cells(%q) = %d, want %d", s, got, want)
		}
	}
	if got := clip("~/.claude/技能/SKILL.md", 14); cells(got) > 14 || !strings.HasSuffix(got, "…") {
		t.Errorf("clip = %q (%d cells)", got, cells(got))
	}
	if got := clipLeft("~/.claude/skills/routing", 12); got != "…lls/routing" {
		t.Errorf("clipLeft = %q", got)
	}
	if got := clip("short", 10) + clipLeft("short", 10) + clip("gone", 0); got != "shortshort" {
		t.Errorf("clip changed text that fits, or kept text with no room: %q", got)
	}
	if got := clipLeft("Xe\u0301ab", 3); got != "…ab" {
		t.Errorf("clipLeft left a combining mark on the ellipsis: %q", got)
	}
	if got := clipStyled("\x1b[1mabcdefgh\x1b[0m tail", 6); cells(got) != 6 || !strings.HasSuffix(got, "…\x1b[0m") {
		t.Errorf("clipStyled = %q (%d cells)", got, cells(got))
	}
	// A word wider than the line is broken, not left for the terminal to cut.
	long := wrap("see /a/very/long/path/without/any/space/in/it/at/all now", 16)
	if strings.Join(long, "") != "see/a/very/long/path/without/any/space/in/it/at/allnow" {
		t.Errorf("wrap lost text: %q", long)
	}
	for _, l := range long {
		if cells(l) > 16 {
			t.Errorf("wrap line %q is wider than 16", l)
		}
	}
	for _, l := range wrap("  one two three four five six seven\n\nlast", 12) {
		if cells(l) > 12 || (l != "" && l != "last" && !strings.HasPrefix(l, "  ")) {
			t.Errorf("wrap line %q", l)
		}
	}
	table := columns([][]string{{"saved", "kind", "size"}, {"2026-11-18 19:30", "apply", "13.8 KiB"}}, 24)
	if table[0] != "saved             kind" || table[1] != "2026-11-18 19:30  apply" {
		t.Errorf("columns = %q", table)
	}
}

func TestShortPathOnlyShortensPathsUnderHome(t *testing.T) {
	home := filepath.Join(string(filepath.Separator)+"home", "u")
	for p, want := range map[string]string{
		filepath.Join(home, ".claude", "CLAUDE.md"): filepath.Join("~", ".claude", "CLAUDE.md"),
		filepath.Join(home, "..notes"):              filepath.Join("~", "..notes"),
		filepath.Join(home, "..", "other", "x"):     filepath.Join(string(filepath.Separator)+"home", "other", "x"),
	} {
		if got := shortPath(home, p); got != want {
			t.Errorf("shortPath(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestAgentsScreenAsksForASelection(t *testing.T) {
	m, _ := newModel(t)
	m = press(t, m, "enter", "enter")
	if m.scr != scrAgents || !strings.Contains(m.render(), "Select at least one agent") {
		t.Fatalf("enter with no agent selected should stay and say why:\n%s", m.render())
	}
	if m = press(t, m, "down"); strings.Contains(m.render(), "Select at least one agent") {
		t.Error("the notice should clear on the next key")
	}
}

func TestBackFromExtraUsageKeepsThePlan(t *testing.T) {
	m, _ := newModel(t)
	m = press(t, m, "enter", "space", "enter", "down", "down", "enter", "esc")
	if m.scr != scrPlan || profile.Plans[profile.Claude][m.cursor].ID != "max-20x" {
		t.Fatalf("esc should return to the chosen plan, got screen %d row %d", m.scr, m.cursor)
	}
}

func TestBlockedPathsAreNotCountedAsKept(t *testing.T) {
	m, _ := newModel(t)
	m = press(t, m, "enter", "space", "enter", "enter", "enter")
	for i := range m.plan.Items {
		m.plan.Items[i].Blocked = "parent directory is not writable"
	}
	if n, kept, blocked := m.counts(); n != 0 || kept != 0 || blocked != len(m.plan.Items) {
		t.Fatalf("counts = %d to write, %d kept, %d blocked", n, kept, blocked)
	}
	view := strings.Join(plain(m), "\n")
	if !strings.Contains(view, fmt.Sprintf("0 to write, 0 kept, %d blocked", len(m.plan.Items))) || strings.Contains(view, "press space on a [keep] row") {
		t.Errorf("preview should count blocked paths apart and not suggest replacing:\n%s", view)
	}
	// With nothing that can be written, apply stays on the preview and says why.
	m = press(t, m, "a")
	if view := strings.Join(plain(m), "\n"); m.scr != scrPreview || !strings.Contains(view, "Nothing can be written") {
		t.Errorf("applying only blocked paths:\n%s", view)
	}
	if m = press(t, m, "space"); !strings.Contains(strings.Join(plain(m), "\n"), "This path is blocked") {
		t.Error("space on a blocked row should say there is no choice to make")
	}
}

func TestPreviewNoteFollowsTheChoice(t *testing.T) {
	m, home := newModel(t)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), []byte("mine\n"), 0o644)
	m = press(t, m, "enter", "space", "enter", "enter", "enter")
	action := string(m.plan.Items[0].Action)
	if view := strings.Join(plain(m), "\n"); !strings.Contains(view, "Kept as it is") || !strings.Contains(view, `Press space to choose "`+action+`"`) {
		t.Fatalf("a kept conflict should say so and how to change it:\n%s", view)
	}
	m = press(t, m, "space")
	if view := strings.Join(plain(m), "\n"); !strings.Contains(view, "Chosen: "+action) || strings.Contains(view, "Kept as it is") {
		t.Fatalf("a chosen replacement should replace the kept note:\n%s", view)
	}
	// Space where there is nothing to choose answers instead of doing nothing.
	m = press(t, m, "down", "space")
	if view := strings.Join(plain(m), "\n"); !strings.Contains(view, "no choice to make") {
		t.Fatalf("space on a path with no choice should say so:\n%s", view)
	}
	if m = press(t, m, "down"); strings.Contains(strings.Join(plain(m), "\n"), "no choice to make") {
		t.Error("the notice should clear on the next key")
	}
}

// A conflict whose path is gone is kept missing, not "kept as it is", and
// space offers to bring it back.
func TestPreviewNoteSaysWhenAKeptPathIsGone(t *testing.T) {
	m, _ := newModel(t)
	m = press(t, m, "enter", "space", "enter", "enter", "enter")
	it := &m.plan.Items[0]
	it.Current, it.Action, it.Conflict, it.Replace = &fsnode.Node{Type: fsnode.Absent}, installer.Recreate, true, false
	it.Status = "missing since that operation"
	view := strings.Join(strings.Fields(strings.Join(plain(m), " ")), " ")
	if !strings.Contains(view, `Kept missing: this path is gone (missing since that operation). Press space to choose "restore missing file" instead.`) ||
		strings.Contains(view, "Kept as it is") {
		t.Fatalf("a kept conflict that is gone should say so:\n%s", strings.Join(plain(m), "\n"))
	}
	m = press(t, m, "space")
	if view := strings.Join(plain(m), "\n"); !it.Replace || !strings.Contains(view, "Chosen: restore missing file") {
		t.Fatalf("space should choose to restore the missing file:\n%s", view)
	}
}

// The confirm screen must not promise more than the state layer does: a
// change found before the first write stops the operation, and one found
// partway is rolled back, so "nothing is written" would overstate it.
func TestConfirmSaysWhatTheRecheckDoes(t *testing.T) {
	m, _ := newModel(t)
	m = resize(m, 80, 24)
	m = press(t, m, "enter", "space", "enter", "enter", "enter", "a")
	if m.scr != scrConfirm {
		t.Fatalf("expected the confirm screen, got %d", m.scr)
	}
	view := strings.Join(strings.Fields(strings.Join(plain(m), " ")), " ")
	for _, want := range []string{
		"Install: write 8 path(s), keep 0 unchanged by your choice?",
		"The current state of each path is backed up first to",
		"If one changed after this preview, the operation stops; a change found partway rolls back what this operation wrote.",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("confirm screen missing %q:\n%s", want, strings.Join(plain(m), "\n"))
		}
	}
	if strings.Contains(view, "nothing is written") {
		t.Errorf("confirm screen claims nothing is written:\n%s", strings.Join(plain(m), "\n"))
	}
}

func TestFailureSaysHowTheOperationEnded(t *testing.T) {
	m, _ := newModel(t)
	for want, err := range map[string]error{
		"Stopped before writing anything": fmt.Errorf("%w: p", state.ErrChanged),
		"Stopped and rolled back":         fmt.Errorf("%w: p (all changes were rolled back)", state.ErrChanged),
		"Recovery needed":                 fmt.Errorf("%w: p; rollback also failed (x): the journal and snapshot s are kept, choose Recover", state.ErrChanged),
		"Error: disk full":                fmt.Errorf("disk full"),
		// A failed rollback is reported as such, whatever stopped it.
		"Recovery needed:": fmt.Errorf("disk full; rollback also failed (%v): the journal and snapshot s are kept, choose Recover",
			fmt.Errorf("%w: p changed after interruption", state.ErrRecoveryConflict)),
		"Recovery stopped":         fmt.Errorf("%w: p changed after interruption", state.ErrRecoveryConflict),
		"Stopped: an interrupted":  state.ErrRecoveryNeeded,
		"Stopped: state is locked": fmt.Errorf("%w by lock: another agentfile may be running; if none is, choose Recover", state.ErrLocked),
	} {
		m.fail(err)
		if !strings.HasPrefix(m.result, want) {
			t.Errorf("fail(%v) = %q, want prefix %q", err, m.result, want)
		}
		if view := strings.Join(plain(m), "\n"); !strings.Contains(view, "✗ "+want) {
			t.Errorf("the failure should be marked as failed:\n%s", view)
		}
		// Whatever leaves a journal or lock behind names the menu entry that clears it.
		if left := strings.HasPrefix(want, "Recovery") || strings.HasPrefix(want, "Stopped: "); left != strings.Contains(m.result, `"`+menuRecover+`"`) {
			t.Errorf("fail(%v) = %q: names %q = %v, want %v", err, m.result, menuRecover, !left, left)
		}
	}
	// A recovery that stops at a conflict may already have restored earlier
	// paths; it must not claim that nothing was overwritten.
	m.fail(fmt.Errorf("%w: p changed after interruption", state.ErrRecoveryConflict))
	if !strings.Contains(m.result, "may already be restored") || strings.Contains(m.result, "Nothing was overwritten") {
		t.Errorf("recovery conflict: %q", m.result)
	}
}

func TestResultShowsShortPathsAndTheNextStep(t *testing.T) {
	m, home := newModel(t)
	m = resize(m, 80, 24)
	m = press(t, m, "enter", "space", "down", "space", "enter", "enter", "enter", "enter", "enter", "a", "y")
	// Every written path is on the result screen, on its first page where the
	// two columns fit and by scrolling where the paths are too long for them.
	seen := ""
	for i := 0; i < 40; i++ {
		seen += strings.Join(plain(m), "\n") + "\n"
		m = press(t, m, "down")
	}
	for _, it := range m.plan.Items {
		if short := shortPath(home, it.Path); !strings.Contains(seen, "• "+short) {
			t.Errorf("result never shows %s", short)
		}
	}
	for _, want := range []string{"✓ Installed 16 entries.", "Start a new session in each agent"} {
		if !strings.Contains(seen, want) {
			t.Errorf("result missing %q:\n%s", want, seen)
		}
	}
	if strings.Contains(seen, home) {
		t.Errorf("result shows the home directory in full:\n%s", seen)
	}
	if first := seen[:strings.Index(seen, "quit")]; runtime.GOOS != "windows" && strings.Contains(first, " more") {
		t.Errorf("a full install should fit one 80x24 page:\n%s", first)
	}
}

// A failure longer than the screen keeps what happened as its title and can
// be read to its end by scrolling.
func TestLongFailureCanBeReadToItsEnd(t *testing.T) {
	m, _ := newModel(t)
	m = resize(m, minWidth, minHeight)
	m.fail(fmt.Errorf("cannot restore this backup: %s the last words", strings.Repeat("it was made by an older version; ", 12)))
	checkFit(t, "long failure", m)
	if view := strings.Join(plain(m), "\n"); !strings.Contains(view, "  ✗ Error:\n") {
		t.Errorf("the title should say what happened and leave the details to the body:\n%s", view)
	}
	seen := ""
	for i := 0; i < 60; i++ {
		seen += strings.Join(plain(m), " ")
		m = press(t, m, "down")
		checkFit(t, "long failure, scrolled", m)
	}
	if seen = strings.Join(strings.Fields(seen), " "); !strings.Contains(seen, "cannot restore this backup") || !strings.Contains(seen, "the last words") {
		t.Errorf("the failure cannot be read from start to end:\n%s", seen)
	}
}

func TestRecoveryReviewFitsAndKeepsItsKeys(t *testing.T) {
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
	if m = press(t, m, "enter"); m.scr != scrRecover || len(m.recovery) < 2 {
		t.Fatalf("expected a recovery review of several files, got screen %d with %d", m.scr, len(m.recovery))
	}
	for _, size := range [][2]int{{80, 24}, {60, 14}, {minWidth, minHeight}} {
		r := resize(m, size[0], size[1])
		if _, ok := r.layout(); !ok {
			t.Fatalf("%dx%d: recovery says the terminal is too small", size[0], size[1])
		}
		for i := range r.recovery {
			checkFit(t, "recovery", r)
			view := strings.Join(strings.Fields(strings.Join(plain(r), " ")), " ")
			// The r key is named after what it does to the selected file.
			restore := "r restore"
			if r.recovery[i].Backup.Type == fsnode.Absent {
				restore = "r remove"
			}
			for _, want := range []string{"y finish", "n later", restore, "c keep", "› [", filepath.Base(r.recovery[i].Path)} {
				if !strings.Contains(view, want) {
					t.Fatalf("%dx%d row %d: recovery lost %q:\n%s", size[0], size[1], i, want, strings.Join(plain(r), "\n"))
				}
			}
			r = press(t, r, "r", "down")
		}
	}
}

// Recovery and clearing the lock are only safe when no other agentfile runs.
// Every size that accepts y on those screens must show that warning: a small
// terminal may lose the explanation, never the warning.
func TestRecoveryAlwaysShowsTheSoleRunWarning(t *testing.T) {
	warned := func(name string, m Model) {
		t.Helper()
		for _, size := range [][2]int{{minWidth, minHeight}, {minWidth, minHeight + 1}, {60, 12}, {80, 24}} {
			r := resize(m, size[0], size[1])
			if _, ok := r.layout(); !ok {
				t.Errorf("%s at %dx%d says the terminal is too small", name, size[0], size[1])
			}
			checkFit(t, name, r)
			if view := strings.Join(strings.Fields(strings.Join(plain(r), " ")), " "); !strings.Contains(view, soleRunWarning) {
				t.Errorf("%s at %dx%d accepts y without the warning:\n%s", name, size[0], size[1], strings.Join(plain(r), "\n"))
			}
		}
	}
	// A lock with no journal behind it, then the same lock with a journal.
	m, _ := newModel(t)
	os.MkdirAll(m.store.Dir, 0o700)
	os.WriteFile(filepath.Join(m.store.Dir, "lock"), []byte("pid 1\n"), 0o600)
	m.goHome()
	if m = press(t, m, "enter"); m.scr != scrRecover || len(m.recovery) != 0 {
		t.Fatalf("expected the lock screen, got screen %d: %s", m.scr, m.result)
	}
	warned("lock only", m)
	if view := strings.Join(plain(resize(m, 80, 24)), "\n"); !strings.Contains(view, "y clear the lock") {
		t.Fatalf("expected the lock-only wording:\n%s", view)
	}
	os.WriteFile(filepath.Join(m.store.Dir, "journal.json"), []byte("{}"), 0o600)
	warned("interrupted operation", m)
	if view := strings.Join(plain(resize(m, 80, 24)), "\n"); !strings.Contains(view, "y recover") {
		t.Fatalf("expected the interrupted-operation wording:\n%s", view)
	}
	// The per-file review on Windows.
	c, home := newContentModel(t)
	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	os.MkdirAll(filepath.Dir(claudeMD), 0o755)
	os.WriteFile(claudeMD, []byte("mine\n"), 0o644)
	plan, err := installer.PlanInstall(c.env, c.store, profile.Profile{Claude: &profile.AgentProfile{Plan: "pro"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range plan.Items {
		plan.Items[i].Replace = true
	}
	snap, err := installer.Execute(c.env, c.store, plan)
	if err != nil {
		t.Fatal(err)
	}
	leaveInterrupted(t, c, snap)
	c.goHome()
	if c = press(t, c, "enter"); c.scr != scrRecover || len(c.recovery) < 2 {
		t.Fatalf("expected a recovery review, got screen %d with %d files", c.scr, len(c.recovery))
	}
	warned("recovery review", c)
}

// interruptedInstall leaves a Windows install of both agents interrupted
// after its writes and opens the recovery review of its sixteen files.
func interruptedInstall(t *testing.T) Model {
	t.Helper()
	m, home := newContentModel(t)
	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	os.MkdirAll(filepath.Dir(claudeMD), 0o755)
	os.WriteFile(claudeMD, []byte("mine\n"), 0o644)
	plan, err := installer.PlanInstall(m.env, m.store, profile.Profile{
		Claude: &profile.AgentProfile{Plan: "pro"}, Codex: &profile.AgentProfile{Plan: "plus"}})
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
	if m = press(t, m, "enter"); m.scr != scrRecover || len(m.recovery) != 16 {
		t.Fatalf("expected a recovery review of 16 files, got screen %d with %d: %s", m.scr, len(m.recovery), m.result)
	}
	return m
}

var reviewSizes = [][2]int{{minWidth, minHeight}, {minWidth, minHeight + 2}, {60, 14}, {80, 10}, {80, 24}}

// flat is the screen as one line of text, so that a wrapped sentence matches.
func flat(m Model) string {
	return strings.Join(strings.Fields(strings.Join(plain(m), " ")), " ")
}

// The preview's count is what "a apply" acts on. It must be on the screen
// at every size that accepts the key, blocked paths included.
func TestPreviewAlwaysShowsItsCount(t *testing.T) {
	m, home := newModel(t)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), []byte("mine\n"), 0o644)
	m = press(t, m, "enter", "space", "down", "space", "enter", "enter", "enter", "enter", "enter")
	if m.scr != scrPreview {
		t.Fatalf("expected the preview, got screen %d: %s", m.scr, m.result)
	}
	m.plan.Items[len(m.plan.Items)-1].Blocked = "parent directory is a symbolic link"
	for _, size := range reviewSizes {
		r := resize(m, size[0], size[1])
		for i := range r.plan.Items {
			if _, ok := r.layout(); !ok {
				t.Fatalf("%dx%d row %d: the preview says the terminal is too small", size[0], size[1], i)
			}
			checkFit(t, "preview", r)
			if view := flat(r); !strings.Contains(view, "14 to write, 1 kept, 1 blocked") || strings.Count(view, "› ") != 1 {
				t.Fatalf("%dx%d row %d: the preview accepts a without its count, or lost its cursor row:\n%s",
					size[0], size[1], i, strings.Join(plain(r), "\n"))
			}
			r = press(t, r, "down")
		}
	}
}

// How many files still need a choice decides whether y can finish recovery.
// The count, and the answer to a y that could not finish, must be on the
// screen at every size that accepts the key.
func TestRecoveryReviewAlwaysShowsItsCount(t *testing.T) {
	m := interruptedInstall(t)
	for _, size := range reviewSizes {
		r := resize(m, size[0], size[1])
		if _, ok := r.layout(); !ok {
			t.Fatalf("%dx%d: the review says the terminal is too small", size[0], size[1])
		}
		checkFit(t, "recovery review", r)
		if view := flat(r); !strings.Contains(view, "16 of 16 files still need a choice.") || strings.Contains(view, "Not finished") {
			t.Fatalf("%dx%d: the review accepts y without its count:\n%s", size[0], size[1], strings.Join(plain(r), "\n"))
		}
		// y with choices missing changes nothing and says so on the screen.
		r = press(t, r, "y")
		if _, ok := r.layout(); !ok || r.scr != scrRecover || !r.store.Pending() {
			t.Fatalf("%dx%d: y with choices missing left the review usable=%v on screen %d", size[0], size[1], ok, r.scr)
		}
		checkFit(t, "recovery review after y", r)
		if view := flat(r); !strings.Contains(view, "Not finished: 16 of 16 files still need a choice.") || !strings.Contains(view, soleRunWarning) {
			t.Fatalf("%dx%d: y with choices missing seems to do nothing:\n%s", size[0], size[1], strings.Join(plain(r), "\n"))
		}
		// The next choice clears that answer and counts down.
		r = press(t, r, "c")
		if view := flat(r); !strings.Contains(view, "15 of 16 files still need a choice.") || strings.Contains(view, "Not finished") {
			t.Fatalf("%dx%d: a choice did not update the count:\n%s", size[0], size[1], strings.Join(plain(r), "\n"))
		}
		r = press(t, r, "c") // undo it: the choices are shared between sizes
	}
	r := resize(m, minWidth, minHeight)
	for range r.recovery {
		r = press(t, r, "c", "down")
	}
	checkFit(t, "recovery review, all chosen", r)
	if view := flat(r); !strings.Contains(view, "Every file has a choice.") {
		t.Fatalf("the review does not say that every file has a choice:\n%s", strings.Join(plain(r), "\n"))
	}
}

// What restoring does to a file is what the user chooses r on: it puts a
// changed file back, removes one that did not exist before, and creates one
// that is missing. Every size that accepts r must say which, and a size with
// room for the notes must give the status whole, not cut off in its row.
func TestRecoveryReviewSaysWhatRestoringDoes(t *testing.T) {
	m := interruptedInstall(t)
	// The file that existed before is now missing; the others are new.
	if err := os.Remove(m.recovery[0].Path); err != nil {
		t.Fatal(err)
	}
	if m.openRecover(); m.scr != scrRecover || len(m.recovery) != 16 || m.recovery[0].Current.Type != fsnode.Absent {
		t.Fatalf("expected the review with its first file missing, got screen %d with %d: %s", m.scr, len(m.recovery), m.result)
	}
	for _, size := range reviewSizes {
		r := resize(m, size[0], size[1])
		for i, file := range r.recovery {
			key, status := "r remove", "did not exist before; restoring removes it"
			if i == 0 {
				key, status = "r recreate", "missing; restoring creates a new copy with default permissions"
			}
			if _, ok := r.layout(); !ok {
				t.Fatalf("%dx%d row %d: the review says the terminal is too small", size[0], size[1], i)
			}
			checkFit(t, "recovery review", r)
			view := flat(r)
			if !strings.Contains(view, key+" ") || strings.Contains(view, "r restore") {
				t.Fatalf("%dx%d row %d: the keys do not say that r is %q here:\n%s", size[0], size[1], i, key, strings.Join(plain(r), "\n"))
			}
			if size[1] >= 24 && !strings.Contains(view, shortPath(r.env.Home, file.Path)+": "+status) {
				t.Fatalf("%dx%d row %d: the status %q is not shown whole:\n%s", size[0], size[1], i, status, strings.Join(plain(r), "\n"))
			}
			r = press(t, r, "down")
		}
	}
	// A file that only differs keeps the plain name, and a terminal wide
	// enough for every status in its row needs no note.
	plainFile := interruptedInstall(t)
	wide := resize(plainFile, 160, 24)
	if view := flat(wide); !strings.Contains(view, "r restore ") || !strings.Contains(view, "differs from backup") ||
		strings.Contains(view, shortPath(wide.env.Home, wide.recovery[0].Path)+": ") {
		t.Fatalf("a file that differs should offer r restore, with its status in its row:\n%s", strings.Join(plain(wide), "\n"))
	}
}

// Rewrapping on a resize must not move the reader to another part of a diff.
func TestDiffKeepsItsPlaceOnResize(t *testing.T) {
	m, _ := newModel(t)
	m = resize(m, 100, 24)
	m = press(t, m, "enter", "space", "enter", "enter", "enter", "enter")
	if m.scr != scrDetail || len(m.detailLines()) < 60 {
		t.Fatalf("expected a long diff, got screen %d with %d rows", m.scr, len(m.detailLines()))
	}
	for i := 0; i < 15; i++ {
		m = press(t, m, "down")
	}
	top := m.detailLines()[m.scroll].src
	for _, width := range []int{50, 140, 60} {
		m = resize(m, width, 24)
		checkFit(t, "diff", m)
		if got := m.detailLines()[m.scroll].src; got != top {
			t.Fatalf("at width %d the diff moved from source line %d to %d", width, top, got)
		}
	}
	m = press(t, m, "G")
	for _, width := range []int{45, 120} {
		m = resize(m, width, 24)
		if lines := m.detailLines(); m.scroll != max(len(lines)-m.detailPage(), 0) {
			t.Fatalf("at width %d the diff left its end: row %d of %d", width, m.scroll, len(lines))
		}
	}
	// The path and action stay in view while the diff scrolls.
	if view := strings.Join(plain(m), "\n"); !strings.Contains(view, "CLAUDE.md  ·  create") {
		t.Errorf("the diff lost its heading:\n%s", view)
	}
}

// One very long line, as in a minified file, must wrap in one pass.
func TestDiffWrapsAVeryLongLineQuickly(t *testing.T) {
	start := time.Now()
	lines := wrapDetail([]string{"path", "action", "", "+" + strings.Repeat("x", 1<<20)}, 76)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("wrapping a 1 MiB line took %s", took)
	}
	if want := ((1<<20)+1-76+73)/74 + 1; len(lines) != want {
		t.Fatalf("%d rows, want %d", len(lines), want)
	}
	for _, l := range lines {
		if cells(l.text) > 76 {
			t.Fatalf("row of %d cells", cells(l.text))
		}
	}
}

// A terminal too small to show a screen says so, and must not act on keys
// whose meaning is not on the screen: "y" on the confirm screen writes.
func TestTerminalTooSmallIgnoresKeys(t *testing.T) {
	m, home := newModel(t)
	m = press(t, m, "enter", "space", "enter", "enter", "enter", "a")
	if m.scr != scrConfirm {
		t.Fatalf("expected the confirm screen, got %d", m.scr)
	}
	for _, size := range [][2]int{{minWidth - 1, 24}, {80, minHeight - 1}, {1, 1}} {
		small := resize(m, size[0], size[1])
		lines, ok := small.layout()
		if ok || len(lines) > size[1] || len(lines) == 0 {
			t.Errorf("%dx%d: usable=%v, %d rows", size[0], size[1], ok, len(lines))
		}
		if size[0] > 20 && !strings.Contains(strings.Join(strings.Fields(strings.Join(lines, " ")), " "), "needs a larger terminal") {
			t.Errorf("%dx%d does not ask for a larger terminal: %q", size[0], size[1], lines)
		}
		if small = press(t, small, "y"); small.scr != scrConfirm {
			t.Errorf("%dx%d: a key acted on a screen that is not shown", size[0], size[1])
		}
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude")); err == nil {
		t.Fatal("y wrote while the confirm screen was not shown")
	}
	// At the smallest supported size the question and its keys are on screen.
	m = resize(m, minWidth, minHeight)
	if view := strings.Join(strings.Fields(strings.Join(plain(m), " ")), " "); !strings.Contains(view, "unchanged by your choice?") || !strings.Contains(view, "y write") {
		t.Fatalf("confirm at the minimum size lost its question or keys:\n%s", strings.Join(plain(m), "\n"))
	}
	if m = press(t, m, "y"); m.scr != scrResult {
		t.Error("y should write once the confirm screen is shown")
	}
}

// The backup table leaves off the columns that do not fit. At 80 columns a
// backup of both agents has no room for its size: the line under the list
// must still say it for the selected backup.
func TestBackupsSayWhatTheTableLeavesOff(t *testing.T) {
	m, _ := newModel(t)
	m = press(t, m, "enter", "space", "down", "space", "enter", "enter", "enter", "enter", "enter", "a", "y", "enter", "down", "enter")
	if m.scr != scrBackups || len(m.backups) != 1 {
		t.Fatalf("expected one backup, got screen %d with %d: %s", m.scr, len(m.backups), m.result)
	}
	b := m.backups[0]
	for _, width := range []int{80, 132} {
		r := resize(m, width, 24)
		checkFit(t, "backups", r)
		view := strings.Join(plain(r), "\n")
		for _, want := range []string{"claude+codex", "v" + b.ContentVersion, size(m.store.DiskSize(b.ID))} {
			if n := strings.Count(view, want); n != 1 {
				t.Errorf("at %d columns the backups screen shows %q %d times:\n%s", width, want, n, view)
			}
		}
	}
}

// A terminal that reports no columns or no rows has not said how large it
// is. That must not count as too small: every key but ctrl+c would be
// ignored on a screen that cannot say why.
func TestUnreportedSizeKeepsKeysWorking(t *testing.T) {
	m, _ := newModel(t)
	for _, size := range [][2]int{{0, 0}, {0, 24}, {80, 0}, {0, minHeight - 1}, {-1, 24}} {
		odd := resize(m, size[0], size[1])
		if _, ok := odd.layout(); !ok {
			t.Errorf("%dx%d: an unreported size is treated as too small", size[0], size[1])
		}
		if view := strings.Join(plain(odd), "\n"); !strings.Contains(view, menuInstall) || !strings.Contains(view, "q quit") {
			t.Errorf("%dx%d: the main menu is not shown:\n%s", size[0], size[1], view)
		}
		if next := press(t, odd, "down"); next.cursor != 1 {
			t.Errorf("%dx%d: down did not move the cursor", size[0], size[1])
		}
		if _, cmd := odd.Update(tea.KeyPressMsg{Code: 'q', Text: "q"}); cmd == nil {
			t.Errorf("%dx%d: q did not quit", size[0], size[1])
		}
		// The next real report is used as usual.
		if back := resize(odd, 80, 24); back.width != 80 || back.height != 24 {
			t.Errorf("%dx%d: a later 80x24 report left %dx%d", size[0], size[1], back.width, back.height)
		}
	}
}

func TestNoColorRendersNoEscapeCodes(t *testing.T) {
	defer func(on bool) { useColor = on }(useColor)
	useColor = false
	walk(t, 80, 24, func(name string, m Model) {
		if strings.Contains(m.render(), "\x1b") {
			t.Errorf("%s: escape code with color off", name)
		}
	})
}

// A wide or combining character in a path must not misalign the columns.
func TestPreviewAlignsWideCharacters(t *testing.T) {
	m, home := newModel(t)
	m = resize(m, 100, 30)
	m = press(t, m, "enter", "space", "enter", "enter", "enter")
	m.plan.Items[1].Path = filepath.Join(home, ".claude", "skills", "技能")
	m.plan.Items[1].Action = installer.Create
	m.plan.Items[2].Path = filepath.Join(home, ".claude", "skills", "cafe\u0301")
	m.plan.Items[2].Action = installer.Create
	col := -1
	for _, l := range plain(m) {
		if i := strings.Index(l, "  create"); i >= 0 {
			if at := cells(l[:i]); col >= 0 && at != col {
				t.Fatalf("action column moved from %d to %d on %q", col, at, l)
			} else {
				col = at
			}
		}
	}
	if col < 0 {
		t.Fatal("no create rows in the preview")
	}
}
