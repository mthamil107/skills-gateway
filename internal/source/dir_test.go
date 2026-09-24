package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/bundle"
)

var ctx = context.Background()

// skillMD renders a minimal SKILL.md.
func skillMD(name string) string {
	return "---\nname: " + name + "\ndescription: Skill " + name + "\n---\n\n# " + name + "\n"
}

// mk writes rel (slash-separated) under root, creating parents.
func mk(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tree builds the standard fixture: three skills at different depths plus
// hidden, node_modules and vendor trees that must be ignored.
func tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk(t, root, "cherry/SKILL.md", skillMD("cherry"))
	mk(t, root, "cherry/scripts/run.sh", "echo cherry\n")
	mk(t, root, "cherry/ref/notes.md", "notes")
	mk(t, root, "group/apple/SKILL.md", skillMD("apple"))
	mk(t, root, "group/deeper/still/banana/SKILL.md", skillMD("banana"))
	mk(t, root, "group/README.md", "not a skill")
	mk(t, root, ".git/hidden/SKILL.md", skillMD("hidden"))
	mk(t, root, ".hidden-skill/SKILL.md", skillMD("hidden-skill"))
	mk(t, root, "node_modules/pkg/SKILL.md", skillMD("pkg"))
	mk(t, root, "vendor/dep/SKILL.md", skillMD("dep"))
	mk(t, root, "cherry/.env", "TOKEN=secret")
	mk(t, root, "cherry/.cache/x.txt", "cached")
	return root
}

func names(refs []Ref) string {
	var out []string
	for _, r := range refs {
		out = append(out, r.String())
	}
	return strings.Join(out, " ")
}

