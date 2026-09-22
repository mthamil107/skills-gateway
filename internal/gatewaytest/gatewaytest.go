// Package gatewaytest starts a complete, in-process Skills Gateway for
// end-to-end tests: SQLite in a temp dir, a YAML policy, static bearer
// tokens, the REST API and the MCP endpoint behind one httptest server.
package gatewaytest

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/auth"
	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/client"
	"github.com/mthamil107/skills-gateway/internal/mcp"
	"github.com/mthamil107/skills-gateway/internal/policy"
	"github.com/mthamil107/skills-gateway/internal/registry"
	"github.com/mthamil107/skills-gateway/internal/server"
	"github.com/mthamil107/skills-gateway/internal/store/sqlite"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

// Tokens used by the default fixture. All are at least 16 characters so
// the same table would pass config validation.
const (
	AdminToken    = "admin-token-0123456789"    // alice: role admin, everything everywhere
	PlatformToken = "platform-token-0123456789" // bob: team platform, publishes to platform/
	PaymentsToken = "payments-token-0123456789" // carol: team payments, publishes to payments/
	ReaderToken   = "reader-token-0123456789"   // dave: no teams, may only fetch platform/
	BotToken      = "bot-token-0123456789"      // robot: agent_type bot, denied platform/secret-*
)

// Policy is the default fixture policy (see the token comments above).
const Policy = `
version: 1
rules:
  - id: admin-all
    effect: allow
    subject: {roles: [admin]}
    actions: [fetch, publish, deprecate, admin]
  - id: platform-team
    effect: allow
    subject: {teams: [platform]}
    actions: [fetch, publish, deprecate]
    resource: {namespace: platform}
  - id: payments-team
    effect: allow
    subject: {teams: [payments]}
    actions: [fetch, publish, deprecate]
    resource: {namespace: payments}
  - id: everyone-reads-platform
    effect: allow
    subject: {any: true}
    actions: [fetch]
    resource: {namespace: platform}
  - id: no-bots-on-secrets
    effect: deny
    subject: {agent_types: [bot]}
    actions: [fetch]
    resource: {namespace: platform, name: "secret-*"}
`

// Principals maps the fixture tokens to principals.
var Principals = map[string]auth.Principal{
	AdminToken:    {Subject: "alice", Roles: []string{"admin"}},
	PlatformToken: {Subject: "bob", Teams: []string{"platform"}},
	PaymentsToken: {Subject: "carol", Teams: []string{"payments"}},
	ReaderToken:   {Subject: "dave"},
	BotToken:      {Subject: "robot", Teams: []string{"platform"}, AgentType: "bot"},
}

// Options tune the fixture.
type Options struct {
	Policy  string // YAML; defaults to Policy
	MaxBody int64  // upload cap; defaults to server default
	Tokens  map[string]auth.Principal
	Version string // reported by MCP serverInfo
}

// Gateway is a running fixture.
type Gateway struct {
	Server *httptest.Server
	URL    string
	Reg    *registry.Service
	DBPath string
}

// New starts a gateway and registers its shutdown with t.
func New(t testing.TB, opt Options) *Gateway {
	t.Helper()
	if opt.Policy == "" {
		opt.Policy = Policy
	}
	if opt.Tokens == nil {
		opt.Tokens = Principals
	}
	if opt.Version == "" {
		opt.Version = "test"
	}
	pol, err := policy.Parse([]byte(opt.Policy))
	if err != nil {
		t.Fatalf("gatewaytest: policy: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "sgw.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("gatewaytest: sqlite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reg := registry.New(st, pol)
	srv := &server.Server{
		Reg:         reg,
		Auth:        auth.NewStatic(opt.Tokens),
		Translators: translate.Default(),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBody:     opt.MaxBody,
		MCP:         mcp.NewHandler(reg, opt.Version),
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &Gateway{Server: hs, URL: hs.URL, Reg: reg, DBPath: dbPath}
}

// Client returns a REST client authenticated with token.
func (g *Gateway) Client(token string) *client.Client {
	return &client.Client{BaseURL: g.URL, Token: token, HTTP: g.Server.Client()}
}

// SkillMD renders a minimal valid SKILL.md.
func SkillMD(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n\nInstructions.\n"
}

// Files builds a bundle file list; SKILL.md is added when absent.
func Files(name string, extra map[string]string) []bundle.File {
	var out []bundle.File
	if _, ok := extra["SKILL.md"]; !ok {
		out = append(out, bundle.File{Path: "SKILL.md", Content: []byte(SkillMD(name, "Skill "+name))})
	}
	for p, c := range extra {
		out = append(out, bundle.File{Path: p, Content: []byte(c)})
	}
	return bundle.FromFiles(out).Files
}

// Publish uploads a skill with token and fails the test on error.
func (g *Gateway) Publish(t testing.TB, token, ns, name, version string, files []bundle.File) {
	t.Helper()
	if _, err := g.Client(token).Publish(context.Background(), ns, name, version, files); err != nil {
		t.Fatalf("gatewaytest: publish %s/%s@%s: %v", ns, name, version, err)
	}
}

// Do performs one raw HTTP request against the gateway.
func (g *Gateway) Do(t testing.TB, method, path, token string, body io.Reader, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, g.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := g.Server.Client().Do(req)
	if err != nil {
		t.Fatalf("gatewaytest: %s %s: %v", method, path, err)
	}
	return resp
}
