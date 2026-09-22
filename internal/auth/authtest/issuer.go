// Package authtest provides a fake OpenID Connect issuer for tests: it
// serves a discovery document and a JWKS over httptest and signs tokens
// with a throwaway RSA key. Everything stays in-process and offline.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Issuer is a running fake OIDC provider.
type Issuer struct {
	// URL is the issuer identifier (the httptest server's base URL).
	URL string
	// KeyID is the kid the JWKS publishes for Key.
	KeyID string
	Key   *rsa.PrivateKey

	srv *httptest.Server
}

// New starts an issuer and registers its shutdown with t.
func New(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("authtest: generate key: %v", err)
	}
	iss := &Issuer{KeyID: "test-key-1", Key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                iss.URL,
			"authorization_endpoint":                iss.URL + "/authorize",
			"token_endpoint":                        iss.URL + "/token",
			"userinfo_endpoint":                     iss.URL + "/userinfo",
			"jwks_uri":                              iss.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &iss.Key.PublicKey, KeyID: iss.KeyID, Algorithm: string(jose.RS256), Use: "sig",
		}}})
	})
	iss.srv = httptest.NewServer(mux)
	iss.URL = iss.srv.URL
	t.Cleanup(iss.srv.Close)
	return iss
}

// Claims returns a baseline claim set valid for aud, expiring in an hour.
// Callers override or delete entries before signing.
func (i *Issuer) Claims(sub, aud string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": i.URL,
		"sub": sub,
		"aud": aud,
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

// Token signs claims with the issuer's published key (RS256, published kid).
func (i *Issuer) Token(t testing.TB, claims map[string]any) string {
	t.Helper()
	return i.TokenWith(t, claims, jose.RS256, i.Key, i.KeyID)
}

// TokenWith signs claims with an arbitrary algorithm, key and kid, for
// negative tests (unknown kid, unpublished key, HS256, ...).
func (i *Issuer) TokenWith(t testing.TB, claims map[string]any, alg jose.SignatureAlgorithm, key any, kid string) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if kid != "" {
		opts = opts.WithHeader("kid", kid)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts)
	if err != nil {
		t.Fatalf("authtest: signer: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("authtest: marshal claims: %v", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("authtest: sign: %v", err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		t.Fatalf("authtest: serialize: %v", err)
	}
	return s
}

// UnsignedToken builds an "alg":"none" JWT (no signature) over claims.
func (i *Issuer) UnsignedToken(t testing.TB, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding
	hdr, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT", "kid": i.KeyID})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("authtest: marshal claims: %v", err)
	}
	return enc.EncodeToString(hdr) + "." + enc.EncodeToString(payload) + "."
}
