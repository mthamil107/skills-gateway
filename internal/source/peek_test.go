package source

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mthamil107/skills-gateway/internal/client"
	"github.com/mthamil107/skills-gateway/internal/gatewaytest"
	"github.com/mthamil107/skills-gateway/internal/registry"
	"github.com/mthamil107/skills-gateway/internal/store"
)

func peek(t *testing.T, p Peeker) string {
	t.Helper()
	s, err := p.Peek(ctx)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	return s
}

func TestDirPeek(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "cherry/SKILL.md", skillMD("cherry"))
	mk(t, root, "cherry/scripts/run.sh", "echo cherry\n")
	d := &Dir{Root: root}

	p1 := peek(t, d)
	if !strings.HasPrefix(p1, "dir:") || len(p1) != len("dir:")+32 {
		t.Errorf("Peek = %q, want dir: plus 32 hex chars", p1)
	}
	if p2 := peek(t, d); p2 != p1 {
		t.Errorf("Peek is not stable: %q then %q", p1, p2)
	}

	// A content change (a different size) changes it.
	mk(t, root, "cherry/scripts/run.sh", "echo cherry, but longer\n")
	p3 := peek(t, d)
	if p3 == p1 {
		t.Error("content change not detected")
	}

	// A modification time change alone changes it.
	run := filepath.Join(root, "cherry", "scripts", "run.sh")
	fi, _ := os.Stat(run)
	if err := os.Chtimes(run, fi.ModTime().Add(time.Hour), fi.ModTime().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p4 := peek(t, d)
	if p4 == p3 {
		t.Error("mtime change not detected")
	}

	// Hidden, node_modules and vendor trees are not part of the offer.
	for _, rel := range []string{".git/HEAD", ".hidden/x/SKILL.md", "node_modules/x/SKILL.md", "vendor/x/SKILL.md"} {
		mk(t, root, rel, "ignored")
	}
	if p5 := peek(t, d); p5 != p4 {
		t.Error("hidden or vendor directories changed the fingerprint")
	}

	// A new visible file changes it; removing it restores the old value.
	mk(t, root, "cherry/ref/notes.md", "notes")
	p6 := peek(t, d)
	if p6 == p4 {
		t.Error("new file not detected")
	}
	os.Remove(filepath.Join(root, "cherry", "ref", "notes.md"))
	os.Remove(filepath.Join(root, "cherry", "ref"))
	if p7 := peek(t, d); p7 != p4 {
		t.Errorf("removing the file did not restore the fingerprint: %q vs %q", p7, p4)
	}

	// Documented limit: no content is read, so a same-size rewrite whose
	// mtime is put back is invisible. A real edit always moves the mtime.
	same := []byte("echo CHERRY, but longer\n")
	fi, _ = os.Stat(run)
	if err := os.WriteFile(run, same, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(run, fi.ModTime(), fi.ModTime())
	if p8 := peek(t, d); p8 != p4 {
		t.Errorf("a same-size rewrite with the old mtime changed the fingerprint; Peek must not read content")
	}

	// Two roots with the same layout differ only by content, size or time.
	other := t.TempDir()
	mk(t, other, "cherry/SKILL.md", skillMD("cherry"))
	if po := peek(t, &Dir{Root: other}); po == p4 || !strings.HasPrefix(po, "dir:") {
		t.Errorf("unrelated root has fingerprint %q", po)
	}

	if _, err := (&Dir{Root: filepath.Join(root, "missing")}).Peek(ctx); err == nil {
		t.Error("a missing root must fail to peek")
	}
}

func TestPeekGit(t *testing.T) {
	r := newRepo(t)
	first := r.commit("first")
	r.git("tag", "v1") // lightweight tag at first
	mk(t, r.path, "skills/demo/ref/more.md", "more")
	second := r.commit("second")
	r.git("tag", "-a", "v2", "-m", "release two") // annotated tag at second
	tagObject := r.git("rev-parse", "v2")
	if tagObject == second {
		t.Fatal("test setup: an annotated tag must be its own object")
	}

	cases := []struct {
		ref  string
		want string
	}{
		{"main", second},
		{"", second}, // HEAD
		{"HEAD", second},
		{"v1", first},
		{"v2", second}, // the commit, not the tag object
		{"refs/tags/v2", second},
		{second, second}, // a commit cannot move; no round trip needed
		{first, first},
	}
	for _, c := range cases {
		got, err := PeekGit(ctx, GitOptions{URL: r.path, Ref: c.ref})
		if err != nil {
			t.Errorf("PeekGit(%q): %v", c.ref, err)
			continue
		}
		if got != "git:"+c.want {
			t.Errorf("PeekGit(%q) = %q, want git:%s", c.ref, got, c.want)
		}
	}
	// An unknown ref is a clear error, not an empty fingerprint.
	if got, err := PeekGit(ctx, GitOptions{URL: r.path, Ref: "nope"}); err == nil || !strings.Contains(err.Error(), `"nope"`) || got != "" {
		t.Errorf("PeekGit(nope) = %q, %v", got, err)
	}
	// So is an unreachable repository.
	if _, err := PeekGit(ctx, GitOptions{URL: filepath.Join(t.TempDir(), "absent"), Ref: "main"}); err == nil {
		t.Error("PeekGit against a missing repository succeeded")
	}
	// A credential in the URL never reaches the error text.
	if _, err := PeekGit(ctx, GitOptions{URL: "https://user:s3cret@127.0.0.1:9/none.git", Ref: "main"}); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("credential leaked or no error: %v", err)
	}

	// The Git source peeks the same commit it checks out.
	src, err := NewGit(ctx, GitOptions{URL: r.path, Ref: "v2", Dir: "skills", Cache: t.TempDir()})
	if err != nil {
		t.Fatalf("NewGit: %v", err)
	}
	if got := peek(t, src); got != "git:"+second {
		t.Errorf("Git.Peek = %q, want git:%s", got, second)
	}
	sk, err := src.Resolve(ctx, Ref{Name: "demo", Version: "latest"})
	if err != nil || !strings.HasPrefix(second, sk.Version) {
		t.Errorf("Resolve version %q does not match peeked commit %s (%v)", sk.Version, second, err)
	}

	// A branch moves with its commits.
	mk(t, r.path, "skills/extra/ref/x.md", "x")
	third := r.commit("third")
	if got, _ := PeekGit(ctx, GitOptions{URL: r.path, Ref: "main"}); got != "git:"+third {
		t.Errorf("after a new commit PeekGit(main) = %q, want git:%s", got, third)
	}
	if got, _ := PeekGit(ctx, GitOptions{URL: r.path, Ref: "v2"}); got != "git:"+second {
		t.Errorf("the tag moved with the branch: %q", got)
	}
}

