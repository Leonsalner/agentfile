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
		"claude-only": {"claude/CLAUDE.md", "claude/skills/routing/SKILL.md", "claude/skills/handoff/SKILL.md", "claude/skills/consult/SKILL.md"},
		"codex-only":  {"codex/AGENTS.md", "codex/skills/routing/SKILL.md", "codex/skills/handoff/SKILL.md", "codex/skills/consult/SKILL.md"},
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
	if got := renderAll(t, fixtures["both"], "linux"); len(got) != 8 {
		t.Errorf("both: got %d files, want 8", len(got))
	}
}

func TestUnselectedProvidersAbsent(t *testing.T) {
	openai := regexp.MustCompile(`\b(Sol|Luna|Astra|OpenAI|ChatGPT|gpt-6|codex exec)\b`)
	anthropic := regexp.MustCompile(`\b(Opus|Sonnet|Anthropic|claude -p)\b`)
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
	win := renderAll(t, fixtures["both"], "windows")["claude/skills/consult/SKILL.md"]
	nix := renderAll(t, fixtures["both"], "linux")["claude/skills/consult/SKILL.md"]
	if strings.Contains(win, "/dev/null") || !strings.Contains(win, "< NUL") {
		t.Error("windows consult skill should use NUL, not /dev/null")
	}
	if !strings.Contains(nix, "< /dev/null") {
		t.Error("posix consult skill should close stdin with /dev/null")
	}
}

func TestClaudeConsultEffortIsBoundSeparately(t *testing.T) {
	for _, name := range []string{"claude-only", "both"} {
		for _, goos := range []string{"darwin", "linux"} {
			s := renderAll(t, fixtures[name], goos)["claude/skills/consult/SKILL.md"]
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
	if len(rows) != 17 {
		t.Fatalf("expected 17 stage rows, got %d", len(rows))
	}
	return rows
}

func routeSet(row []string) map[string]bool {
	return map[string]bool{row[0]: true, row[1]: true}
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
	if claudeFav["Plan, Bounded"][0] != "Opus 5.5 low" {
		t.Errorf("claude-favored Plan, Bounded preferred = %q", claudeFav["Plan, Bounded"][0])
	}
	// Deep rows are fixed regardless of tier.
	codexFav := stageTable(t, fixtures["codex-favored"])
	if codexFav["Plan, Deep"][0] != "Opus 5.5 high" {
		t.Errorf("Deep row reordered: %q", codexFav["Plan, Deep"][0])
	}
	if codexFav["Verify Plan, Standard"][0] != "Sol xhigh" {
		t.Errorf("codex-favored Verify Plan, Standard preferred = %q", codexFav["Verify Plan, Standard"][0])
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
	if !strings.Contains(out["claude/CLAUDE.md"], "## Output") {
		t.Error("CLAUDE.md missing Output section")
	}
	if strings.Contains(out["codex/AGENTS.md"], "## Output") {
		t.Error("AGENTS.md should not carry the Claude-only Output section")
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
