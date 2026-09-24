package syncer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/source"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

// git runs a command in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// mustWrite writes a file under dir, creating parents.
func mustWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A tag is a fixed point. If it is moved to another commit, the next sync
// must refuse it rather than quietly installing different content.
func TestMovedTagIsRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	git(t, repo, "init", "--quiet", "-b", "main")
	write := func(text string) {
		mustWrite(t, repo, "demo/SKILL.md", "---\nname: demo\ndescription: "+text+"\n---\nbody\n")
		git(t, repo, "add", "-A")
		git(t, repo, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", text)
	}
	write("first")
	git(t, repo, "tag", "v1")

	cache := t.TempDir()
	m := &Manifest{Formats: []string{"claude"}, Skills: []string{"demo"}}
	newSrc := func() source.Source {
		t.Helper()
		src, err := source.NewGit(ctx, source.GitOptions{URL: repo, Ref: "v1", Cache: cache})
		if err != nil {
			t.Fatalf("NewGit: %v", err)
		}
		return src
	}

	root := t.TempDir()
	rep, err := Sync(ctx, newSrc(), translate.Default(), root, m)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first := rep.Skills[0].Version

	// The tag now points at different content.
	write("second")
	git(t, repo, "tag", "-f", "v1")

	_, err = Sync(ctx, newSrc(), translate.Default(), root, m)
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("a moved tag must be refused, got %v", err)
	}
	if got := read(t, root, ".claude/skills/demo/SKILL.md"); !strings.Contains(got, "first") {
		t.Errorf("installed files changed despite the failure: %q", got)
	}
	if lock := readLockFile(t, root); lock.Skills[0].Version != first {
		t.Errorf("lock was rewritten: %+v", lock.Skills[0])
	}
}
