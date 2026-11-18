// Package tui is the interactive installer: agent selection, plan and
// extra-usage questions, an always-on preview with per-target choices,
// backups, restore and recovery. It never asks the user to pick models.
package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"agentfile/internal/fsnode"
	"agentfile/internal/installer"
	"agentfile/internal/profile"
	"agentfile/internal/render"
	"agentfile/internal/state"
)

type screen int

const (
	scrHome screen = iota
	scrAgents
	scrPlan
	scrCredits
	scrPreview
	scrDetail
	scrConfirm
	scrBackups
	scrRecover
	scrResult
)

// Model is the Bubble Tea model. Fields are unexported; New builds one.
type Model struct {
	env   installer.Env
	store *state.Store

	scr    screen
	cursor int
	// width and height are the terminal size, 0 until the terminal reports
	// it; an unknown height shows every row instead of a window of them.
	width, height int
	version       string // content version, for the header
	notice        string // one-line reply to the last key, cleared by the next
	agents        []installer.AgentState
	menu          []string
	chosen        map[profile.Agent]bool
	queue         []profile.Agent // agents still to ask about
	asking        profile.Agent
	prof          profile.Profile
	plan          *installer.Plan
	backups       []*state.Manifest
	backupAt      int // row of the backup a restore preview was opened from
	// recovery lists content-only targets an interrupted operation left
	// different from their backup; choice holds choiceRestore or choiceKeep
	// for each one the user has decided.
	recovery       []state.RecoveryItem
	choice         map[string]string
	recoveryNotice string
	detail         []string
	wrapped        []detailLine // detail wrapped for wrappedWidth
	wrappedWidth   int
	scroll         int
	back           screen
	result         string
	now            func() time.Time
}

func New(env installer.Env, st *state.Store) Model {
	m := Model{env: env, store: st, now: time.Now}
	if v, err := render.LoadProvenance(); err == nil {
		m.version = v.ContentVersion
	}
	m.goHome()
	return m
}

func (m Model) Init() tea.Cmd { return nil }

const (
	menuInstall = "Install or update"
	menuRestore = "Restore from a backup"
	menuRecover = "Fix interrupted setup"
	menuQuit    = "Quit"
)

func (m *Model) goHome() {
	m.scr, m.cursor = scrHome, 0
	m.agents = installer.Discover(m.env, m.store)
	m.menu = []string{menuInstall, menuRestore}
	if m.store.Pending() || m.store.Locked() {
		m.menu = []string{menuRecover}
	}
	m.menu = append(m.menu, menuQuit)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Rewrapping moves every row of a diff: stay on the same source line,
		// or at the end if the reader was there.
		lines := m.detailLines()
		atEnd := m.scr == scrDetail && m.scroll > 0 && m.scroll >= len(lines)-m.detailPage()
		src := 0
		if m.scroll < len(lines) {
			src = lines[m.scroll].src
		}
		m.width, m.height = msg.Width, msg.Height
		// No columns or no rows is a size the terminal did not report, not a
		// small one: keys must keep working, as before the first report.
		if msg.Width <= 0 || msg.Height <= 0 {
			m.width, m.height = 0, 0
		}
		m.wrapDetail()
		if m.scr == scrDetail {
			lines = m.detailLines()
			m.scroll = max(len(lines)-m.detailPage(), 0)
			for i := 0; !atEnd && i < len(lines); i++ {
				if lines[i].src >= src {
					m.scroll = min(i, m.scroll)
					break
				}
			}
		}
		return m, nil
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		// A terminal too small to show the screen must not act on keys the
		// user cannot see the meaning of: "y" would still write.
		if _, ok := m.layout(); !ok {
			return m, nil
		}
		return m.key(key)
	}
	return m, nil
}

func (m *Model) move(key string, n int) {
	switch key {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < n-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = max(n-1, 0)
	}
}

func (m Model) key(key string) (tea.Model, tea.Cmd) {
	m.notice = ""
	switch m.scr {
	case scrHome:
		m.move(key, len(m.menu))
		if key == "q" {
			return m, tea.Quit
		}
		if key != "enter" {
			return m, nil
		}
		switch m.menu[m.cursor] {
		case menuInstall:
			m.scr, m.cursor = scrAgents, 0
			m.chosen = map[profile.Agent]bool{}
		case menuRestore:
			m.openBackups()
		case menuRecover:
			m.openRecover()
		case menuQuit:
			return m, tea.Quit
		}

	case scrAgents:
		all := []profile.Agent{profile.Claude, profile.Codex}
		m.move(key, len(all))
		switch key {
		case "space", "x":
			a := all[m.cursor]
			m.chosen[a] = !m.chosen[a]
		case "enter":
			m.queue = nil
			for _, a := range all {
				if m.chosen[a] {
					m.queue = append(m.queue, a)
				}
			}
			if len(m.queue) > 0 {
				m.prof = profile.Profile{}
				m.nextAgent()
			} else {
				m.notice = "Select at least one agent with space, then press enter."
			}
		case "esc":
			m.goHome()
		}

	case scrPlan:
		plans := profile.Plans[m.asking]
		m.move(key, len(plans))
		switch key {
		case "enter":
			ap := &profile.AgentProfile{Plan: plans[m.cursor].ID}
			if m.asking == profile.Claude {
				m.prof.Claude = ap
			} else {
				m.prof.Codex = ap
			}
			m.scr, m.cursor = scrCredits, 1
		case "esc":
			m.scr, m.cursor = scrAgents, 0
		}

	case scrCredits:
		m.move(key, 2)
		switch key {
		case "enter":
			m.prof.Get(m.asking).Credits = m.cursor == 0
			if len(m.queue) > 0 {
				m.nextAgent()
			} else {
				m.buildInstallPlan()
			}
		case "esc":
			m.scr, m.cursor = scrPlan, 0
			for i, p := range profile.Plans[m.asking] {
				if p.ID == m.prof.Get(m.asking).Plan {
					m.cursor = i
				}
			}
		}

	case scrPreview:
		m.move(key, len(m.plan.Items))
		it := &m.plan.Items[m.cursor]
		switch key {
		case "space", "x":
			switch {
			case it.Blocked != "":
				m.notice = "This path is blocked, so there is no choice to make. Enter shows the reason."
			case it.Conflict:
				it.Replace = !it.Replace
			case it.Action == installer.Unchanged:
				m.notice = "This path already matches, so there is no choice to make."
			default:
				m.notice = "This path has nothing in its way, so there is no choice to make. Enter shows what will be written."
			}
		case "enter", "d":
			m.showDetail(*it)
		case "a":
			switch n, kept, blocked := m.counts(); {
			case n > 0:
				m.scr = scrConfirm
			case blocked > 0 && kept == 0:
				m.notice = "Nothing can be written: every path that would change is blocked. Enter on a [skip] row shows the reason."
			default:
				m.finish("Nothing to write: every path is unchanged or kept. No backup was made.")
			}
		case "esc":
			// A restore preview goes back to the backup it was opened from.
			if at := m.backupAt; m.plan.Kind == "restore" && at < len(m.backups) {
				m.scr, m.cursor = scrBackups, at
			} else {
				m.goHome()
			}
		}

	case scrDetail:
		page := m.detailPage()
		last := max(len(m.detailLines())-page, 0)
		switch key {
		case "up", "k":
			m.scroll = max(min(m.scroll, last)-1, 0)
		case "down", "j":
			m.scroll = min(m.scroll+1, last)
		case "pgup", "b":
			m.scroll = max(min(m.scroll, last)-page, 0)
		case "pgdown", "space", "f":
			m.scroll = min(m.scroll+page, last)
		case "home", "g":
			m.scroll = 0
		case "end", "G":
			m.scroll = last
		case "esc", "q", "enter":
			m.scr = m.back
		}

	case scrConfirm:
		switch key {
		case "y":
			m.execute()
		case "n", "esc":
			m.scr = scrPreview
		}

	case scrBackups:
		m.move(key, len(m.backups))
		switch key {
		case "enter":
			if len(m.backups) > 0 {
				plan, err := installer.PlanRestore(m.env, m.store, m.backups[m.cursor].ID)
				if err != nil {
					m.fail(err)
					return m, nil
				}
				m.plan, m.scr, m.backupAt, m.cursor = plan, scrPreview, m.cursor, 0
			}
		case "esc":
			m.goHome()
		}

	case scrRecover:
		m.move(key, len(m.recovery))
		switch key {
		case "space", "x", "r", "c":
			if len(m.recovery) > 0 {
				p, c := m.recovery[m.cursor].Path, choiceRestore
				if key == "c" {
					c = choiceKeep
				}
				if m.choice[p] == c {
					c = ""
				}
				m.choice[p] = c
				m.recoveryNotice = ""
			}
		case "enter", "d":
			if len(m.recovery) > 0 {
				r := m.recovery[m.cursor]
				m.showDetail(installer.Item{Path: r.Path, Current: r.Current, Want: r.Backup,
					Action: "differs from its backup; " + installer.Action(m.choiceLabel(r.Path))})
				m.back = scrRecover
			}
		case "y":
			approved, keep := map[string]string{}, map[string]string{}
			for _, r := range m.recovery {
				switch m.choice[r.Path] {
				case choiceRestore:
					approved[r.Path] = r.Token
				case choiceKeep:
					keep[r.Path] = r.Token
				}
			}
			if len(approved)+len(keep) != len(m.recovery) {
				m.recoveryNotice = "Choose restore or keep for every file before continuing. Nothing was changed."
				break
			}
			if err := m.store.RecoverWith(approved, keep); err != nil {
				m.fail(err)
			} else if len(keep) > 0 {
				m.finish(fmt.Sprintf("Recovery finished. Restored %d saved file(s), kept %d as they are. You can use agentfile again.\n"+
					"The saved copies stay under \"%s\" if you change your mind.", len(approved), len(keep), menuRestore))
			} else if len(approved) > 0 {
				m.finish(fmt.Sprintf("Recovery finished. Restored %d saved file(s). You can use agentfile again.", len(approved)))
			} else {
				m.finish("Recovery finished. You can use agentfile again.")
			}
		case "n", "esc":
			m.goHome()
		}

	case scrResult:
		_, body := m.resultText()
		last := lastStart(len(body)-1, m.resultPage())
		switch key {
		case "up", "k":
			m.scroll = max(min(m.scroll, last)-1, 0)
		case "down", "j":
			m.scroll = min(m.scroll+1, last)
		case "enter", "esc":
			m.goHome()
		case "q":
			return m, tea.Quit
		}
	}
	return m, nil
}

