package render

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"agentfile/internal/profile"
)

var fixtures = map[string]profile.Profile{
	"claude-only":      {Claude: &profile.AgentProfile{Plan: "max-5x"}},
	"codex-only":       {Codex: &profile.AgentProfile{Plan: "plus"}},
	"both":             {Claude: &profile.AgentProfile{Plan: "max-5x"}, Codex: &profile.AgentProfile{Plan: "plus"}},
	"lower-tier":       {Claude: &profile.AgentProfile{Plan: "pro"}, Codex: &profile.AgentProfile{Plan: "plus"}},
	"higher-tier":      {Claude: &profile.AgentProfile{Plan: "max-20x"}, Codex: &profile.AgentProfile{Plan: "pro"}},
	"codex-favored":    {Claude: &profile.AgentProfile{Plan: "pro"}, Codex: &profile.AgentProfile{Plan: "pro"}},
	"paid-credit":      {Claude: &profile.AgentProfile{Plan: "pro", Credits: true}, Codex: &profile.AgentProfile{Plan: "plus", Credits: true}},
	"unknown-tier":     {Claude: &profile.AgentProfile{Plan: "unknown"}, Codex: &profile.AgentProfile{Plan: "business-enterprise"}},
	"claude-unknown":   {Claude: &profile.AgentProfile{Plan: "team-enterprise"}},
	"codex-pro-credit": {Codex: &profile.AgentProfile{Plan: "pro", Credits: true}},
}

var goosList = []string{"darwin", "linux", "windows"}

func renderAll(t *testing.T, p profile.Profile, goos string) map[string]string {
	t.Helper()
	arts, err := Render(p, goos)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, a := range arts {
		if a.IsDir() {
			for name, b := range a.Files {
				out[string(a.Agent)+"/"+a.Path+"/"+name] = string(b)
			}
		} else {
			out[string(a.Agent)+"/"+a.Path] = string(a.Data)
		}
	}
	return out
}

