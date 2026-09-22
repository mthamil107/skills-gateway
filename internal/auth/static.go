package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
)

// Static authenticates against a fixed token table. It is meant for local
// development and CI, not production.
type Static struct {
	tokens map[[32]byte]Principal
}

// NewStatic builds a Static authenticator from token -> principal.
func NewStatic(tokens map[string]Principal) *Static {
	s := &Static{tokens: make(map[[32]byte]Principal, len(tokens))}
	for tok, p := range tokens {
		s.tokens[sha256.Sum256([]byte(tok))] = p
	}
	return s
}

// Authenticate looks the token up by its hash, comparing in constant time.
func (s *Static) Authenticate(_ context.Context, bearer string) (Principal, error) {
	if bearer == "" {
		return Principal{}, ErrUnauthenticated
	}
	want := sha256.Sum256([]byte(bearer))
	for h, p := range s.tokens {
		if subtle.ConstantTimeCompare(h[:], want[:]) == 1 {
			return p, nil
		}
	}
	return Principal{}, ErrUnauthenticated
}

// None accepts every request as a fixed principal. The server refuses to
// start with it unless explicitly told the deployment is insecure.
type None struct{ P Principal }

// Authenticate always succeeds.
func (n None) Authenticate(context.Context, string) (Principal, error) { return n.P, nil }