// finish shows the outcome of an operation on the result screen.
func (m *Model) finish(result string) {
	m.scr, m.result, m.scroll = scrResult, result, 0
}

func (m *Model) nextAgent() {
	m.asking, m.queue = m.queue[0], m.queue[1:]
	m.scr, m.cursor = scrPlan, 0
}

func (m *Model) buildInstallPlan() {
	plan, err := installer.PlanInstall(m.env, m.store, m.prof)
	if err != nil {
		m.fail(err)
		return
	}
	m.plan, m.scr, m.cursor = plan, scrPreview, 0
}

func (m *Model) openBackups() {
	list, err := m.store.List()
	if err != nil {
		m.fail(err)
		return
	}
	m.backups, m.scr, m.cursor = list, scrBackups, 0
}

// openRecover reviews a pending operation without changing anything.
func (m *Model) openRecover() {
	items, err := m.store.RecoveryReview()
	if err != nil {
		m.fail(err)
		return
	}
	m.recovery, m.choice, m.recoveryNotice, m.scr, m.cursor = items, map[string]string{}, "", scrRecover, 0
}

const (
	choiceRestore = "restore"
	choiceKeep    = "keep"
)

// soleRunWarning is shown wherever y recovers or clears the lock.
const soleRunWarning = "Only continue if no other agentfile is running."

func (m Model) choiceLabel(path string) string {
	switch m.choice[path] {
	case choiceRestore:
		return "restore backed-up bytes"
	case choiceKeep:
		return "keep the current file"
	}
	return "no choice yet"
}

func (m *Model) showDetail(it installer.Item) {
	var b strings.Builder
	choice := ""
	switch {
	case it.Blocked != "":
		choice = "[skip] "
	case it.Conflict && it.Replace:
		choice = "[repl] "
	case it.Conflict:
		choice = "[keep] "
	}
	fmt.Fprintf(&b, "%s\n%s%s", shortPath(m.env.Home, it.Path), choice, it.Action)
	if it.Status != "" {
		fmt.Fprintf(&b, " — %s", it.Status)
	}
	b.WriteString("\n\n")
	if it.Note != "" {
		b.WriteString(it.Note + "\n\n")
	}
	if it.Blocked != "" {
		fmt.Fprintf(&b, "Blocked: %s\n", it.Blocked)
	} else if d := installer.Diff(it); d != "" {
		b.WriteString(d)
	} else {
		b.WriteString("No differences.\n")
	}
	m.detail = strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	m.scroll, m.back, m.scr = 0, scrPreview, scrDetail
	m.wrapDetail()
}

// counts returns how many items will be written, how many differing paths
// are kept by the user's choice, and how many cannot be written at all.
func (m Model) counts() (write, kept, blocked int) {
	for _, it := range m.plan.Items {
		switch {
		case it.Proceeds():
			write++
		case it.Blocked != "":
			blocked++
		case it.Action != installer.Unchanged:
			kept++
		}
	}
	return
}

func (m *Model) execute() {
	man, err := installer.Execute(m.env, m.store, m.plan)
	if err != nil {
		m.fail(err)
		return
	}
	if man == nil {
		m.finish("Nothing to change: every selected target already matches. No backup was made.")
		return
	}
	verb := "Installed"
	if m.plan.Kind == "restore" {
		verb = "Restored"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d entries. Backup of the previous state: %s\n", verb, len(man.Entries)+len(man.RemovedDirs), man.ID)
	if m.plan.Kind != "restore" {
		b.WriteString("Start a new session in each agent to load the new instructions and skills.\n\n")
	}
	for _, e := range man.Entries {
		fmt.Fprintf(&b, "  %s\n", e.Path)
	}
	for _, dir := range man.RemovedDirs {
		fmt.Fprintf(&b, "  %s (empty parent removed)\n", dir)
	}
	fmt.Fprintf(&b, "\nBackups are kept in %s until you remove them.", filepath.Join(m.store.Dir, "backups"))
	m.finish(b.String())
}

func (m *Model) fail(err error) {
	m.scr, m.scroll = scrResult, 0
	// The state layer says how a failed operation ended in the text around
	// the error it wraps. Match that text where it stands, at the end of the
	// message, so a path that happens to contain the words cannot.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "; rollback also failed (") && strings.HasSuffix(msg, "are kept, choose Recover"):
		m.result = "Recovery needed: " + msg + ".\nSome changes may remain. Choose \"" + menuRecover + "\" from the main menu."
	case strings.HasSuffix(msg, " (all changes were rolled back)"):
		m.result = "Stopped and rolled back: " + msg + ".\nWhat this operation had written was undone. Review the new preview and choose again."
	case errors.Is(err, state.ErrChanged):
		m.result = "Stopped before writing anything: " + msg + ".\nA target changed after the preview. Review the new preview and choose again."
	case errors.Is(err, state.ErrRecoveryConflict):
		m.result = "Recovery stopped: " + msg + ".\nPaths recovered before this one may already be restored. The journal and verified backup are kept. Choose \"" +
			menuRecover + "\" again to review each target, or resolve the listed paths yourself first."
	case errors.Is(err, state.ErrRecoveryNeeded), errors.Is(err, state.ErrLocked):
		m.result = "Stopped: " + err.Error() + ".\nChoose \"" + menuRecover + "\" from the main menu."
	default:
		m.result = "Error: " + err.Error()
	}
}

