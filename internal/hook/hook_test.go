package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// home points both home variables at a fresh temp dir, so nothing in this
// package can reach the real ~/.claude or ~/.sgw, and proves it did.
func home(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	p, err := claude().SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, h) {
		t.Fatalf("settings path %s escaped the temp home %s", p, h)
	}
	return h
}

func claude() Agent {
	a, err := Find("claude")
	if err != nil {
		panic(err)
	}
	return a
}

func settingsFile(t *testing.T) string {
	t.Helper()
	p, err := claude().SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeSettings(t *testing.T, body string) string {
	t.Helper()
	p := settingsFile(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func parse(t *testing.T, body string) map[string]any {
	t.Helper()
	if strings.TrimSpace(body) == "" {
		return map[string]any{}
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	return doc
}

// canon renders a value the way encoding/json does for map[string]any,
// with keys sorted, so two parses of equivalent JSON compare byte for byte.
func canon(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ours returns the SessionStart entries carrying the marker, and the rest.
func ours(doc map[string]any) (mine, others []any) {
	for _, e := range sessionStart(doc) {
		m, _ := e.(map[string]any)
		found := hasOurs(m)
		if found {
			mine = append(mine, e)
		} else {
			others = append(others, e)
		}
	}
	return mine, others
}

var testCmd = Command{
	Exe:      filepath.Join(string(filepath.Separator), "opt", "sgw", "sgw"),
	Manifest: filepath.Join(string(filepath.Separator), "work", "sgw-sync.yaml"),
	Out:      filepath.Join(string(filepath.Separator), "home", "dev"),
	Every:    4 * time.Hour,
}

const (
	unrelated = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls:*)", "Read"], "deny": []},
  "env": {"FOO": "bar", "N": 1},
  "statusLine": {"type": "command", "command": "echo status"}
}
`
	withHooks = `{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "echo mine", "timeout": 5}]}
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "lint"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "notify"}]}
    ]
  },
  "env": {"FOO": "bar"}
}
`
)

func TestInstallPreservesEverythingElseAndRemoveRestores(t *testing.T) {
	cases := []struct {
		name    string
		initial *string // nil: no file
	}{
		{"missing", nil},
		{"empty", ptr("")},
		{"whitespace", ptr("  \n\t\n")},
		{"unrelated keys", ptr(unrelated)},
		{"own hooks", ptr(withHooks)},
		{"empty hooks object", ptr(`{"hooks": {}, "model": "opus"}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home(t)
			var path string
			original := map[string]any{}
			if c.initial != nil {
				path = writeSettings(t, *c.initial)
				original = parse(t, *c.initial)
			} else {
				path = settingsFile(t)
			}

			got, err := Install(claude(), testCmd, "startup|resume")
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			if got != path {
				t.Errorf("Install wrote %s, want %s", got, path)
			}
			doc := parse(t, readFile(t, path))

			// Every unrelated top-level key survives byte for byte.
			for k, v := range original {
				if k == "hooks" {
					continue
				}
				if canon(t, doc[k]) != canon(t, v) {
					t.Errorf("key %q changed: %s -> %s", k, canon(t, v), canon(t, doc[k]))
				}
			}
			for k := range doc {
				if _, ok := original[k]; !ok && k != "hooks" {
					t.Errorf("unexpected new key %q", k)
				}
			}
			// Every hook event other than SessionStart survives byte for byte,
			// and so does the user's own SessionStart entry.
			origHooks, _ := original["hooks"].(map[string]any)
			for ev, v := range origHooks {
				if ev == "SessionStart" {
					continue
				}
				if canon(t, hooksMap(doc)[ev]) != canon(t, v) {
					t.Errorf("hooks.%s changed: %s -> %s", ev, canon(t, v), canon(t, hooksMap(doc)[ev]))
				}
			}
			mine, others := ours(doc)
			origSS, _ := origHooks["SessionStart"].([]any)
			if canon(t, others) != canon(t, origSS) && !(len(others) == 0 && len(origSS) == 0) {
				t.Errorf("user SessionStart entries changed: %s -> %s", canon(t, origSS), canon(t, others))
			}
			if len(mine) != 1 {
				t.Fatalf("want exactly one sgw entry, got %d: %s", len(mine), canon(t, mine))
			}
			entry := mine[0].(map[string]any)
			if entry["matcher"] != "startup|resume" {
				t.Errorf("matcher = %v", entry["matcher"])
			}
			// Exec form: the program in "command", every argument its own
			// string in "args", so no shell ever sees the paths.
			cmds := commands(entry)
			if len(cmds) != 1 || cmds[0]["type"] != "command" || cmds[0]["command"] != testCmd.Exe || cmds[0]["timeout"] != float64(60) {
				t.Errorf("hook = %s", canon(t, cmds))
			}
			if got := stringsOf(cmds[0]["args"]); strings.Join(got, "\x00") != strings.Join(testCmd.Args(), "\x00") {
				t.Errorf("args = %q, want %q", got, testCmd.Args())
			}
			// The file must be valid JSON ending in a newline, with no temp file left.
			if body := readFile(t, path); !strings.HasSuffix(body, "\n") {
				t.Error("settings file does not end in a newline")
			}
			if _, err := os.Stat(path + ".sgw-tmp"); err == nil {
				t.Error("temp file left behind")
			}

			// Status sees it.
			st, err := Read(claude())
			if err != nil || !st.Exists || st.Command != testCmd.String() || st.Matchers != "startup|resume" {
				t.Errorf("Read = %+v, %v", st, err)
			}

			// Remove puts the parsed document back exactly.
			_, removed, err := Remove(claude())
			if err != nil || !removed {
				t.Fatalf("Remove = %v, %v", removed, err)
			}
			after := parse(t, readFile(t, path))
			// Known deviation: an empty "hooks": {} that was there before
			// install is dropped on remove, because Remove cannot tell it
			// from the object Install itself created. Claude Code reads
			// both the same way.
			for _, d := range []map[string]any{after, original} {
				if h, ok := d["hooks"].(map[string]any); ok && len(h) == 0 {
					delete(d, "hooks")
				}
			}
			if !reflect.DeepEqual(after, original) {
				t.Errorf("after Remove:\n got %s\nwant %s", canon(t, after), canon(t, original))
			}
			st, err = Read(claude())
			if err != nil || st.Command != "" {
				t.Errorf("Read after Remove = %+v, %v", st, err)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestInstallTwiceIsIdempotentAndReplaces(t *testing.T) {
	home(t)
	path := writeSettings(t, withHooks)
	for i := 0; i < 2; i++ {
		if _, err := Install(claude(), testCmd, "startup"); err != nil {
			t.Fatalf("Install #%d: %v", i+1, err)
		}
	}
	doc := parse(t, readFile(t, path))
	mine, others := ours(doc)
	if len(mine) != 1 || len(others) != 1 {
		t.Fatalf("after two installs: %d ours, %d others: %s", len(mine), len(others), canon(t, sessionStart(doc)))
	}
	// A different command or matcher replaces ours in place rather than
	// adding a second entry.
	other := testCmd
	other.Every = time.Hour
	if _, err := Install(claude(), other, "resume"); err != nil {
		t.Fatal(err)
	}
	doc = parse(t, readFile(t, path))
	mine, others = ours(doc)
	if len(mine) != 1 || len(others) != 1 {
		t.Fatalf("after reinstall: %d ours, %d others", len(mine), len(others))
	}
	e := mine[0].(map[string]any)
	got := commands(e)[0]
	if e["matcher"] != "resume" || got["command"] != other.Exe ||
		strings.Join(stringsOf(got["args"]), "\x00") != strings.Join(other.Args(), "\x00") {
		t.Errorf("reinstall did not replace: %s", canon(t, e))
	}
	if !strings.Contains(strings.Join(other.Args(), " "), "-every 1h0m0s") {
		t.Errorf("args = %q", other.Args())
	}
}

func TestRemoveWhenNothingInstalled(t *testing.T) {
	home(t)
	t.Run("missing file", func(t *testing.T) {
		home(t)
		path, removed, err := Remove(claude())
		if err != nil || removed {
			t.Errorf("Remove = %v, %v", removed, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("Remove created the settings file")
		}
	})
	t.Run("unrelated content", func(t *testing.T) {
		home(t)
		path := writeSettings(t, withHooks)
		before, _ := os.Stat(path)
		old := time.Now().Add(-2 * time.Hour)
		os.Chtimes(path, old, old)
		before, _ = os.Stat(path)

		got, removed, err := Remove(claude())
		if err != nil || removed || got != path {
			t.Errorf("Remove = %s, %v, %v", got, removed, err)
		}
		if readFile(t, path) != withHooks {
			t.Error("Remove rewrote a file it had nothing to remove from")
		}
		after, _ := os.Stat(path)
		if !after.ModTime().Equal(before.ModTime()) {
			t.Error("Remove touched the file's mtime")
		}
		if _, err := os.Stat(path + ".sgw-backup"); err == nil {
			t.Error("Remove wrote a backup without editing anything")
		}
	})
}

func TestInvalidJSONIsRefusedUntouched(t *testing.T) {
	for _, body := range []string{`{"model": `, `[1, 2]`, `"a string"`, `{"hooks": {"SessionStart": [}}`} {
		t.Run(body, func(t *testing.T) {
			home(t)
			path := writeSettings(t, body)
			old := time.Now().Add(-2 * time.Hour)
			os.Chtimes(path, old, old)

			if _, err := Install(claude(), testCmd, "startup"); err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("Install error = %v, want one naming %s", err, path)
			}
			if _, removed, err := Remove(claude()); err == nil || removed || !strings.Contains(err.Error(), path) {
				t.Errorf("Remove = %v, %v, want an error naming %s", removed, err, path)
			}
			if _, err := Read(claude()); err == nil {
				t.Error("Read accepted invalid JSON")
			}
			if readFile(t, path) != body {
				t.Error("invalid file was modified")
			}
			if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
				t.Error("invalid file was touched")
			}
			for _, extra := range []string{".sgw-backup", ".sgw-tmp"} {
				if _, err := os.Stat(path + extra); err == nil {
					t.Errorf("%s written for an invalid file", extra)
				}
			}
		})
	}
}