func TestGatewayPeek(t *testing.T) {
	g, src := gatewayFixture(t)
	p1 := peek(t, src)
	if !strings.HasPrefix(p1, "gw:") || len(p1) != len("gw:")+32 {
		t.Errorf("Peek = %q", p1)
	}
	if p2 := peek(t, src); p2 != p1 {
		t.Errorf("Peek is not stable: %q then %q", p1, p2)
	}
	// A new version of an existing skill changes it.
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.2.0", gatewaytest.Files("demo", map[string]string{"ref/new.md": "new"}))
	p3 := peek(t, src)
	if p3 == p1 {
		t.Error("a new published version was not detected")
	}
	if p4 := peek(t, src); p4 != p3 {
		t.Error("Peek is not stable after a publish")
	}
	// A brand new skill changes it.
	g.Publish(t, gatewaytest.PaymentsToken, "payments", "chargebacks", "1.0.0", gatewaytest.Files("chargebacks", nil))
	if p5 := peek(t, src); p5 == p3 {
		t.Error("a new skill was not detected")
	}
	// The fingerprint is of what this caller may see: a reader who cannot
	// fetch payments/* sees a different offer than the admin.
	reader := &Gateway{Client: g.Client(gatewaytest.ReaderToken)}
	if pr := peek(t, reader); pr == peek(t, src) {
		t.Error("two principals with different visibility share a fingerprint")
	}
	// An unusable token is an error, never a fingerprint of nothing.
	if _, err := (&Gateway{Client: g.Client("not-a-token-0123456789")}).Peek(ctx); err == nil {
		t.Error("Peek with a bad token succeeded")
	}
}

// TestGatewayPeekPages serves the list in two pages and checks that Peek
// follows the cursor and hashes the whole offer, not just the first page.
func TestGatewayPeekPages(t *testing.T) {
	items := []store.Version{
		{Namespace: "a", Name: "one", Version: "1.0.0", Digest: "sha256:1"},
		{Namespace: "a", Name: "two", Version: "1.0.0", Digest: "sha256:2"},
		{Namespace: "b", Name: "three", Version: "2.0.0", Digest: "sha256:3"},
	}
	serve := func(pageSize int) (*httptest.Server, *int32) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			if r.URL.Path != "/v1/skills" {
				http.NotFound(w, r)
				return
			}
			if r.URL.Query().Get("limit") != "200" {
				t.Errorf("limit = %q, want the maximum page size", r.URL.Query().Get("limit"))
			}
			start := 0
			if c := r.URL.Query().Get("cursor"); c != "" {
				for i, v := range items {
					if v.Namespace+"/"+v.Name == c {
						start = i + 1
					}
				}
			}
			end := start + pageSize
			if end > len(items) {
				end = len(items)
			}
			page := registry.Page{Items: items[start:end]}
			if end < len(items) {
				last := items[end-1]
				page.NextCursor = last.Namespace + "/" + last.Name
			}
			json.NewEncoder(w).Encode(page)
		}))
		t.Cleanup(srv.Close)
		return srv, &calls
	}
	paged, pagedCalls := serve(2)
	whole, wholeCalls := serve(3)
	gwFor := func(s *httptest.Server) *Gateway {
		return &Gateway{Client: &client.Client{BaseURL: s.URL, Token: "t", HTTP: s.Client()}}
	}
	a := peek(t, gwFor(paged))
	b := peek(t, gwFor(whole))
	if a != b {
		t.Errorf("paged fingerprint %q differs from whole %q", a, b)
	}
	if atomic.LoadInt32(pagedCalls) != 2 || atomic.LoadInt32(wholeCalls) != 1 {
		t.Errorf("requests: paged=%d whole=%d", *pagedCalls, *wholeCalls)
	}
	// A change on the second page is seen.
	items[2].Digest = "sha256:changed"
	if c := peek(t, gwFor(paged)); c == a {
		t.Error("a change on the second page was not detected")
	}
}
