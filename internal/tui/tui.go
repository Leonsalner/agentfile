// Package tui is the interactive installer: agent selection, plan and
// extra-usage questions, an always-on preview with per-target choices,
// backups, restore and recovery. It never asks the user to pick models.
package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

	scr     screen
	cursor  int
	height  int
	agents  []installer.AgentState
	menu    []string
	chosen  map[profile.Agent]bool
	queue   []profile.Agent // agents still to ask about
	asking  profile.Agent
	prof    profile.Profile
	plan    *installer.Plan
	backups []*state.Manifest
	// recovery lists content-only targets an interrupted operation left
	// different from their backup; choice holds choiceRestore or choiceKeep
	// for each one the user has decided.
	recovery       []state.RecoveryItem
	choice         map[string]string
	recoveryNotice string
	detail         []string
	scroll         int
	back           screen
	result         string
	now            func() time.Time
}

func New(env installer.Env, st *state.Store) Model {
	m := Model{env: env, store: st, height: 24, now: time.Now}
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
		m.height = msg.Height
		return m, nil
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return m, tea.Quit
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
	}
}

func (m Model) key(key string) (tea.Model, tea.Cmd) {
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
		}

	case scrPreview:
		m.move(key, len(m.plan.Items))
		it := &m.plan.Items[m.cursor]
		switch key {
		case "space", "x":
			if it.Conflict && it.Blocked == "" {
				it.Replace = !it.Replace
			}
		case "enter", "d":
			m.showDetail(*it)
		case "a":
			if n, _ := m.counts(); n > 0 {
				m.scr = scrConfirm
			} else {
				m.scr, m.result = scrResult, "Nothing to write: every path is unchanged or kept. No backup was made."
			}
		case "esc":
			m.goHome()
		}

	case scrDetail:
		page := max(m.height-4, 1)
		switch key {
		case "up", "k":
			m.scroll = max(m.scroll-1, 0)
		case "down", "j":
			m.scroll = min(m.scroll+1, max(len(m.detail)-page, 0))
		case "pgup", "b":
			m.scroll = max(m.scroll-page, 0)
		case "pgdown", "space", "f":
			m.scroll = min(m.scroll+page, max(len(m.detail)-page, 0))
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
				m.plan, m.scr, m.cursor = plan, scrPreview, 0
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
				m.scr, m.result = scrResult, fmt.Sprintf("Recovery finished. Restored %d saved file(s), kept %d as they are. You can use agentfile again.\n"+
					"The saved copies stay under \"%s\" if you change your mind.", len(approved), len(keep), menuRestore)
			} else if len(approved) > 0 {
				m.scr, m.result = scrResult, fmt.Sprintf("Recovery finished. Restored %d saved file(s). You can use agentfile again.", len(approved))
			} else {
				m.scr, m.result = scrResult, "Recovery finished. No files needed restoring. You can use agentfile again."
			}
		case "n", "esc":
			m.goHome()
		}

	case scrResult:
		if key == "enter" || key == "esc" {
			m.goHome()
		}
		if key == "q" {
			return m, tea.Quit
		}
	}
	return m, nil
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
	fmt.Fprintf(&b, "%s\n%s", it.Path, it.Action)
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
}

