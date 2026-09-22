// Package policy decides whether a principal may perform an action on a
// skill. Evaluation is default-deny: any matching deny rule wins, otherwise
// any matching allow rule permits, otherwise the request is denied.
//
// The input shape (principal, action, resource) is deliberately the same
// document an OPA or Cedar backend would receive, so the built-in engine
// can be swapped for either behind the Authorizer interface.
package policy

import (
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"

	"github.com/mthamil107/skills-gateway/internal/auth"
)

// Action is an operation on a skill.
type Action string

const (
	Fetch     Action = "fetch"
	Publish   Action = "publish"
	Deprecate Action = "deprecate"
	Admin     Action = "admin"
)

// Resource identifies what is being accessed. Version is empty when the
// action is not tied to one version (listing, admin).
type Resource struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
}

// Decision is the result of an authorization check.
type Decision struct {
	Allow  bool   `json:"allow"`
	RuleID string `json:"rule_id,omitempty"` // matched rule, empty for default deny
}

// Authorizer is implemented by the built-in engine and, later, OPA/Cedar.
type Authorizer interface {
	Decide(ctx context.Context, p auth.Principal, a Action, r Resource) Decision
}

// Subject selects principals. Within a subject, all set fields must match
// (AND); within a list, any element may match (OR).
type Subject struct {
	Any        bool     `yaml:"any"`
	Users      []string `yaml:"users"`
	Teams      []string `yaml:"teams"`
	Roles      []string `yaml:"roles"`
	AgentTypes []string `yaml:"agent_types"`
}

// ResourceMatch selects skills. Namespace and Name are path.Match globs;
// Version is a constraint list like ">=1.0.0 <2.0.0" (empty matches all).
type ResourceMatch struct {
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`
	Version   string `yaml:"version"`
}

// Rule is one policy statement.
type Rule struct {
	ID       string        `yaml:"id"`
	Effect   string        `yaml:"effect"` // allow | deny
	Subject  Subject       `yaml:"subject"`
	Actions  []Action      `yaml:"actions"`
	Resource ResourceMatch `yaml:"resource"`
}

// Policy is the full rule set.
type Policy struct {
	Version int    `yaml:"version"`
	Rules   []Rule `yaml:"rules"`
}

// Load reads and validates a policy YAML file.
func Load(file string) (*Policy, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse validates a policy document.
func Parse(data []byte) (*Policy, error) {
	var p Policy
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	if p.Version != 1 {
		return nil, fmt.Errorf("policy: unsupported version %d (want 1)", p.Version)
	}
	ids := map[string]bool{}
	for i, r := range p.Rules {
		if r.ID == "" {
			return nil, fmt.Errorf("policy: rule %d has no id", i)
		}
		if ids[r.ID] {
			return nil, fmt.Errorf("policy: duplicate rule id %q", r.ID)
		}
		ids[r.ID] = true
		if r.Effect != "allow" && r.Effect != "deny" {
			return nil, fmt.Errorf("policy: rule %q effect must be allow or deny", r.ID)
		}
		if len(r.Actions) == 0 {
			return nil, fmt.Errorf("policy: rule %q has no actions", r.ID)
		}
		for _, a := range r.Actions {
			switch a {
			case Fetch, Publish, Deprecate, Admin:
			default:
				return nil, fmt.Errorf("policy: rule %q has unknown action %q", r.ID, a)
			}
		}
		s := r.Subject
		if !s.Any && len(s.Users)+len(s.Teams)+len(s.Roles)+len(s.AgentTypes) == 0 {
			return nil, fmt.Errorf("policy: rule %q subject is empty (use any: true to match everyone)", r.ID)
		}
		for _, g := range []string{r.Resource.Namespace, r.Resource.Name} {
			if _, err := path.Match(g, ""); err != nil {
				return nil, fmt.Errorf("policy: rule %q bad glob %q", r.ID, g)
			}
		}
		if _, err := parseConstraints(r.Resource.Version); err != nil {
			return nil, fmt.Errorf("policy: rule %q: %w", r.ID, err)
		}
	}
	return &p, nil
}

// Decide implements Authorizer.
func (p *Policy) Decide(_ context.Context, pr auth.Principal, a Action, r Resource) Decision {
	var allow *Rule
	for i := range p.Rules {
		rule := &p.Rules[i]
		if !slices.Contains(rule.Actions, a) || !rule.Subject.matches(pr) || !rule.Resource.matches(r) {
			continue
		}
		if rule.Effect == "deny" {
			return Decision{Allow: false, RuleID: rule.ID}
		}
		if allow == nil {
			allow = rule
		}
	}
	if allow != nil {
		return Decision{Allow: true, RuleID: allow.ID}
	}
	return Decision{}
}

func (s Subject) matches(p auth.Principal) bool {
	if s.Any {
		return true
	}
	if len(s.Users) > 0 && !slices.Contains(s.Users, p.Subject) {
		return false
	}
	if len(s.Teams) > 0 && !overlaps(s.Teams, p.Teams) {
		return false
	}
	if len(s.Roles) > 0 && !overlaps(s.Roles, p.Roles) {
		return false
	}
	if len(s.AgentTypes) > 0 && !slices.Contains(s.AgentTypes, p.AgentType) {
		return false
	}
	return true
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

func (m ResourceMatch) matches(r Resource) bool {
	if !glob(m.Namespace, r.Namespace) || !glob(m.Name, r.Name) {
		return false
	}
	if m.Version == "" {
		return true
	}
	if r.Version == "" {
		// A version-constrained rule cannot grant (or deny) a request that
		// is not tied to a version; listing filters per item instead.
		return false
	}
	cs, _ := parseConstraints(m.Version)
	return satisfies(r.Version, cs)
}

func glob(pattern, s string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	ok, _ := path.Match(pattern, s)
	return ok
}

type constraint struct{ op, ver string }

func parseConstraints(expr string) ([]constraint, error) {
	var out []constraint
	for _, f := range strings.Fields(expr) {
		op := ""
		for _, o := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(f, o) {
				op = o
				break
			}
		}
		v := strings.TrimPrefix(f, op)
		if op == "" {
			op = "="
		}
		if !semver.IsValid("v" + v) {
			return nil, fmt.Errorf("invalid version constraint %q", f)
		}
		out = append(out, constraint{op, "v" + v})
	}
	return out, nil
}

func satisfies(version string, cs []constraint) bool {
	v := "v" + version
	if !semver.IsValid(v) {
		return false
	}
	for _, c := range cs {
		cmp := semver.Compare(v, c.ver)
		ok := map[string]bool{">=": cmp >= 0, "<=": cmp <= 0, ">": cmp > 0, "<": cmp < 0, "=": cmp == 0}[c.op]
		if !ok {
			return false
		}
	}
	return true
}
