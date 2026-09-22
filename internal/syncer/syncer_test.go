package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/client"
	"github.com/mthamil107/skills-gateway/internal/gatewaytest"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

var ctx = context.Background()

func setup(t *testing.T) (*gatewaytest.Gateway, *client.Client, string) {
	t.Helper()
	g := gatewaytest.New(t, gatewaytest.Options{})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.0.0", gatewaytest.Files("demo", map[string]string{"scripts/run.sh": "echo demo\n"}))
	g.Publish(t, gatewaytest.PlatformToken, "platform", "other", "1.0.0", gatewaytest.Files("other", map[string]string{"scripts/run.sh": "echo other\n", "ref/a.md": "a"}))
	g.Publish(t, gatewaytest.PaymentsToken, "payments", "demo", "1.0.0", gatewaytest.Files("demo", nil))
	return g, g.Client(gatewaytest.AdminToken), t.TempDir()
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func exists(root, rel string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

func readLockFile(t *testing.T, root string) Lock {
	t.Helper()
	var l Lock
	if err := json.Unmarshal([]byte(read(t, root, LockFile)), &l); err != nil {
		t.Fatalf("lock: %v", err)
	}
	return l
}

// listFiles returns every regular file under root, slash-separated.
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func TestSyncWritesNativeLayoutsAndLock(t *testing.T) {
	_, c, root := setup(t)
	m := &Manifest{Formats: []string{"claude", "cursor-rules"}, Skills: []string{"platform/demo"}}
	rep, err := Sync(ctx, c, translate.Default(), root, m)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rep.Written != 3 || rep.Removed != 0 || len(rep.Skills) != 1 {
		t.Errorf("report = %+v", rep)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "cursor-rules carries only SKILL.md") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	if got := read(t, root, ".claude/skills/demo/SKILL.md"); got != gatewaytest.SkillMD("demo", "Skill demo") {
		t.Errorf("SKILL.md = %q", got)
	}
	if got := read(t, root, ".claude/skills/demo/scripts/run.sh"); got != "echo demo\n" {
		t.Errorf("run.sh = %q", got)
	}
	mdc := read(t, root, ".cursor/rules/demo.mdc")
	if !strings.HasPrefix(mdc, "---\ndescription: Skill demo\nglobs:\nalwaysApply: false\n---\n") || !strings.Contains(mdc, "# demo") {
		t.Errorf("demo.mdc = %q", mdc)
	}
	want := []string{".claude/skills/demo/SKILL.md", ".claude/skills/demo/scripts/run.sh", ".cursor/rules/demo.mdc", LockFile}
	if got := listFiles(t, root); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("files on disk = %v", got)
	}
	if exists(root, ".claude/skills/demo/SKILL.md.sgw-tmp") {
		t.Error("temp file left behind")
	}

	lock := readLockFile(t, root)
	if len(lock.Skills) != 1 {
		t.Fatalf("lock = %+v", lock)
	}
	s := lock.Skills[0]
	v, _ := c.Get(ctx, "platform", "demo", "1.0.0")
	if s.Ref != "platform/demo" || s.Version != "1.0.0" || s.Digest != v.Digest || len(s.Files) != 3 {
		t.Errorf("locked skill = %+v", s)
	}
	for _, f := range s.Files {
		sum := sha256.Sum256([]byte(read(t, root, f.Path)))
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			t.Errorf("lock sha256 for %s does not match disk", f.Path)
		}
	}
	// A second identical sync is a no-op that rewrites the same lock.
	before := read(t, root, LockFile)
	rep, err = Sync(ctx, c, translate.Default(), root, m)
	if err != nil || rep.Removed != 0 || read(t, root, LockFile) != before {
		t.Errorf("idempotent sync: %+v %v", rep, err)
	}
}

