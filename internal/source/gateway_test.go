package source

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/client"
	"github.com/mthamil107/skills-gateway/internal/gatewaytest"
)

func gatewayFixture(t *testing.T) (*gatewaytest.Gateway, *Gateway) {
	t.Helper()
	g := gatewaytest.New(t, gatewaytest.Options{})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.0.0", gatewaytest.Files("demo", map[string]string{"scripts/run.sh": "echo demo\n"}))
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.1.0", gatewaytest.Files("demo", nil))
	g.Publish(t, gatewaytest.PlatformToken, "platform", "other", "1.0.0", gatewaytest.Files("other", nil))
	g.Publish(t, gatewaytest.PaymentsToken, "payments", "refunds", "2.0.0", gatewaytest.Files("refunds", nil))
	return g, &Gateway{Client: g.Client(gatewaytest.AdminToken)}
}

func TestGatewayResolve(t *testing.T) {
	g, src := gatewayFixture(t)
	if !src.Pinnable() {
		t.Error("a gateway must be pinnable")
	}
	if src.Describe() != "gateway:"+g.URL {
		t.Errorf("Describe = %q", src.Describe())
	}
	// A namespace is required: a bare name is ambiguous on a gateway.
	if _, err := src.Resolve(ctx, Ref{Name: "demo", Version: "latest"}); err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("bare name: %v", err)
	}
	// latest, explicit and empty versions.
	sk, err := src.Resolve(ctx, Ref{Namespace: "platform", Name: "demo", Version: "latest"})
	if err != nil {
		t.Fatalf("Resolve latest: %v", err)
	}
	meta, _ := g.Client(gatewaytest.AdminToken).Get(ctx, "platform", "demo", "1.1.0")
	if sk.Version != "1.1.0" || sk.Digest != meta.Digest || sk.Namespace != "platform" || sk.Name != "demo" || len(sk.Files) != 1 {
		t.Errorf("latest = %+v", sk)
	}
	sk, err = src.Resolve(ctx, Ref{Namespace: "platform", Name: "demo", Version: "1.0.0"})
	if err != nil || sk.Version != "1.0.0" || len(sk.Files) != 2 {
		t.Errorf("pinned = %+v, %v", sk, err)
	}
	sk, err = src.Resolve(ctx, Ref{Namespace: "platform", Name: "demo"})
	if err != nil || sk.Version != "1.1.0" {
		t.Errorf("empty version should mean latest: %+v, %v", sk, err)
	}
	for _, r := range []Ref{
		{Namespace: "platform", Name: "ghost", Version: "latest"},
		{Namespace: "platform", Name: "demo", Version: "9.9.9"},
		{Namespace: "nope", Name: "demo", Version: "latest"},
	} {
		if _, err := src.Resolve(ctx, r); err == nil {
			t.Errorf("Resolve(%+v) succeeded", r)
		}
	}
	// A reader who may not see payments gets an error, not another skill.
	reader := &Gateway{Client: g.Client(gatewaytest.ReaderToken)}
	if _, err := reader.Resolve(ctx, Ref{Namespace: "payments", Name: "refunds", Version: "latest"}); err == nil {
		t.Error("policy-denied skill resolved")
	}
}

var digestField = regexp.MustCompile(`"digest":\s*"(sha256:[0-9a-f]{64})"`)

// tamperingProxy sits between the source and the gateway and lets a test
// rewrite requests and responses.
func tamperingProxy(t *testing.T, target string, director func(*http.Request), modify func(*http.Response) error) *client.Client {
	t.Helper()
	u, _ := url.Parse(target)
	proxy := httputil.NewSingleHostReverseProxy(u)
	inner := proxy.Director
	proxy.Director = func(r *http.Request) {
		inner(r)
		if director != nil {
			director(r)
		}
	}
	proxy.ModifyResponse = modify
	ps := httptest.NewServer(proxy)
	t.Cleanup(ps.Close)
	return &client.Client{BaseURL: ps.URL, Token: gatewaytest.AdminToken}
}

// A server whose metadata advertises one digest but whose bundle carries
// another is caught even when the bundle itself is internally consistent.
func TestGatewayDetectsMetadataDigestMismatch(t *testing.T) {
	g, _ := gatewayFixture(t)
	fake := "sha256:" + strings.Repeat("f", 64)
	c := tamperingProxy(t, g.URL, nil, func(r *http.Response) error {
		if !strings.Contains(r.Request.URL.Path, "/versions/") || strings.HasSuffix(r.Request.URL.Path, "/bundle") {
			return nil
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return err
		}
		// Replace the advertised digest in the metadata JSON only.
		loc := digestField.FindSubmatchIndex(body)
		if loc == nil {
			return fmt.Errorf("no digest in metadata: %s", body)
		}
		body = append(append(append([]byte{}, body[:loc[2]]...), fake...), body[loc[3]:]...)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", fmt.Sprint(len(body)))
		return nil
	})
	src := &Gateway{Client: c}
	_, err := src.Resolve(ctx, Ref{Namespace: "platform", Name: "demo", Version: "1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "metadata digest") || !strings.Contains(err.Error(), fake) {
		t.Errorf("metadata/bundle mismatch not detected: %v", err)
	}
	// Tampering the bundle header alone is caught by the client layer.
	c2 := tamperingProxy(t, g.URL, nil, func(r *http.Response) error {
		if strings.HasSuffix(r.Request.URL.Path, "/bundle") {
			r.Header.Set("X-Bundle-Digest", fake)
		}
		return nil
	})
	if _, err := (&Gateway{Client: c2}).Resolve(ctx, Ref{Namespace: "platform", Name: "demo", Version: "1.0.0"}); err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Errorf("tampered bundle header: %v", err)
	}
}

// List must follow next_cursor until the server stops returning one. The
// proxy shrinks the page size so a small fixture spans several pages.
func TestGatewayListPagesThroughEverything(t *testing.T) {
	g, direct := gatewayFixture(t)
	pages := 0
	c := tamperingProxy(t, g.URL, func(r *http.Request) {
		if r.URL.Path == "/v1/skills" {
			q := r.URL.Query()
			q.Set("limit", "1")
			r.URL.RawQuery = q.Encode()
			pages++
		}
	}, nil)
	refs, err := (&Gateway{Client: c}).List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := names(refs); got != "payments/refunds platform/demo platform/other" {
		t.Errorf("List = %q", got)
	}
	if pages < 3 {
		t.Errorf("server was asked for %d page(s); paging did not happen", pages)
	}
	for _, r := range refs {
		if r.Version != "latest" {
			t.Errorf("List ref %+v should be latest", r)
		}
	}
	// The unproxied list agrees, and a reader only sees what policy allows.
	all, err := direct.List(ctx)
	if err != nil || names(all) != names(refs) {
		t.Errorf("direct List = %q, %v", names(all), err)
	}
	reader := &Gateway{Client: g.Client(gatewaytest.ReaderToken)}
	visible, err := reader.List(ctx)
	if err != nil || names(visible) != "platform/demo platform/other" {
		t.Errorf("reader List = %q, %v", names(visible), err)
	}
}
