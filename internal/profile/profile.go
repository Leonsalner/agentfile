// Package profile describes what the user declared: which agents to install
// for, their subscription plans, and whether paid extra usage is enabled.
// A profile is a routing hint only; it never proves that a model is enabled.
package profile

import (
	"fmt"
	"strings"
)

type Agent string

const (
	Claude Agent = "claude"
	Codex  Agent = "codex"
)

// Allowance is the conservative allowance class a declared plan maps to.
type Allowance int

const (
	AllowanceUnknown Allowance = iota
	AllowanceLimited
	AllowanceStandard
	AllowanceHigh
)

// Plan is one selectable subscription tier for an agent.
type Plan struct {
	ID        string
	Label     string
	Allowance Allowance
}

// Plans lists the selectable tiers per agent, in menu order. "unknown" is
// always last and always allowed.
var Plans = map[Agent][]Plan{
	Claude: {
		{"pro", "Claude Pro", AllowanceLimited},
		{"max-5x", "Claude Max 5x", AllowanceStandard},
		{"max-20x", "Claude Max 20x", AllowanceHigh},
		{"team-enterprise", "Claude Team or Enterprise", AllowanceUnknown},
		{"unknown", "Other or not sure", AllowanceUnknown},
	},
	Codex: {
		{"plus", "ChatGPT Plus", AllowanceLimited},
		{"pro", "ChatGPT Pro", AllowanceHigh},
		{"business-enterprise", "ChatGPT Business, Enterprise or Edu", AllowanceUnknown},
		{"unknown", "Other or not sure", AllowanceUnknown},
	},
}

// AgentProfile is the declaration for one selected agent.
type AgentProfile struct {
	Plan    string // Plan.ID
	Credits bool   // paid extra usage already enabled by the user
}

// Profile is the full declaration. A nil entry means the agent is not selected.
type Profile struct {
	Claude *AgentProfile
	Codex  *AgentProfile
}

func (p Profile) Has(a Agent) bool {
	if a == Claude {
		return p.Claude != nil
	}
	return p.Codex != nil
}

func (p Profile) Get(a Agent) *AgentProfile {
	if a == Claude {
		return p.Claude
	}
	return p.Codex
}

// Agents returns the selected agents in fixed order.
func (p Profile) Agents() []Agent {
	var out []Agent
	for _, a := range []Agent{Claude, Codex} {
		if p.Has(a) {
			out = append(out, a)
		}
	}
	return out
}

func LookupPlan(a Agent, id string) (Plan, bool) {
	for _, pl := range Plans[a] {
		if pl.ID == id {
			return pl, true
		}
	}
	return Plan{}, false
}

// PlanFor returns the declared plan, mapping anything unrecognized to the
// conservative "unknown" plan.
func (p Profile) PlanFor(a Agent) Plan {
	ap := p.Get(a)
	if ap != nil {
		if pl, ok := LookupPlan(a, ap.Plan); ok {
			return pl
		}
	}
	pl, _ := LookupPlan(a, "unknown")
	return pl
}

func (p Profile) Validate() error {
	if p.Claude == nil && p.Codex == nil {
		return fmt.Errorf("select at least one agent")
	}
	for _, a := range p.Agents() {
		if _, ok := LookupPlan(a, p.Get(a).Plan); !ok {
			return fmt.Errorf("%s: unknown plan %q", a, p.Get(a).Plan)
		}
	}
	return nil
}

// ClientName is the user-facing product name for an agent.
func ClientName(a Agent) string {
	if a == Claude {
		return "Claude Code"
	}
	return "Codex"
}

// Summary is a one-line human description, used in generated headers and
// backup manifests.
func (p Profile) Summary() string {
	var parts []string
	for _, a := range p.Agents() {
		credits := "no extra usage"
		if p.Get(a).Credits {
			credits = "extra usage enabled"
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", ClientName(a), p.PlanFor(a).Label, credits))
	}
	return strings.Join(parts, " + ")
}
