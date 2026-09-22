package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/client"
	"github.com/mthamil107/skills-gateway/internal/gatewaytest"
	"github.com/mthamil107/skills-gateway/internal/store"
)

var ctx = context.Background()

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type envelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// expectError asserts an error envelope with the given status and code.
func expectError(t *testing.T, resp *http.Response, status int, code string) envelope {
	t.Helper()
	if resp.StatusCode != status {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body %s", resp.StatusCode, status, b)
	}
	var env envelope
	decode(t, resp, &env)
	if env.Error.Code != code {
		t.Errorf("error.code = %q, want %q (%s)", env.Error.Code, code, env.Error.Message)
	}
	if env.Error.RequestID == "" || env.Error.RequestID != resp.Header.Get("X-Request-Id") {
		t.Errorf("error.request_id = %q, header X-Request-Id = %q", env.Error.RequestID, resp.Header.Get("X-Request-Id"))
	}
	return env
}

func clientErr(t *testing.T, err error, status int, code string) *client.Error {
	t.Helper()
	var ce *client.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a client.Error", err)
	}
	if ce.Status != status || ce.Code != code {
		t.Fatalf("got %d %s, want %d %s (%s)", ce.Status, ce.Code, status, code, ce.Message)
	}
	return ce
}

func TestPublishAndImmutability(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	files := gatewaytest.Files("demo", map[string]string{"scripts/run.sh": "echo one\n"})
	data, _ := bundle.Encode(files)
	want := bundle.FromFiles(files).Digest

	resp := g.Do(t, http.MethodPut, "/v1/skills/platform/demo/versions/1.0.0", gatewaytest.PlatformToken, bytes.NewReader(data),
		map[string]string{"Content-Type": "application/gzip", "X-Bundle-Digest": want})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish status = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/skills/platform/demo/versions/1.0.0" {
		t.Errorf("Location = %q", loc)
	}
	var v store.Version
	decode(t, resp, &v)
	if v.Namespace != "platform" || v.Name != "demo" || v.Version != "1.0.0" || v.Status != store.Published ||
		v.Digest != want || v.Publisher != "bob" || v.Description != "Skill demo" || len(v.Files) != 2 {
		t.Errorf("published version = %+v", v)
	}
	if time.Since(v.PublishedAt) > time.Minute || v.PublishedAt.Location() != time.UTC {
		t.Errorf("published_at = %v", v.PublishedAt)
	}

	// Re-publishing the same version, even with different content, is a
	// 409 and leaves the stored bytes untouched (also for an admin).
	changed := gatewaytest.Files("demo", map[string]string{"scripts/run.sh": "echo TWO\n"})
	for _, tok := range []string{gatewaytest.PlatformToken, gatewaytest.AdminToken} {
		_, err := g.Client(tok).Publish(ctx, "platform", "demo", "1.0.0", changed)
		clientErr(t, err, http.StatusConflict, "version_exists")
	}
	got, err := g.Client(gatewaytest.ReaderToken).Get(ctx, "platform", "demo", "1.0.0")
	if err != nil || got.Digest != want {
		t.Errorf("after 409: %+v, %v", got, err)
	}
	b, err := g.Client(gatewaytest.ReaderToken).Bundle(ctx, "platform", "demo", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := b.File("scripts/run.sh"); string(f.Content) != "echo one\n" || b.Digest != want {
		t.Errorf("bundle bytes changed: %q %s", f.Content, b.Digest)
	}
	// A new version of the same skill is fine.
	if _, err := g.Client(gatewaytest.PlatformToken).Publish(ctx, "platform", "demo", "1.0.1", changed); err != nil {
		t.Errorf("publish 1.0.1: %v", err)
	}
}

