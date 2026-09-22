package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/auth"
)

func mustParse(t *testing.T, doc string) *Policy {
	t.Helper()
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, doc)
	}
	return p
}

func TestParseRejectsBadDocuments(t *testing.T) {
	base := func(rule string) string { return "version: 1\nrules:\n" + rule }
	cases := map[string]string{
		"unknown top-level field": "version: 1\nrulez: []\n",
		"unknown rule field":      base("  - id: a\n    effect: allow\n    color: red\n    subject: {any: true}\n    actions: [fetch]\n"),
		"unknown subject field":   base("  - id: a\n    effect: allow\n    subject: {everyone: true}\n    actions: [fetch]\n"),
		"missing version":         "rules: []\n",
		"wrong version":           "version: 2\nrules: []\n",
		"bad effect":              base("  - id: a\n    effect: permit\n    subject: {any: true}\n    actions: [fetch]\n"),
		"missing effect":          base("  - id: a\n    subject: {any: true}\n    actions: [fetch]\n"),
		"empty subject":           base("  - id: a\n    effect: allow\n    actions: [fetch]\n"),
		"subject any false":       base("  - id: a\n    effect: allow\n    subject: {any: false}\n    actions: [fetch]\n"),
		"no actions":              base("  - id: a\n    effect: allow\n    subject: {any: true}\n"),
		"unknown action":          base("  - id: a\n    effect: allow\n    subject: {any: true}\n    actions: [read]\n"),
		"missing id":              base("  - effect: allow\n    subject: {any: true}\n    actions: [fetch]\n"),
		"duplicate id":            base("  - id: a\n    effect: allow\n    subject: {any: true}\n    actions: [fetch]\n  - id: a\n    effect: deny\n    subject: {any: true}\n    actions: [fetch]\n"),
		"bad version constraint":  base("  - id: a\n    effect: allow\n    subject: {any: true}\n    actions: [fetch]\n    resource: {version: '^1.0'}\n"),
		"bad version constraint2": base("  - id: a\n    effect: allow\n    subject: {any: true}\n    actions: [fetch]\n    resource: {version: '>=banana'}\n"),
		"bad version with v":      base("  - id: a\n    effect: allow\n    subject: {any: true}\n    actions: [fetch]\n    resource: {version: '>=v1.0.0'}\n"),
		"bad glob":                base("  - id: a\n    effect: allow\n    subject: {any: true}\n    actions: [fetch]\n    resource: {name: '[a'}\n"),
		"not yaml":                "{{{",
	}
	for label, doc := range cases {
		t.Run(label, func(t *testing.T) {
			if p, err := Parse([]byte(doc)); err == nil {
				t.Fatalf("expected error, got %+v", p)
			}
		})
	}
}

func TestParseAcceptsValidDocument(t *testing.T) {
	p := mustParse(t, `
version: 1
rules:
  - id: all
    effect: allow
    subject: {any: true}
    actions: [fetch, publish, deprecate, admin]
    resource: {namespace: "*", name: "*", version: ">=1.0.0 <2.0.0"}
  - id: users
    effect: deny
    subject:
      users: [alice]
      teams: [a, b]
      roles: [r]
      agent_types: [bot]
    actions: [fetch]
`)
	if len(p.Rules) != 2 || p.Rules[0].ID != "all" || p.Rules[1].Effect != "deny" {
		t.Errorf("unexpected policy: %+v", p)
	}
}

var (
	alice = auth.Principal{Subject: "alice", Teams: []string{"payments", "platform"}, Roles: []string{"dev"}}
	bob   = auth.Principal{Subject: "bob", Teams: []string{"marketing"}}
	root  = auth.Principal{Subject: "root", Roles: []string{"admin"}}
	bot   = auth.Principal{Subject: "svc", Teams: []string{"payments"}, AgentType: "bot"}
	ctx   = context.Background()
)

func decide(p *Policy, pr auth.Principal, a Action, ns, name, ver string) Decision {
	return p.Decide(ctx, pr, a, Resource{Namespace: ns, Name: name, Version: ver})
}

func TestDefaultDeny(t *testing.T) {
	p := mustParse(t, "version: 1\nrules: []\n")
	if d := decide(p, root, Fetch, "x", "y", "1.0.0"); d.Allow || d.RuleID != "" {
		t.Errorf("empty policy allowed: %+v", d)
	}
	p = mustParse(t, "version: 1\n")
	if d := decide(p, root, Admin, "*", "*", ""); d.Allow {
		t.Errorf("nil rules allowed: %+v", d)
	}
	// A rule for a different action does not grant this one.
	p = mustParse(t, "version: 1\nrules:\n  - id: a\n    effect: allow\n    subject: {any: true}\n    actions: [fetch]\n")
	if decide(p, alice, Publish, "x", "y", "1.0.0").Allow {
		t.Error("fetch rule granted publish")
	}
	if !decide(p, alice, Fetch, "x", "y", "").Allow {
		t.Error("any-subject fetch rule with no resource should match everything")
	}
}