func TestSyncRemovesStaleFilesAfterManifestShrink(t *testing.T) {
	_, c, root := setup(t)
	full := &Manifest{Formats: []string{"claude", "cursor-rules"}, Skills: []string{"platform/demo", "platform/other"}}
	if _, err := Sync(ctx, c, translate.Default(), root, full); err != nil {
		t.Fatal(err)
	}
	if !exists(root, ".claude/skills/other/ref/a.md") || !exists(root, ".cursor/rules/other.mdc") {
		t.Fatal("first sync did not write other")
	}
	// An unrelated user file in a shared directory must survive.
	os.WriteFile(filepath.Join(root, ".cursor", "rules", "mine.mdc"), []byte("mine"), 0o644)

	shrunk := &Manifest{Formats: []string{"claude", "cursor-rules"}, Skills: []string{"platform/demo"}}
	rep, err := Sync(ctx, c, translate.Default(), root, shrunk)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Removed != 4 || len(rep.Warnings) != 1 {
		t.Errorf("report = %+v", rep)
	}
	want := []string{".claude/skills/demo/SKILL.md", ".claude/skills/demo/scripts/run.sh", ".cursor/rules/demo.mdc", ".cursor/rules/mine.mdc", LockFile}
	if got := listFiles(t, root); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("files after shrink = %v", got)
	}
	if exists(root, ".claude/skills/other") {
		t.Error("empty skill directory not pruned")
	}
	lock := readLockFile(t, root)
	if len(lock.Skills) != 1 || lock.Skills[0].Ref != "platform/demo" {
		t.Errorf("lock after shrink = %+v", lock)
	}
	// Dropping a format removes that format's files too.
	rep, err = Sync(ctx, c, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}})
	if err != nil || rep.Removed != 1 || exists(root, ".cursor/rules/demo.mdc") || !exists(root, ".cursor/rules/mine.mdc") {
		t.Errorf("format drop: %+v %v", rep, err)
	}
}

func TestSyncKeepsLocallyModifiedStaleFile(t *testing.T) {
	_, c, root := setup(t)
	full := &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo", "platform/other"}}
	if _, err := Sync(ctx, c, translate.Default(), root, full); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(root, ".claude", "skills", "other", "SKILL.md")
	if err := os.WriteFile(edited, []byte("my local edits\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Sync(ctx, c, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, ".claude/skills/other/SKILL.md"); got != "my local edits\n" {
		t.Errorf("locally modified file was clobbered: %q", got)
	}
	if exists(root, ".claude/skills/other/scripts/run.sh") || exists(root, ".claude/skills/other/ref/a.md") {
		t.Error("unmodified stale files were kept")
	}
	if rep.Removed != 2 {
		t.Errorf("removed = %d, want 2", rep.Removed)
	}
	var found bool
	for _, w := range rep.Warnings {
		if w == "kept locally modified stale file .claude/skills/other/SKILL.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	// The kept file is no longer tracked by the lock.
	for _, s := range readLockFile(t, root).Skills {
		if s.Ref == "platform/other" {
			t.Error("stale skill still in lock")
		}
	}
}

func TestSyncNameCollisionWritesNothing(t *testing.T) {
	_, c, root := setup(t)
	m := &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo", "payments/demo"}}
	_, err := Sync(ctx, c, translate.Default(), root, m)
	if err == nil {
		t.Fatal("expected collision error")
	}
	if !strings.Contains(err.Error(), "platform/demo") || !strings.Contains(err.Error(), "payments/demo") || !strings.Contains(err.Error(), ".claude/skills/demo/SKILL.md") {
		t.Errorf("error = %v", err)
	}
	if got := listFiles(t, root); len(got) != 0 {
		t.Errorf("collision wrote files: %v", got)
	}
	// The same collision via Install of the second skill is not detected
	// (Install has no lock), so Sync is the safe path; but a valid
	// pinned ref alongside works.
	m = &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo@1.0.0", "platform/other"}}
	if _, err := Sync(ctx, c, translate.Default(), root, m); err != nil {
		t.Errorf("valid manifest after collision: %v", err)
	}
}

func TestSyncPinnedDigestMismatchDetected(t *testing.T) {
	_, c, root := setup(t)
	m := &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo@1.0.0"}}
	if _, err := Sync(ctx, c, translate.Default(), root, m); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, LockFile)
	lock := readLockFile(t, root)
	lock.Skills[0].Digest = "sha256:" + strings.Repeat("0", 64)
	data, _ := json.MarshalIndent(lock, "", "  ")
	os.WriteFile(lockPath, data, 0o644)
	before := listFiles(t, root)
	// Make the on-disk copy differ so we can prove nothing was rewritten.
	os.WriteFile(filepath.Join(root, ".claude", "skills", "demo", "SKILL.md"), []byte("canary"), 0o644)

	_, err := Sync(ctx, c, translate.Default(), root, m)
	if err == nil {
		t.Fatal("expected pinned digest mismatch")
	}
	if !strings.Contains(err.Error(), "does not match the pinned") || !strings.Contains(err.Error(), LockFile) || !strings.Contains(err.Error(), "tampering") {
		t.Errorf("error = %v", err)
	}
	if got := read(t, root, ".claude/skills/demo/SKILL.md"); got != "canary" {
		t.Error("files were rewritten despite the digest failure")
	}
	if after := listFiles(t, root); strings.Join(after, " ") != strings.Join(before, " ") {
		t.Errorf("file set changed: %v", after)
	}
	if string(data) != read(t, root, LockFile) {
		t.Error("lock rewritten despite the digest failure")
	}
	// A "latest" ref is allowed to move, so a stale lock digest is not an error.
	os.WriteFile(lockPath, data, 0o644)
	if _, err := Sync(ctx, c, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}}); err != nil {
		t.Errorf("latest ref with stale lock digest: %v", err)
	}
	// A pin for a different version than the one now requested does not apply.
	os.WriteFile(lockPath, data, 0o644)
	if _, err := Sync(ctx, c, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo@1.0.0"}}); err == nil {
		t.Error("tampered pin for the requested version should still fail")
	}
}

