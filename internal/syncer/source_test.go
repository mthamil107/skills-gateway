package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/client"
	"github.com/mthamil107/skills-gateway/internal/gatewaytest"
	"github.com/mthamil107/skills-gateway/internal/source"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

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

// skillsDir builds a folder of skills, the no-server source.
func skillsDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk(t, root, "demo/SKILL.md", gatewaytest.SkillMD("demo", "Skill demo"))
	mk(t, root, "demo/scripts/run.sh", "echo demo\n")
	mk(t, root, "nested/other/SKILL.md", gatewaytest.SkillMD("other", "Skill other"))
	mk(t, root, ".git/HEAD", "ref: refs/heads/main")
	return root
}

// stubSource is a Source with scripted answers, for the corners a real
// source cannot easily produce.
type stubSource struct {
	list     []source.Ref
	pinnable bool
	skills   map[string]source.Skill
}

func (s *stubSource) Describe() string { return "stub" }
func (s *stubSource) Fixed() bool      { return false }

func (s *stubSource) Pinnable() bool                             { return s.pinnable }
func (s *stubSource) List(context.Context) ([]source.Ref, error) { return s.list, nil }
func (s *stubSource) Resolve(_ context.Context, r source.Ref) (source.Skill, error) {
	sk, ok := s.skills[r.Name]
	if !ok {
		return source.Skill{}, fmt.Errorf("stub: no %s", r.Name)
	}
	return sk, nil
}

func TestManifestSource(t *testing.T) {
	skills := skillsDir(t)
	base := t.TempDir()
	// A relative path resolves against the manifest's directory, not the
	// process working directory.
	rel, err := filepath.Rel(base, skills)
	if err != nil {
		t.Skipf("no relative path between temp dirs: %v", err)
	}
	gatewayCalls := 0
	newGateway := func(url string) (source.Source, error) {
		gatewayCalls++
		return &source.Gateway{Client: &client.Client{BaseURL: "stub:" + url}}, nil
	}

	src, err := (&Manifest{Path: filepath.ToSlash(rel), Formats: []string{"claude"}, Skills: []string{"*"}}).Source(ctx, base, newGateway)
	if err != nil {
		t.Fatalf("relative path: %v", err)
	}
	d, ok := src.(*source.Dir)
	if !ok || d.Root != filepath.Join(base, rel) || d.Pinnable() {
		t.Errorf("relative path source = %#v", src)
	}
	if refs, err := src.List(ctx); err != nil || len(refs) != 2 {
		t.Errorf("relative path List = %v, %v", refs, err)
	}

	src, err = (&Manifest{Path: skills}).Source(ctx, filepath.Join(base, "elsewhere"), newGateway)
	if err != nil {
		t.Fatalf("absolute path: %v", err)
	}
	if d, ok := src.(*source.Dir); !ok || d.Root != skills {
		t.Errorf("absolute path source = %#v", src)
	}
	if _, err := (&Manifest{Path: "missing"}).Source(ctx, base, newGateway); err == nil || !strings.Contains(err.Error(), "path source") {
		t.Errorf("missing path: %v", err)
	}

	// No source named: the gateway callback decides, with an empty URL.
	src, err = (&Manifest{}).Source(ctx, base, newGateway)
	if err != nil || gatewayCalls != 1 {
		t.Fatalf("default gateway: %v (calls=%d)", err, gatewayCalls)
	}
	if g, ok := src.(*source.Gateway); !ok || g.Client.BaseURL != "stub:" || !g.Pinnable() {
		t.Errorf("default gateway source = %#v", src)
	}
	src, err = (&Manifest{Gateway: "https://skills.example.com"}).Source(ctx, base, newGateway)
	if err != nil || src.(*source.Gateway).Client.BaseURL != "stub:https://skills.example.com" {
		t.Errorf("explicit gateway = %#v, %v", src, err)
	}
	gatewayErr := errors.New("no SGW_URL")
	if _, err := (&Manifest{}).Source(ctx, base, func(string) (source.Source, error) { return nil, gatewayErr }); !errors.Is(err, gatewayErr) {
		t.Errorf("gateway error not propagated: %v", err)
	}

	// Two sources at once is a mistake, whichever pair it is, and the
	// gateway callback must not even be consulted.
	gatewayCalls = 0
	for label, m := range map[string]*Manifest{
		"gateway+path": {Gateway: "https://x", Path: skills},
		"gateway+git":  {Gateway: "https://x", Git: "https://y"},
		"path+git":     {Path: skills, Git: "https://y"},
		"all three":    {Gateway: "https://x", Path: skills, Git: "https://y"},
	} {
		if _, err := m.Source(ctx, base, newGateway); err == nil || !strings.Contains(err.Error(), "only one") {
			t.Errorf("%s: %v", label, err)
		}
	}
	if gatewayCalls != 0 {
		t.Error("gateway consulted despite conflicting sources")
	}
	// A git URL that cannot be cloned is an error, not a fallback.
	if _, err := exec.LookPath("git"); err == nil {
		if _, err := (&Manifest{Git: filepath.Join(t.TempDir(), "nope")}).Source(ctx, base, newGateway); err == nil {
			t.Error("unclonable git source accepted")
		}
	}
}

func TestManifestSourceGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[core]\n\tautocrlf = true\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())    // linux cache dir
	t.Setenv("LocalAppData", t.TempDir())      // windows cache dir
	t.Setenv("HOME", t.TempDir())              // darwin cache dir
	t.Setenv("USERPROFILE", os.Getenv("HOME")) // keep windows consistent

	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "-b", "main")
	mk(t, repo, "team/demo/SKILL.md", gatewaytest.SkillMD("demo", "Skill demo"))
	git("add", "-A")
	git("commit", "--quiet", "-m", "first")
	head := git("rev-parse", "HEAD")
	git("tag", "v1")

	m := &Manifest{Git: repo, Ref: "v1", GitDir: "team", Formats: []string{"claude"}, Skills: []string{"demo"}}
	src, err := m.Source(ctx, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("git source: %v", err)
	}
	if !src.Pinnable() {
		t.Error("a tag must be pinnable")
	}
	root := t.TempDir()
	rep, err := Sync(ctx, src, translate.Default(), root, m)
	if err != nil {
		t.Fatalf("Sync from git: %v", err)
	}
	if len(rep.Skills) != 1 || rep.Skills[0].Version != head[:12] || rep.Skills[0].Source != "git:"+repo+"@v1" {
		t.Errorf("lock from git = %+v", rep.Skills)
	}
	if got := read(t, root, ".claude/skills/demo/SKILL.md"); got != gatewaytest.SkillMD("demo", "Skill demo") {
		t.Errorf("SKILL.md from git is not byte-exact: %q", got)
	}
	// A subdirectory that is not in the repository is a clear error.
	if _, err := (&Manifest{Git: repo, Ref: "v1", GitDir: "nowhere"}).Source(ctx, "", nil); err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("missing git dir: %v", err)
	}
}

