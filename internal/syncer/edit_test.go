package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/source"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

const installed = ".claude/skills/demo/SKILL.md"

// skillDoc renders a SKILL.md for demo whose body is v.
func skillDoc(v string) string {
	return "---\nname: demo\ndescription: Skill demo\n---\n\n# demo\n\n" + v + "\n"
}

// folder is a path source holding one skill, demo, whose body the test
// can change between syncs.
type folder struct {
	t    *testing.T
	root string
}

func newFolder(t *testing.T, body string) *folder {
	t.Helper()
	f := &folder{t: t, root: t.TempDir()}
	f.set(body)
	return f
}

func (f *folder) set(body string) {
	f.t.Helper()
	p := filepath.Join(f.root, "demo", "SKILL.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(skillDoc(body)), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *folder) src() source.Source { return &source.Dir{Root: f.root} }

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hasWarning(rep Report, text string) bool {
	for _, w := range rep.Warnings {
		if strings.Contains(w, text) {
			return true
		}
	}
	return false
}

var demoManifest = &Manifest{Formats: []string{"claude"}, Skills: []string{"demo"}}

func syncDemo(t *testing.T, f *folder, root string, opt Options) Report {
	t.Helper()
	rep, err := SyncWith(ctx, f.src(), translate.Default(), root, demoManifest, opt)
	if err != nil {
		t.Fatalf("SyncWith: %v", err)
	}
	return rep
}

func TestEditedFileIsKeptUntilForced(t *testing.T) {
	f := newFolder(t, "v1")
	root := t.TempDir()
	if rep := syncDemo(t, f, root, Options{}); rep.Written != 1 || rep.Kept != 0 {
		t.Fatalf("first sync = %+v", rep)
	}

	// The developer edits the installed file, then upstream moves on.
	edit := skillDoc("my unpublished edit")
	write(t, root, installed, edit)
	f.set("v2")

	rep := syncDemo(t, f, root, Options{})
	if rep.Kept != 1 || rep.Written != 0 || rep.Removed != 0 {
		t.Errorf("sync over an edit = %+v", rep)
	}
	if !hasWarning(rep, "kept your edited "+installed) || !hasWarning(rep, "sync -force") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	if got := read(t, root, installed); got != edit {
		t.Errorf("the edit was overwritten:\n%s", got)
	}
	// What the lock records: the hash of the content the source offers,
	// not of the edited file. That is what keeps the file protected on
	// the next run (disk differs from the lock and from the incoming
	// bytes) and what marks it "locally modified" if it ever goes stale.
	lock := readLockFile(t, root)
	if len(lock.Skills) != 1 || len(lock.Skills[0].Files) != 1 {
		t.Fatalf("lock = %+v", lock)
	}
	if got := lock.Skills[0].Files[0].SHA256; got != sum(skillDoc("v2")) {
		if got == sum(edit) {
			t.Errorf("lock records the hash of the edited file")
		} else {
			t.Errorf("lock records %s, neither the source's nor the edit's hash", got)
		}
	}

	// It stays kept on every later run while the edit stands, including
	// one where upstream has not moved.
	for i := 0; i < 2; i++ {
		rep = syncDemo(t, f, root, Options{})
		if rep.Kept != 1 || rep.Written != 0 || read(t, root, installed) != edit {
			t.Errorf("run %d over the edit = %+v", i+2, rep)
		}
	}
	// A later upstream change is still not allowed to win.
	f.set("v3")
	if rep = syncDemo(t, f, root, Options{}); rep.Kept != 1 || read(t, root, installed) != edit {
		t.Errorf("v3 over the edit = %+v", rep)
	}

	// Force overwrites it, once, and reports nothing kept.
	rep = syncDemo(t, f, root, Options{Force: true})
	if rep.Kept != 0 || rep.Written != 1 || hasWarning(rep, "kept") {
		t.Errorf("forced sync = %+v", rep)
	}
	if got := read(t, root, installed); got != skillDoc("v3") {
		t.Errorf("forced sync did not overwrite:\n%s", got)
	}
	// Back to normal afterwards: the lock and the disk agree again.
	rep = syncDemo(t, f, root, Options{})
	if rep.Kept != 0 || rep.Written != 1 {
		t.Errorf("sync after force = %+v", rep)
	}
	if got := readLockFile(t, root).Skills[0].Files[0].SHA256; got != sum(skillDoc("v3")) {
		t.Errorf("lock after force = %s", got)
	}
}

func TestEditEqualToIncomingIsNotKept(t *testing.T) {
	f := newFolder(t, "v1")
	root := t.TempDir()
	syncDemo(t, f, root, Options{})

	// The developer hand-applies the exact change upstream then ships.
	write(t, root, installed, skillDoc("v2"))
	f.set("v2")
	rep := syncDemo(t, f, root, Options{})
	if rep.Kept != 0 || rep.Written != 1 || hasWarning(rep, "kept") {
		t.Errorf("sync with an edit equal to the incoming file = %+v", rep)
	}
	if got := read(t, root, installed); got != skillDoc("v2") {
		t.Errorf("file = %s", got)
	}
	if got := readLockFile(t, root).Skills[0].Files[0].SHA256; got != sum(skillDoc("v2")) {
		t.Errorf("lock = %s", got)
	}
}

func TestUnknownFileIsAdoptedNotProtected(t *testing.T) {
	f := newFolder(t, "v1")
	root := t.TempDir()
	// A file at the install path that no sync ever wrote: the first
	// install adopts it. A neighbour the source does not produce is not
	// touched at all.
	write(t, root, installed, skillDoc("an old copy from somewhere"))
	write(t, root, ".claude/skills/demo/notes.md", "my notes")

	rep := syncDemo(t, f, root, Options{})
	if rep.Kept != 0 || rep.Written != 1 || hasWarning(rep, "kept") {
		t.Errorf("first sync over an unknown file = %+v", rep)
	}
	if got := read(t, root, installed); got != skillDoc("v1") {
		t.Errorf("unknown file was not adopted:\n%s", got)
	}
	if got := read(t, root, ".claude/skills/demo/notes.md"); got != "my notes" {
		t.Errorf("a file sync does not produce was changed: %q", got)
	}

	// The same holds for a file the lock does not know because the lock
	// was lost: sync starts over and adopts, rather than refusing.
	os.Remove(filepath.Join(root, LockFile))
	write(t, root, installed, skillDoc("edited after the lock vanished"))
	f.set("v2")
	rep = syncDemo(t, f, root, Options{})
	if rep.Kept != 0 || rep.Written != 1 || read(t, root, installed) != skillDoc("v2") {
		t.Errorf("sync without a lock = %+v, file = %q", rep, read(t, root, installed))
	}
}

func TestDeletedFileIsRewrittenNotKept(t *testing.T) {
	f := newFolder(t, "v1")
	root := t.TempDir()
	syncDemo(t, f, root, Options{})
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(installed))); err != nil {
		t.Fatal(err)
	}
	rep := syncDemo(t, f, root, Options{})
	if rep.Kept != 0 || rep.Written != 1 || !exists(root, installed) {
		t.Errorf("sync after a delete = %+v, exists=%v", rep, exists(root, installed))
	}
}

func TestKeptEditSurvivesLeavingTheManifest(t *testing.T) {
	f := newFolder(t, "v1")
	root := t.TempDir()
	syncDemo(t, f, root, Options{})
	edit := skillDoc("edited")
	write(t, root, installed, edit)
	f.set("v2")
	syncDemo(t, f, root, Options{}) // kept; lock now carries v2's hash

	// The skill leaves the manifest: the stale-file removal must see the
	// edit as a local modification and leave it alone.
	f2 := newFolder(t, "v2")
	os.Rename(filepath.Join(f2.root, "demo"), filepath.Join(f2.root, "other"))
	write(t, f2.root, "other/SKILL.md", "---\nname: other\ndescription: Skill other\n---\n\n# other\n")
	m := &Manifest{Formats: []string{"claude"}, Skills: []string{"other"}}
	rep, err := Sync(ctx, f2.src(), translate.Default(), root, m)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rep.Removed != 0 || !hasWarning(rep, "kept locally modified stale file "+installed) {
		t.Errorf("report = %+v", rep)
	}
	if got := read(t, root, installed); got != edit {
		t.Errorf("edit lost when the skill left the manifest: %q", got)
	}
}