func TestPublishDigestMismatchStoresNothing(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	files := gatewaytest.Files("demo", nil)
	data, _ := bundle.Encode(files)
	resp := g.Do(t, http.MethodPut, "/v1/skills/platform/demo/versions/1.0.0", gatewaytest.PlatformToken, bytes.NewReader(data),
		map[string]string{"X-Bundle-Digest": "sha256:" + strings.Repeat("0", 64)})
	env := expectError(t, resp, http.StatusBadRequest, "digest_mismatch")
	if !strings.Contains(env.Error.Message, bundle.FromFiles(files).Digest) {
		t.Errorf("message should name the computed digest: %s", env.Error.Message)
	}
	_, err := g.Client(gatewaytest.PlatformToken).Get(ctx, "platform", "demo", "1.0.0")
	clientErr(t, err, http.StatusNotFound, "not_found")
	_, err = g.Client(gatewaytest.PlatformToken).Get(ctx, "platform", "demo", "latest")
	clientErr(t, err, http.StatusNotFound, "not_found")
	page, _ := g.Client(gatewaytest.AdminToken).List(ctx, "", "", "", "", 0)
	if len(page.Items) != 0 {
		t.Errorf("list after failed publish: %+v", page.Items)
	}
	// Without the header the same bundle publishes fine.
	resp = g.Do(t, http.MethodPut, "/v1/skills/platform/demo/versions/1.0.0", gatewaytest.PlatformToken, bytes.NewReader(data), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("publish without digest header: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPublishInvalid(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	ok := gatewaytest.Files("demo", nil)
	okData, _ := bundle.Encode(ok)
	noMD, _ := bundle.Encode([]bundle.File{{Path: "README.md", Content: []byte("x")}})
	badName, _ := bundle.Encode(gatewaytest.Files("other-name", nil))
	badMD, _ := bundle.Encode([]bundle.File{{Path: "SKILL.md", Content: []byte("no frontmatter")}})
	cases := []struct {
		label string
		path  string
		body  []byte
		want  string
	}{
		{"garbage body", "/v1/skills/platform/demo/versions/1.0.0", []byte("this is not gzip"), "not gzip"},
		{"empty body", "/v1/skills/platform/demo/versions/1.0.0", nil, "not gzip"},
		{"missing SKILL.md", "/v1/skills/platform/demo/versions/1.0.0", noMD, "SKILL.md missing"},
		{"unparseable SKILL.md", "/v1/skills/platform/demo/versions/1.0.0", badMD, "frontmatter"},
		{"name mismatch", "/v1/skills/platform/demo/versions/1.0.0", badName, "does not match"},
		{"version with v", "/v1/skills/platform/demo/versions/v1.0.0", okData, "semantic version"},
		{"short version", "/v1/skills/platform/demo/versions/1.0", okData, "semantic version"},
		{"latest as version", "/v1/skills/platform/demo/versions/latest", okData, "semantic version"},
		{"bad namespace", "/v1/skills/Platform/demo/versions/1.0.0", okData, "namespace"},
		{"bad name", "/v1/skills/platform/De_mo/versions/1.0.0", okData, "skill name"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			resp := g.Do(t, http.MethodPut, c.path, gatewaytest.AdminToken, bytes.NewReader(c.body), nil)
			env := expectError(t, resp, http.StatusBadRequest, "invalid_request")
			if !strings.Contains(env.Error.Message, c.want) {
				t.Errorf("message %q does not mention %q", env.Error.Message, c.want)
			}
		})
	}
	page, _ := g.Client(gatewaytest.AdminToken).List(ctx, "", "", "", "", 0)
	if len(page.Items) != 0 {
		t.Errorf("invalid publishes stored something: %+v", page.Items)
	}
}

func TestPublishOversize(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{MaxBody: 2048})
	big := gatewaytest.Files("demo", map[string]string{"data.bin": randomText(8192)})
	data, _ := bundle.Encode(big)
	if len(data) <= 2048 {
		t.Fatalf("test bundle too small: %d", len(data))
	}
	resp := g.Do(t, http.MethodPut, "/v1/skills/platform/demo/versions/1.0.0", gatewaytest.PlatformToken, bytes.NewReader(data), nil)
	env := expectError(t, resp, http.StatusRequestEntityTooLarge, "payload_too_large")
	if !strings.Contains(env.Error.Message, "2048") {
		t.Errorf("message = %q", env.Error.Message)
	}
	_, err := g.Client(gatewaytest.PlatformToken).Get(ctx, "platform", "demo", "1.0.0")
	clientErr(t, err, http.StatusNotFound, "not_found")
	// A small bundle still works under the same cap.
	if _, err := g.Client(gatewaytest.PlatformToken).Publish(ctx, "platform", "demo", "1.0.0", gatewaytest.Files("demo", nil)); err != nil {
		t.Errorf("small publish: %v", err)
	}
}

// randomText returns n bytes that do not compress well.
func randomText(n int) string {
	var b strings.Builder
	x := uint32(2463534242)
	for b.Len() < n {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		fmt.Fprintf(&b, "%08x", x)
	}
	return b.String()[:n]
}

func TestUnauthenticated(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.0.0", gatewaytest.Files("demo", nil))
	paths := []string{"/v1/whoami", "/v1/skills", "/v1/skills/platform/demo", "/v1/skills/platform/demo/versions/1.0.0",
		"/v1/skills/platform/demo/versions/1.0.0/bundle", "/v1/skills/platform/demo/versions/1.0.0/files/SKILL.md",
		"/v1/skills/platform/demo/versions/1.0.0/translate", "/v1/audit", "/v1/formats"}
	for _, p := range paths {
		for label, hdr := range map[string]map[string]string{
			"no header":    nil,
			"bad token":    {"Authorization": "Bearer nope"},
			"basic scheme": {"Authorization": "Basic " + gatewaytest.AdminToken},
			"empty bearer": {"Authorization": "Bearer "},
		} {
			resp := g.Do(t, http.MethodGet, p, "", nil, hdr)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s: status %d", label, p, resp.StatusCode)
				resp.Body.Close()
				continue
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s: no WWW-Authenticate", label, p)
			}
			expectError(t, resp, http.StatusUnauthorized, "unauthenticated")
		}
	}
	// Mutations are rejected before any body is looked at.
	resp := g.Do(t, http.MethodPut, "/v1/skills/platform/x/versions/1.0.0", "", strings.NewReader("junk"), nil)
	expectError(t, resp, http.StatusUnauthorized, "unauthenticated")
	resp = g.Do(t, http.MethodPost, "/v1/skills/platform/demo/versions/1.0.0/deprecate", "", nil, nil)
	expectError(t, resp, http.StatusUnauthorized, "unauthenticated")
	resp = g.Do(t, http.MethodPost, "/mcp", "", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), nil)
	expectError(t, resp, http.StatusUnauthorized, "unauthenticated")

	// Public endpoints need no token; the request id is always present.
	resp = g.Do(t, http.MethodGet, "/healthz", "", nil, nil)
	if resp.StatusCode != 200 || resp.Header.Get("X-Request-Id") == "" {
		t.Errorf("healthz: %d %q", resp.StatusCode, resp.Header.Get("X-Request-Id"))
	}
	resp.Body.Close()
	resp = g.Do(t, http.MethodGet, "/openapi.yaml", "", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(readAll(t, resp)), "openapi") {
		t.Error("openapi.yaml not served")
	}
	// The scheme keyword is case-insensitive and the token is trimmed.
	resp = g.Do(t, http.MethodGet, "/v1/whoami", "", nil, map[string]string{"Authorization": "BEARER   " + gatewaytest.ReaderToken + "  "})
	var p struct{ Subject string }
	decode(t, resp, &p)
	if p.Subject != "dave" {
		t.Errorf("whoami = %+v", p)
	}
}