func TestLoadManifestSources(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	m, err := LoadManifest(write("path.yaml", "path: ../team-skills\nformats: [claude]\nskills: ['*']\n"))
	if err != nil || m.Path != "../team-skills" || m.Skills[0] != "*" || m.Gateway != "" || m.Git != "" {
		t.Errorf("path manifest = %+v, %v", m, err)
	}
	m, err = LoadManifest(write("git.yaml", "git: https://github.com/acme/skills\nref: v1.2.0\ndir: skills\nformats: [claude, codex]\nskills: [demo, other]\n"))
	if err != nil || m.Git != "https://github.com/acme/skills" || m.Ref != "v1.2.0" || m.GitDir != "skills" || len(m.Skills) != 2 {
		t.Errorf("git manifest = %+v, %v", m, err)
	}
	m, err = LoadManifest(write("gw.yaml", "gateway: https://skills.example.com\nformats: [claude]\nskills: [platform/demo@1.0.0]\n"))
	if err != nil || m.Gateway != "https://skills.example.com" {
		t.Errorf("gateway manifest = %+v, %v", m, err)
	}
	// Two sources load (the loader does not decide) but Source() refuses.
	m, err = LoadManifest(write("two.yaml", "gateway: https://x\npath: ./skills\nformats: [claude]\nskills: [demo]\n"))
	if err != nil {
		t.Fatalf("two sources should load and fail later: %v", err)
	}
	if _, err := m.Source(ctx, dir, func(string) (source.Source, error) { return nil, nil }); err == nil {
		t.Error("two sources accepted by Source()")
	}
	for label, body := range map[string]string{
		"ref without git":   "path: ./skills\nref: main\nformats: [claude]\nskills: [demo]\n",
		"dir without git":   "gateway: https://x\ndir: skills\nformats: [claude]\nskills: [demo]\n",
		"ref alone":         "ref: main\nformats: [claude]\nskills: [demo]\n",
		"unknown field":     "path: ./skills\nbranch: main\nformats: [claude]\nskills: [demo]\n",
		"typo in source":    "gitt: https://x\nformats: [claude]\nskills: [demo]\n",
		"missing skills":    "path: ./skills\nformats: [claude]\n",
		"missing formats":   "path: ./skills\nskills: [demo]\n",
		"path is a list":    "path: [a]\nformats: [claude]\nskills: [demo]\n",
		"git is a map":      "git: {url: x}\nformats: [claude]\nskills: [demo]\n",
		"skills not a list": "path: ./skills\nformats: [claude]\nskills: '*'\n",
	} {
		if _, err := LoadManifest(write(strings.ReplaceAll(label, " ", "-")+".yaml", body)); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

func TestRefsStar(t *testing.T) {
	dir := &source.Dir{Root: skillsDir(t)}
	got, err := refs(ctx, dir, []string{"*"})
	if err != nil {
		t.Fatalf("refs(*): %v", err)
	}
	if len(got) != 2 || got[0].Name != "demo" || got[1].Name != "other" {
		t.Errorf("refs(*) = %+v", got)
	}
	// Whitespace around the star is tolerated.
	if got, err := refs(ctx, dir, []string{" * "}); err != nil || len(got) != 2 {
		t.Errorf("refs(' * ') = %+v, %v", got, err)
	}
	// The star cannot be mixed with explicit entries, in either position.
	for _, list := range [][]string{{"*", "demo"}, {"demo", "*"}, {"*", "*"}} {
		if _, err := refs(ctx, dir, list); err == nil || !strings.Contains(err.Error(), `"*" must be the only entry`) {
			t.Errorf("refs(%v): %v", list, err)
		}
	}
	// A star over an empty source is an error, not an empty sync that
	// would silently remove every installed skill.
	if _, err := refs(ctx, &stubSource{}, []string{"*"}); err == nil || !strings.Contains(err.Error(), "offers no skills") {
		t.Errorf("empty source: %v", err)
	}
	if _, err := refs(ctx, &source.Dir{Root: t.TempDir()}, []string{"*"}); err == nil {
		t.Error("star over a folder with no SKILL.md accepted")
	}
	// Explicit entries are parsed, and a bad one fails the whole list.
	got, err = refs(ctx, dir, []string{"demo", "platform/other@1.0.0"})
	if err != nil || len(got) != 2 || got[1].Namespace != "platform" || got[1].Version != "1.0.0" {
		t.Errorf("explicit refs = %+v, %v", got, err)
	}
	if _, err := refs(ctx, dir, []string{"demo", "Bad Name"}); err == nil {
		t.Error("invalid ref accepted")
	}
	// A star sync against a gateway lists only what policy allows.
	g := gatewaytest.New(t, gatewaytest.Options{})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.0.0", gatewaytest.Files("demo", nil))
	g.Publish(t, gatewaytest.PaymentsToken, "payments", "refunds", "1.0.0", gatewaytest.Files("refunds", nil))
	got, err = refs(ctx, gw(g.Client(gatewaytest.ReaderToken)), []string{"*"})
	if err != nil || len(got) != 1 || got[0].String() != "platform/demo" {
		t.Errorf("gateway star for reader = %+v, %v", got, err)
	}
}

func TestSyncFromDirEndToEnd(t *testing.T) {
	skills := skillsDir(t)
	src := &source.Dir{Root: skills}
	root := t.TempDir()
	m := &Manifest{Path: skills, Formats: []string{"claude"}, Skills: []string{"*"}}

	rep, err := Sync(ctx, src, translate.Default(), root, m)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rep.Written != 3 || rep.Removed != 0 || len(rep.Skills) != 2 {
		t.Errorf("report = %+v", rep)
	}
	if got := read(t, root, ".claude/skills/demo/SKILL.md"); got != gatewaytest.SkillMD("demo", "Skill demo") {
		t.Errorf("SKILL.md = %q", got)
	}
	if got := read(t, root, ".claude/skills/demo/scripts/run.sh"); got != "echo demo\n" {
		t.Errorf("run.sh = %q", got)
	}
	want := []string{".claude/skills/demo/SKILL.md", ".claude/skills/demo/scripts/run.sh", ".claude/skills/other/SKILL.md", LockFile}
	if got := listFiles(t, root); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("files on disk = %v", got)
	}
	lock := readLockFile(t, root)
	if len(lock.Skills) != 2 {
		t.Fatalf("lock = %+v", lock)
	}
	for i, name := range []string{"demo", "other"} {
		s := lock.Skills[i]
		if s.Ref != name || s.Version != "-" || s.Source != "path:"+filepath.ToSlash(skills) || !strings.HasPrefix(s.Digest, "sha256:") {
			t.Errorf("locked %s = %+v", name, s)
		}
	}
	// The digest in the lock is the same one a gateway would compute for
	// these files, so moving to a gateway later keeps the lock meaningful.
	files := []bundle.File{
		{Path: "SKILL.md", Content: []byte(gatewaytest.SkillMD("demo", "Skill demo"))},
		{Path: "scripts/run.sh", Content: []byte("echo demo\n")},
	}
	if lock.Skills[0].Digest != bundle.FromFiles(files).Digest {
		t.Errorf("lock digest %s differs from the bundle digest", lock.Skills[0].Digest)
	}

	// A second sync is a no-op.
	before := read(t, root, LockFile)
	rep, err = Sync(ctx, src, translate.Default(), root, m)
	if err != nil || rep.Removed != 0 || read(t, root, LockFile) != before {
		t.Errorf("second sync: %+v %v", rep, err)
	}

	// A changed file in the folder is the normal case, not tampering:
	// the version stays "-" but the digest moves and the file lands.
	mk(t, skills, "demo/SKILL.md", gatewaytest.SkillMD("demo", "Skill demo, revised"))
	mk(t, skills, "demo/ref/new.md", "new")
	os.Remove(filepath.Join(skills, "demo", "scripts", "run.sh"))
	rep, err = Sync(ctx, src, translate.Default(), root, m)
	if err != nil {
		t.Fatalf("sync after folder change: %v", err)
	}
	if !strings.Contains(read(t, root, ".claude/skills/demo/SKILL.md"), "revised") || !exists(root, ".claude/skills/demo/ref/new.md") {
		t.Error("changed folder content did not land")
	}
	if exists(root, ".claude/skills/demo/scripts/run.sh") || rep.Removed != 1 {
		t.Errorf("file deleted from the folder survived: removed=%d", rep.Removed)
	}
	after := readLockFile(t, root)
	if after.Skills[0].Digest == lock.Skills[0].Digest || after.Skills[0].Version != "-" {
		t.Errorf("lock after change = %+v", after.Skills[0])
	}
	// A stale lock digest for a folder is never an error either.
	after.Skills[0].Digest = "sha256:" + strings.Repeat("0", 64)
	data, _ := json.MarshalIndent(after, "", "  ")
	os.WriteFile(filepath.Join(root, LockFile), data, 0o644)
	if _, err := Sync(ctx, src, translate.Default(), root, m); err != nil {
		t.Errorf("folder sync with a stale lock digest: %v", err)
	}

	// Explicit names work too, a namespaced name is tolerated, and an
	// unknown name is an error that writes nothing new.
	if _, err := Sync(ctx, src, translate.Default(), t.TempDir(), &Manifest{Formats: []string{"claude"}, Skills: []string{"demo", "platform/other"}}); err != nil {
		t.Errorf("explicit names: %v", err)
	}
	fresh := t.TempDir()
	if _, err := Sync(ctx, src, translate.Default(), fresh, &Manifest{Formats: []string{"claude"}, Skills: []string{"demo", "ghost"}}); err == nil || len(listFiles(t, fresh)) != 0 {
		t.Errorf("unknown skill: %v, files=%v", err, listFiles(t, fresh))
	}
	// Install (sgw fetch) from a folder as well.
	res, err := Install(ctx, src, translate.Default(), fresh, source.Ref{Name: "other", Version: "latest"}, []string{"codex"})
	if err != nil || res.Version != "-" || len(res.Written) != 1 || !exists(fresh, ".agents/skills/other/SKILL.md") {
		t.Errorf("Install from folder = %+v, %v", res, err)
	}
}

var digestField = regexp.MustCompile(`"digest":\s*"(sha256:[0-9a-f]{64})"`)

// TestSyncSameChangeIsTamperingOnGateway is the counterpart of the folder
// test above: the identical edit to a published version, served by a
// gateway that claims immutability, must be refused.
func TestSyncSameChangeIsTamperingOnGateway(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{})
	g.Publish(t, gatewaytest.PlatformToken, "platform", "demo", "1.0.0", gatewaytest.Files("demo", map[string]string{"scripts/run.sh": "echo demo\n"}))
	root := t.TempDir()
	m := &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}}
	if _, err := Sync(ctx, gw(g.Client(gatewaytest.AdminToken)), translate.Default(), root, m); err != nil {
		t.Fatal(err)
	}
	lock := readLockFile(t, root)
	if lock.Skills[0].Version != "1.0.0" || !strings.HasPrefix(lock.Skills[0].Source, "gateway:") {
		t.Fatalf("lock = %+v", lock.Skills[0])
	}

	// A "server" that serves the same version 1.0.0 with revised content,
	// internally consistent: bundle, digest header and metadata all agree.
	revised := gatewaytest.Files("demo", map[string]string{"SKILL.md": gatewaytest.SkillMD("demo", "Skill demo, revised"), "ref/new.md": "new"})
	tar, err := bundle.Encode(revised)
	if err != nil {
		t.Fatal(err)
	}
	revisedDigest := bundle.FromFiles(revised).Digest
	target, _ := url.Parse(g.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(r *http.Response) error {
		p := r.Request.URL.Path
		switch {
		case strings.HasSuffix(p, "/bundle"):
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(tar))
			r.ContentLength = int64(len(tar))
			r.Header.Set("Content-Length", fmt.Sprint(len(tar)))
			r.Header.Set("X-Bundle-Digest", revisedDigest)
		case strings.Contains(p, "/versions/"):
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				return err
			}
			loc := digestField.FindSubmatchIndex(body)
			if loc == nil {
				return fmt.Errorf("no digest in %s", body)
			}
			body = append(append(append([]byte{}, body[:loc[2]]...), revisedDigest...), body[loc[3]:]...)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Set("Content-Length", fmt.Sprint(len(body)))
		}
		return nil
	}
	ps := httptest.NewServer(proxy)
	defer ps.Close()
	c := &client.Client{BaseURL: ps.URL, Token: gatewaytest.AdminToken}

	// Against a fresh project the revised content is simply what 1.0.0 is.
	if _, err := Sync(ctx, gw(c), translate.Default(), t.TempDir(), m); err != nil {
		t.Fatalf("revised server without a lock: %v", err)
	}
	// Against the locked project it is tampering, and nothing is written.
	before := listFiles(t, root)
	lockBefore := read(t, root, LockFile)
	_, err = Sync(ctx, gw(c), translate.Default(), root, m)
	if err == nil || !strings.Contains(err.Error(), "does not match the pinned") || !strings.Contains(err.Error(), "tampering") {
		t.Errorf("gateway change of an immutable version accepted: %v", err)
	}
	if strings.Contains(read(t, root, ".claude/skills/demo/SKILL.md"), "revised") || exists(root, ".claude/skills/demo/ref/new.md") {
		t.Error("tampered content was written")
	}
	if after := listFiles(t, root); strings.Join(after, " ") != strings.Join(before, " ") || read(t, root, LockFile) != lockBefore {
		t.Error("file set or lock changed despite the digest failure")
	}
}

