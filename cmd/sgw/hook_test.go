package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mthamil107/skills-gateway/internal/hook"
	"github.com/mthamil107/skills-gateway/internal/syncer"
)

// tempHome keeps every hook test away from the real ~/.claude and ~/.sgw.
func tempHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	p, err := hook.StatePath()
	if err != nil || !strings.HasPrefix(p, h) {
		t.Fatalf("state path %s is not under the temp home %s (%v)", p, h, err)
	}
	return h
}

// capture runs fn with stdout and stderr redirected and returns both.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldO, oldE := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wo, we
	var bo, be bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(&bo, ro) }()
	go func() { defer wg.Done(); io.Copy(&be, re) }()
	func() {
		defer func() {
			os.Stdout, os.Stderr = oldO, oldE
			wo.Close()
			we.Close()
		}()
		fn()
	}()
	wg.Wait()
	return bo.String(), be.String()
}

func put(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func slurp(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func demoDoc(v string) string {
	return "---\nname: demo\ndescription: Skill demo\n---\n\n# demo\n\n" + v + "\n"
}

// hookFixture builds a path source with one skill, a manifest pointing at
// it and an empty install root, all under temp dirs.
func hookFixture(t *testing.T) (manifest, srcRoot, root string) {
	t.Helper()
	srcRoot = t.TempDir()
	put(t, filepath.Join(srcRoot, "demo", "SKILL.md"), demoDoc("v1"))
	manifest = filepath.Join(t.TempDir(), "sgw-sync.yaml")
	put(t, manifest, "path: "+filepath.ToSlash(srcRoot)+"\nformats: [claude]\nskills: [demo]\n")
	return manifest, srcRoot, t.TempDir()
}

func TestRunHookSyncIsThrottledSilentAndOneLine(t *testing.T) {
	tempHome(t)
	manifest, srcRoot, root := hookFixture(t)
	m, err := syncer.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	key := hook.Key(manifest, root)
	installed := filepath.Join(root, ".claude", "skills", "demo", "SKILL.md")
	run := func(every time.Duration) string {
		t.Helper()
		out, errOut := capture(t, func() { runHookSync(context.Background(), m, manifest, root, every) })
		if errOut != "" {
			t.Errorf("hook wrote to stderr: %q", errOut)
		}
		return out
	}

	// First run: one line, the files land, the state remembers the offer.
	out := run(4 * time.Hour)
	if !strings.HasPrefix(out, "skills updated from path:") || !strings.Contains(out, "1 file(s) written") || strings.Count(out, "\n") != 1 {
		t.Errorf("first run printed %q", out)
	}
	if slurp(t, installed) != demoDoc("v1") {
		t.Error("first run did not install the skill")
	}
	st := hook.LoadState().Runs[key]
	if !strings.HasPrefix(st.Peek, "dir:") || st.Version == "" || time.Since(st.LastRun) > time.Minute {
		t.Errorf("state after first run = %+v", st)
	}

	// Throttled: upstream moves, but within the interval nothing happens.
	put(t, filepath.Join(srcRoot, "demo", "SKILL.md"), demoDoc("v2"))
	if out := run(4 * time.Hour); out != "" {
		t.Errorf("throttled run printed %q", out)
	}
	if slurp(t, installed) != demoDoc("v1") {
		t.Error("throttled run synced anyway")
	}

	// Due: the change is picked up, in one line.
	out = run(0)
	if !strings.Contains(out, "1 file(s) written") || strings.Count(out, "\n") != 1 {
		t.Errorf("due run printed %q", out)
	}
	if slurp(t, installed) != demoDoc("v2") {
		t.Error("due run did not update the skill")
	}
	peek := hook.LoadState().Runs[key].Peek

	// Nothing changed: silent, but the state notes the check happened.
	before := hook.LoadState().Runs[key].LastRun
	time.Sleep(10 * time.Millisecond)
	if out := run(0); out != "" {
		t.Errorf("unchanged run printed %q", out)
	}
	if after := hook.LoadState().Runs[key]; !after.LastRun.After(before) || after.Peek != peek {
		t.Errorf("unchanged run state = %+v, want a later LastRun and the same peek", after)
	}
	if got := slurp(t, filepath.Join(root, syncer.LockFile)); !strings.Contains(got, "demo") {
		t.Errorf("lock = %s", got)
	}

	// An edited file is kept and said so, in the same single line.
	put(t, installed, demoDoc("my edit"))
	put(t, filepath.Join(srcRoot, "demo", "SKILL.md"), demoDoc("v3"))
	out = run(0)
	if !strings.Contains(out, "0 file(s) written") || !strings.Contains(out, "1 kept because you edited them") || strings.Count(out, "\n") != 1 {
		t.Errorf("run over an edit printed %q", out)
	}
	if slurp(t, installed) != demoDoc("my edit") {
		t.Error("the hook overwrote an edited file")
	}
}

func TestRunHookSyncNeverFailsAndRetriesAfterFailure(t *testing.T) {
	tempHome(t)
	manifest, srcRoot, root := hookFixture(t)
	m, err := syncer.LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	key := hook.Key(manifest, root)
	installed := filepath.Join(root, ".claude", "skills", "demo", "SKILL.md")
	quiet := func(label string, m *syncer.Manifest, every time.Duration) {
		t.Helper()
		out, errOut := capture(t, func() { runHookSync(context.Background(), m, manifest, root, every) })
		if out != "" || errOut != "" {
			t.Errorf("%s: hook was not silent: stdout=%q stderr=%q", label, out, errOut)
		}
	}

	// A source that does not exist, or a manifest naming nothing usable,
	// is silent and leaves no state behind.
	quiet("missing source", &syncer.Manifest{Path: filepath.Join(t.TempDir(), "absent"), Formats: []string{"claude"}, Skills: []string{"demo"}}, 0)
	quiet("no gateway configured", &syncer.Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}}, 0)
	if runs := hook.LoadState().Runs; len(runs) != 0 {
		t.Errorf("a failed source check recorded state: %+v", runs)
	}

	// A broken skill upstream (its name does not match its folder) fails
	// the sync quietly; nothing is written.
	put(t, filepath.Join(srcRoot, "demo", "SKILL.md"), "---\nname: other\ndescription: x\n---\n\n# other\n")
	quiet("broken skill", m, 0)
	if _, err := os.Stat(installed); err == nil {
		t.Error("a failed sync wrote files")
	}
	// The failure is throttled like any run, so a persistently broken
	// source costs one attempt per interval, not one per session.
	if run, ok := hook.LoadState().Runs[key]; !ok || run.Peek != "" || time.Since(run.LastRun) > time.Minute {
		t.Errorf("state after a failed sync = %+v, %v; want LastRun set and no peek", run, ok)
	}

	// Once healthy, the next due run installs. Then a failure that is not
	// upstream's doing (a corrupt lock file) must not be mistaken for
	// "up to date": when it clears, the same offer must still be synced.
	put(t, filepath.Join(srcRoot, "demo", "SKILL.md"), demoDoc("v1"))
	out, _ := capture(t, func() { runHookSync(context.Background(), m, manifest, root, 0) })
	if !strings.Contains(out, "1 file(s) written") {
		t.Fatalf("healthy run printed %q", out)
	}
	good := hook.LoadState().Runs[key].Peek

	put(t, filepath.Join(srcRoot, "demo", "SKILL.md"), demoDoc("v2"))
	put(t, filepath.Join(root, syncer.LockFile), "{corrupt")
	quiet("corrupt lock", m, 0)
	if slurp(t, installed) != demoDoc("v1") {
		t.Error("a run with a corrupt lock wrote files")
	}
	if got := hook.LoadState().Runs[key].Peek; got != good {
		t.Errorf("a failed sync recorded the new fingerprint %q; the next session would skip the retry", got)
	}
	os.Remove(filepath.Join(root, syncer.LockFile))
	out, _ = capture(t, func() { runHookSync(context.Background(), m, manifest, root, 0) })
	if !strings.Contains(out, "1 file(s) written") || slurp(t, installed) != demoDoc("v2") {
		t.Errorf("retry after the failure cleared printed %q; file = %q", out, slurp(t, installed))
	}
}

