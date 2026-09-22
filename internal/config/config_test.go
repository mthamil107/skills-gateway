package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/auth"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sgw.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const staticCfg = `
listen: "0.0.0.0:9090"
database: ${SGW_DB}
policy: policy.yaml
max_upload_bytes: 1234
auth:
  mode: static
  static:
    - token: ${SGW_TOKEN}
      subject: alice
      teams: [platform, payments]
      roles: [admin]
      agent_type: claude-code
    - token: literal-token-0123456789
      subject: bob
`

func TestLoadExpandsEnvironment(t *testing.T) {
	t.Setenv("SGW_DB", "/var/lib/sgw/data.db")
	t.Setenv("SGW_TOKEN", "secret-token-from-env-value")
	c, err := Load(write(t, staticCfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != "0.0.0.0:9090" || c.Database != "/var/lib/sgw/data.db" || c.Policy != "policy.yaml" || c.MaxUploadBytes != 1234 {
		t.Errorf("config = %+v", c)
	}
	if c.Auth.Mode != "static" || len(c.Auth.Static) != 2 || c.Auth.Static[0].Token != "secret-token-from-env-value" {
		t.Errorf("auth = %+v", c.Auth)
	}
	want := map[string]auth.Principal{
		"secret-token-from-env-value": {Subject: "alice", Teams: []string{"platform", "payments"}, Roles: []string{"admin"}, AgentType: "claude-code"},
		"literal-token-0123456789":    {Subject: "bob"},
	}
	if got := c.Auth.StaticTable(); !reflect.DeepEqual(got, want) {
		t.Errorf("StaticTable = %+v", got)
	}
}

func TestLoadMissingEnvironmentVariable(t *testing.T) {
	os.Unsetenv("SGW_DEFINITELY_UNSET_1")
	os.Unsetenv("SGW_DEFINITELY_UNSET_2")
	t.Setenv("SGW_DB", "x.db")
	_, err := Load(write(t, "database: ${SGW_DB}\npolicy: p.yaml\nauth:\n  mode: static\n  static:\n    - token: ${SGW_DEFINITELY_UNSET_1}\n      subject: ${SGW_DEFINITELY_UNSET_2}\n"))
	if err == nil {
		t.Fatal("expected error for unset variables")
	}
	if !strings.Contains(err.Error(), "SGW_DEFINITELY_UNSET_1") || !strings.Contains(err.Error(), "SGW_DEFINITELY_UNSET_2") {
		t.Errorf("error should name every missing variable: %v", err)
	}
	// An empty-but-set variable is not "missing"; it then fails validation.
	t.Setenv("SGW_EMPTY", "")
	_, err = Load(write(t, "policy: p.yaml\nauth:\n  mode: static\n  static:\n    - token: ${SGW_EMPTY}\n      subject: a\n"))
	if err == nil || strings.Contains(err.Error(), "unset environment") {
		t.Errorf("empty variable: %v", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(write(t, "policy: p.yaml\nauth:\n  mode: none\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" || c.Database != "skills-gateway.db" || c.MaxUploadBytes != 10<<20 {
		t.Errorf("defaults = %+v", c)
	}
	// OIDC settings load without any network access.
	c, err = Load(write(t, "policy: p.yaml\nauth:\n  mode: oidc\n  oidc:\n    issuer: https://id.example/realms/x\n    audience: sgw\n    roles_claim: realm_access.roles\n"))
	if err != nil || c.Auth.OIDC.Issuer != "https://id.example/realms/x" || c.Auth.OIDC.RolesClaim != "realm_access.roles" {
		t.Errorf("oidc config: %+v %v", c, err)
	}
}

func TestLoadValidation(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"unknown top-level field": {"policy: p.yaml\nlisten_addr: x\nauth:\n  mode: none\n", "listen_addr"},
		"unknown auth field":      {"policy: p.yaml\nauth:\n  mode: none\n  tokens: []\n", "tokens"},
		"unknown static field":    {"policy: p.yaml\nauth:\n  mode: static\n  static:\n    - token: abcdefghijklmnopq\n      subject: a\n      team: x\n", "team"},
		"missing policy":          {"auth:\n  mode: none\n", "policy"},
		"bad mode":                {"policy: p.yaml\nauth:\n  mode: basic\n", "auth.mode"},
		"missing mode":            {"policy: p.yaml\n", "auth.mode"},
		"static without tokens":   {"policy: p.yaml\nauth:\n  mode: static\n", "no tokens"},
		"short token":             {"policy: p.yaml\nauth:\n  mode: static\n  static:\n    - token: short\n      subject: a\n", "at least 16"},
		"15-char token":           {"policy: p.yaml\nauth:\n  mode: static\n  static:\n    - token: abcdefghijklmno\n      subject: a\n", "at least 16"},
		"missing subject":         {"policy: p.yaml\nauth:\n  mode: static\n  static:\n    - token: abcdefghijklmnopq\n", "subject"},
		"second token bad":        {"policy: p.yaml\nauth:\n  mode: static\n  static:\n    - token: abcdefghijklmnopq\n      subject: a\n    - token: x\n      subject: b\n", "auth.static[1]"},
		"not yaml":                {"{{{", "config"},
		"wrong type":              {"policy: p.yaml\nmax_upload_bytes: lots\nauth:\n  mode: none\n", "config"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := Load(write(t, c.body))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
	// A 16-character token is the minimum.
	if _, err := Load(write(t, "policy: p.yaml\nauth:\n  mode: static\n  static:\n    - token: abcdefghijklmnop\n      subject: a\n")); err != nil {
		t.Errorf("16-char token rejected: %v", err)
	}
}

// The shipped example must load as documented in the README quick start.
func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("SGW_ADMIN_TOKEN", "admin-token-0123456789")
	t.Setenv("SGW_DEV_TOKEN", "dev-token-0123456789ab")
	c, err := Load("../../examples/sgw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.Mode != "static" || len(c.Auth.Static) != 2 || c.Auth.Static[0].Token != "admin-token-0123456789" {
		t.Fatalf("config = %+v", c.Auth)
	}
}