// prepare must honour Pinnable() rather than the source type.
func TestPrepareHonoursPinnable(t *testing.T) {
	files := gatewaytest.Files("demo", nil)
	sk := source.Skill{Name: "demo", Version: "v1", Digest: bundle.FromFiles(files).Digest, Files: files}
	stub := &stubSource{skills: map[string]source.Skill{"demo": sk}}
	pin := &LockedSkill{Ref: "demo", Version: "v1", Digest: "sha256:" + strings.Repeat("0", 64)}
	ref := source.Ref{Name: "demo", Version: "latest"}
	if _, err := prepare(ctx, stub, translate.Default(), ref, pin, []string{"claude"}); err != nil {
		t.Errorf("unpinnable source with a stale pin: %v", err)
	}
	stub.pinnable = true
	if _, err := prepare(ctx, stub, translate.Default(), ref, pin, []string{"claude"}); err == nil || !strings.Contains(err.Error(), "tampering") {
		t.Errorf("pinnable source with a stale pin: %v", err)
	}
	// A different version than the pinned one is not compared.
	pin.Version = "v0"
	if _, err := prepare(ctx, stub, translate.Default(), ref, pin, []string{"claude"}); err != nil {
		t.Errorf("pin for another version applied: %v", err)
	}
	// A source whose advertised digest does not cover its files is caught
	// before anything else.
	bad := sk
	bad.Digest = "sha256:" + strings.Repeat("1", 64)
	stub.skills["demo"] = bad
	if _, err := prepare(ctx, stub, translate.Default(), ref, nil, []string{"claude"}); err == nil || !strings.Contains(err.Error(), "does not match the received files") {
		t.Errorf("digest/file mismatch: %v", err)
	}
}