func TestPublishWrongNamespaceForbidden(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	files := gatewaytest.Files("demo", nil)
	_, err := g.Client(gatewaytest.PlatformToken).Publish(ctx, "payments", "demo", "1.0.0", files)
	clientErr(t, err, http.StatusForbidden, "forbidden")
	_, err = g.Client(gatewaytest.ReaderToken).Publish(ctx, "platform", "demo", "1.0.0", files)
	clientErr(t, err, http.StatusForbidden, "forbidden")
	_, err = g.Client(gatewaytest.PaymentsToken).Publish(ctx, "platform", "demo", "1.0.0", files)
	clientErr(t, err, http.StatusForbidden, "forbidden")
	// Nothing was stored by the denied attempts.
	page, _ := g.Client(gatewaytest.AdminToken).List(ctx, "", "", "", "", 0)
	if len(page.Items) != 0 {
		t.Errorf("denied publishes stored something: %+v", page.Items)
	}
	// The right team (and admin) can.
	if _, err := g.Client(gatewaytest.PaymentsToken).Publish(ctx, "payments", "demo", "1.0.0", files); err != nil {
		t.Errorf("payments team: %v", err)
	}
	if _, err := g.Client(gatewaytest.AdminToken).Publish(ctx, "anything", "demo", "1.0.0", files); err != nil {
		t.Errorf("admin: %v", err)
	}
}

func TestDeniedFetchIsNotFound(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	g.Publish(t, gatewaytest.PaymentsToken, "payments", "refunds", "1.0.0", gatewaytest.Files("refunds", map[string]string{"notes.md": "n"}))
	dave := g.Client(gatewaytest.ReaderToken)
	// Every read path answers exactly as it would for a skill that does not exist.
	for _, path := range []string{
		"/v1/skills/payments/refunds",
		"/v1/skills/payments/refunds/versions/1.0.0",
		"/v1/skills/payments/refunds/versions/latest",
		"/v1/skills/payments/refunds/versions/1.0.0/bundle",
		"/v1/skills/payments/refunds/versions/1.0.0/files/notes.md",
		"/v1/skills/payments/refunds/versions/1.0.0/translate?format=claude",
	} {
		denied := g.Do(t, http.MethodGet, path, gatewaytest.ReaderToken, nil, nil)
		env := expectError(t, denied, http.StatusNotFound, "not_found")
		missing := g.Do(t, http.MethodGet, strings.Replace(path, "refunds", "ghost", 1), gatewaytest.ReaderToken, nil, nil)
		env2 := expectError(t, missing, http.StatusNotFound, "not_found")
		if env.Error.Message != env2.Error.Message {
			t.Errorf("%s: denied and missing messages differ: %q vs %q", path, env.Error.Message, env2.Error.Message)
		}
	}
	if _, err := dave.Deprecate(ctx, "payments", "refunds", "1.0.0"); err == nil {
		t.Error("dave deprecated a payments skill")
	}
	// The owner sees it.
	if _, err := g.Client(gatewaytest.PaymentsToken).Get(ctx, "payments", "refunds", "latest"); err != nil {
		t.Errorf("carol: %v", err)
	}
}

func TestAgentTypeDenyRule(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "secret-keys", "1.0.0", gatewaytest.Files("secret-keys", nil))
	g.Publish(t, gatewaytest.PlatformToken, "platform", "public-info", "1.0.0", gatewaytest.Files("public-info", nil))
	robot := g.Client(gatewaytest.BotToken)
	_, err := robot.Get(ctx, "platform", "secret-keys", "latest")
	clientErr(t, err, http.StatusNotFound, "not_found")
	if _, err := robot.Get(ctx, "platform", "public-info", "latest"); err != nil {
		t.Errorf("robot public: %v", err)
	}
	if _, err := g.Client(gatewaytest.ReaderToken).Get(ctx, "platform", "secret-keys", "latest"); err != nil {
		t.Errorf("human reader secret: %v", err)
	}
	page, _ := robot.List(ctx, "", "", "", "", 0)
	if len(page.Items) != 1 || page.Items[0].Name != "public-info" {
		t.Errorf("robot list = %+v", page.Items)
	}
	// robot is on the platform team; the deny still wins for publish? No:
	// the deny rule covers fetch only, so the team allow applies to publish
	// but a bot can never read back what it published.
	if _, err := robot.Publish(ctx, "platform", "secret-two", "1.0.0", gatewaytest.Files("secret-two", nil)); err != nil {
		t.Errorf("robot publish: %v", err)
	}
	_, err = robot.Get(ctx, "platform", "secret-two", "1.0.0")
	clientErr(t, err, http.StatusNotFound, "not_found")
}