func TestSyncFollowsLatestAndPins(t *testing.T) {
	g, c, root := setup(t)
	if _, err := Sync(ctx, c, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}}); err != nil {
		t.Fatal(err)
	}
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.1.0", gatewaytest.Files("demo", map[string]string{"SKILL.md": gatewaytest.SkillMD("demo", "Skill demo v2")}))
	rep, err := Sync(ctx, c, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skills[0].Version != "1.1.0" || !strings.Contains(read(t, root, ".claude/skills/demo/SKILL.md"), "v2") {
		t.Errorf("latest did not move: %+v", rep.Skills)
	}
	// run.sh existed only in 1.0.0 and is now stale.
	if exists(root, ".claude/skills/demo/scripts/run.sh") || rep.Removed != 1 {
		t.Errorf("stale file from the previous version kept: removed=%d", rep.Removed)
	}
	// A pinned older version is honoured.
	rep, err = Sync(ctx, c, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo@1.0.0"}})
	if err != nil || rep.Skills[0].Version != "1.0.0" || strings.Contains(read(t, root, ".claude/skills/demo/SKILL.md"), "v2") {
		t.Errorf("pin to 1.0.0: %+v %v", rep, err)
	}
}

func TestSyncErrors(t *testing.T) {
	_, c, root := setup(t)
	cases := map[string]*Manifest{
		"unknown format": {Formats: []string{"emacs"}, Skills: []string{"platform/demo"}},
		"missing skill":  {Formats: []string{"claude"}, Skills: []string{"platform/ghost"}},
		"bad ref":        {Formats: []string{"claude"}, Skills: []string{"demo"}},
		"missing ver":    {Formats: []string{"claude"}, Skills: []string{"platform/demo@9.9.9"}},
	}
	for label, m := range cases {
		if _, err := Sync(ctx, c, translate.Default(), root, m); err == nil {
			t.Errorf("%s: expected error", label)
		}
		if got := listFiles(t, root); len(got) != 0 {
			t.Errorf("%s: wrote %v", label, got)
		}
	}
	// A reader who may not see payments gets an error, not silent skipping.
	_, err := Sync(ctx, c, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"payments/demo"}})
	if err != nil {
		t.Errorf("admin payments: %v", err)
	}
	dave := &client.Client{BaseURL: c.BaseURL, Token: gatewaytest.ReaderToken, HTTP: c.HTTP}
	if _, err := Sync(ctx, dave, translate.Default(), t.TempDir(), &Manifest{Formats: []string{"claude"}, Skills: []string{"payments/demo"}}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("denied skill: %v", err)
	}
	// A corrupt lock file is an error rather than silently ignored.
	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, LockFile), []byte("{not json"), 0o644)
	if _, err := Sync(ctx, c, translate.Default(), bad, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}}); err == nil {
		t.Error("corrupt lock accepted")
	}
}

