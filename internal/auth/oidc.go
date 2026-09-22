package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDCConfig configures bearer-token verification against any OpenID
// Connect provider (Keycloak, Entra ID, Okta, Auth0, Cognito, ...).
type OIDCConfig struct {
	Issuer   string `yaml:"issuer"`
	Audience string `yaml:"audience"`
	// Claim names; dotted paths reach into nested objects, e.g.
	// "realm_access.roles" for Keycloak.
	SubjectClaim string `yaml:"subject_claim"`
	TeamsClaim   string `yaml:"teams_claim"`
	RolesClaim   string `yaml:"roles_claim"`
	AgentClaim   string `yaml:"agent_claim"`
	// SkipAudience disables the aud check. Only for providers that do not
	// issue an audience for access tokens; avoid in production.
	SkipAudience bool `yaml:"skip_audience"`
}

// OIDC verifies JWT access tokens using the provider's discovery document
// and JWKS (fetched and refreshed by go-oidc).
type OIDC struct {
	cfg      OIDCConfig
	verifier *oidc.IDTokenVerifier
}

// NewOIDC performs discovery against cfg.Issuer.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("oidc: issuer is required")
	}
	if cfg.Audience == "" && !cfg.SkipAudience {
		return nil, fmt.Errorf("oidc: audience is required (or set skip_audience)")
	}
	if cfg.SubjectClaim == "" {
		cfg.SubjectClaim = "sub"
	}
	if cfg.TeamsClaim == "" {
		cfg.TeamsClaim = "groups"
	}
	if cfg.RolesClaim == "" {
		cfg.RolesClaim = "roles"
	}
	if cfg.AgentClaim == "" {
		cfg.AgentClaim = "sgw_agent"
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	v := provider.Verifier(&oidc.Config{
		ClientID:             cfg.Audience,
		SkipClientIDCheck:    cfg.SkipAudience,
		SupportedSigningAlgs: []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "EdDSA"},
	})
	return &OIDC{cfg: cfg, verifier: v}, nil
}

// Authenticate verifies signature, issuer, audience and expiry, then maps
// claims to a Principal.
func (o *OIDC) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	if bearer == "" {
		return Principal{}, ErrUnauthenticated
	}
	tok, err := o.verifier.Verify(ctx, bearer)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	p := Principal{
		Subject:   claimString(claims, o.cfg.SubjectClaim),
		Teams:     claimStrings(claims, o.cfg.TeamsClaim),
		Roles:     claimStrings(claims, o.cfg.RolesClaim),
		AgentType: claimString(claims, o.cfg.AgentClaim),
	}
	if p.Subject == "" {
		return Principal{}, fmt.Errorf("%w: token has no %q claim", ErrUnauthenticated, o.cfg.SubjectClaim)
	}
	return p, nil
}

func lookup(claims map[string]any, dotted string) any {
	var cur any = claims
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func claimString(claims map[string]any, name string) string {
	s, _ := lookup(claims, name).(string)
	return s
}

func claimStrings(claims map[string]any, name string) []string {
	switch v := lookup(claims, name).(type) {
	case string:
		return []string{strings.TrimPrefix(v, "/")}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				// Entra/Keycloak group paths like "/payments" become "payments".
				out = append(out, strings.TrimPrefix(s, "/"))
			}
		}
		return out
	}
	return nil
}
