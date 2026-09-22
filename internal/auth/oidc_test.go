package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/mthamil107/skills-gateway/internal/auth"
	"github.com/mthamil107/skills-gateway/internal/auth/authtest"
)

const aud = "skills-gateway"

func newOIDC(t *testing.T, iss *authtest.Issuer, cfg auth.OIDCConfig) *auth.OIDC {
	t.Helper()
	cfg.Issuer = iss.URL
	if cfg.Audience == "" && !cfg.SkipAudience {
		cfg.Audience = aud
	}
	o, err := auth.NewOIDC(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	return o
}

func TestOIDCValidTokenMapsClaims(t *testing.T) {
	iss := authtest.New(t)
	o := newOIDC(t, iss, auth.OIDCConfig{})
	claims := iss.Claims("alice", aud)
	claims["groups"] = []string{"/payments", "platform"}
	claims["roles"] = []string{"dev", "reviewer"}
	claims["sgw_agent"] = "claude-code"
	p, err := o.Authenticate(context.Background(), iss.Token(t, claims))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	want := auth.Principal{Subject: "alice", Teams: []string{"payments", "platform"}, Roles: []string{"dev", "reviewer"}, AgentType: "claude-code"}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("principal = %+v, want %+v", p, want)
	}
}

func TestOIDCDottedClaimPaths(t *testing.T) {
	iss := authtest.New(t)
	o := newOIDC(t, iss, auth.OIDCConfig{
		SubjectClaim: "preferred_username",
		TeamsClaim:   "org.teams",
		RolesClaim:   "realm_access.roles",
		AgentClaim:   "ext.agent.type",
	})
	claims := iss.Claims("ignored-sub", aud)
	claims["preferred_username"] = "bob"
	claims["org"] = map[string]any{"teams": "solo-team"} // scalar string becomes a one-element list
	claims["realm_access"] = map[string]any{"roles": []any{"admin", 42, "ops"}}
	claims["ext"] = map[string]any{"agent": map[string]any{"type": "codex"}}
	p, err := o.Authenticate(context.Background(), iss.Token(t, claims))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	want := auth.Principal{Subject: "bob", Teams: []string{"solo-team"}, Roles: []string{"admin", "ops"}, AgentType: "codex"}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("principal = %+v, want %+v", p, want)
	}

	// Missing optional claims leave the fields empty; a missing subject fails.
	minimal := iss.Claims("x", aud)
	minimal["preferred_username"] = "carol"
	p, err = o.Authenticate(context.Background(), iss.Token(t, minimal))
	if err != nil || p.Subject != "carol" || p.Teams != nil || p.Roles != nil || p.AgentType != "" {
		t.Errorf("minimal token: %+v, %v", p, err)
	}
	noSub := iss.Claims("x", aud) // has sub but not preferred_username
	if _, err := o.Authenticate(context.Background(), iss.Token(t, noSub)); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("token without configured subject claim: %v", err)
	}
	// A dotted path through a non-object yields nothing rather than panicking.
	weird := iss.Claims("x", aud)
	weird["preferred_username"] = "dan"
	weird["realm_access"] = "string-not-object"
	weird["ext"] = []any{"list"}
	p, err = o.Authenticate(context.Background(), iss.Token(t, weird))
	if err != nil || p.Subject != "dan" || p.Roles != nil || p.AgentType != "" {
		t.Errorf("weird claims: %+v, %v", p, err)
	}
}