func TestDenyBeatsAllowRegardlessOfOrder(t *testing.T) {
	allowFirst := `
version: 1
rules:
  - id: allow-all
    effect: allow
    subject: {any: true}
    actions: [fetch]
  - id: deny-bob
    effect: deny
    subject: {users: [bob]}
    actions: [fetch]
`
	denyFirst := `
version: 1
rules:
  - id: deny-bob
    effect: deny
    subject: {users: [bob]}
    actions: [fetch]
  - id: allow-all
    effect: allow
    subject: {any: true}
    actions: [fetch]
`
	for label, doc := range map[string]string{"allow first": allowFirst, "deny first": denyFirst} {
		p := mustParse(t, doc)
		if d := decide(p, bob, Fetch, "x", "y", "1.0.0"); d.Allow || d.RuleID != "deny-bob" {
			t.Errorf("%s: bob = %+v, want deny by deny-bob", label, d)
		}
		if d := decide(p, alice, Fetch, "x", "y", "1.0.0"); !d.Allow || d.RuleID != "allow-all" {
			t.Errorf("%s: alice = %+v, want allow by allow-all", label, d)
		}
	}
	// A deny for a different action must not block.
	p := mustParse(t, `
version: 1
rules:
  - id: deny-publish
    effect: deny
    subject: {users: [bob]}
    actions: [publish]
  - id: allow-fetch
    effect: allow
    subject: {any: true}
    actions: [fetch]
`)
	if !decide(p, bob, Fetch, "x", "y", "").Allow {
		t.Error("deny on publish blocked fetch")
	}
}

func TestSubjectMatching(t *testing.T) {
	p := mustParse(t, `
version: 1
rules:
  - id: by-team
    effect: allow
    subject: {teams: [payments]}
    actions: [fetch]
    resource: {namespace: payments}
  - id: by-role
    effect: allow
    subject: {roles: [admin]}
    actions: [admin]
  - id: by-user
    effect: allow
    subject: {users: [bob]}
    actions: [fetch]
    resource: {namespace: marketing}
  - id: by-agent
    effect: deny
    subject: {agent_types: [bot]}
    actions: [fetch]
  - id: combined
    effect: allow
    subject: {teams: [platform], roles: [dev]}
    actions: [publish]
    resource: {namespace: platform}
`)
	tests := []struct {
		label string
		pr    auth.Principal
		a     Action
		ns    string
		allow bool
		rule  string
	}{
		{"team match", alice, Fetch, "payments", true, "by-team"},
		{"team miss", bob, Fetch, "payments", false, ""},
		{"role match", root, Admin, "", true, "by-role"},
		{"role miss", alice, Admin, "", false, ""},
		{"user match", bob, Fetch, "marketing", true, "by-user"},
		{"user miss", alice, Fetch, "marketing", false, ""},
		{"agent deny overrides team", bot, Fetch, "payments", false, "by-agent"},
		{"combined AND both", alice, Publish, "platform", true, "combined"},
		{"combined AND team only", auth.Principal{Subject: "x", Teams: []string{"platform"}}, Publish, "platform", false, ""},
		{"combined AND role only", auth.Principal{Subject: "x", Roles: []string{"dev"}}, Publish, "platform", false, ""},
	}
	for _, tc := range tests {
		d := decide(p, tc.pr, tc.a, tc.ns, "skill", "1.0.0")
		if d.Allow != tc.allow || d.RuleID != tc.rule {
			t.Errorf("%s: got %+v, want allow=%v rule=%q", tc.label, d, tc.allow, tc.rule)
		}
	}
}

func TestResourceGlobs(t *testing.T) {
	p := mustParse(t, `
version: 1
rules:
  - id: star
    effect: allow
    subject: {any: true}
    actions: [fetch]
    resource: {namespace: "team-*", name: "*-review"}
  - id: exact
    effect: allow
    subject: {any: true}
    actions: [publish]
    resource: {namespace: platform, name: exact}
  - id: charclass
    effect: allow
    subject: {any: true}
    actions: [deprecate]
    resource: {name: "v[0-9]"}
`)
	tests := []struct {
		a        Action
		ns, name string
		allow    bool
	}{
		{Fetch, "team-a", "code-review", true},
		{Fetch, "team-", "x-review", true},
		{Fetch, "team", "code-review", false},
		{Fetch, "team-a", "review", false},
		{Fetch, "team-a", "code-review-2", false},
		{Publish, "platform", "exact", true},
		{Publish, "platform", "exactly", false},
		{Publish, "platform2", "exact", false},
		{Deprecate, "any", "v1", true},
		{Deprecate, "any", "v10", false},
		{Deprecate, "any", "vx", false},
	}
	for _, tc := range tests {
		if got := decide(p, alice, tc.a, tc.ns, tc.name, "1.0.0").Allow; got != tc.allow {
			t.Errorf("%s %s/%s: allow=%v, want %v", tc.a, tc.ns, tc.name, got, tc.allow)
		}
	}
	// path.Match globs do not cross "/" and are not substring matches.
	if decide(p, alice, Fetch, "team-a/b", "code-review", "1.0.0").Allow {
		t.Error("glob crossed a slash")
	}
}

