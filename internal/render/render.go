// Package render assembles the selected instruction files and skills from the
// embedded, reviewed content for one declared profile and target OS.
// It never executes content, fetches anything, or guesses a route: every
// route string comes from the reviewed roster.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"text/template"

	"agentfile/content"
	"agentfile/internal/profile"
)

// SkillNames are the managed skills, in install order.
var SkillNames = []string{"routing", "handoff", "consult"}

// Artifact is one managed entry relative to an agent home: either the
// instruction file or a named skill directory.
type Artifact struct {
	Agent profile.Agent
	Path  string            // slash-separated, relative to the agent home
	Data  []byte            // instruction file contents; nil for skills
	Files map[string][]byte // skill directory contents; nil for files
}

func (a Artifact) IsDir() bool { return a.Files != nil }

type Provenance struct {
	ContentVersion  string `json:"content_version"`
	UpstreamCommit  string `json:"upstream_commit"`
	UpstreamVersion string `json:"upstream_version"`
	Imported        string `json:"imported"`
}

func LoadProvenance() (Provenance, error) {
	var p Provenance
	b, err := content.FS.ReadFile("provenance.json")
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(b, &p)
	return p, err
}

type rosterModel struct {
	Name     string            `json:"name"`
	Short    string            `json:"short"`
	Provider string            `json:"provider"`
	CLIModel string            `json:"cli_model"`
	Efforts  string            `json:"efforts"`
	Advisor  bool              `json:"advisor"`
	Role     map[string]string `json:"role"`
}

type route struct {
	Preferred   string `json:"preferred"`
	Alternative string `json:"alternative"`
	Fallback    string `json:"fallback"`
	Escalate    string `json:"escalate"`
}

type stageRow struct {
	Stage   string `json:"stage"`
	Reorder bool   `json:"reorder"`
	Both    route  `json:"both"`
	Claude  route  `json:"claude"`
	Codex   route  `json:"codex"`
}

type Roster struct {
	Schema   int           `json:"schema"`
	Reviewed string        `json:"reviewed"`
	Models   []rosterModel `json:"models"`
	Ladder   [][]struct {
		Model  string `json:"model"`
		Effort string `json:"effort"`
	} `json:"ladder"`
	Evidence struct {
		Note    string   `json:"note"`
		Columns []string `json:"columns"`
		Rows    []struct {
			Effort string            `json:"effort"`
			Values map[string]string `json:"values"`
		} `json:"rows"`
	} `json:"evidence"`
	Stages []stageRow `json:"stages"`
}

func LoadRoster() (*Roster, error) {
	b, err := content.FS.ReadFile("roster.json")
	if err != nil {
		return nil, err
	}
	var r Roster
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("roster.json: %w", err)
	}
	if r.Schema != 1 {
		return nil, fmt.Errorf("roster.json: unsupported schema %d", r.Schema)
	}
	return &r, nil
}

func (r *Roster) model(short string) (rosterModel, bool) {
	for _, m := range r.Models {
		if m.Short == short {
			return m, true
		}
	}
	return rosterModel{}, false
}

func providerAgent(provider string) profile.Agent {
	if provider == "anthropic" {
		return profile.Claude
	}
	return profile.Codex
}

type modelView struct{ Name, Efforts, Role string }
type stageView struct{ Stage, Preferred, Alternative, Fallback, Escalate string }
type allowanceView struct{ Client, Plan, Guidance, Credits string }

type data struct {
	FileName, ContentVersion, ProfileSummary string
	Claude, Codex, Both, Windows             bool
	OnlyProvider, AgentList                  string
	Models                                   []modelView
	Ladder                                   string
	Evidence                                 struct{ Note, Table string }
	AlternativeHeader                        string
	Stages                                   []stageView
	Allowance                                []allowanceView
	OrderNote                                string
	AdvisorList, AdvisorModels, NonAdvisors  string
}