func TestDirListDiscoversNestedSkillsSorted(t *testing.T) {
	d := &Dir{Root: tree(t)}
	refs, err := d.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := names(refs); got != "apple banana cherry" {
		t.Errorf("List = %q, want sorted apple banana cherry", got)
	}
	for _, r := range refs {
		if r.Namespace != "" || r.Version != "latest" {
			t.Errorf("List ref = %+v, want no namespace and version latest", r)
		}
	}
	if d.Pinnable() {
		t.Error("a folder must not be pinnable")
	}
	if !strings.HasPrefix(d.Describe(), "path:") || strings.Contains(d.Describe(), `\`) {
		t.Errorf("Describe = %q", d.Describe())
	}
	// A folder that merely contains skills, but has no SKILL.md of its
	// own, is not a skill.
	for _, ghost := range []string{"group", "deeper", "still", "hidden", "hidden-skill", "pkg", "dep"} {
		if _, err := d.Resolve(ctx, Ref{Name: ghost, Version: "latest"}); err == nil {
			t.Errorf("Resolve(%q) succeeded for a non-skill folder", ghost)
		}
	}
}

func TestDirResolveDigestMatchesBundle(t *testing.T) {
	root := tree(t)
	d := &Dir{Root: root}
	sk, err := d.Resolve(ctx, Ref{Name: "cherry", Version: "latest"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sk.Name != "cherry" || sk.Namespace != "" || sk.Version != "-" {
		t.Errorf("skill = %+v", sk)
	}
	var paths []string
	for _, f := range sk.Files {
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, " "); got != "SKILL.md ref/notes.md scripts/run.sh" {
		t.Errorf("files = %v (hidden files must be skipped, order sorted)", paths)
	}
	want := bundle.FromFiles([]bundle.File{
		{Path: "SKILL.md", Content: []byte(skillMD("cherry"))},
		{Path: "scripts/run.sh", Content: []byte("echo cherry\n")},
		{Path: "ref/notes.md", Content: []byte("notes")},
	})
	if sk.Digest != want.Digest {
		t.Errorf("digest = %s, want %s", sk.Digest, want.Digest)
	}
	if bundle.FromFiles(sk.Files).Digest != sk.Digest {
		t.Error("digest does not cover the returned files")
	}
	// Every returned file carries its own hash, the way a bundle does.
	for _, f := range sk.Files {
		if f.SHA256 == "" || f.Size != int64(len(f.Content)) {
			t.Errorf("file %s lacks size/sha256: %+v", f.Path, f)
		}
	}

	// A namespaced reference is accepted and the namespace is echoed
	// back, so one manifest works against a folder and a gateway.
	sk2, err := d.Resolve(ctx, Ref{Namespace: "platform", Name: "cherry", Version: "latest"})
	if err != nil {
		t.Fatalf("namespaced Resolve: %v", err)
	}
	if sk2.Namespace != "platform" || sk2.Digest != sk.Digest {
		t.Errorf("namespaced skill = %+v", sk2)
	}
	// Version and Pin are recorded verbatim (a git source sets them).
	pinned := &Dir{Root: root, Version: "abc123", Pinned: true, Label: "git:x"}
	sk3, err := pinned.Resolve(ctx, Ref{Name: "apple", Version: "latest"})
	if err != nil || sk3.Version != "abc123" || !pinned.Pinnable() || pinned.Describe() != "git:x" {
		t.Errorf("labelled dir = %+v %v pinnable=%v describe=%q", sk3, err, pinned.Pinnable(), pinned.Describe())
	}
	if _, err := d.Resolve(ctx, Ref{Name: "ghost", Version: "latest"}); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("missing skill: %v", err)
	}
}

func TestDirErrors(t *testing.T) {
	// Two skills with the same folder name in different subtrees.
	dup := t.TempDir()
	mk(t, dup, "a/demo/SKILL.md", skillMD("demo"))
	mk(t, dup, "b/demo/SKILL.md", skillMD("demo"))
	d := &Dir{Root: dup}
	if _, err := d.List(ctx); err == nil || !strings.Contains(err.Error(), `"demo"`) {
		t.Errorf("duplicate names: %v", err)
	}
	if _, err := d.Resolve(ctx, Ref{Name: "demo"}); err == nil {
		t.Error("Resolve succeeded with duplicate skill names")
	}

	// No SKILL.md anywhere, including only in ignored trees.
	empty := t.TempDir()
	mk(t, empty, "README.md", "nothing here")
	mk(t, empty, ".git/x/SKILL.md", skillMD("x"))
	mk(t, empty, "node_modules/y/SKILL.md", skillMD("y"))
	mk(t, empty, "vendor/z/SKILL.md", skillMD("z"))
	if _, err := (&Dir{Root: empty}).List(ctx); err == nil || !strings.Contains(err.Error(), "no SKILL.md") {
		t.Errorf("empty tree: %v", err)
	}
	if _, err := (&Dir{Root: filepath.Join(empty, "missing")}).List(ctx); err == nil {
		t.Error("missing root accepted")
	}

	// SKILL.md name must match the folder name.
	mism := t.TempDir()
	mk(t, mism, "demo/SKILL.md", skillMD("other"))
	if _, err := (&Dir{Root: mism}).Resolve(ctx, Ref{Name: "demo"}); err == nil || !strings.Contains(err.Error(), "does not match its folder") {
		t.Errorf("name mismatch: %v", err)
	}
	// A folder name that is not a valid skill name cannot be resolved by
	// a parsed ref, and an invalid frontmatter is a parse error.
	bad := t.TempDir()
	mk(t, bad, "demo/SKILL.md", "---\nname: demo\n---\n")
	if _, err := (&Dir{Root: bad}).Resolve(ctx, Ref{Name: "demo"}); err == nil || !strings.Contains(err.Error(), "description") {
		t.Errorf("invalid SKILL.md: %v", err)
	}
}

func TestDirRefusesSymlinkInsideSkill(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "demo/SKILL.md", skillMD("demo"))
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "demo", "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := (&Dir{Root: root}).Resolve(ctx, Ref{Name: "demo"})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked file accepted: %v", err)
	}
	// A symlinked directory is refused too.
	os.Remove(filepath.Join(root, "demo", "link.txt"))
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "demo", "linkdir")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if _, err := (&Dir{Root: root}).Resolve(ctx, Ref{Name: "demo"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked directory accepted: %v", err)
	}
}

func TestDirEnforcesBundleLimits(t *testing.T) {
	lim := bundle.DefaultLimits

	many := t.TempDir()
	mk(t, many, "demo/SKILL.md", skillMD("demo"))
	for i := 0; i < lim.MaxFiles; i++ { // SKILL.md makes it MaxFiles+1
		mk(t, many, fmt.Sprintf("demo/f/%d.txt", i), "x")
	}
	if _, err := (&Dir{Root: many}).Resolve(ctx, Ref{Name: "demo"}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("too many files accepted: %v", err)
	}

	big := t.TempDir()
	mk(t, big, "demo/SKILL.md", skillMD("demo"))
	p := filepath.Join(big, "demo", "big.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(lim.MaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := (&Dir{Root: big}).Resolve(ctx, Ref{Name: "demo"}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized file accepted: %v", err)
	}
	// Exactly at the limit is fine.
	os.Truncate(p, lim.MaxFileBytes)
	if _, err := (&Dir{Root: big}).Resolve(ctx, Ref{Name: "demo"}); err != nil {
		t.Errorf("file at the limit rejected: %v", err)
	}
}