// TestDump writes every fixture's output for manual review when
// AGENTFILE_DUMP names a directory.
func TestDump(t *testing.T) {
	dir := os.Getenv("AGENTFILE_DUMP")
	if dir == "" {
		t.Skip("set AGENTFILE_DUMP to write rendered fixtures")
	}
	for name, p := range fixtures {
		for _, goos := range goosList {
			for path, s := range renderAll(t, p, goos) {
				f := filepath.Join(dir, name, goos, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f, []byte(s), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestSelectionControlsArtifacts(t *testing.T) {
	cases := map[string][]string{
		"claude-only": {"claude/CLAUDE.md", "claude/skills/routing/SKILL.md", "claude/skills/handoff/SKILL.md", "claude/skills/delegate/SKILL.md", "claude/skills/consult/SKILL.md", "claude/skills/subagents/SKILL.md", "claude/skills/dialogue/SKILL.md", "claude/skills/precompact/SKILL.md"},
		"codex-only":  {"codex/AGENTS.md", "codex/skills/routing/SKILL.md", "codex/skills/handoff/SKILL.md", "codex/skills/delegate/SKILL.md", "codex/skills/consult/SKILL.md", "codex/skills/subagents/SKILL.md", "codex/skills/dialogue/SKILL.md", "codex/skills/precompact-codex/SKILL.md"},
	}
	for name, want := range cases {
		got := renderAll(t, fixtures[name], "linux")
		if len(got) != len(want) {
			t.Errorf("%s: got %d files, want %d", name, len(got), len(want))
		}
		for _, w := range want {
			if _, ok := got[w]; !ok {
				t.Errorf("%s: missing %s", name, w)
			}
		}
	}
	if got := renderAll(t, fixtures["both"], "linux"); len(got) != 16 {
		t.Errorf("both: got %d files, want 16", len(got))
	}
}

func TestUnselectedProvidersAbsent(t *testing.T) {
	openai := regexp.MustCompile(`\b(Sol|Luna|Astra|OpenAI|ChatGPT|gpt-6|codex exec)\b`)
	anthropic := regexp.MustCompile(`\b(Opus|Sonnet|Haiku|Anthropic|Claude Code|claude -p)\b`)
	for _, goos := range goosList {
		for path, s := range renderAll(t, fixtures["claude-only"], goos) {
			if m := openai.FindString(s); m != "" {
				t.Errorf("claude-only %s %s mentions %q", goos, path, m)
			}
		}
		for path, s := range renderAll(t, fixtures["codex-only"], goos) {
			if m := anthropic.FindString(s); m != "" {
				t.Errorf("codex-only %s %s mentions %q", goos, path, m)
			}
		}
	}
}

func TestFableAbsentAndAstraGated(t *testing.T) {
	for name, p := range fixtures {
		for path, s := range renderAll(t, p, "darwin") {
			if strings.Contains(s, "Fable") {
				t.Errorf("%s %s mentions Fable", name, path)
			}
			if strings.HasSuffix(path, "routing/SKILL.md") && p.Codex != nil &&
				!strings.Contains(s, "explicit per-scope user approval") {
				t.Errorf("%s: Astra approval gate missing", name)
			}
		}
	}
}

// Astra's routes stop at high: no route, ladder or worker/advisor table line
// may name a higher Astra effort; the evidence tables keep them as data.
func TestAstraCappedAtHigh(t *testing.T) {
	above := regexp.MustCompile(`Astra (xhigh|max|ultra)\b`)
	for name, p := range fixtures {
		if p.Codex == nil {
			continue
		}
		for _, goos := range goosList {
			for path, s := range renderAll(t, p, goos) {
				for _, line := range strings.Split(s, "\n") {
					if (strings.HasPrefix(line, "|") || strings.HasPrefix(line, "**Capability ladder**")) && above.MatchString(line) {
						t.Errorf("%s %s %s routes Astra above high: %q", name, goos, path, above.FindString(line))
					}
				}
				if strings.HasSuffix(path, "routing/SKILL.md") && !strings.Contains(s, "High is its routing ceiling") {
					t.Errorf("%s %s: Astra high cap missing", name, goos)
				}
			}
		}
	}
}

func TestNoModelSelectionOrLiveQuota(t *testing.T) {
	banned := []string{"CODEX_HOME", "usedPercent", "zsh -lic", "grok", "Gemini", "gemini"} // helper and account names: see internal/scan
	for name, p := range fixtures {
		for _, goos := range goosList {
			for path, s := range renderAll(t, p, goos) {
				for _, b := range banned {
					if strings.Contains(s, b) {
						t.Errorf("%s %s %s contains %q", name, goos, path, b)
					}
				}
			}
		}
	}
}

func TestPlatformCommands(t *testing.T) {
	win := renderAll(t, fixtures["both"], "windows")["claude/skills/delegate/SKILL.md"]
	nix := renderAll(t, fixtures["both"], "linux")["claude/skills/delegate/SKILL.md"]
	if strings.Contains(win, "/dev/null") || !strings.Contains(win, "< NUL") {
		t.Error("windows delegate skill should use NUL, not /dev/null")
	}
	if !strings.Contains(nix, "< /dev/null") {
		t.Error("posix delegate skill should close stdin with /dev/null")
	}
}

func TestClaudeAdvisorEffortIsBoundSeparately(t *testing.T) {
	for _, name := range []string{"claude-only", "both"} {
		for _, goos := range []string{"darwin", "linux"} {
			s := renderAll(t, fixtures[name], goos)["claude/skills/delegate/SKILL.md"]
			if !strings.Contains(s, "OPUS_EFFORT=medium") ||
				!strings.Contains(s, `--model opus --effort "$OPUS_EFFORT"`) ||
				strings.Contains(s, `--model opus --effort "$ADVISOR_EFFORT"`) {
				t.Errorf("%s %s: Claude advisor effort is not bound independently", name, goos)
			}
		}
	}
}

// stageTable extracts the routing table rows as "stage|preferred|alternative|fallback".
func stageTable(t *testing.T, p profile.Profile) map[string][]string {
	t.Helper()
	s := renderAll(t, p, "linux")[string(p.Agents()[0])+"/skills/routing/SKILL.md"]
	rows := map[string][]string{}
	s = s[strings.Index(s, "| Stage / shape"):]
	for _, line := range strings.Split(s, "\n")[2:] {
		if !strings.HasPrefix(line, "| ") {
			break
		}
		cells := strings.Split(line, " | ")
		rows[strings.TrimPrefix(cells[0], "| ")] = cells[1:4]
	}
	if len(rows) != 19 {
		t.Fatalf("expected 19 stage rows, got %d", len(rows))
	}
	return rows
}

var modelName = regexp.MustCompile(`Opus|Sonnet|Haiku|Sol|Luna|Astra`)

// routeSet is a row's two routes regardless of order. A condition that leads
// the Preferred cell ("skip; if useful ") is kept apart from the route it
// precedes, because it stays in Preferred when a tier reorders the row.
func routeSet(row []string) map[string]bool {
	pref, lead := row[0], ""
	if loc := modelName.FindStringIndex(pref); loc != nil {
		lead, pref = pref[:loc[0]], pref[loc[0]:]
	}
	return map[string]bool{"lead: " + lead: true, pref: true, row[1]: true}
}

// Tiers may reorder comparable routes but never change which routes are
// eligible, so the capability threshold is identical for every tier.
func TestTiersKeepCapabilityThreshold(t *testing.T) {
	base := stageTable(t, fixtures["both"])
	for _, name := range []string{"lower-tier", "higher-tier", "codex-favored", "paid-credit", "unknown-tier"} {
		got := stageTable(t, fixtures[name])
		for stage, row := range base {
			g := got[stage]
			if !mapsEqual(routeSet(row), routeSet(g)) || row[2] != g[2] {
				t.Errorf("%s %s: routes %v differ from base %v", name, stage, g, row)
			}
		}
	}
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func TestTierOrdering(t *testing.T) {
	// Claude Max 20x vs ChatGPT Plus: Claude favored on comparable rows.
	claudeFav := stageTable(t, profile.Profile{Claude: &profile.AgentProfile{Plan: "max-20x"}, Codex: &profile.AgentProfile{Plan: "plus"}})
	if claudeFav["Verify Plan, Bounded"][0] != "Opus 5.5 medium" {
		t.Errorf("claude-favored Verify Plan, Bounded preferred = %q", claudeFav["Verify Plan, Bounded"][0])
	}
	// Deep rows are fixed regardless of tier.
	codexFav := stageTable(t, fixtures["codex-favored"])
	if codexFav["Plan, Deep"][0] != "Opus 5.5 xhigh" {
		t.Errorf("Deep row reordered: %q", codexFav["Plan, Deep"][0])
	}
	if codexFav["Verify Plan, Standard"][0] != "Sol high" {
		t.Errorf("codex-favored Verify Plan, Standard preferred = %q", codexFav["Verify Plan, Standard"][0])
	}
	// A condition that leads the Preferred cell ("skip; if useful ") is not a
	// model name: it stays in Preferred, and only the model routes trade places.
	for stage, lead := range map[string]string{"Plan, Micro": "skip; if useful ", "Plan Next": "skip when done; otherwise "} {
		if got, want := claudeFav[stage][:2], []string{lead + "Opus 5.5 medium", "Sol medium"}; got[0] != want[0] || got[1] != want[1] {
			t.Errorf("claude-favored %s = %q, want %q", stage, got, want)
		}
		if got, want := codexFav[stage][:2], []string{lead + "Sol medium", "Opus 5.5 medium"}; got[0] != want[0] || got[1] != want[1] {
			t.Errorf("codex-favored %s = %q, want %q", stage, got, want)
		}
	}
	// Unknown tier keeps reviewed default order.
	unk := stageTable(t, fixtures["unknown-tier"])
	def := stageTable(t, fixtures["higher-tier"]) // equal allowances keep the default order
	for stage := range def {
		if unk[stage][0] != def[stage][0] {
			t.Errorf("unknown tier reordered %s", stage)
		}
	}
}

func TestCreditsOnlyChangeFallbackGuidance(t *testing.T) {
	with := renderAll(t, fixtures["paid-credit"], "linux")
	without := renderAll(t, fixtures["lower-tier"], "linux")
	for path, s := range with {
		w := without[path]
		if strings.HasSuffix(path, "routing/SKILL.md") {
			if !strings.Contains(s, "paid extra usage as already enabled") || strings.Contains(w, "paid extra usage as already enabled") {
				t.Errorf("%s: credit guidance not toggled", path)
			}
			if !strings.Contains(s, "Never purchase, enable or raise paid usage yourself") {
				t.Errorf("%s: credit guardrail missing", path)
			}
			continue
		}
		// Other files differ only in the header's profile summary.
		if stripSummary(s) != stripSummary(w) {
			t.Errorf("%s differs beyond the profile summary", path)
		}
	}
}

// Guidance may point at "a listed allowance fallback" only where the stage
// table lists one. A Codex-only install lists none, so without paid extra
// usage an exhausted allowance means stopping, not looking for a fallback.
func TestFallbackGuidanceMatchesTheStageTable(t *testing.T) {
	for name, p := range fixtures {
		listed := false
		for _, row := range stageTable(t, p) {
			listed = listed || row[2] != "—"
		}
		if want := p.Claude != nil; listed != want {
			t.Fatalf("%s: stage table lists an allowance fallback = %v, want %v", name, listed, want)
		}
		for _, goos := range goosList {
			for path, s := range renderAll(t, p, goos) {
				if strings.HasSuffix(path, "routing/SKILL.md") && !listed && strings.Contains(s, "listed allowance fallback") {
					t.Errorf("%s %s %s points at a listed allowance fallback, but its stage table lists none", name, goos, path)
				}
			}
		}
	}
	for name, want := range map[string]string{
		"codex-only":  "No paid extra usage declared: once the included allowance is exhausted, stop and tell the user.",
		"claude-only": "No paid extra usage declared: once the included allowance is exhausted, use a listed allowance fallback where the work fits, or stop and tell the user.",
		"both":        "No paid extra usage declared: once the included allowance is exhausted, move to the other selected provider's capable route or a listed allowance fallback, or stop and tell the user.",
	} {
		p := fixtures[name]
		if s := renderAll(t, p, "linux")[string(p.Agents()[0])+"/skills/routing/SKILL.md"]; !strings.Contains(s, want) {
			t.Errorf("%s: missing %q", name, want)
		}
	}
}

func stripSummary(s string) string {
	lines := strings.SplitN(s, "\n", 3)
	if len(lines) == 3 && strings.HasPrefix(lines[1], "agentfile ") {
		return lines[0] + "\n" + lines[2]
	}
	return s
}

func TestUnknownTierExplained(t *testing.T) {
	s := renderAll(t, fixtures["unknown-tier"], "linux")["claude/skills/routing/SKILL.md"]
	if !strings.Contains(s, "does not know this plan's allowance") || !strings.Contains(s, "unknown allowance, so comparable routes keep the reviewed default order") {
		t.Error("unknown tier guidance missing")
	}
}

func TestDeterministicAndNoTemplateResidue(t *testing.T) {
	for name, p := range fixtures {
		a := renderAll(t, p, "darwin")
		b := renderAll(t, p, "darwin")
		for path, s := range a {
			if b[path] != s {
				t.Errorf("%s %s not deterministic", name, path)
			}
			if strings.Contains(s, "{{") || strings.Contains(s, "<no value>") || strings.Contains(s, "\n\n\n") {
				t.Errorf("%s %s has template residue", name, path)
			}
			if strings.HasSuffix(path, "SKILL.md") && !strings.HasPrefix(s, "---\nname: ") {
				t.Errorf("%s %s lacks skill front matter", name, path)
			}
		}
	}
}

func TestNormalizeCRLFTemplates(t *testing.T) {
	got := string(normalize([]byte("---\r\nname: routing\r\n\r\n\r\n# Guide\r\n")))
	want := "---\nname: routing\n\n# Guide\n"
	if got != want {
		t.Errorf("normalized template = %q, want %q", got, want)
	}
}

func TestInstructionFileTails(t *testing.T) {
	out := renderAll(t, fixtures["both"], "linux")
	for _, f := range []string{"claude/CLAUDE.md", "codex/AGENTS.md"} {
		if strings.Count(out[f], "## Output") != 1 {
			t.Errorf("%s should carry exactly one shared Output section", f)
		}
	}
	claudeOnly, codexOnly := "Don't narrate routine tool calls", "Finish the work already authorized before you ask"
	if !strings.Contains(out["claude/CLAUDE.md"], claudeOnly) || strings.Contains(out["codex/AGENTS.md"], claudeOnly) {
		t.Error("the Claude-only output tail must appear in CLAUDE.md only")
	}
	for _, line := range []string{codexOnly, "report every blocker, material workaround, skipped deliverable"} {
		if !strings.Contains(out["codex/AGENTS.md"], line) || strings.Contains(out["claude/CLAUDE.md"], line) {
			t.Errorf("the Codex-only output line %q must appear in AGENTS.md only", line)
		}
	}
	if !strings.Contains(out["claude/CLAUDE.md"], "run `precompact` unprompted") || !strings.Contains(out["codex/AGENTS.md"], "run `precompact-codex` unprompted") {
		t.Error("the long-session rule must name the agent's own precompact skill")
	}
	if !strings.HasPrefix(out["codex/AGENTS.md"], "# AGENTS.md\n") || !strings.HasPrefix(out["claude/CLAUDE.md"], "# CLAUDE.md\n") {
		t.Error("wrong instruction file titles")
	}
}

func TestProvenance(t *testing.T) {
	p, err := LoadProvenance()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(p.UpstreamCommit) {
		t.Errorf("upstream commit %q is not a full hash", p.UpstreamCommit)
	}
	if p.ContentVersion == "" || p.Imported == "" {
		t.Error("provenance incomplete")
	}
}

func TestEvidenceTablesComplete(t *testing.T) {
	r, err := LoadRoster()
	if err != nil {
		t.Fatal(err)
	}
	bad := r.Evidence
	bad.Columns = append(append([]string{}, bad.Columns...), "Missing")
	if r.checkTable(bad) == nil {
		t.Error("a column naming no model was accepted")
	}
	r.Models = append(r.Models, rosterModel{Short: "Missing"})
	if r.checkTable(bad) == nil {
		t.Error("a row without a value for a column was accepted")
	}
}