// counts returns how many items will be written and how many are kept.
func (m Model) counts() (write, kept int) {
	for _, it := range m.plan.Items {
		switch {
		case it.Proceeds():
			write++
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
	m.scr = scrResult
	if man == nil {
		m.result = "Nothing to change: every selected target already matches. No backup was made."
		return
	}
	verb := "Installed"
	if m.plan.Kind == "restore" {
		verb = "Restored"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d entries. Backup of the previous state: %s\n", verb, len(man.Entries)+len(man.RemovedDirs), man.ID)
	for _, e := range man.Entries {
		fmt.Fprintf(&b, "  %s\n", e.Path)
	}
	for _, dir := range man.RemovedDirs {
		fmt.Fprintf(&b, "  %s (empty parent removed)\n", dir)
	}
	fmt.Fprintf(&b, "\nBackups are kept in %s until you remove them.", filepath.Join(m.store.Dir, "backups"))
	m.result = b.String()
}

func (m *Model) fail(err error) {
	m.scr = scrResult
	switch {
	case errors.Is(err, state.ErrChanged):
		m.result = "Stopped before writing anything: " + err.Error() + ".\nA target changed after the preview. Review the new preview and choose again."
	case errors.Is(err, state.ErrRecoveryConflict):
		m.result = "Not recovered: " + err.Error() + ".\nNothing was overwritten; the journal and verified backup are kept. Choose \"" + menuRecover +
			"\" again to review each target, or resolve the listed paths yourself first."
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

// Minimal styling with plain SGR codes; NO_COLOR disables it.
var useColor = os.Getenv("NO_COLOR") == ""

func sgr(code, s string) string {
	if !useColor || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string   { return sgr("1", s) }
func dim(s string) string    { return sgr("2", s) }
func accent(s string) string { return sgr("1;36", s) }
func green(s string) string  { return sgr("32", s) }
func yellow(s string) string { return sgr("33", s) }
func red(s string) string    { return sgr("31", s) }

func pad(s string, n int) string {
	if w := len([]rune(s)); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func row(on bool, text string) string {
	if on {
		return accent("›") + " " + bold(text)
	}
	return "  " + text
}

func help(keys ...string) string { return "\n" + dim(strings.Join(keys, "  ·  ")) }

func statusColor(s string) string {
	switch {
	case s == "absent":
		return dim(s)
	case s == "installed by agentfile", s == "unchanged since that operation":
		return green(s)
	case strings.HasPrefix(s, "cannot"):
		return red(s)
	}
	return yellow(s)
}

func actionColor(a installer.Action) string {
	switch a {
	case installer.Unchanged:
		return dim(string(a))
	case installer.Create:
		return green(string(a))
	case installer.Remove, installer.RemoveParent:
		return red(string(a))
	}
	return yellow(string(a))
}

func diffLine(l string) string {
	switch {
	case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "==="):
		return bold(l)
	case strings.HasPrefix(l, "@@"):
		return accent(l)
	case strings.HasPrefix(l, "+"):
		return green(l)
	case strings.HasPrefix(l, "-"):
		return red(l)
	}
	return l
}

func (m Model) render() string {
	var b strings.Builder
	b.WriteString(sgr("7;1", " agentfile ") + "  " + dim("instructions & skills for Claude Code and Codex") + "\n\n")
	switch m.scr {
	case scrHome:
		for _, a := range m.agents {
			bin := green(a.Binary)
			if a.Binary == "" {
				bin = yellow("not on PATH")
			}
			fmt.Fprintf(&b, "%s  %s  %s\n", bold(profile.ClientName(a.Agent)), dim(shortPath(m.env.Home, a.Home)), bin)
			if a.Problem != "" {
				fmt.Fprintf(&b, "  %s %s\n", red("✗ cannot install here:"), a.Problem)
			}
			for _, w := range a.Warnings {
				fmt.Fprintf(&b, "  %s %s\n", yellow("!"), dim(w))
			}
			for _, t := range a.Targets {
				fmt.Fprintf(&b, "  %s %s\n", pad(shortPath(m.env.Home, t.Path), 30), statusColor(t.Status))
			}
			b.WriteString("\n")
		}
		if m.store.Pending() || m.store.Locked() {
			b.WriteString(yellow("An earlier operation did not finish, or another agentfile is running.") + "\n\n")
		}
		for i, item := range m.menu {
			b.WriteString(row(i == m.cursor, item) + "\n")
		}
		b.WriteString(help("↑/↓ move", "enter select", "q quit"))

	case scrAgents:
		b.WriteString(bold("Which agents should get the instructions and skills?") + "\n\n")
		for i, a := range []profile.Agent{profile.Claude, profile.Codex} {
			box := dim("○")
			if m.chosen[a] {
				box = green("●")
			}
			b.WriteString(row(i == m.cursor, box+" "+profile.ClientName(a)) + "\n")
		}
		b.WriteString("\n" + dim("A missing CLI is only a warning. You will not be asked to choose models.") + "\n")
		b.WriteString(help("space toggle", "enter continue", "esc back"))

	case scrPlan:
		fmt.Fprintf(&b, "%s\n\n", bold("Which "+profile.ClientName(m.asking)+" plan do you use?"))
		for i, p := range profile.Plans[m.asking] {
			b.WriteString(row(i == m.cursor, p.Label) + "\n")
		}
		b.WriteString("\n" + dim("The plan only orders comparable routes and sets how sparingly to spend a\n"+
			"limited allowance. It never changes which models are capable of a task.\n"+
			"An unknown plan gets conservative guidance.") + "\n")
		b.WriteString(help("enter select", "esc back"))

	case scrCredits:
		fmt.Fprintf(&b, "%s\n\n", bold("Have you already enabled paid extra usage for "+profile.ClientName(m.asking)+"?"))
		for i, l := range []string{"Yes, it is enabled", "No"} {
			b.WriteString(row(i == m.cursor, l) + "\n")
		}
		b.WriteString("\n" + dim("\"Yes\" only lets the guidance continue on already-enabled paid usage after\n"+
			"the included allowance runs out. agentfile never buys or enables anything.") + "\n")
		b.WriteString(help("enter select", "esc back"))

	case scrPreview:
		m.renderPreview(&b)

	case scrDetail:
		page := max(m.height-5, 1)
		end := min(m.scroll+page, len(m.detail))
		for i, l := range m.detail[m.scroll:end] {
			if m.scroll+i < 2 {
				l = bold(l)
			} else {
				l = diffLine(l)
			}
			b.WriteString(l + "\n")
		}
		b.WriteString(help(fmt.Sprintf("%d–%d of %d", m.scroll+1, end, len(m.detail)), "↑/↓ scroll", "space/b page", "esc back"))

	case scrConfirm:
		n, kept := m.counts()
		verb := "Install"
		if m.plan.Kind == "restore" {
			verb = "Restore"
		}
		fmt.Fprintf(&b, "%s\n\n", bold(fmt.Sprintf("%s: write %d path(s), keep %d unchanged by your choice?", verb, n, kept)))
		fmt.Fprintf(&b, "The current state of each path is backed up first to\n%s\n\n", accent(filepath.Join(m.store.Dir, "backups")))
		b.WriteString(dim("Every path is rechecked; if anything changed since this preview, nothing is written.") + "\n")
		b.WriteString(help("y write", "n back"))

	case scrBackups:
		if len(m.backups) == 0 {
			b.WriteString("No backups yet.\n" + help("esc back"))
			break
		}
		b.WriteString(bold("Backups") + dim("  newest first; each records the state before one operation") + "\n\n")
		page := max(m.height-8, 3)
		start := windowStart(m.cursor, len(m.backups), page)
		for i := start; i < len(m.backups) && i < start+page; i++ {
			s := m.backups[i]
			line := fmt.Sprintf("%s  %s %s  v%s  %s  %d target(s)  %s",
				s.Created.Local().Format("2006-01-02 15:04"), pad(s.Kind, 7), pad(age(m.now().Sub(s.Created)), 8),
				s.ContentVersion, strings.Join(s.Agents, "+"), len(s.Entries)+len(s.RemovedDirs), size(m.store.DiskSize(s.ID)))
			b.WriteString(row(i == m.cursor, line) + "\n")
		}
		b.WriteString(help("enter preview restore", "esc back"))

	case scrRecover:
		if len(m.recovery) > 0 {
			m.renderRecoveryReview(&b)
			break
		}
		b.WriteString(bold("An operation was interrupted, or a lock is present.") + "\n\n")
		b.WriteString("Recovery rolls every path it touched back to its verified snapshot and\n")
		b.WriteString("clears the lock. " + yellow("Only continue if no other agentfile is running.") + "\n")
		b.WriteString(help("y recover", "n back"))

	case scrResult:
		title, rest, _ := strings.Cut(m.result, "\n")
		switch {
		case strings.HasPrefix(title, "Error"), strings.HasPrefix(title, "Stopped"), strings.HasPrefix(title, "Not recovered"):
			title = red(title)
		default:
			title = green(title)
		}
		b.WriteString(title + "\n" + rest + "\n")
		b.WriteString(help("enter main menu", "q quit"))
	}
	return b.String()
}

func (m Model) renderPreview(b *strings.Builder) {
	if m.plan.Kind == "restore" {
		s := m.plan.Snapshot
		fmt.Fprintf(b, "%s %s\n", bold("Restore preview"), dim(fmt.Sprintf("undo %s from %s", s.Kind, s.Created.Local().Format("2006-01-02 15:04"))))
	} else {
		v, _ := render.LoadProvenance()
		fmt.Fprintf(b, "%s %s\n", bold("Install preview"), dim("content "+v.ContentVersion+" for "+m.plan.Profile.Summary()))
	}
	b.WriteString(dim("Exact paths and actions. Nothing is written until you confirm.") + "\n\n")
	for i, it := range m.plan.Items {
		mark := "      "
		switch {
		case it.Blocked != "":
			mark = red("[skip]")
		case it.Conflict && it.Replace:
			mark = sgr("35", "[repl]")
		case it.Conflict:
			mark = yellow("[keep]")
		}
		line := mark + " " + pad(shortPath(m.env.Home, it.Path), 30) + " " + actionColor(it.Action)
		if it.Status != "" && it.Action != installer.Unchanged {
			line += dim(" · " + it.Status)
		}
		if i == m.cursor {
			line = accent("›") + " " + line
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	n, kept := m.counts()
	it := m.plan.Items[m.cursor]
	if it.Blocked != "" {
		fmt.Fprintf(b, "\n%s %s\n", red("blocked:"), it.Blocked)
	} else if it.Conflict {
		b.WriteString("\n" + yellow("This path exists and differs. It is kept unless you choose to replace it.") + "\n")
	}
	if it.Note != "" {
		b.WriteString(dim(it.Note) + "\n")
	}
	switch it.Action {
	case installer.UpdateInPlace:
		b.WriteString(dim("Only this file's contents change. If setup stops halfway through,\n"+
			"the file may be partly updated. You can then choose to restore its saved copy.") + "\n")
	case installer.Recreate:
		b.WriteString(dim("This missing file will be created again. Its old permissions and dates\n"+
			"cannot be restored.") + "\n")
	}
	fmt.Fprintf(b, "\n%s to write, %s kept\n", bold(fmt.Sprint(n)), bold(fmt.Sprint(kept)))
	b.WriteString(help("↑/↓ move", "enter diff", "space keep/replace", "a apply", "esc cancel"))
}

func (m Model) renderRecoveryReview(b *strings.Builder) {
	b.WriteString(bold("Setup stopped before it finished.") + "\n\n")
	b.WriteString("These files now differ from their saved copies. They may contain your own edits.\n")
	b.WriteString("Review each file, then choose: restore its saved copy, or keep it as it is now.\n")
	b.WriteString("To finish recovery, every listed file needs a choice.\n\n")
	for i, r := range m.recovery {
		mark := yellow("[ ] Choose restore or keep")
		switch m.choice[r.Path] {
		case choiceRestore:
			mark = sgr("35", "[x] Restore saved copy    ")
		case choiceKeep:
			mark = sgr("35", "[x] Keep current file     ")
		}
		status := "differs from backup"
		switch {
		case r.Current.Type == fsnode.Absent:
			status = "missing; restoring creates a new copy with default permissions"
		case r.Backup.Type == fsnode.Absent:
			status = "did not exist before; restoring removes it"
		}
		b.WriteString(row(i == m.cursor, mark+" "+pad(shortPath(m.env.Home, r.Path), 30)+" "+dim(status)) + "\n")
	}
	if m.recoveryNotice != "" {
		b.WriteString("\n" + yellow(m.recoveryNotice) + "\n")
	}
	b.WriteString("\n" + yellow("Only continue if no other agentfile is running.") + "\n")
	b.WriteString(help("↑/↓ move", "enter compare files", "r restore saved copy", "c keep current file", "y finish", "n leave for now"))
}

func shortPath(home, p string) string {
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return p
}

func windowStart(cursor, n, page int) int {
	if cursor < page {
		return 0
	}
	return min(cursor-page+1, max(n-page, 0))
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
