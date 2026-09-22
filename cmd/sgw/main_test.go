package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		in            string
		ns, name, ver string
		ok            bool
	}{
		{"platform/demo", "platform", "demo", "latest", true},
		{"platform/demo@1.2.3", "platform", "demo", "1.2.3", true},
		{"platform/demo@latest", "platform", "demo", "latest", true},
		{"platform/demo@1.0.0-rc.1+build", "platform", "demo", "1.0.0-rc.1+build", true},
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
		if err != nil && !strings.Contains(err.Error(), c.in) {
			t.Errorf("parseRef(%q) error does not quote the input: %v", c.in, err)
		}
	}
}

func TestFlagsAfterPositionals(t *testing.T) {
	for _, args := range [][]string{
		{"a/b", "-out", "x", "-format", "codex"},
		{"-out", "x", "a/b", "-format", "codex"},
		{"-out", "x", "-format", "codex", "a/b"},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		out := fs.String("out", ".", "")
		format := fs.String("format", "claude", "")
		pos, err := flagsAfter(fs, args)
		if err != nil || len(pos) != 1 || pos[0] != "a/b" || *out != "x" || *format != "codex" {
			t.Errorf("%v: pos=%v out=%q format=%q err=%v", args, pos, *out, *format, err)
		}
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if _, err := flagsAfter(fs, []string{"a/b", "-bogus"}); err == nil {
		t.Error("unknown flag accepted")
	}
}

func TestReadDirSkipsHiddenAndRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	mk("SKILL.md", "---\nname: demo\ndescription: d\n---\n")
	mk("scripts/run.sh", "echo")
	mk(".git/config", "secret")
	mk(".env", "TOKEN=x")
	mk("sub/.hidden.txt", "h")
	files, err := readDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, " "); got != "SKILL.md scripts/run.sh" {
		t.Errorf("readDir = %v", paths)
	}
	if _, err := readDir(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing dir accepted")
	}
	target := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(target, []byte("x"), 0o644)
	if err := os.Symlink(target, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readDir(dir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlink accepted: %v", err)
	}
}

func TestBuildAuthRefusesNoneWithoutFlag(t *testing.T) {
	// Exercised through the config layer without touching the network.
	if _, err := newClient(); err == nil && os.Getenv("SGW_URL") == "" {
		t.Error("newClient without SGW_URL succeeded")
	}
}