func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

// Styling uses plain SGR codes from the terminal's own 16-color palette, so
// it follows the user's theme; NO_COLOR disables it.
var useColor = os.Getenv("NO_COLOR") == ""

var sgrCodes = regexp.MustCompile("\x1b\\[[0-9;]*m")

func sgr(code, s string) string {
	if !useColor || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string    { return sgr("1", s) }
func dim(s string) string     { return sgr("2", s) }
func accent(s string) string  { return sgr("1;36", s) }
func green(s string) string   { return sgr("32", s) }
func yellow(s string) string  { return sgr("33", s) }
func red(s string) string     { return sgr("31", s) }
func magenta(s string) string { return sgr("35", s) }

// wide lists the code points outside the East Asian blocks that terminals
// draw two cells wide: the symbols with emoji presentation.
var wide = &unicode.RangeTable{R16: []unicode.Range16{
	{0x231a, 0x231b, 1}, {0x23e9, 0x23ec, 1}, {0x23f0, 0x23f0, 1}, {0x23f3, 0x23f3, 1}, {0x25fd, 0x25fe, 1},
	{0x2614, 0x2615, 1}, {0x2648, 0x2653, 1}, {0x267f, 0x267f, 1}, {0x2693, 0x2693, 1}, {0x26a1, 0x26a1, 1},
	{0x26aa, 0x26ab, 1}, {0x26bd, 0x26be, 1}, {0x26c4, 0x26c5, 1}, {0x26ce, 0x26ce, 1}, {0x26d4, 0x26d4, 1},
	{0x26ea, 0x26ea, 1}, {0x26f2, 0x26f3, 1}, {0x26f5, 0x26f5, 1}, {0x26fa, 0x26fa, 1}, {0x26fd, 0x26fd, 1},
	{0x2705, 0x2705, 1}, {0x270a, 0x270b, 1}, {0x2728, 0x2728, 1}, {0x274c, 0x274c, 1}, {0x274e, 0x274e, 1},
	{0x2753, 0x2755, 1}, {0x2757, 0x2757, 1}, {0x2795, 0x2797, 1}, {0x27b0, 0x27b0, 1}, {0x27bf, 0x27bf, 1},
	{0x2b1b, 0x2b1c, 1}, {0x2b50, 0x2b50, 1}, {0x2b55, 0x2b55, 1},
}}

// runeCells is how many terminal cells a rune takes: none for combining marks
// and joiners, two for East Asian wide characters and emoji, one otherwise.
func runeCells(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf && r != 0x303f, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1faff, r >= 0x20000 && r <= 0x3fffd, unicode.Is(wide, r):
		return 2
	}
	return 1
}

// cells is the number of terminal cells s takes, ignoring styling.
func cells(s string) int {
	n := 0
	for _, r := range sgrCodes.ReplaceAllString(s, "") {
		n += runeCells(r)
	}
	return n
}

// fit is how many runes from the start of r fit in n cells.
func fit(r []rune, n int) int {
	for i, c := range r {
		if n -= runeCells(c); n < 0 {
			return i
		}
	}
	return len(r)
}