func TestLatestResolution(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	bob := g.Client(gatewaytest.PlatformToken)
	files := gatewaytest.Files("rel", nil)
	// Publish out of order so "latest" cannot mean "most recently published".
	for _, v := range []string{"1.1.0", "2.0.0-rc.1", "1.0.0", "1.2.0-beta.2", "0.9.0"} {
		if _, err := bob.Publish(ctx, "platform", "rel", v, files); err != nil {
			t.Fatal(err)
		}
	}
	latest := func() string {
		t.Helper()
		v, err := bob.Get(ctx, "platform", "rel", "latest")
		if err != nil {
			t.Fatalf("latest: %v", err)
		}
		return v.Version
	}
	if got := latest(); got != "1.1.0" {
		t.Errorf("latest = %s, want 1.1.0 (stable beats 2.0.0-rc.1)", got)
	}
	if _, err := bob.Deprecate(ctx, "platform", "rel", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if got := latest(); got != "1.0.0" {
		t.Errorf("latest after deprecating 1.1.0 = %s, want 1.0.0", got)
	}
	bob.Deprecate(ctx, "platform", "rel", "1.0.0")
	bob.Deprecate(ctx, "platform", "rel", "0.9.0")
	if got := latest(); got != "2.0.0-rc.1" {
		t.Errorf("latest with only prereleases = %s, want 2.0.0-rc.1", got)
	}
	bob.Deprecate(ctx, "platform", "rel", "2.0.0-rc.1")
	bob.Deprecate(ctx, "platform", "rel", "1.2.0-beta.2")
	_, err := bob.Get(ctx, "platform", "rel", "latest")
	clientErr(t, err, http.StatusNotFound, "not_found")
	// Explicit versions still resolve when deprecated, with the status visible.
	v, err := bob.Get(ctx, "platform", "rel", "1.1.0")
	if err != nil || v.Status != store.Deprecated {
		t.Errorf("deprecated explicit get: %+v %v", v, err)
	}
	// The versions listing is newest first and includes deprecated ones.
	resp := g.Do(t, http.MethodGet, "/v1/skills/platform/rel", gatewaytest.ReaderToken, nil, nil)
	var vl struct{ Versions []store.Version }
	decode(t, resp, &vl)
	var order []string
	for _, v := range vl.Versions {
		order = append(order, v.Version)
	}
	if strings.Join(order, " ") != "2.0.0-rc.1 1.2.0-beta.2 1.1.0 1.0.0 0.9.0" {
		t.Errorf("versions order = %v", order)
	}
}

func TestETags(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	md := gatewaytest.SkillMD("demo", "Skill demo")
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.0.0", gatewaytest.Files("demo", map[string]string{"SKILL.md": md, "a.txt": "aaa"}))
	v, _ := g.Client(gatewaytest.ReaderToken).Get(ctx, "platform", "demo", "1.0.0")
	sum := sha256.Sum256([]byte("aaa"))
	base := "/v1/skills/platform/demo/versions/1.0.0"
	cases := []struct{ path, etag string }{
		{base, v.Digest},
		{base + "/bundle", v.Digest},
		{base + "/files/a.txt", "sha256:" + hex.EncodeToString(sum[:])},
	}
	for _, c := range cases {
		resp := g.Do(t, http.MethodGet, c.path, gatewaytest.ReaderToken, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", c.path, resp.StatusCode)
		}
		got := resp.Header.Get("ETag")
		body := readAll(t, resp)
		if got != `"`+c.etag+`"` {
			t.Errorf("%s: ETag = %s, want %q", c.path, got, c.etag)
		}
		for _, inm := range []string{got, `"other", ` + got, got + `, "other"`, c.etag} {
			resp = g.Do(t, http.MethodGet, c.path, gatewaytest.ReaderToken, nil, map[string]string{"If-None-Match": inm})
			b := readAll(t, resp)
			if resp.StatusCode != http.StatusNotModified || len(b) != 0 {
				t.Errorf("%s If-None-Match %q: %d, %d bytes", c.path, inm, resp.StatusCode, len(b))
			}
			if resp.Header.Get("ETag") != got {
				t.Errorf("%s: 304 lacks ETag", c.path)
			}
		}
		resp = g.Do(t, http.MethodGet, c.path, gatewaytest.ReaderToken, nil, map[string]string{"If-None-Match": `"stale"`})
		if resp.StatusCode != 200 || !bytes.Equal(readAll(t, resp), body) {
			t.Errorf("%s stale If-None-Match: %d", c.path, resp.StatusCode)
		}
	}
	// A 304 is never given to someone who may not fetch the skill.
	resp := g.Do(t, http.MethodGet, base, gatewaytest.ReaderToken, nil, map[string]string{"If-None-Match": `"` + v.Digest + `"`})
	resp.Body.Close()
	g.Publish(t, gatewaytest.PaymentsToken, "payments", "p", "1.0.0", gatewaytest.Files("p", nil))
	pv, _ := g.Client(gatewaytest.PaymentsToken).Get(ctx, "payments", "p", "1.0.0")
	resp = g.Do(t, http.MethodGet, "/v1/skills/payments/p/versions/1.0.0", gatewaytest.ReaderToken, nil, map[string]string{"If-None-Match": `"` + pv.Digest + `"`})
	expectError(t, resp, http.StatusNotFound, "not_found")
}

func TestFileEndpoint(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	files := gatewaytest.Files("demo", map[string]string{
		"scripts/run.sh": "#!/bin/sh\n",
		"docs/x.html":    "<script>alert(1)</script>",
		"bin/blob":       "\x00\x01\x02",
		"data.json":      `{"a":1}`,
	})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.0.0", files)
	base := "/v1/skills/platform/demo/versions/1.0.0/files/"
	get := func(p string) (*http.Response, []byte) {
		resp := g.Do(t, http.MethodGet, base+p, gatewaytest.ReaderToken, nil, nil)
		return resp, readAll(t, resp)
	}
	resp, body := get("SKILL.md")
	if resp.StatusCode != 200 || !bytes.Equal(body, files[0].Content) {
		t.Errorf("SKILL.md: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("SKILL.md Content-Type = %q (markdown must not render)", ct)
	}
	for _, p := range []string{"SKILL.md", "docs/x.html", "scripts/run.sh", "bin/blob"} {
		resp, _ := get(p)
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Security-Policy") != "sandbox" {
			t.Errorf("%s: missing hardening headers: %v", p, resp.Header)
		}
	}
	resp, body = get("bin/blob")
	if resp.Header.Get("Content-Type") != "application/octet-stream" || !bytes.Equal(body, []byte("\x00\x01\x02")) {
		t.Errorf("blob: %q %q", resp.Header.Get("Content-Type"), body)
	}
	resp, _ = get("data.json")
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("json Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	resp, _ = get("scripts/run.sh")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") == "" {
		t.Errorf("run.sh: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	expectError(t, g.Do(t, http.MethodGet, base+"nope.txt", gatewaytest.ReaderToken, nil, nil), http.StatusNotFound, "not_found")
	expectError(t, g.Do(t, http.MethodGet, base+"scripts", gatewaytest.ReaderToken, nil, nil), http.StatusNotFound, "not_found")
	expectError(t, g.Do(t, http.MethodGet, base, gatewaytest.ReaderToken, nil, nil), http.StatusBadRequest, "invalid_request")

	// Traversal and other unsafe paths never reach the store.
	for _, p := range []string{
		"%2e%2e/%2e%2e/etc/passwd", "..%2F..%2Fetc%2Fpasswd", "scripts%2F..%2F..%2FSKILL.md", "scripts/%2e%2e/SKILL.md",
		"scripts%5Crun.sh", "%2Fetc%2Fpasswd", "C%3A%5Cwindows", "scripts%2F%2Frun.sh", "a%00b", "%2e%2e",
	} {
		resp := g.Do(t, http.MethodGet, base+p, gatewaytest.ReaderToken, nil, nil)
		b := readAll(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, body %q", p, resp.StatusCode, b)
			continue
		}
		if !strings.Contains(string(b), "invalid_request") {
			t.Errorf("%s: body %q", p, b)
		}
	}
	// Literal dot-dot segments are cleaned by the mux (redirect) and can
	// never serve a file either.
	req, _ := http.NewRequest(http.MethodGet, g.URL+base+"../../../../v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+gatewaytest.ReaderToken)
	noRedirect := *g.Server.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp2, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b := readAll(t, resp2)
	if resp2.StatusCode == 200 && !strings.Contains(string(b), "dave") {
		t.Errorf("dot-dot path served %d: %q", resp2.StatusCode, b)
	}
}

func TestTranslate(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	md := "---\nname: demo\ndescription: Review code carefully\nagents:\n  cursor:\n    globs: \"**/*.go\"\n---\n\n# Demo\n\nDo the thing.\n"
	files := gatewaytest.Files("demo", map[string]string{"SKILL.md": md, "scripts/run.sh": "echo\n", "ref/a.md": "a"})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.0.0", files)
	dave := g.Client(gatewaytest.ReaderToken)

	tr, err := dave.Translate(ctx, "platform", "demo", "latest", "cursor-rules")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Version != "1.0.0" || tr.Name != "demo" || tr.Namespace != "platform" || tr.Digest != bundle.FromFiles(files).Digest {
		t.Errorf("translation meta = %+v", tr)
	}
	if tr.Result.Format != "cursor-rules" || len(tr.Result.Files) != 1 || tr.Result.Files[0].Path != ".cursor/rules/demo.mdc" {
		t.Fatalf("cursor-rules result = %+v", tr.Result)
	}
	mdc := string(tr.Result.Files[0].Content)
	for _, want := range []string{"---\ndescription: Review code carefully\n", "globs: **/*.go\n", "alwaysApply: false\n---\n", "# Demo\n\nDo the thing.\n"} {
		if !strings.Contains(mdc, want) {
			t.Errorf(".mdc lacks %q:\n%s", want, mdc)
		}
	}
	if len(tr.Result.Warnings) != 1 || !strings.Contains(tr.Result.Warnings[0], "2 bundled file(s) dropped") ||
		!strings.Contains(tr.Result.Warnings[0], "scripts/run.sh") || !strings.Contains(tr.Result.Warnings[0], "ref/a.md") {
		t.Errorf("warnings = %v", tr.Result.Warnings)
	}

	// Default format is claude: full directory, no warnings.
	resp := g.Do(t, http.MethodGet, "/v1/skills/platform/demo/versions/1.0.0/translate", gatewaytest.ReaderToken, nil, nil)
	var out client.Translation
	decode(t, resp, &out)
	var paths []string
	for _, f := range out.Result.Files {
		paths = append(paths, f.Path)
	}
	if out.Result.Format != "claude" || strings.Join(paths, " ") != ".claude/skills/demo/SKILL.md .claude/skills/demo/ref/a.md .claude/skills/demo/scripts/run.sh" || len(out.Result.Warnings) != 0 {
		t.Errorf("claude result = %+v", out.Result)
	}
	if string(out.Result.Files[0].Content) != md {
		t.Error("SKILL.md content altered by pass-through translation")
	}
	for _, f := range []string{"codex", "cursor", "copilot", "gemini", "kiro", "windsurf"} {
		if _, err := dave.Translate(ctx, "platform", "demo", "1.0.0", f); err != nil {
			t.Errorf("format %s: %v", f, err)
		}
	}
	resp = g.Do(t, http.MethodGet, "/v1/skills/platform/demo/versions/1.0.0/translate?format=emacs", gatewaytest.ReaderToken, nil, nil)
	expectError(t, resp, http.StatusBadRequest, "invalid_request")
	resp = g.Do(t, http.MethodGet, "/v1/formats", gatewaytest.ReaderToken, nil, nil)
	var fl struct {
		Formats []struct{ Format, Description string }
	}
	decode(t, resp, &fl)
	if len(fl.Formats) != 8 || fl.Formats[0].Format != "claude" || fl.Formats[1].Format != "codex" {
		t.Errorf("formats = %+v", fl.Formats)
	}
}

func TestListPagination(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	bob := g.Client(gatewaytest.PlatformToken)
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("skill-%02d", i)
		files := gatewaytest.Files(name, nil)
		if _, err := bob.Publish(ctx, "platform", name, "1.0.0", files); err != nil {
			t.Fatal(err)
		}
		// A second version on every other skill must not create extra items.
		if i%2 == 0 {
			if _, err := bob.Publish(ctx, "platform", name, "1.1.0", files); err != nil {
				t.Fatal(err)
			}
		}
	}
	seen := map[string]int{}
	cursor := ""
	var pages int
	for {
		page, err := bob.List(ctx, "", "", "", cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if pages < 3 && len(page.Items) != 10 || pages == 3 && len(page.Items) != 5 {
			t.Errorf("page %d has %d items", pages, len(page.Items))
		}
		for _, it := range page.Items {
			seen[it.Name]++
			if it.Version != "1.0.0" && it.Version != "1.1.0" {
				t.Errorf("%s: version %s", it.Name, it.Version)
			}
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor != "platform/"+page.Items[len(page.Items)-1].Name {
			t.Errorf("cursor %q is not the last key", page.NextCursor)
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination never terminates")
		}
	}
	if pages != 3 || len(seen) != 25 {
		t.Errorf("pages = %d, distinct = %d", pages, len(seen))
	}
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("skill-%02d", i)
		if seen[name] != 1 {
			t.Errorf("%s seen %d times", name, seen[name])
		}
	}
	// Default and oversized limits fall back to 50; a nonsense cursor is
	// past the end and yields an empty page with an items array.
	for _, limit := range []int{0, -1, 201} {
		resp := g.Do(t, http.MethodGet, fmt.Sprintf("/v1/skills?limit=%d", limit), gatewaytest.PlatformToken, nil, nil)
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"next_cursor"`
		}
		decode(t, resp, &page)
		if len(page.Items) != 25 || page.NextCursor != "" {
			t.Errorf("limit %d: %d items, cursor %q", limit, len(page.Items), page.NextCursor)
		}
	}
	resp := g.Do(t, http.MethodGet, "/v1/skills?cursor=zzz", gatewaytest.PlatformToken, nil, nil)
	if b := readAll(t, resp); !strings.Contains(string(b), `"items": []`) {
		t.Errorf("empty page body = %s", b)
	}
	// Query and namespace filters compose with pagination.
	page, _ := bob.List(ctx, "platform", "skill-1", "", "", 3)
	if len(page.Items) != 3 || page.NextCursor == "" {
		t.Errorf("filtered page = %+v", page)
	}
}

func TestListHidesDeniedSkills(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "shared", "1.0.0", gatewaytest.Files("shared", nil))
	g.Publish(t, gatewaytest.PlatformToken, "platform", "secret-x", "1.0.0", gatewaytest.Files("secret-x", nil))
	g.Publish(t, gatewaytest.PaymentsToken, "payments", "refunds", "1.0.0", gatewaytest.Files("refunds", nil))
	g.Publish(t, gatewaytest.AdminToken, "ops", "runbook", "1.0.0", gatewaytest.Files("runbook", nil))
	names := func(tok, ns string) string {
		page, err := g.Client(tok).List(ctx, ns, "", "", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range page.Items {
			out = append(out, it.Namespace+"/"+it.Name)
		}
		return strings.Join(out, " ")
	}
	if got := names(gatewaytest.ReaderToken, ""); got != "platform/secret-x platform/shared" {
		t.Errorf("dave sees %q", got)
	}
	if got := names(gatewaytest.BotToken, ""); got != "platform/shared" {
		t.Errorf("robot sees %q", got)
	}
	if got := names(gatewaytest.PaymentsToken, ""); got != "payments/refunds platform/secret-x platform/shared" {
		t.Errorf("carol sees %q", got)
	}
	if got := names(gatewaytest.AdminToken, ""); got != "ops/runbook payments/refunds platform/secret-x platform/shared" {
		t.Errorf("alice sees %q", got)
	}
	if got := names(gatewaytest.ReaderToken, "payments"); got != "" {
		t.Errorf("dave filtered to payments sees %q", got)
	}
	// A skill whose only versions are deprecated disappears from the list.
	g.Client(gatewaytest.PlatformToken).Deprecate(ctx, "platform", "shared", "1.0.0")
	if got := names(gatewaytest.ReaderToken, ""); got != "platform/secret-x" {
		t.Errorf("after deprecation dave sees %q", got)
	}
}

func TestDeprecate(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.0.0", gatewaytest.Files("demo", nil))
	bob := g.Client(gatewaytest.PlatformToken)
	for i := 0; i < 2; i++ {
		v, err := bob.Deprecate(ctx, "platform", "demo", "1.0.0")
		if err != nil || v.Status != store.Deprecated || v.Version != "1.0.0" {
			t.Errorf("deprecate #%d: %+v %v", i+1, v, err)
		}
	}
	_, err := g.Client(gatewaytest.ReaderToken).Deprecate(ctx, "platform", "demo", "1.0.0")
	clientErr(t, err, http.StatusForbidden, "forbidden")
	_, err = g.Client(gatewaytest.PaymentsToken).Deprecate(ctx, "platform", "demo", "1.0.0")
	clientErr(t, err, http.StatusForbidden, "forbidden")
	_, err = bob.Deprecate(ctx, "platform", "demo", "9.9.9")
	clientErr(t, err, http.StatusNotFound, "not_found")
	// Denied deprecation of a hidden skill is 403 (mutation), not 404.
	g.Publish(t, gatewaytest.PaymentsToken, "payments", "p", "1.0.0", gatewaytest.Files("p", nil))
	_, err = g.Client(gatewaytest.ReaderToken).Deprecate(ctx, "payments", "p", "1.0.0")
	clientErr(t, err, http.StatusForbidden, "forbidden")
	// Only one audit record for the two successful calls.
	var buf bytes.Buffer
	if err := g.Client(gatewaytest.AdminToken).Audit(ctx, "", &buf); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), `"action":"deprecate","namespace":"platform","name":"demo","version":"1.0.0","outcome":"ok"`); n != 1 {
		t.Errorf("deprecate ok events = %d, want 1:\n%s", n, buf.String())
	}
	if n := strings.Count(buf.String(), `"action":"deprecate","namespace":"platform","name":"demo","version":"1.0.0","outcome":"denied"`); n != 2 {
		t.Errorf("deprecate denied events = %d, want 2", n)
	}
}

func TestAuditExport(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	files := gatewaytest.Files("demo", nil)
	data, _ := bundle.Encode(files)
	resp := g.Do(t, http.MethodPut, "/v1/skills/platform/demo/versions/1.0.0", gatewaytest.PlatformToken, bytes.NewReader(data), nil)
	if resp.StatusCode != 201 {
		t.Fatal(resp.Status)
	}
	resp.Body.Close()
	reqID := resp.Header.Get("X-Request-Id")
	if len(reqID) != 16 {
		t.Fatalf("X-Request-Id = %q", reqID)
	}
	// A denied publish and a denied fetch are recorded too.
	g.Client(gatewaytest.ReaderToken).Publish(ctx, "platform", "demo", "2.0.0", files)
	g.Client(gatewaytest.ReaderToken).Get(ctx, "payments", "nothing", "1.0.0")
	g.Client(gatewaytest.ReaderToken).Bundle(ctx, "platform", "demo", "1.0.0")

	for _, tok := range []string{gatewaytest.ReaderToken, gatewaytest.PlatformToken, gatewaytest.PaymentsToken, gatewaytest.BotToken} {
		resp := g.Do(t, http.MethodGet, "/v1/audit", tok, nil, nil)
		expectError(t, resp, http.StatusForbidden, "forbidden")
	}
	resp = g.Do(t, http.MethodGet, "/v1/audit", gatewaytest.AdminToken, nil, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("audit: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var events []store.Event
	for _, line := range bytes.Split(bytes.TrimSpace(readAll(t, resp)), []byte("\n")) {
		var e store.Event
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		events = append(events, e)
	}
	find := func(action, outcome, subject string) *store.Event {
		for i := range events {
			e := &events[i]
			if e.Action == action && e.Outcome == outcome && e.Subject == subject {
				return e
			}
		}
		return nil
	}
	pub := find("publish", "ok", "bob")
	if pub == nil {
		t.Fatalf("no publish event: %+v", events)
	}
	if pub.RequestID != reqID || pub.Namespace != "platform" || pub.Name != "demo" || pub.Version != "1.0.0" ||
		pub.Detail != bundle.FromFiles(files).Digest || pub.RemoteAddr == "" || pub.Time.IsZero() {
		t.Errorf("publish event = %+v (want request_id %s)", pub, reqID)
	}
	if e := find("publish", "denied", "dave"); e == nil || e.Detail != "" || e.Version != "2.0.0" {
		t.Errorf("denied publish event = %+v", e)
	}
	if e := find("fetch", "denied", "dave"); e == nil || e.Namespace != "payments" {
		t.Errorf("denied fetch event = %+v", e)
	}
	if e := find("fetch", "ok", "dave"); e == nil || e.Detail != "bundle" {
		t.Errorf("bundle fetch event = %+v", e)
	}
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Errorf("events not in id order at %d", i)
		}
	}
	// since: RFC 3339 only; a future time yields nothing.
	resp = g.Do(t, http.MethodGet, "/v1/audit?since=yesterday", gatewaytest.AdminToken, nil, nil)
	expectError(t, resp, http.StatusBadRequest, "invalid_request")
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	resp = g.Do(t, http.MethodGet, "/v1/audit?since="+future, gatewaytest.AdminToken, nil, nil)
	if b := readAll(t, resp); resp.StatusCode != 200 || len(bytes.TrimSpace(b)) != 0 {
		t.Errorf("future since: %d %q", resp.StatusCode, b)
	}
	var buf bytes.Buffer
	if err := g.Client(gatewaytest.AdminToken).Audit(ctx, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), &buf); err != nil || buf.Len() == 0 {
		t.Errorf("client audit since: %v, %d bytes", err, buf.Len())
	}
}

func TestWhoami(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	resp := g.Do(t, http.MethodGet, "/v1/whoami", gatewaytest.BotToken, nil, nil)
	var p map[string]any
	decode(t, resp, &p)
	if p["subject"] != "robot" || p["agent_type"] != "bot" {
		t.Errorf("whoami = %v", p)
	}
	if _, ok := p["roles"]; ok {
		t.Errorf("empty roles should be omitted: %v", p)
	}
}

func TestVersionETagChangesWithStatus(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.0.0", gatewaytest.Files("demo", nil))
	path := "/v1/skills/platform/demo/versions/1.0.0"
	resp := g.Do(t, http.MethodGet, path, gatewaytest.ReaderToken, nil, nil)
	etag := resp.Header.Get("ETag")
	readAll(t, resp)
	if _, err := g.Client(gatewaytest.PlatformToken).Deprecate(ctx, "platform", "demo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	// The representation changed (status), so the old validator must not
	// produce a 304 or a cached client never learns of the deprecation.
	resp = g.Do(t, http.MethodGet, path, gatewaytest.ReaderToken, nil, map[string]string{"If-None-Match": etag})
	if resp.StatusCode != 200 {
		t.Fatalf("after deprecation with old ETag: %d (stale cache would hide the status change)", resp.StatusCode)
	}
	var v store.Version
	decode(t, resp, &v)
	if v.Status != store.Deprecated {
		t.Errorf("status = %s", v.Status)
	}
	if resp.Header.Get("ETag") == etag {
		t.Error("ETag unchanged although the body changed")
	}
	// Immutable content endpoints keep the digest as their validator.
	resp = g.Do(t, http.MethodGet, path+"/bundle", gatewaytest.ReaderToken, nil, map[string]string{"If-None-Match": `"` + v.Digest + `"`})
	readAll(t, resp)
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("bundle after deprecation: %d", resp.StatusCode)
	}
}

func TestConcurrentPublishAndRead(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	const n = 20
	errs := make(chan error, n*3)
	for i := 0; i < n; i++ {
		go func(i int) {
			name := fmt.Sprintf("par-%02d", i)
			_, err := g.Client(gatewaytest.PlatformToken).Publish(ctx, "platform", name, "1.0.0", gatewaytest.Files(name, map[string]string{"data.txt": randomText(4096)}))
			errs <- err
			_, err = g.Client(gatewaytest.ReaderToken).List(ctx, "", "", "", "", 0)
			errs <- err
			_, err = g.Client(gatewaytest.ReaderToken).Bundle(ctx, "platform", name, "latest")
			errs <- err
		}(i)
	}
	for i := 0; i < n*3; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent op: %v", err)
		}
	}
	page, err := g.Client(gatewaytest.ReaderToken).List(ctx, "", "", "", "", 0)
	if err != nil || len(page.Items) != n {
		t.Errorf("after concurrent publishes: %d items, %v", len(page.Items), err)
	}
	var buf bytes.Buffer
	g.Client(gatewaytest.AdminToken).Audit(ctx, "", &buf)
	if got := strings.Count(buf.String(), `"action":"publish"`); got != n {
		t.Errorf("publish audit events = %d, want %d", got, n)
	}
}