// Render produces every artifact for the selected agents. goos selects
// platform-appropriate command examples ("windows" or anything else).
func Render(p profile.Profile, goos string) ([]Artifact, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	r, err := LoadRoster()
	if err != nil {
		return nil, err
	}
	prov, err := LoadProvenance()
	if err != nil {
		return nil, err
	}
	d := buildData(r, p, goos)
	d.ContentVersion = prov.ContentVersion
	d.ProfileSummary = p.Summary()

	skills := map[string][]byte{}
	for _, name := range SkillNames {
		out, err := execute("skills/"+name+"/SKILL.md.tmpl", d)
		if err != nil {
			return nil, err
		}
		skills[name] = out
	}

	var arts []Artifact
	for _, a := range p.Agents() {
		fileName, tail := "AGENTS.md", ""
		if a == profile.Claude {
			fileName, tail = "CLAUDE.md", "fragments/claude.tail.md"
		}
		d.FileName = fileName
		var buf bytes.Buffer
		for _, part := range []string{"fragments/head.md.tmpl", "fragments/core.md.tmpl"} {
			out, err := execute(part, d)
			if err != nil {
				return nil, err
			}
			buf.Write(out)
		}
		if tail != "" {
			b, err := content.FS.ReadFile(tail)
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		}
		arts = append(arts, Artifact{Agent: a, Path: fileName, Data: normalize(buf.Bytes())})
		for _, name := range SkillNames {
			arts = append(arts, Artifact{Agent: a, Path: "skills/" + name, Files: map[string][]byte{"SKILL.md": skills[name]}})
		}
	}
	return arts, nil
}

func execute(name string, d data) ([]byte, error) {
	src, err := fs.ReadFile(content.FS, name)
	if err != nil {
		return nil, err
	}
	t, err := template.New(name).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if strings.HasSuffix(name, ".tmpl") && strings.HasPrefix(name, "skills/") {
		return normalize(buf.Bytes()), nil
	}
	return buf.Bytes(), nil
}

var blankRuns = regexp.MustCompile(`\n{3,}`)

// normalize uses LF across checkout platforms, collapses blank-line runs
// left by template conditionals, and ends the file with exactly one newline.
func normalize(b []byte) []byte {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = blankRuns.ReplaceAllString(s, "\n\n")
	return []byte(strings.TrimRight(s, "\n") + "\n")
}

func buildData(r *Roster, p profile.Profile, goos string) data {
	d := data{
		Claude:  p.Has(profile.Claude),
		Codex:   p.Has(profile.Codex),
		Windows: goos == "windows",
	}
	d.Both = d.Claude && d.Codex
	selected := func(provider string) bool { return p.Has(providerAgent(provider)) }

	switch {
	case d.Both:
		d.AgentList = "Claude Code and Codex"
		d.AlternativeHeader = "Other-provider alternative"
		d.AdvisorList = "Opus 5.5, Sol or Astra"
		d.AdvisorModels = "Claude Opus 5.5, OpenAI Sol, and OpenAI Astra"
		d.NonAdvisors = "OpenAI Luna and Claude Sonnet 5 are not advisors."
	case d.Claude:
		d.AgentList, d.OnlyProvider = "Claude Code", "Anthropic"
		d.AlternativeHeader = "Alternative"
		d.AdvisorList = "Opus 5.5"
		d.AdvisorModels = "Claude Opus 5.5"
		d.NonAdvisors = "Claude Sonnet 5 is not an advisor."
	default:
		d.AgentList, d.OnlyProvider = "Codex", "OpenAI"
		d.AlternativeHeader = "Alternative"
		d.AdvisorList = "Sol or Astra"
		d.AdvisorModels = "OpenAI Sol and OpenAI Astra"
		d.NonAdvisors = "OpenAI Luna is not an advisor."
	}

	variant := "single"
	if d.Both {
		variant = "both"
	}
	for _, m := range r.Models {
		if selected(m.Provider) {
			d.Models = append(d.Models, modelView{m.Name, m.Efforts, m.Role[variant]})
		}
	}

	var groups []string
	for _, g := range r.Ladder {
		var members []string
		for _, e := range g {
			if m, ok := r.model(e.Model); ok && selected(m.Provider) {
				members = append(members, e.Model+" "+e.Effort)
			}
		}
		if len(members) > 0 {
			groups = append(groups, strings.Join(members, " ≈ "))
		}
	}
	d.Ladder = strings.Join(groups, " < ")

	d.Evidence.Note = r.Evidence.Note
	var cols []string
	for _, c := range r.Evidence.Columns {
		if m, ok := r.model(c); ok && selected(m.Provider) {
			cols = append(cols, c)
		}
	}
	var tb strings.Builder
	tb.WriteString("| Effort |")
	for _, c := range cols {
		m, _ := r.model(c)
		tb.WriteString(" " + m.Name + " |")
	}
	tb.WriteString("\n|---|" + strings.Repeat("---|", len(cols)) + "\n")
	for _, row := range r.Evidence.Rows {
		tb.WriteString("| " + row.Effort + " |")
		for _, c := range cols {
			tb.WriteString(" " + row.Values[c] + " |")
		}
		tb.WriteString("\n")
	}
	d.Evidence.Table = strings.TrimRight(tb.String(), "\n")

	favored, note := favoredAgent(p)
	d.OrderNote = note
	for _, s := range r.Stages {
		rt := s.Codex
		switch {
		case d.Both:
			rt = s.Both
		case d.Claude:
			rt = s.Claude
		}
		if d.Both && s.Reorder && favored != "" && routeAgent(r, rt.Preferred) != favored {
			rt.Preferred, rt.Alternative = rt.Alternative, rt.Preferred
		}
		d.Stages = append(d.Stages, stageView{s.Stage, rt.Preferred, rt.Alternative, rt.Fallback, rt.Escalate})
	}

	for _, a := range p.Agents() {
		pl := p.PlanFor(a)
		av := allowanceView{Client: profile.ClientName(a), Plan: pl.Label, Guidance: allowanceGuidance(pl.Allowance)}
		switch {
		case p.Get(a).Credits:
			av.Credits = "You declared paid extra usage as already enabled: once the included allowance is exhausted, you may continue on the same capable route with that paid usage when the task justifies the cost; say so when you do. Never purchase, enable or raise paid usage yourself."
		case d.Both:
			av.Credits = "No paid extra usage declared: once the included allowance is exhausted, move to the other selected provider's capable route or a listed allowance fallback, or stop and tell the user."
		default:
			av.Credits = "No paid extra usage declared: once the included allowance is exhausted, use a listed allowance fallback where the work fits, or stop and tell the user."
		}
		d.Allowance = append(d.Allowance, av)
	}
	return d
}