func pad(s string, n int) string {
	if w := cells(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// clip shortens unstyled text to n cells and marks the cut.
func clip(s string, n int) string {
	switch {
	case cells(s) <= n:
		return s
	case n < 1:
		return ""
	}
	r := []rune(s)
	return string(r[:fit(r, n-1)]) + "…"
}

// clipStyled shortens styled text to n cells and marks the cut. It is the
// last guard on a list row: a row that wrapped would take two terminal rows
// and push the key line off the screen.
func clipStyled(s string, n int) string {
	if cells(s) <= n {
		return s
	}
	var b strings.Builder
	used, styledText := 0, false
	for len(s) > 0 && n > 0 {
		if loc := sgrCodes.FindStringIndex(s); loc != nil && loc[0] == 0 {
			b.WriteString(s[:loc[1]])
			s, styledText = s[loc[1]:], true
			continue
		}
		r := []rune(s)[0]
		if used+runeCells(r) > n-1 {
			break
		}
		b.WriteRune(r)
		used += runeCells(r)
		s = s[len(string(r)):]
	}
	if n > 0 {
		b.WriteString("…")
	}
	if styledText {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// clipLeft shortens unstyled text to n cells from the left, keeping its end.
func clipLeft(s string, n int) string {
	switch {
	case cells(s) <= n:
		return s
	case n < 1:
		return ""
	}
	r := []rune(s)
	// Drop from the left until the rest fits and does not start with a mark
	// that would combine with the ellipsis.
	for len(r) > 0 && (cells(string(r)) > n-1 || runeCells(r[0]) == 0) {
		r = r[1:]
	}
	return "…" + string(r)
}

// wrap breaks unstyled text at spaces into lines of at most width cells. It
// keeps the text's own line breaks and each line's indentation. A word wider
// than a line is broken across lines: the terminal would cut it off instead.
func wrap(s string, width int) []string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		indent := para[:len(para)-len(strings.TrimLeft(para, " "))]
		room := max(width-cells(indent), 1)
		line := indent
		for _, word := range strings.Fields(para) {
			if line != indent && cells(line)+1+cells(word) > width {
				out = append(out, line)
				line = indent
			}
			for r := []rune(word); cells(word) > room; r = []rune(word) {
				if line != indent {
					out = append(out, line)
				}
				n := max(fit(r, room), 1)
				out, line, word = append(out, indent+string(r[:n])), indent, string(r[n:])
			}
			if line != indent {
				line += " "
			}
			line += word
		}
		out = append(out, line)
	}
	return out
}

// styled wraps unstyled text to width and applies one style to every line.
func styled(style func(string) string, width int, s string) []string {
	lines := wrap(s, width)
	for i, l := range lines {
		lines[i] = style(l)
	}
	return lines
}

// row is one selectable line: the cursor, an already styled lead (a checkbox
// or mark), the label and an already styled tail. Only the label is
// emphasized, so styling inside lead or tail cannot cut the emphasis short.
func row(on bool, lead, label, tail string) string {
	if on {
		return accent("›") + " " + lead + accent(label) + tail
	}
	return "  " + lead + label + tail
}

// help is the key line, on more lines when the terminal is narrow. Each hint
// is "key what it does"; the key stays bright. status leads the first line.
func help(width int, status string, hints ...string) []string {
	lines, sep := []string{status}, "  ·  "
	for i, h := range hints {
		key, what, found := strings.Cut(h, " ")
		hint := bold(key)
		if found {
			hint += dim(" " + what)
		}
		last := &lines[len(lines)-1]
		switch {
		case i == 0 && cells(status)+cells(hint) > width && status != "":
			lines = append(lines, hint)
		case i == 0:
			*last += hint
		case cells(*last)+cells(sep)+cells(hint) > width:
			lines = append(lines, hint)
		default:
			*last += dim(sep) + hint
		}
	}
	return lines
}

func rule(width int) string { return dim(strings.Repeat("─", width)) }

func statusStyle(s string) func(string) string {
	switch {
	case s == "absent":
		return dim
	case s == "installed by agentfile", s == "unchanged since that operation":
		return green
	case strings.HasPrefix(s, "cannot"):
		return red
	}
	return yellow
}

func statusMark(s string) string {
	switch {
	case s == "absent":
		return dim("○")
	case s == "installed by agentfile", s == "unchanged since that operation":
		return green("✓")
	case strings.HasPrefix(s, "cannot"):
		return red("✗")
	}
	return yellow("!")
}

func actionStyle(a installer.Action) func(string) string {
	switch a {
	case installer.Unchanged:
		return dim
	case installer.Create:
		return green
	case installer.Remove, installer.RemoveParent:
		return red
	}
	return yellow
}

// diffStyle picks the style of one diff line from how the line starts.
func diffStyle(l string) func(string) string {
	switch {
	case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "==="):
		return bold
	case strings.HasPrefix(l, "@@"):
		return accent
	case strings.HasPrefix(l, "+"):
		return green
	case strings.HasPrefix(l, "-"):
		return red
	}
	return func(s string) string { return s }
}

// margin is the blank column count left and right of every line.
const margin = 2

// contentWidth is the width available between the margins.
func (m Model) contentWidth() int {
	w := m.width
	if w == 0 {
		w = 80
	}
	return max(w-2*margin, 20)
}

// rows is how many terminal rows the lines take. The renderer cuts a line
// wider than the terminal instead of wrapping it, so each takes one.
func rows(groups ...[]string) int {
	n := 0
	for _, lines := range groups {
		n += len(lines)
	}
	return n
}

// view is one screen between the header and the key line.
type view struct {
	top    []string // above the list, after one blank row
	keep   int      // leading rows of top that must stay: the title or question
	list   []string // one row per item; windowed around cursor when it does not fit
	cursor int
	bottom []string // below the list
	pin    []string // below those, never dropped: what the user needs to decide
	status string   // shown before the key hints
	keys   []string
}

// The smallest terminal the screens are laid out for.
const minWidth, minHeight = 40, 10

func (m Model) render() string {
	lines, _ := m.layout()
	return strings.Join(lines, "\n")
}

// layout is the screen row by row. It reports false when the terminal cannot
// show what the screen needs to be used: its title or question, a row of its
// list, its pinned rows and its keys. The rows then ask for a larger terminal,
// and keys other than ctrl+c do nothing.
func (m Model) layout() ([]string, bool) {
	w := m.contentWidth()
	tooSmall := func() ([]string, bool) {
		lines := wrap(fmt.Sprintf("agentfile needs a larger terminal for this screen: at least %d columns and %d rows. "+
			"Nothing happens until it fits; ctrl+c quits.", minWidth, minHeight), max(m.width, 1))
		return lines[:min(len(lines), max(m.height, 1))], false
	}
	if m.height > 0 && (m.width < minWidth || m.height < minHeight) {
		return tooSmall()
	}
	v := m.view(w, false)
	head := []string{clipStyled(m.header(w), w), rule(w)}
	foot := append([]string{rule(w)}, help(w, v.status, v.keys...)...)
	if m.scr == scrHome && m.height > 0 && rows(head, v.top, v.list, v.bottom, foot)+1 > m.height {
		v = m.view(w, true)
	}
	// No row may be wider than the terminal, which would cut it off without
	// a mark. Each screen fits its own rows; this is the guard behind them.
	for _, rows := range [][]string{v.top, v.list, v.bottom, v.pin, foot} {
		for i, l := range rows {
			rows[i] = clipStyled(l, w)
		}
	}
	gap, title, rest, bottom := []string{""}, v.top[:v.keep], v.top[v.keep:], v.bottom
	var lines []string
	if m.height > 0 {
		// The header is decoration: when the rows a screen cannot do without
		// leave no room for it, its rule goes, then its name.
		for len(head) > 0 && rows(head, title, v.pin, foot)+min(len(v.list), 1) > m.height {
			head = head[:len(head)-1]
		}
		if rows(title, v.pin, foot)+min(len(v.list), 1) > m.height {
			return tooSmall()
		}
		// A terminal too short for everything keeps the title, the list, the
		// pinned rows and the keys. Blank rows go first; then whole
		// paragraphs, never part of one: the text above the list from its
		// end, then the notes under it from their start; and last the list
		// shrinks to the cursor row.
		need, above, below := min(len(v.list), 3), paragraphs(rest), paragraphs(bottom)
		for rows(head, gap, title, rest, bottom, v.pin, foot)+need > m.height {
			switch {
			case len(bottom) > rows(below...):
				bottom = slices.Concat(below...)
			case len(rest) > rows(above...):
				rest = slices.Concat(above...)
			case len(gap) > 0:
				gap = nil
			case len(above) > 0:
				above = above[:len(above)-1]
				rest = slices.Concat(above...)
			case len(below) > 0:
				below = below[1:]
				bottom = slices.Concat(below...)
			default:
				need--
			}
		}
		lines = slices.Concat(head, gap, title, rest)
		lines = append(lines, window(v.list, v.cursor, m.height-rows(head, gap, title, rest, bottom, v.pin, foot))...)
		lines = append(append(lines, bottom...), v.pin...)
		// A known height pins the key line to the bottom of the screen.
		for rows(lines, foot) < m.height {
			lines = append(lines, "")
		}
	} else {
		lines = slices.Concat(head, gap, v.top, v.list, bottom, v.pin, []string{""})
	}
	lines = append(lines, foot...)
	out := make([]string, len(lines))
	for i, l := range lines {
		if l != "" {
			out[i] = strings.Repeat(" ", margin) + l
		}
	}
	return out, true
}

// paragraphs splits rows at their blank rows.
func paragraphs(lines []string) [][]string {
	var out [][]string
	open := false
	for _, l := range lines {
		switch {
		case l == "":
			open = false
		case open:
			out[len(out)-1] = append(out[len(out)-1], l)
		default:
			out, open = append(out, []string{l}), true
		}
	}
	return out
}

// header is the name badge, then the steps of the current flow or, outside
// one, what agentfile installs and which content version this binary carries.
func (m Model) header(w int) string {
	badge := sgr("1;7;36", " agentfile ")
	if steps, at := m.steps(); steps != nil {
		return badge + "  " + stepper(steps, at, w-cells(badge)-2)
	}
	left, right := "instructions & skills for Claude Code and Codex", "content "+m.version
	if gap := w - cells(badge) - 2 - cells(left) - cells(right); gap >= 2 && m.version != "" {
		return badge + "  " + dim(left) + strings.Repeat(" ", gap) + dim(right)
	}
	return badge + "  " + dim(clip(left, w-cells(badge)-2))
}

// steps names the screens of the flow the current screen belongs to and the
// index of the current one; nil outside a flow.
func (m Model) steps() ([]string, int) {
	install := []string{"Agents", "Plan", "Extra usage", "Preview", "Confirm"}
	restore := []string{"Backup", "Preview", "Confirm"}
	scr := m.scr
	if scr == scrDetail {
		scr = m.back
	}
	switch scr {
	case scrAgents:
		return install, 0
	case scrPlan:
		return install, 1
	case scrCredits:
		return install, 2
	case scrBackups:
		return restore, 0
	case scrPreview, scrConfirm:
		flow := install
		if m.plan.Kind == "restore" {
			flow = restore
		}
		at := len(flow) - 2
		if scr == scrConfirm {
			at++
		}
		return flow, at
	}
	return nil, 0
}

// stepper draws a flow as connected steps: done, current, still to come. A
// terminal too narrow for it gets "Step 2 of 5 · Plan", then "2/5 Plan".
func stepper(steps []string, at, width int) string {
	parts, n := make([]string, len(steps)), 0
	for i, s := range steps {
		switch {
		case i < at:
			parts[i] = green("●") + " " + s
		case i == at:
			parts[i] = accent("● " + s)
		default:
			parts[i] = dim("○ " + s)
		}
		n += cells(parts[i]) + 4
	}
	switch short := fmt.Sprintf("Step %d of %d · ", at+1, len(steps)); {
	case n-4 <= width:
		return strings.Join(parts, dim(" ── "))
	case cells(short)+cells(steps[at]) <= width:
		return dim(short) + accent(steps[at])
	}
	return accent(clip(fmt.Sprintf("%d/%d %s", at+1, len(steps), steps[at]), width))
}

func (m Model) view(w int, compact bool) view {
	switch m.scr {
	case scrHome:
		return m.homeView(w, compact)
	case scrAgents:
		return m.agentsView(w)
	case scrPlan:
		return m.planView(w)
	case scrCredits:
		v := view{cursor: m.cursor, keys: []string{"↑/↓ move", "enter select", "esc back"}}
		v.top = m.question(w, "Have you already enabled paid extra usage for "+profile.ClientName(m.asking)+"?")
		v.keep = len(v.top)
		v.top = append(v.top, "")
		for i, l := range []string{"Yes, it is enabled", "No"} {
			v.list = append(v.list, row(i == m.cursor, "", l, ""))
		}
		v.bottom = append([]string{""}, styled(dim, w, "\"Yes\" only lets the guidance continue on already-enabled paid usage after "+
			"the included allowance runs out. agentfile never buys or enables anything.")...)
		return v
	case scrPreview:
		return m.previewView(w)
	case scrDetail:
		return m.detailView(w)
	case scrConfirm:
		n, kept, _ := m.counts()
		verb := "Install"
		if m.plan.Kind == "restore" {
			verb = "Restore"
		}
		v := view{keys: []string{"y write", "n back"}}
		v.top = styled(bold, w, fmt.Sprintf("%s: write %d path(s), keep %d unchanged by your choice?", verb, n, kept))
		v.keep = len(v.top)
		v.top = append(append(v.top, ""), wrap("The current state of each path is backed up first to", w)...)
		v.top = append(v.top, styled(accent, w, shortPath(m.env.Home, filepath.Join(m.store.Dir, "backups")))...)
		v.top = append(append(v.top, ""), styled(dim, w, "Every path is rechecked first. If one changed after this preview, the operation stops; "+
			"a change found partway rolls back what this operation wrote.")...)
		return v
	case scrBackups:
		return m.backupsView(w)
	case scrRecover:
		if len(m.recovery) > 0 {
			return m.recoveryView(w)
		}
		v := view{keys: []string{"y recover", "n back"}}
		what := "Recovery rolls every path it touched back to its verified snapshot and clears the lock."
		v.top = styled(bold, w, "An operation was interrupted.")
		if !m.store.Pending() {
			v.keys[0] = "y clear the lock"
			v.top = styled(bold, w, "A lock is present, with no interrupted operation behind it.")
			what = "Another agentfile may be running, or one stopped without cleaning up. Clearing the lock changes no managed path."
		}
		// The warning is the only guard against a second agentfile at work:
		// it stays with the title on a terminal too small for the rest.
		v.top = append(v.top, styled(yellow, w, soleRunWarning)...)
		v.keep = len(v.top)
		v.top = append(append(v.top, ""), wrap(what, w)...)
		return v
	}
	return m.resultView()
}

func (m Model) homeView(w int, compact bool) view {
	v := view{cursor: m.cursor, keys: []string{"↑/↓ move", "enter select", "q quit"}}
	pathw := 0
	for _, a := range m.agents {
		for _, t := range a.Targets {
			pathw = max(pathw, cells(shortPath(m.env.Home, t.Path)))
		}
	}
	// note is one marked line under an agent, wrapped with a hanging indent.
	note := func(mark string, style func(string) string, text string) {
		for i, l := range wrap(text, w-4) {
			lead := "  " + mark + " "
			if i > 0 {
				lead = "    "
			}
			v.top = append(v.top, lead+style(l))
		}
	}
	missing := false                      // some agent's CLI is not on PATH
	pathw = max(min(pathw, w-4-2-12), 12) // a narrow terminal keeps the end of each path and some of its status
	target := func(t installer.TargetState) string {
		return "  " + statusMark(t.Status) + " " + pad(clipLeft(shortPath(m.env.Home, t.Path), pathw), pathw) + "  " +
			statusStyle(t.Status)(clip(t.Status, w-4-pathw-2))
	}
	for _, a := range m.agents {
		name, where := profile.ClientName(a.Agent), "  ·  "+shortPath(m.env.Home, a.Home)+"  ·  "
		room := w - cells(name) - cells(where)
		cli := dim("CLI ") + green(clipLeft(a.Binary, room-4))
		if a.Binary == "" {
			cli, missing = yellow(clip("CLI not on PATH", room)), true
		}
		v.top = append(v.top, bold(name)+dim(where)+cli)
		if a.Problem != "" {
			note(red("✗"), red, "cannot install here: "+a.Problem)
		}
		for _, warn := range a.Warnings {
			// The heading already says when the CLI is missing.
			if a.Binary != "" || !strings.Contains(warn, "was not found on PATH") {
				note(yellow("!"), dim, warn)
			}
		}
		// Short terminals get one line per status, in the order the targets
		// list them; a status only one or two paths have still names them.
		var order []string
		byStatus := map[string][]installer.TargetState{}
		for _, t := range a.Targets {
			if byStatus[t.Status] == nil {
				order = append(order, t.Status)
			}
			byStatus[t.Status] = append(byStatus[t.Status], t)
		}
		for _, s := range order {
			group := byStatus[s]
			switch {
			case !compact, len(group) <= 2 && len(order) > 1:
				for _, t := range group {
					v.top = append(v.top, target(t))
				}
			case len(group) == len(a.Targets) && s == "absent":
				v.top = append(v.top, "  "+statusMark(s)+" "+dim("not installed yet"))
			case len(group) == len(a.Targets):
				v.top = append(v.top, "  "+statusMark(s)+" "+statusStyle(s)(fmt.Sprintf("all %d paths %s", len(group), s)))
			default:
				v.top = append(v.top, "  "+statusMark(s)+" "+statusStyle(s)(fmt.Sprintf("%d of %d paths %s", len(group), len(a.Targets), s)))
			}
		}
		v.top = append(v.top, "")
	}
	if missing {
		v.top = append(v.top, styled(dim, w, "A missing CLI is only a warning; you can still install for it.")...)
		v.top = append(v.top, "")
	}
	if m.store.Pending() || m.store.Locked() {
		v.top = append(v.top, styled(yellow, w, "An earlier operation did not finish, or another agentfile is running.")...)
		v.top = append(v.top, "")
	}
	about := map[string]string{
		menuInstall: "choose agents and plans, then preview every change",
		menuRestore: "put back the state from before an operation",
		menuRecover: "review what an unfinished operation left behind",
	}
	labelw := 0
	for _, item := range m.menu {
		labelw = max(labelw, cells(item))
	}
	for i, item := range m.menu {
		tail := ""
		if about[item] != "" {
			tail = strings.Repeat(" ", labelw-cells(item)+3) + dim(clip(about[item], w-labelw-5))
		}
		v.list = append(v.list, row(i == m.cursor, "", item, tail))
	}
	return v
}

func (m Model) agentsView(w int) view {
	v := view{cursor: m.cursor, keys: []string{"↑/↓ move", "space toggle", "enter continue", "esc back"}}
	v.top = styled(bold, w, "Which agents should get the instructions and skills?")
	v.keep = len(v.top)
	v.top = append(v.top, "")
	all := []profile.Agent{profile.Claude, profile.Codex}
	namew := 0
	for _, a := range all {
		namew = max(namew, cells(profile.ClientName(a)))
	}
	for i, a := range all {
		box := dim("○") + " "
		if m.chosen[a] {
			box = green("●") + " "
		}
		name, tail := profile.ClientName(a), ""
		for _, st := range m.agents {
			if st.Agent != a {
				continue
			}
			tail = strings.Repeat(" ", namew-cells(name)+3) + dim(shortPath(m.env.Home, st.Home))
			switch {
			case st.Problem != "":
				tail += dim(" · ") + red("cannot install here")
			case st.Binary == "":
				tail += dim(" · ") + yellow("CLI not on PATH")
			}
		}
		v.list = append(v.list, row(i == m.cursor, box, name, tail))
	}
	v.bottom = append([]string{""}, styled(dim, w, "A missing CLI is only a warning. You will not be asked to choose models.")...)
	if m.notice != "" {
		v.bottom = append(append(v.bottom, ""), styled(yellow, w, m.notice)...)
	}
	return v
}

// allowanceNote says in a few words how the generated guidance treats a plan.
func allowanceNote(a profile.Allowance) string {
	switch a {
	case profile.AllowanceLimited:
		return "limited allowance"
	case profile.AllowanceStandard:
		return "moderate allowance"
	case profile.AllowanceHigh:
		return "generous allowance"
	}
	return "allowance unknown, treated as limited"
}

func (m Model) planView(w int) view {
	v := view{cursor: m.cursor, keys: []string{"↑/↓ move", "enter select", "esc agents"}}
	v.top = m.question(w, "Which "+profile.ClientName(m.asking)+" plan do you use?")
	v.keep = len(v.top)
	v.top = append(v.top, "")
	plans := profile.Plans[m.asking]
	labelw := 0
	for _, p := range plans {
		labelw = max(labelw, cells(p.Label))
	}
	for i, p := range plans {
		tail := strings.Repeat(" ", labelw-cells(p.Label)+3) + dim(clip(allowanceNote(p.Allowance), w-labelw-5))
		v.list = append(v.list, row(i == m.cursor, "", p.Label, tail))
	}
	v.bottom = append([]string{""}, styled(dim, w, "Your plan sets how sparingly the installed instructions spend your included "+
		"allowance, and which of two comparable routes they list first. It never changes which models are capable of a task.")...)
	return v
}

// question is a per-agent question; with two agents selected it also says
// which one is being asked about, since the same questions come twice.
func (m Model) question(w int, text string) []string {
	total := 0
	for _, on := range m.chosen {
		if on {
			total++
		}
	}
	lines := styled(bold, w, text)
	if at := total - len(m.queue); total > 1 && cells(text)+16 <= w {
		lines[len(lines)-1] += dim(fmt.Sprintf("  ·  agent %d of %d", at, total))
	}
	return lines
}

func (m Model) previewView(w int) view {
	v := view{cursor: m.cursor, keys: []string{"↑/↓ move", "enter diff", "space keep/replace", "a apply", "esc cancel"}}
	if m.plan.Kind == "restore" {
		s := m.plan.Snapshot
		what := "install"
		if s.Kind == "restore" {
			what = "restore"
		}
		v.keys[len(v.keys)-1] = "esc backups"
		v.top = []string{bold("Restore preview") + "  " + dim(clip(fmt.Sprintf("to the state before the %s of %s", what, s.Created.Local().Format("2006-01-02 15:04")), w-17))}
	} else {
		v.top = []string{bold("Install preview") + "  " + dim(clip("content "+m.version, w-17))}
		namew := 0
		for _, a := range m.plan.Profile.Agents() {
			namew = max(namew, cells(profile.ClientName(a)))
		}
		for _, a := range m.plan.Profile.Agents() {
			usage := "no extra usage"
			if m.plan.Profile.Get(a).Credits {
				usage = "extra usage enabled"
			}
			v.top = append(v.top, pad(profile.ClientName(a), namew)+"  "+dim(clip(m.plan.Profile.PlanFor(a).Label+", "+usage, w-namew-2)))
		}
	}
	v.keep = 1
	v.top = append(v.top, "")

	// Columns: cursor, mark (only when some path needs a choice or is
	// blocked), path, action, status. A narrow terminal keeps the end of each
	// path, then as much of the action as fits.
	pathw, markw, actionw := 0, 0, 0
	for _, it := range m.plan.Items {
		pathw = max(pathw, cells(shortPath(m.env.Home, it.Path)))
		actionw = max(actionw, cells(string(it.Action)))
		if it.Conflict || it.Blocked != "" {
			markw = 7
		}
	}
	full := pathw
	pathw = max(min(pathw, w-2-markw-2-actionw), min(pathw, 20))
	for i, it := range m.plan.Items {
		mark, style := strings.Repeat(" ", markw), actionStyle(it.Action)
		switch {
		case it.Blocked != "":
			mark, style = red("[skip]")+" ", dim
		case it.Conflict && it.Replace:
			mark = magenta("[repl]") + " "
		case it.Conflict:
			mark, style = yellow("[keep]")+" ", dim // the action will not happen
		}
		action := clip(string(it.Action), w-2-markw-pathw-2)
		tail := "  " + style(action)
		// The two statuses that only confirm the action are left to the diff view.
		routine := it.Status == "" || it.Status == "absent" || it.Status == "unchanged since that operation"
		if room := w - 2 - markw - pathw - 2 - cells(action) - 3; !routine && it.Action != installer.Unchanged && room >= 8 {
			tail += dim(" · " + clip(it.Status, room))
		}
		v.list = append(v.list, row(i == m.cursor, mark, pad(clipLeft(shortPath(m.env.Home, it.Path), pathw), pathw), tail))
	}

	// Under the list: what the focused row means and what space would do.
	it := m.plan.Items[m.cursor]
	var notes []string
	if m.notice != "" {
		notes = append(notes, styled(yellow, w, m.notice)...)
	}
	if pathw < full {
		notes = append(notes, styled(dim, w, shortPath(m.env.Home, it.Path)+"  "+string(it.Action))...)
	}
	status := ""
	if it.Status != "" {
		status = " (" + it.Status + ")"
	}
	choose := ". Press space to choose \"" + string(it.Action) + "\" instead."
	switch {
	case it.Blocked != "":
		notes = append(notes, styled(red, w, "Blocked: "+it.Blocked)...)
	case it.Conflict && it.Replace:
		notes = append(notes, styled(magenta, w, "Chosen: "+string(it.Action)+". Press space to keep the current path as it is.")...)
	case it.Conflict && it.Current != nil && it.Current.Type == fsnode.Absent:
		notes = append(notes, styled(yellow, w, "Kept missing: this path is gone"+status+choose)...)
	case it.Conflict:
		notes = append(notes, styled(yellow, w, "Kept as it is: this path exists and differs"+status+choose)...)
	}
	if it.Note != "" {
		notes = append(notes, styled(dim, w, it.Note)...)
	}
	switch it.Action {
	case installer.UpdateInPlace:
		notes = append(notes, styled(dim, w, "Only this file's contents change. If setup stops halfway through, "+
			"the file may be partly updated. You can then choose to restore its saved copy.")...)
	case installer.Recreate:
		notes = append(notes, styled(dim, w, "This missing file will be created again. Its old permissions and dates cannot be restored.")...)
	}
	// Two rows are always set aside for the notes when there is room, so the
	// list does not jump as the cursor moves between rows with and without one.
	for m.height >= 20 && len(notes) < 2 {
		notes = append(notes, "")
	}
	v.bottom = []string{""}
	if len(notes) > 0 {
		v.bottom = append(append(v.bottom, notes...), "")
	}

	n, kept, blocked := m.counts()
	count := func(n int, style func(string) string) string {
		if n == 0 {
			return bold("0")
		}
		return style(bold(fmt.Sprint(n)))
	}
	total := fmt.Sprintf("%s to write, %s kept", count(n, green), count(kept, yellow))
	if blocked > 0 {
		total += dim(", ") + red(fmt.Sprintf("%d blocked", blocked))
	}
	hint := "nothing is written until you confirm"
	switch {
	case n > 0:
	case kept > 0:
		hint = "press space on a [keep] row to replace that path"
	case blocked > 0:
		hint = "move to a [skip] row to see what blocks it"
	default:
		hint = "everything already matches"
	}
	// The count is what "a apply" acts on: it stays when the notes do not fit.
	v.pin = []string{total + dim(clip("  ·  "+hint, w-cells(total)))}
	return v
}

// detailLine is one screen row of the detail text; src is the line of
// m.detail it came from, which decides its style.
type detailLine struct {
	text string
	src  int
}

// The detail text opens with the path, then its action, then a blank line;
// those stay in view as the heading while the rest scrolls under it.
const detailHead = 3

// wrapDetail wraps the detail text for the current width once, so that
// paging a long diff does not wrap it again on every key.
func (m *Model) wrapDetail() {
	m.wrapped, m.wrappedWidth = wrapDetail(m.detail, m.contentWidth()), m.contentWidth()
}

func (m Model) detailLines() []detailLine {
	if m.wrappedWidth == m.contentWidth() {
		return m.wrapped
	}
	return wrapDetail(m.detail, m.contentWidth())
}

// wrapDetail wraps the detail text under its heading to w cells, at a space
// where one falls in the second half of a row, so a long diff line stays
// readable and paging counts real rows. Continuation rows are indented. Each
// line is walked once, whatever its length.
func wrapDetail(detail []string, w int) []detailLine {
	var out []detailLine
	for i := detailHead; i < len(detail); i++ {
		r, indent := []rune(strings.ReplaceAll(detail[i], "\t", "    ")), ""
		for start := 0; ; indent = "  " {
			room, used, end, space := max(w-len(indent), 1), 0, start, -1
			for end < len(r) && used+runeCells(r[end]) <= room {
				if used += runeCells(r[end]); r[end] == ' ' {
					space = end
				}
				end++
			}
			if end < len(r) && space > start+(end-start)/2 {
				end = space
			}
			end = max(end, min(start+1, len(r))) // a rune wider than the row still moves on
			out = append(out, detailLine{indent + string(r[start:end]), i})
			if start = end; start >= len(r) {
				break
			}
			if r[start] == ' ' {
				start++
			}
		}
	}
	return out
}

var detailKeys = []string{"↑/↓ scroll", "space/b page", "g/G top/end", "esc back"}

func detailStatus(from, to, of int) string {
	return dim(fmt.Sprintf("%d–%d of %d  ·  ", from, to, of))
}

// detailPage is how many detail rows fit between the heading and the key
// line, which takes more than one row on a narrow terminal.
func (m Model) detailPage() int {
	h, n := m.height, len(m.detailLines())
	if h == 0 {
		h = 24
	}
	return max(h-2-1-1-1-1-len(help(m.contentWidth(), detailStatus(n, n, n), detailKeys...)), 1)
}

func (m Model) detailView(w int) view {
	lines, page := m.detailLines(), m.detailPage()
	start := min(m.scroll, max(len(lines)-page, 0))
	end := min(start+page, len(lines))
	v := view{keep: 1, keys: detailKeys, status: detailStatus(min(start+1, end), end, len(lines))}
	path, action := "", ""
	if len(m.detail) > 1 {
		path, action = m.detail[0], m.detail[1]
	}
	path = clipLeft(path, max(w-cells(action)-5, w/2))
	v.top = []string{bold(path) + dim(clip("  ·  "+action, w-cells(path))), ""}
	for _, l := range lines[start:end] {
		v.top = append(v.top, diffStyle(m.detail[l.src])(l.text))
	}
	return v
}

func (m Model) backupsView(w int) view {
	if len(m.backups) == 0 {
		v := view{top: []string{"No backups yet.", ""}, keep: 1, keys: []string{"esc back"}}
		v.top = append(v.top, styled(dim, w, "A backup is made each time an install or restore writes something.")...)
		return v
	}
	v := view{cursor: m.cursor, keys: []string{"↑/↓ move", "enter preview restore", "esc back"}}
	v.top = []string{bold("Backups") + "  " + dim(clip("newest first; each records the state before one operation", w-9)), ""}
	v.keep = 1
	table := [][]string{{"saved", "operation", "age", "agents", "paths", "content", "size"}}
	for _, s := range m.backups {
		table = append(table, []string{s.Created.Local().Format("2006-01-02 15:04"), s.Kind, age(m.now().Sub(s.Created)),
			strings.Join(s.Agents, "+"), fmt.Sprint(len(s.Entries) + len(s.RemovedDirs)), "v" + s.ContentVersion, size(m.store.DiskSize(s.ID))})
	}
	lines := columns(table, w-2)
	v.top = append(v.top, "  "+dim(lines[0]))
	for i, l := range lines[1:] {
		// Only the date carries the selection color; the columns stay plain.
		date, rest := l, ""
		if n := cells(table[i+1][0]); len(l) >= n {
			date, rest = l[:n], l[n:]
		}
		v.list = append(v.list, row(i == m.cursor, "", date, rest))
	}
	// Two backups made in the same minute look alike in the table. The line
	// under it also says what the table had no room for: at 80 columns a
	// backup of both agents loses its size column.
	at := min(m.cursor, len(m.backups)-1)
	detail := "backup " + m.backups[at].ID + " · saved " + m.backups[at].Created.Local().Format("15:04:05")
	for i := len(strings.Fields(lines[0])); i < len(table[0]); i++ {
		detail += " · " + table[0][i] + " " + table[at+1][i]
	}
	v.bottom = []string{"", dim(clip(detail, w))}
	return v
}

// columns lays a table out with two spaces between columns, each as wide as
// its widest cell. Columns that do not fit in width are left off the end.
func columns(table [][]string, width int) []string {
	var widths []int
	for _, r := range table {
		for i, c := range r {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], cells(c))
		}
	}
	fit, used := 0, 0
	for fit < len(widths) && (fit == 0 || used+2+widths[fit] <= width) {
		used += widths[fit]
		if fit > 0 {
			used += 2
		}
		fit++
	}
	out := make([]string, len(table))
	for n, r := range table {
		cols := make([]string, 0, fit)
		for i, c := range r[:min(fit, len(r))] {
			cols = append(cols, pad(c, widths[i]))
		}
		out[n] = strings.TrimRight(strings.Join(cols, "  "), " ")
	}
	return out
}