func TestSyncDetectsTamperingServer(t *testing.T) {
	g, _, root := setup(t)
	target, _ := url.Parse(g.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	mode := "digest-header"
	proxy.ModifyResponse = func(r *http.Response) error {
		if strings.HasSuffix(r.Request.URL.Path, "/bundle") && mode == "digest-header" {
			r.Header.Set("X-Bundle-Digest", "sha256:"+strings.Repeat("a", 64))
		}
		if strings.HasSuffix(r.Request.URL.Path, "/versions/1.0.0") && mode == "metadata" {
			r.Header.Set("X-Tampered", "1")
		}
		return nil
	}
	ps := httptest.NewServer(proxy)
	defer ps.Close()
	c := &client.Client{BaseURL: ps.URL, Token: gatewaytest.AdminToken}
	_, err := Sync(ctx, c, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}})
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Errorf("tampered digest header: %v", err)
	}
	if got := listFiles(t, root); len(got) != 0 {
		t.Errorf("tampered sync wrote %v", got)
	}
}

func TestInstall(t *testing.T) {
	_, c, root := setup(t)
	res, err := Install(ctx, c, translate.Default(), root, "platform", "other", "latest", []string{"codex", "cursor-rules"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "1.0.0" || len(res.Written) != 4 || len(res.Warnings) != 1 {
		t.Errorf("installed = %+v", res)
	}
	want := []string{".agents/skills/other/SKILL.md", ".agents/skills/other/ref/a.md", ".agents/skills/other/scripts/run.sh", ".cursor/rules/other.mdc"}
	if got := listFiles(t, root); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("files = %v", got)
	}
	if _, err := Install(ctx, c, translate.Default(), root, "platform", "other", "2.0.0", []string{"claude"}); err == nil {
		t.Error("missing version installed")
	}
	if _, err := Install(ctx, c, translate.Default(), root, "platform", "other", "latest", []string{"nope"}); err == nil {
		t.Error("unknown format installed")
	}
}

func TestWriteFileRefusesSymlinkAndEscape(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../escape", "/abs", "a/../../b", "a\\b", ""} {
		if _, err := safeJoin(root, rel); err == nil {
			t.Errorf("safeJoin(%q) accepted", rel)
		}
	}
	if err := writeAll(root, []translate.OutFile{{Path: "ok.txt", Content: []byte("x")}, {Path: "../bad", Content: []byte("y")}}); err == nil {
		t.Error("writeAll with an escaping path succeeded")
	}
	if exists(root, "ok.txt") {
		t.Error("writeAll wrote before validating every path")
	}
	outside := filepath.Join(t.TempDir(), "target.txt")
	os.WriteFile(outside, []byte("original"), 0o644)
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := writeFile(root, "link.txt", []byte("overwritten"))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("writeFile through symlink: %v", err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "original" {
		t.Error("symlink target was overwritten")
	}
}

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	m, err := LoadManifest(write("ok.yaml", "formats: [claude, cursor]\nskills:\n  - platform/demo\n  - payments/refunds@1.2.0\n"))
	if err != nil || len(m.Formats) != 2 || len(m.Skills) != 2 || m.Skills[1] != "payments/refunds@1.2.0" {
		t.Errorf("LoadManifest ok = %+v, %v", m, err)
	}
	for label, body := range map[string]string{
		"unknown field":   "formats: [claude]\nskills: [a/b]\nextra: 1\n",
		"missing formats": "skills: [a/b]\n",
		"missing skills":  "formats: [claude]\n",
		"empty lists":     "formats: []\nskills: []\n",
		"not yaml":        "{{{",
		"wrong types":     "formats: claude\nskills: {a: b}\n",
	} {
		if _, err := LoadManifest(write(label+".yaml", body)); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
	if _, err := LoadManifest(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in            string
		ns, name, ver string
		ok            bool
	}{
		{"platform/demo", "platform", "demo", "latest", true},
		{"platform/demo@1.2.3", "platform", "demo", "1.2.3", true},
		{"platform/demo@latest", "platform", "demo", "latest", true},
		{"platform/demo@1.0.0-rc.1", "platform", "demo", "1.0.0-rc.1", true},
		{"demo", "", "", "", false},
		{"/demo", "", "", "", false},
		{"platform/", "", "", "", false},
		{"a/b/c", "", "", "", false},
		{"", "", "", "", false},
		{"@1.0.0", "", "", "", false},
		{"platform/demo@", "", "", "", false},
	}
	for _, c := range cases {
		ns, name, ver, err := parseRef(c.in)
		if (err == nil) != c.ok || ns != c.ns || name != c.name || ver != c.ver {
			t.Errorf("parseRef(%q) = %q %q %q %v, want %q %q %q ok=%v", c.in, ns, name, ver, err, c.ns, c.name, c.ver, c.ok)
		}
	}
}