// routeAgent identifies which agent a route string starts with.
func routeAgent(r *Roster, s string) profile.Agent {
	for _, m := range r.Models {
		if strings.HasPrefix(s, m.Short) {
			return providerAgent(m.Provider)
		}
	}
	return ""
}

// favoredAgent decides which provider's route leads comparable pairs. Only a
// strictly larger known allowance changes the reviewed default order.
func favoredAgent(p profile.Profile) (profile.Agent, string) {
	if !p.Has(profile.Claude) || !p.Has(profile.Codex) {
		return "", ""
	}
	c, x := p.PlanFor(profile.Claude).Allowance, p.PlanFor(profile.Codex).Allowance
	switch {
	case c == profile.AllowanceUnknown || x == profile.AllowanceUnknown:
		return "", "At least one declared plan has an unknown allowance, so comparable routes keep the reviewed default order and the unknown allowance is treated as limited."
	case c > x:
		return profile.Claude, "Your declared Claude plan has the larger allowance, so where the Preferred and Other-provider alternative routes are comparable, the Claude route is listed first."
	case x > c:
		return profile.Codex, "Your declared ChatGPT plan has the larger allowance, so where the Preferred and Other-provider alternative routes are comparable, the Codex route is listed first."
	}
	return "", "Your declared allowances are comparable, so routes keep the reviewed default order."
}

func allowanceGuidance(a profile.Allowance) string {
	switch a {
	case profile.AllowanceLimited:
		return "Treat this allowance as limited: take the lowest capable route, reserve Deep routes for work whose shape requires them, and avoid parallel owners on this provider."
	case profile.AllowanceStandard:
		return "Treat this allowance as moderate: take the lowest capable route; Deep routes are fine when the shape requires them."
	case profile.AllowanceHigh:
		return "Treat this allowance as generous, but still take the lowest capable route; a large allowance never makes an expensive route the default."
	}
	return "agentfile does not know this plan's allowance, so treat it as limited: take the lowest capable route, reserve Deep routes for work whose shape requires them, and avoid parallel owners on this provider."
}