// recoveryStatus says how a file in the recovery review stands against its
// saved copy, and with it what restoring would do.
func recoveryStatus(r state.RecoveryItem) string {
	switch {
	case r.Current.Type == fsnode.Absent:
		return "missing; restoring creates a new copy with default permissions"
	case r.Backup.Type == fsnode.Absent:
		return "did not exist before; restoring removes it"
	}
	return "differs from backup"
}

func (m Model) recoveryView(w int) view {
	v := view{cursor: m.cursor, keys: []string{"↑/↓ move", "enter compare", "r restore", "c keep", "y finish", "n later"}}
	v.top = append([]string{bold(clip("Setup stopped before it finished.", w))}, styled(yellow, w, soleRunWarning)...)
	v.keep = len(v.top) // the warning stays with the title, as on the screen without a list
	v.top = append(v.top, "")
	v.top = append(v.top, wrap("These files now differ from their saved copies. They may contain your own edits.\n"+
		"Review each file, then choose: restore its saved copy, or keep it as it is now.\n"+
		"To finish recovery, every listed file needs a choice.", w)...)
	v.top = append(v.top, "")
	// A narrow terminal gets one-letter marks, named after their keys, so
	// that the path still fits.
	marks := map[string]string{"": "[ ] Choose restore or keep", choiceRestore: "[x] Restore saved copy    ", choiceKeep: "[x] Keep current file     ", "remove": "[x] Remove, as it was     "}
	if w < 72 {
		marks = map[string]string{"": "[ ]", choiceRestore: "[r]", choiceKeep: "[c]", "remove": "[r]"}
	}
	pathw := 0
	for _, r := range m.recovery {
		pathw = max(pathw, cells(shortPath(m.env.Home, r.Path)))
	}
	pathw = max(min(pathw, w-2-cells(marks[""])-1), 12)
	cut := false // some row has no room for its whole status
	for i, r := range m.recovery {
		choice := m.choice[r.Path]
		if choice == choiceRestore && r.Backup.Type == fsnode.Absent {
			choice = "remove" // there was no file before
		}
		mark := magenta(marks[choice]) + " "
		if choice == "" {
			mark = yellow(marks[choice]) + " "
		}
		status, tail := recoveryStatus(r), ""
		room := w - 2 - cells(mark) - pathw - 2
		if room >= 8 {
			tail = "  " + dim(clip(status, room))
		}
		cut = cut || room < cells(status)
		v.list = append(v.list, row(i == m.cursor, mark, pad(clipLeft(shortPath(m.env.Home, r.Path), pathw), pathw), tail))
	}
	open := 0
	for _, r := range m.recovery {
		if m.choice[r.Path] == "" {
			open++
		}
	}
	// How many files still need a choice decides whether y can finish, so the
	// count stays on a terminal too small for the notes. After a y that could
	// not finish, it says so in the same place: the longer notice may not fit.
	count := fmt.Sprintf("%d of %d files still need a choice.", open, len(m.recovery))
	v.bottom = []string{""}
	switch {
	case open == 0:
		v.pin = []string{green(clip("Every file has a choice. Press y to finish.", w))}
	case m.recoveryNotice != "":
		v.bottom = append(append(v.bottom, styled(yellow, w, m.recoveryNotice)...), "")
		v.pin = styled(yellow, w, "Not finished: "+count)
	default:
		v.pin = []string{yellow(clip(count, w))}
	}
	// What restoring does to the selected file is what r is chosen on. The
	// key line, which is never dropped, names it: restoring a file that did
	// not exist before removes it, and restoring a missing one creates it.
	// Where the rows cut the status short, a note under them gives it whole.
	at := m.recovery[m.cursor]
	switch {
	case at.Current.Type == fsnode.Absent:
		v.keys[2] = "r recreate"
	case at.Backup.Type == fsnode.Absent:
		v.keys[2] = "r remove"
	}
	if cut {
		v.bottom = append(append(v.bottom, styled(dim, w, shortPath(m.env.Home, at.Path)+": "+recoveryStatus(at))...), "")
	}
	return v
}