func TestBackupHoldsPreEditContent(t *testing.T) {
	home(t)
	path := writeSettings(t, unrelated)
	if _, err := Install(claude(), testCmd, "startup"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path+".sgw-backup"); got != unrelated {
		t.Errorf("backup = %q, want the pre-edit content", got)
	}
	// A missing file has nothing to back up.
	home(t)
	if _, err := Install(claude(), testCmd, "startup"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settingsFile(t) + ".sgw-backup"); err == nil {
		t.Error("a backup was written for a file that did not exist")
	}
}

func TestMarkerMatchesAnyExeAndQuotedPaths(t *testing.T) {
	cmds := []Command{
		{Exe: `C:\Users\dev\go\bin\sgw.exe`, Manifest: `C:\work\sgw-sync.yaml`, Out: `C:\Users\dev`, Every: 4 * time.Hour},
		{Exe: `C:\Program Files\Skills Gateway\sgw.exe`, Manifest: `C:\My Work\sgw-sync.yaml`, Out: `C:\Users\Dev Name`, Every: time.Hour},
		{Exe: "/usr/local/bin/skills-sync-renamed", Manifest: "/srv/skills/sgw-sync.yaml", Out: "/home/dev", Every: 30 * time.Minute},
		{Exe: "/Applications/My Tools/sgw", Manifest: "/Users/dev/sgw-sync.yaml", Out: "/Users/dev", Every: 4 * time.Hour},
	}
	// Quoting: only paths with whitespace are wrapped, the flags never are.
	// On Windows the hook runs under Git Bash, where an unquoted backslash
	// is an escape, so Windows paths are rendered with forward slashes.
	if runtime.GOOS == "windows" {
		if s := cmds[1].String(); s != `"C:/Program Files/Skills Gateway/sgw.exe" sync -hook -file "C:/My Work/sgw-sync.yaml" -out "C:/Users/Dev Name" -every 1h0m0s` {
			t.Errorf("String() = %q", s)
		}
		if s := cmds[0].String(); s != `C:/Users/dev/go/bin/sgw.exe sync -hook -file C:/work/sgw-sync.yaml -out C:/Users/dev -every 4h0m0s` {
			t.Errorf("String() = %q", s)
		}
	}
	if s := cmds[2].String(); s != `/usr/local/bin/skills-sync-renamed sync -hook -file /srv/skills/sgw-sync.yaml -out /home/dev -every 30m0s` {
		t.Errorf("String() = %q", s)
	}
	if s := cmds[3].String(); !strings.HasPrefix(s, `"/Applications/My Tools/sgw" sync -hook `) {
		t.Errorf("String() = %q", s)
	}
	for _, c := range cmds {
		if !strings.Contains(c.String(), Marker) {
			t.Errorf("%q does not carry the marker", c.String())
		}
	}

	// A file holding hooks from every one of those exes, plus the user's
	// own: Read finds ours, Remove drops all of ours and nothing else.
	home(t)
	var entries []any
	entries = append(entries, map[string]any{"matcher": "startup", "hooks": []any{map[string]any{"type": "command", "command": "echo mine"}}})
	for _, c := range cmds {
		entries = append(entries, map[string]any{"matcher": "startup", "hooks": []any{map[string]any{"type": "command", "command": c.String()}}})
	}
	// A user command that merely mentions sync, or -hook, is not ours.
	entries = append(entries, map[string]any{"matcher": "resume", "hooks": []any{
		map[string]any{"type": "command", "command": "sgw sync -file x.yaml"},
		map[string]any{"type": "command", "command": "other -hook"},
	}})
	doc := map[string]any{"hooks": map[string]any{"SessionStart": entries}}
	path := writeSettings(t, canon(t, doc))

	st, err := Read(claude())
	if err != nil || st.Command == "" || !strings.Contains(st.Command, Marker) {
		t.Fatalf("Read = %+v, %v", st, err)
	}
	_, removed, err := Remove(claude())
	if err != nil || !removed {
		t.Fatalf("Remove = %v, %v", removed, err)
	}
	after := parse(t, readFile(t, path))
	mine, others := ours(after)
	if len(mine) != 0 {
		t.Errorf("sgw entries survived Remove: %s", canon(t, mine))
	}
	if len(others) != 2 {
		t.Errorf("user entries after Remove = %s", canon(t, others))
	}
}

