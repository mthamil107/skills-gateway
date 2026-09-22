// Package auth authenticates bearer tokens into a Principal.
package auth

import (
	"context"
	"errors"
)

// Principal is the authenticated caller. AgentType comes from the token,
// never from a request header, so it can be used in authorization.
type Principal struct {
	Subject   string   `json:"subject"`
	Teams     []string `json:"teams,omitempty"`
	Roles     []string `json:"roles,omitempty"`
	AgentType string   `json:"agent_type,omitempty"`
}

// ErrUnauthenticated is returned for missing, malformed or invalid tokens.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticator turns a raw bearer token into a Principal.
type Authenticator interface {
	Authenticate(ctx context.Context, bearer string) (Principal, error)
}

type ctxKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal stored by WithPrincipal.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