func TestOIDCRejectsBadTokens(t *testing.T) {
	iss := authtest.New(t)
	other := authtest.New(t)
	o := newOIDC(t, iss, auth.OIDCConfig{})
	ctx := context.Background()

	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	expired := iss.Claims("alice", aud)
	expired["exp"] = time.Now().Add(-time.Minute).Unix()
	expired["iat"] = time.Now().Add(-time.Hour).Unix()
	wrongIss := iss.Claims("alice", aud)
	wrongIss["iss"] = other.URL
	wrongAud := iss.Claims("alice", "someone-else")
	multiAud := iss.Claims("alice", aud)
	multiAud["aud"] = []string{"a", "b"}
	noExp := iss.Claims("alice", aud)
	delete(noExp, "exp")
	good := iss.Claims("alice", aud)

	cases := map[string]string{
		"empty":                  "",
		"garbage":                "not.a.jwt",
		"expired":                iss.Token(t, expired),
		"wrong issuer":           iss.Token(t, wrongIss),
		"wrong audience":         iss.Token(t, wrongAud),
		"audience list missing":  iss.Token(t, multiAud),
		"no exp":                 iss.Token(t, noExp),
		"unknown kid":            iss.TokenWith(t, good, jose.RS256, iss.Key, "unknown-kid"),
		"unpublished key":        iss.TokenWith(t, good, jose.RS256, otherKey, iss.KeyID),
		"other issuer's key":     other.Token(t, good),
		"alg none":               iss.UnsignedToken(t, good),
		"HS256 with public key":  iss.TokenWith(t, good, jose.HS256, publicKeyBytes(t, iss), iss.KeyID),
		"HS256 with shared key":  iss.TokenWith(t, good, jose.HS256, []byte("secret-secret-secret-secret-secret"), iss.KeyID),
		"tampered payload":       tamper(iss.Token(t, good)),
		"signature stripped":     stripSig(iss.Token(t, good)),
		"subject missing":        iss.Token(t, without(iss.Claims("alice", aud), "sub")),
		"subject empty":          iss.Token(t, with(iss.Claims("", aud), "sub", "")),
		"subject not a string":   iss.Token(t, with(iss.Claims("x", aud), "sub", 7)),
		"issued by other issuer": other.Token(t, other.Claims("alice", aud)),
	}
	for label, tok := range cases {
		t.Run(label, func(t *testing.T) {
			p, err := o.Authenticate(ctx, tok)
			if err == nil {
				t.Fatalf("accepted: %+v", p)
			}
			if !errors.Is(err, auth.ErrUnauthenticated) {
				t.Errorf("error %v is not ErrUnauthenticated", err)
			}
			if p.Subject != "" {
				t.Errorf("principal leaked on failure: %+v", p)
			}
		})
	}
	// Sanity: the baseline token is accepted, so the failures above are real.
	if _, err := o.Authenticate(ctx, iss.Token(t, good)); err != nil {
		t.Fatalf("baseline token rejected: %v", err)
	}
}

func TestOIDCSkipAudience(t *testing.T) {
	iss := authtest.New(t)
	o := newOIDC(t, iss, auth.OIDCConfig{SkipAudience: true})
	claims := iss.Claims("alice", "whatever")
	if _, err := o.Authenticate(context.Background(), iss.Token(t, claims)); err != nil {
		t.Errorf("skip_audience should accept any aud: %v", err)
	}
}

func TestNewOIDCConfigErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := auth.NewOIDC(ctx, auth.OIDCConfig{Audience: "x"}); err == nil {
		t.Error("missing issuer accepted")
	}
	if _, err := auth.NewOIDC(ctx, auth.OIDCConfig{Issuer: "http://127.0.0.1:1"}); err == nil {
		t.Error("missing audience accepted")
	}
	// Discovery against a dead endpoint fails fast rather than hanging.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := auth.NewOIDC(ctx, auth.OIDCConfig{Issuer: "http://127.0.0.1:1", Audience: "x"}); err == nil {
		t.Error("unreachable issuer accepted")
	}
}

func publicKeyBytes(t *testing.T, iss *authtest.Issuer) []byte {
	t.Helper()
	return iss.Key.PublicKey.N.Bytes()
}

func with(m map[string]any, k string, v any) map[string]any {
	m[k] = v
	return m
}

func without(m map[string]any, k string) map[string]any {
	delete(m, k)
	return m
}

// tamper flips one character in the middle of the payload segment.
func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	p := []byte(parts[1])
	i := len(p) / 2
	if p[i] == 'A' {
		p[i] = 'B'
	} else {
		p[i] = 'A'
	}
	parts[1] = string(p)
	return strings.Join(parts, ".")
}

func stripSig(tok string) string { return tok[:strings.LastIndexByte(tok, '.')+1] }

func TestOIDCGroupPathPrefixStrippedForScalarAndList(t *testing.T) {
	iss := authtest.New(t)
	o := newOIDC(t, iss, auth.OIDCConfig{})
	claims := iss.Claims("alice", aud)
	claims["groups"] = "/payments" // a single group as a scalar, as some IdPs emit
	claims["roles"] = []any{"/admin", "dev"}
	p, err := o.Authenticate(context.Background(), iss.Token(t, claims))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Teams, []string{"payments"}) || !reflect.DeepEqual(p.Roles, []string{"admin", "dev"}) {
		t.Errorf("teams = %v, roles = %v; leading slashes must be stripped consistently", p.Teams, p.Roles)
	}
}