func TestVersionConstraints(t *testing.T) {
	p := mustParse(t, `
version: 1
rules:
  - id: range
    effect: allow
    subject: {any: true}
    actions: [fetch]
    resource: {name: ranged, version: ">=1.0.0 <2.0.0"}
  - id: exact
    effect: allow
    subject: {any: true}
    actions: [fetch]
    resource: {name: pinned, version: "1.2.3"}
  - id: gt
    effect: deny
    subject: {any: true}
    actions: [fetch]
    resource: {name: old, version: "<=0.9.9"}
  - id: old-allow
    effect: allow
    subject: {any: true}
    actions: [fetch]
    resource: {name: old}
`)
	tests := []struct {
		name, ver string
		allow     bool
	}{
		{"ranged", "1.0.0", true},
		{"ranged", "1.9.9", true},
		{"ranged", "2.0.0", false},
		{"ranged", "0.9.9", false},
		{"ranged", "1.5.0-rc.1", true},
		{"ranged", "2.0.0-rc.1", true}, // semver: prerelease sorts before the release
		{"ranged", "not-a-version", false},
		{"pinned", "1.2.3", true},
		{"pinned", "1.2.4", false},
		{"old", "0.9.9", false},
		{"old", "0.1.0", false},
		{"old", "1.0.0", true},
	}
	for _, tc := range tests {
		if got := decide(p, alice, Fetch, "ns", tc.name, tc.ver).Allow; got != tc.allow {
			t.Errorf("%s@%s: allow=%v, want %v", tc.name, tc.ver, got, tc.allow)
		}
	}
}

func TestVersionConstrainedRuleDoesNotMatchVersionlessResource(t *testing.T) {
	p := mustParse(t, `
version: 1
rules:
  - id: range
    effect: allow
    subject: {any: true}
    actions: [fetch, publish]
    resource: {version: ">=1.0.0"}
  - id: deny-range
    effect: deny
    subject: {users: [bob]}
    actions: [fetch]
    resource: {version: ">=1.0.0"}
  - id: unversioned
    effect: allow
    subject: {users: [bob]}
    actions: [fetch]
`)
	if d := decide(p, alice, Fetch, "ns", "x", ""); d.Allow {
		t.Errorf("version-constrained allow matched a version-less resource: %+v", d)
	}
	if d := decide(p, alice, Fetch, "ns", "x", "1.0.0"); !d.Allow || d.RuleID != "range" {
		t.Errorf("versioned resource: %+v", d)
	}
	// The version-constrained deny does not fire on a version-less request
	// either; the unversioned allow does.
	if d := decide(p, bob, Fetch, "ns", "x", ""); !d.Allow || d.RuleID != "unversioned" {
		t.Errorf("bob version-less: %+v", d)
	}
	if d := decide(p, bob, Fetch, "ns", "x", "1.0.0"); d.Allow || d.RuleID != "deny-range" {
		t.Errorf("bob versioned: %+v", d)
	}
}

func TestParseConstraints(t *testing.T) {
	cs, err := parseConstraints(">=1.0.0 <2.0.0 =3.0.0 4.0.0 >0.1.0 <=9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, c := range cs {
		ops = append(ops, c.op+c.ver)
	}
	if got := strings.Join(ops, " "); got != ">=v1.0.0 <v2.0.0 =v3.0.0 =v4.0.0 >v0.1.0 <=v9.9.9" {
		t.Errorf("parsed %q", got)
	}
	if cs, err := parseConstraints(""); err != nil || len(cs) != 0 {
		t.Errorf("empty constraint: %v %v", cs, err)
	}
	for _, bad := range []string{"~1.0.0", ">=1.0.0 foo", "1.0.0.0", "=", ">="} {
		if _, err := parseConstraints(bad); err == nil {
			t.Errorf("parseConstraints(%q) accepted", bad)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir() + "/nope.yaml"); err == nil {
		t.Error("expected error for missing file")
	}
}