// resultText is the result as shown: its title and the rows under it. Paths
// under the home are shortened, and the listed entries are set in two
// columns when they fit, so a full install needs no scrolling.
func (m Model) resultText() (title, body []string) {
	w := m.contentWidth()
	result := m.result
	if sep := string(filepath.Separator); m.env.Home != "" && m.env.Home != sep {
		result = strings.ReplaceAll(result, m.env.Home+sep, "~"+sep)
	}
	first, rest, more := strings.Cut(result, "\n")
	mark, style := "✓ ", green
	for _, bad := range []string{"Error", "Stopped", "Recovery stopped", "Recovery needed"} {
		if strings.HasPrefix(first, bad) {
			mark, style = "✗ ", red
		}
	}
	// A title too long for two rows keeps what happened and moves the
	// details into the body, which scrolls.
	lead := ""
	if what, why, cut := strings.Cut(first, ": "); cut && mark == "✗ " && len(wrap(first, w-2)) > 2 {
		first, lead = what+":", why
	}
	if cells(first) > w-2 {
		first = strings.ReplaceAll(first, ". ", ".\n") // break at sentences before words
	}
	for i, l := range wrap(first, w-2) {
		if i > 0 {
			mark = "  "
		}
		title = append(title, style(mark+l))
	}
	if !more && lead == "" {
		return title, nil
	}
	if lead != "" {
		body = append([]string{""}, wrap(lead, w)...)
	}
	if !more {
		return title, body
	}
	body = append(body, "")
	var entries []string
	flush := func() {
		colw := 0
		for _, e := range entries {
			colw = max(colw, cells(e)+5)
		}
		half := (len(entries) + 1) / 2
		if len(entries) < 6 || 2*colw > w {
			half = len(entries)
		}
		for i := 0; i < half; i++ {
			line := "  " + dim("•") + " " + clipLeft(entries[i], w-4)
			if i+half < len(entries) {
				line = pad(line, colw) + "  " + dim("•") + " " + entries[i+half]
			}
			body = append(body, line)
		}
		entries = nil
	}
	for _, l := range strings.Split(rest, "\n") {
		if strings.HasPrefix(l, "  ") {
			entries = append(entries, strings.TrimPrefix(l, "  "))
			continue
		}
		flush()
		body = append(body, wrap(l, w)...)
	}
	flush()
	return title, body
}

