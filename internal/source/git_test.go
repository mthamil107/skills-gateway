package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/bundle"
)

// repo is a local git repository the tests commit to.
type repo struct {
	t    *testing.T
	path string
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = r.path
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "--quiet", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// newRepo creates a repository on branch main with skills under skills/.
func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	// Run every git in this test, including the source's own, with the
	// worst-case user configuration: autocrlf rewrites line endings on
	// checkout, which must never reach the synced files.
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[core]\n\tautocrlf = true\n[init]\n\tdefaultBranch = main\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	r := &repo{t: t, path: t.TempDir()}
	r.git("init", "--quiet", "-b", "main")
	mk(t, r.path, "skills/demo/SKILL.md", skillMD("demo"))
	mk(t, r.path, "skills/demo/scripts/run.sh", "echo demo\n")
	mk(t, r.path, "skills/extra/SKILL.md", skillMD("extra"))
	mk(t, r.path, "README.md", "not a skill")
	return r
}

func TestGitResolvesSkillsAndRecordsCommit(t *testing.T) {
	r := newRepo(t)
	head := r.commit("first")
	cache := t.TempDir()

	src, err := NewGit(ctx, GitOptions{URL: r.path, Dir: "skills", Cache: cache})
	if err != nil {
		t.Fatalf("NewGit: %v", err)
	}
	if src.Pinnable() {
		t.Error("the default (remote HEAD) branch must not be pinnable")
	}
	refs, err := src.List(ctx)
	if err != nil || names(refs) != "demo extra" {
		t.Errorf("List = %q, %v", names(refs), err)
	}
	sk, err := src.Resolve(ctx, Ref{Name: "demo", Version: "latest"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sk.Version != head[:12] {
		t.Errorf("Version = %q, want short commit %q", sk.Version, head[:12])
	}
	if len(sk.Files) != 2 || string(sk.Files[1].Content) != "echo demo\n" || string(sk.Files[0].Content) != skillMD("demo") {
		t.Errorf("files are not byte-exact (line endings rewritten?): %+v", sk.Files)
	}
	want := bundle.FromFiles([]bundle.File{
		{Path: "SKILL.md", Content: []byte(skillMD("demo"))},
		{Path: "scripts/run.sh", Content: []byte("echo demo\n")},
	})
	if sk.Digest != want.Digest {
		t.Errorf("digest = %s, want the platform-independent %s", sk.Digest, want.Digest)
	}
	// The label is stable: it carries the ref as written, never the
	// resolved commit, so a lock file does not churn on every upstream push.
	if !strings.HasPrefix(src.Describe(), "git:"+r.path) || strings.Contains(src.Describe(), head[:12]) {
		t.Errorf("Describe = %q", src.Describe())
	}
	// The checkout lives under the cache, not the user's cache dir.
	entries, _ := os.ReadDir(cache)
	if len(entries) != 1 {
		t.Errorf("cache holds %d entries, want 1 clone", len(entries))
	}
	// Without dir, the whole repository is the tree: README.md is not a
	// skill and the skills are still found.
	whole, err := NewGit(ctx, GitOptions{URL: r.path, Cache: cache})
	if err != nil {
		t.Fatalf("NewGit without dir: %v", err)
	}
	if refs, err := whole.List(ctx); err != nil || names(refs) != "demo extra" {
		t.Errorf("List without dir = %q, %v", names(refs), err)
	}
}

func TestGitPinnableForTagAndCommitOnly(t *testing.T) {
	r := newRepo(t)
	first := r.commit("first")
	r.git("tag", "v1")
	mk(t, r.path, "skills/demo/SKILL.md", skillMD("demo")+"\nsecond\n")
	second := r.commit("second")
	r.git("branch", "release")

	cache := t.TempDir()
	for _, c := range []struct {
		ref     string
		want    string
		pinned  bool
		content string
	}{
		{"main", second, false, "second"},
		{"release", second, false, "second"},
		{"v1", first, true, ""},
		{first, first, true, ""},
		{first[:10], first, true, ""},
	} {
		src, err := NewGit(ctx, GitOptions{URL: r.path, Ref: c.ref, Dir: "skills", Cache: cache})
		if err != nil {
			t.Fatalf("NewGit(%s): %v", c.ref, err)
		}
		if src.Pinnable() != c.pinned {
			t.Errorf("ref %s: Pinnable = %v, want %v", c.ref, src.Pinnable(), c.pinned)
		}
		sk, err := src.Resolve(ctx, Ref{Name: "demo"})
		if err != nil {
			t.Fatalf("ref %s: Resolve: %v", c.ref, err)
		}
		if sk.Version != c.want[:12] {
			t.Errorf("ref %s: Version = %s, want %s", c.ref, sk.Version, c.want[:12])
		}
		md := string(sk.Files[0].Content)
		if strings.Contains(md, "second") != (c.content == "second") {
			t.Errorf("ref %s: SKILL.md content = %q", c.ref, md)
		}
	}
}

// A branch is a moving target: a second sync from the same cache must see
// commits pushed after the first clone.
func TestGitBranchFollowsRemoteAfterFirstClone(t *testing.T) {
	r := newRepo(t)
	first := r.commit("first")
	cache := t.TempDir()
	for _, ref := range []string{"", "main"} {
		src, err := NewGit(ctx, GitOptions{URL: r.path, Ref: ref, Dir: "skills", Cache: cache})
		if err != nil {
			t.Fatalf("NewGit(%q): %v", ref, err)
		}
		if sk, _ := src.Resolve(ctx, Ref{Name: "demo"}); sk.Version != first[:12] {
			t.Fatalf("ref %q: first sync = %s, want %s", ref, sk.Version, first[:12])
		}
	}
	mk(t, r.path, "skills/demo/SKILL.md", skillMD("demo")+"\nmoved\n")
	moved := r.commit("moved")
	for _, ref := range []string{"", "main"} {
		src, err := NewGit(ctx, GitOptions{URL: r.path, Ref: ref, Dir: "skills", Cache: cache})
		if err != nil {
			t.Fatalf("NewGit(%q) again: %v", ref, err)
		}
		sk, err := src.Resolve(ctx, Ref{Name: "demo"})
		if err != nil {
			t.Fatal(err)
		}
		if sk.Version != moved[:12] || !strings.Contains(string(sk.Files[0].Content), "moved") {
			t.Errorf("ref %q: second sync stayed at %s, want %s (branch did not follow the remote)", ref, sk.Version, moved[:12])
		}
	}
}

func TestGitErrors(t *testing.T) {
	r := newRepo(t)
	r.commit("first")
	cache := t.TempDir()
	_, err := NewGit(ctx, GitOptions{URL: r.path, Dir: "nope", Cache: cache})
	if err == nil || !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing dir: %v", err)
	}
	if _, err := NewGit(ctx, GitOptions{URL: r.path, Ref: "no-such-ref", Cache: cache}); err == nil {
		t.Error("unknown ref accepted")
	}
	if _, err := NewGit(ctx, GitOptions{Cache: cache}); err == nil || !strings.Contains(err.Error(), "url") {
		t.Errorf("missing url: %v", err)
	}
	if _, err := NewGit(ctx, GitOptions{URL: filepath.Join(t.TempDir(), "not-a-repo"), Cache: cache}); err == nil {
		t.Error("clone of a missing repository succeeded")
	}
}
