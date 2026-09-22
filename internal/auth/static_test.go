package auth_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/auth"
)

func TestStaticAuthenticate(t *testing.T) {
	alice := auth.Principal{Subject: "alice", Teams: []string{"payments"}, Roles: []string{"dev"}, AgentType: "claude-code"}
	bob := auth.Principal{Subject: "bob"}
	s := auth.NewStatic(map[string]auth.Principal{
		"alice-token-0123456789": alice,
		"bob-token-0123456789":   bob,
	})
	ctx := context.Background()

	p, err := s.Authenticate(ctx, "alice-token-0123456789")
	if err != nil || !reflect.DeepEqual(p, alice) {
		t.Errorf("good token: %+v, %v", p, err)
	}
	p, err = s.Authenticate(ctx, "bob-token-0123456789")
	if err != nil || !reflect.DeepEqual(p, bob) {
		t.Errorf("second token: %+v, %v", p, err)
	}
	for _, bad := range []string{"", "alice-token-012345678", "alice-token-0123456789x", "ALICE-TOKEN-0123456789", " alice-token-0123456789", "nope"} {
		p, err := s.Authenticate(ctx, bad)
		if !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("token %q: err = %v, want ErrUnauthenticated", bad, err)
		}
		if p.Subject != "" {
			t.Errorf("token %q leaked principal %+v", bad, p)
		}
	}
	// An empty table rejects everything, including the empty token.
	empty := auth.NewStatic(nil)
	if _, err := empty.Authenticate(ctx, ""); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("empty table, empty token: %v", err)
	}
	if _, err := empty.Authenticate(ctx, "x"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("empty table: %v", err)
	}
}

func TestNoneAndContext(t *testing.T) {
	n := auth.None{P: auth.Principal{Subject: "anon"}}
	p, err := n.Authenticate(context.Background(), "")
	if err != nil || p.Subject != "anon" {
		t.Errorf("None: %+v, %v", p, err)
	}
	ctx := auth.WithPrincipal(context.Background(), p)
	got, ok := auth.FromContext(ctx)
	if !ok || got.Subject != "anon" {
		t.Errorf("FromContext = %+v, %v", got, ok)
	}
	if _, ok := auth.FromContext(context.Background()); ok {
		t.Error("FromContext on empty context reported a principal")
	}
}