func TestHookInstallStatusRemoveEndToEnd(t *testing.T) {
	home := tempHome(t)
	manifest, _, root := hookFixture(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	put(t, settings, `{"model": "opus", "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "lint"}]}]}}`)
	// An author's unpublished skill sits where the hook will sync.
	put(t, filepath.Join(root, ".claude", "skills", "wip", "SKILL.md"), "---\nname: wip\ndescription: draft\n---\n")

	var err error
	out, errOut := capture(t, func() {
		err = hookInstall([]string{"-file", manifest, "-out", root, "-every", "1h"})
	})
	if err != nil {
		t.Fatalf("hook install: %v", err)
	}
	if !strings.Contains(errOut, "warning: 1 skill folder(s)") || !strings.Contains(errOut, ".claude/skills/wip") || !strings.Contains(errOut, "publish anything you want to keep") {
		t.Errorf("install did not warn about the local-only skill:\n%s", errOut)
	}
	if !strings.Contains(out, "installed the Claude Code session hook in "+settings) || !strings.Contains(out, "-every 1h0m0s") || !strings.Contains(out, "startup|resume") {
		t.Errorf("install printed:\n%s", out)
	}
	// Exec form: the program, then one JSON string per argument, so a path
	// with spaces or backslashes never passes through a shell.
	body := slurp(t, settings)
	for _, want := range []string{`"sync"`, `"-hook"`, `"args"`, `"lint"`, `"opus"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings after install lack %s:\n%s", want, body)
		}
	}
	if !strings.Contains(slurp(t, settings+".sgw-backup"), `"lint"`) {
		t.Error("no backup of the pre-edit settings")
	}

	out, _ = capture(t, func() { err = hookStatus(nil) })
	if err != nil || !strings.Contains(out, "installed in "+settings) || !strings.Contains(out, "no sync has run from a hook yet") {
		t.Errorf("hook status = %v:\n%s", err, out)
	}

	out, _ = capture(t, func() { err = hookRemove(nil) })
	if err != nil || !strings.Contains(out, "removed the sgw hook from "+settings) {
		t.Errorf("hook remove = %v:\n%s", err, out)
	}
	body = slurp(t, settings)
	if strings.Contains(body, hook.Marker) || !strings.Contains(body, `"lint"`) || !strings.Contains(body, `"opus"`) {
		t.Errorf("settings after remove:\n%s", body)
	}
	out, _ = capture(t, func() { err = hookRemove(nil) })
	if err != nil || !strings.Contains(out, "no sgw hook in "+settings) {
		t.Errorf("second hook remove = %v:\n%s", err, out)
	}

	// Refusals: an unsupported agent, a planned one, and a bad manifest.
	if err := hookInstall([]string{"-agent", "emacs", "-file", manifest}); err == nil || !strings.Contains(err.Error(), "emacs") {
		t.Errorf("unknown agent: %v", err)
	}
	if err := hookInstall([]string{"-agent", "cursor", "-file", manifest}); err == nil || !strings.Contains(err.Error(), hook.Planned["cursor"]) {
		t.Errorf("planned agent: %v", err)
	}
	if err := hookRemove([]string{"-agent", "codex"}); err == nil || !strings.Contains(err.Error(), hook.Planned["codex"]) {
		t.Errorf("planned agent remove: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	put(t, bad, "formats: [claude]\n")
	if err := hookInstall([]string{"-file", bad, "-out", root}); err == nil || !strings.Contains(err.Error(), "must be valid first") {
		t.Errorf("bad manifest: %v", err)
	}
	if _, err := os.Stat(settings + ".sgw-tmp"); err == nil {
		t.Error("temp file left behind")
	}
	if err := hookCmd([]string{"bogus"}); err == nil {
		t.Error("unknown hook subcommand accepted")
	}
}