func TestReadToleratesOddShapes(t *testing.T) {
	home(t)
	// Entries that are not objects, hooks that are not lists, and a hooks
	// key that is not an object must neither crash nor count as ours.
	odd := `{"hooks": {"SessionStart": ["junk", 3, {"hooks": "nope"}, {"hooks": []}, {"hooks": [1, {"command": 2}, {"type": "prompt", "prompt": "hi"}]}]}}`
	path := writeSettings(t, odd)
	st, err := Read(claude())
	if err != nil || st.Command != "" {
		t.Errorf("Read = %+v, %v", st, err)
	}
	if _, removed, err := Remove(claude()); err != nil || removed {
		t.Errorf("Remove = %v, %v", removed, err)
	}
	if readFile(t, path) != odd {
		t.Error("Remove rewrote entries that were not ours")
	}
	// Install then Remove around them leaves every odd entry in place.
	if _, err := Install(claude(), testCmd, "startup"); err != nil {
		t.Fatal(err)
	}
	if _, removed, err := Remove(claude()); err != nil || !removed {
		t.Fatalf("Remove after Install = %v, %v", removed, err)
	}
	if got, want := canon(t, parse(t, readFile(t, path))), canon(t, parse(t, odd)); got != want {
		t.Errorf("odd entries changed:\n got %s\nwant %s", got, want)
	}
}