// resultPage is how many rows the result's body may take on the screen.
func (m Model) resultPage() int {
	title, body := m.resultText()
	if m.height == 0 {
		return len(body)
	}
	keys := help(m.contentWidth(), "", "↑/↓ scroll", "enter main menu", "q quit")
	free := m.height - 2 - len(title) - 1 - len(keys)
	if len(body) <= free-1 {
		return len(body) // everything fits under a blank row
	}
	return max(free, 1) // the layout gives up the blank row above the title
}

func (m Model) resultView() view {
	title, body := m.resultText()
	v := view{top: title, keep: len(title), keys: []string{"enter main menu", "q quit"}}
	if page := m.resultPage(); page < len(body) {
		v.keys = append([]string{"↑/↓ scroll"}, v.keys...)
		body = scrollWindow(body[1:], m.scroll, page) // without its opening blank row
	}
	v.top = append(v.top, body...)
	return v
}

func shortPath(home, p string) string {
	rel, err := filepath.Rel(home, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.Join("~", rel)
}

// more is the marker row for n rows hidden in the arrow's direction.
func more(arrow string, n int) string {
	return dim(fmt.Sprintf("  %s %d more", arrow, n))
}

// lastStart is the largest useful start for a scrolled list of n rows shown
// in page rows: the one that brings the last row into view.
func lastStart(n, page int) int {
	switch {
	case n <= page:
		return 0
	case page < 3:
		return n - page
	}
	return n - page + 1 // one row goes to the marker for the rows above
}

// scrollWindow is the part of list that fits in page rows from start on. A
// marker row counts the rows hidden above, and another those hidden below.
func scrollWindow(list []string, start, page int) []string {
	if len(list) <= page {
		return list
	}
	if page < 3 { // no room for marker rows
		start = max(min(start, len(list)-page), 0)
		return list[start : start+max(page, 0)]
	}
	start = max(min(start, lastStart(len(list), page)), 0)
	var out []string
	if start > 0 {
		out = append(out, more("↑", start))
	}
	end := start + page - len(out)
	if end < len(list) {
		end-- // the marker for the rows below takes this row
	}
	out = append(out, list[start:end]...)
	if end < len(list) {
		out = append(out, more("↓", len(list)-end))
	}
	return out
}

// window is scrollWindow placed so that the cursor row is in view and, away
// from both ends of the list, in the middle of it.
func window(list []string, cursor, page int) []string {
	if page < 3 {
		return scrollWindow(list, cursor, page)
	}
	start := cursor - (page-2)/2
	if start <= 1 {
		start = 0 // the marker row would hide one row: show that row instead
	}
	return scrollWindow(list, start, page)
}

func age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func size(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}