func TestFind(t *testing.T) {
	a, err := Find("claude")
	if err != nil || a.ID != "claude" || a.Name != "Claude Code" || a.Settings != filepath.Join(".claude", "settings.json") {
		t.Errorf("Find(claude) = %+v, %v", a, err)
	}
	if _, err := Find("emacs"); err == nil || !strings.Contains(err.Error(), `"emacs"`) || !strings.Contains(err.Error(), "claude") {
		t.Errorf("Find(emacs) = %v", err)
	}
	if _, err := Find(""); err == nil {
		t.Error("Find(\"\") succeeded")
	}
	for id, doc := range Planned {
		_, err := Find(id)
		if err == nil || !strings.Contains(err.Error(), doc) || !strings.Contains(err.Error(), Marker) {
			t.Errorf("Find(%s) = %v, want an error naming %s and the command", id, err, doc)
		}
	}
	for _, id := range []string{"cursor", "codex", "gemini"} {
		if _, ok := Planned[id]; !ok {
			t.Errorf("%s is not listed as planned", id)
		}
	}
}

func TestLocalOnly(t *testing.T) {
	root := t.TempDir()
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkfile := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir(".claude/skills/alpha")
	mkdir(".claude/skills/zeta")
	mkdir(".claude/skills/beta")
	mkfile(".claude/skills/notes.md") // a file is not a skill folder
	mkdir(".agents/skills/alpha")
	mkdir(".agents/skills/gamma")
	mkdir(".cursor/skills/alpha")
	mkdir(".github/skills/delta")
	mkfile(".cursor/rules/alpha.mdc") // single-file format: no folders to protect

	formats := []string{"claude", "codex", "cursor", "cursor-rules", "copilot", "gemini", "bogus"}
	got := LocalOnly(root, formats, map[string]bool{"alpha": true})
	want := []string{".agents/skills/gamma", ".claude/skills/beta", ".claude/skills/zeta", ".github/skills/delta"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("LocalOnly = %v, want %v", got, want)
	}
	// Everything known: nothing to report. Missing roots are not an error.
	all := map[string]bool{"alpha": true, "beta": true, "zeta": true, "gamma": true, "delta": true}
	if got := LocalOnly(root, formats, all); len(got) != 0 {
		t.Errorf("LocalOnly with everything known = %v", got)
	}
	if got := LocalOnly(filepath.Join(root, "nowhere"), formats, nil); len(got) != 0 {
		t.Errorf("LocalOnly on a missing root = %v", got)
	}
	if got := LocalOnly(root, nil, nil); len(got) != 0 {
		t.Errorf("LocalOnly with no formats = %v", got)
	}
	// Every directory format the translators know has a root here.
	for _, f := range []string{"claude", "codex", "cursor", "copilot", "gemini", "kiro", "windsurf"} {
		if formatRoot(f) == "" {
			t.Errorf("formatRoot(%s) is empty", f)
		}
	}
}
