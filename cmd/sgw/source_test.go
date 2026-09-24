package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/source"
)

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	cases := map[string]string{
		"~":                      home,
		"~/x":                    filepath.Join(home, "x"),
		"~/a/b/c":                filepath.Join(home, "a", "b", "c"),
		`~\x`:                    filepath.Join(home, "x"),
		".":                      ".",
		"":                       "",
		"plain/relative":         "plain/relative",
		"/abs/path":              "/abs/path",
		"~user/x":                "~user/x", // another user's home is not expanded
		"~x":                     "~x",
		"a/~/b":                  "a/~/b", // only a leading tilde
		"C:\\Users\\me\\~":       "C:\\Users\\me\\~",
		filepath.Join(home, "y"): filepath.Join(home, "y"), // already absolute
	}
	for in, want := range cases {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
	// "~/" alone is the home directory, not home plus an empty segment.
	if got := expandHome("~/"); filepath.Clean(got) != filepath.Clean(home) {
		t.Errorf("expandHome(\"~/\") = %q, want %q", got, home)
	}
}

func TestSourceForPicksOneSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, err := sourceFor(ctx, dir, "", "", "")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if d, ok := src.(*source.Dir); !ok || d.Root != dir || d.Pinnable() {
		t.Errorf("path source = %#v", src)
	}
	// A path is home-expanded like -out.
	if home, err := os.UserHomeDir(); err == nil {
		src, _ := sourceFor(ctx, "~/skills", "", "", "")
		if d, ok := src.(*source.Dir); !ok || d.Root != filepath.Join(home, "skills") {
			t.Errorf("tilde path source = %#v", src)
		}
	}
	if _, err := sourceFor(ctx, dir, "https://example.com/x.git", "", ""); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("path and git together: %v", err)
	}
	// Without a path or git, the gateway needs SGW_URL.
	t.Setenv("SGW_URL", "")
	if _, err := sourceFor(ctx, "", "", "", ""); err == nil || !strings.Contains(err.Error(), "SGW_URL") {
		t.Errorf("no source and no SGW_URL: %v", err)
	}
	t.Setenv("SGW_URL", "http://127.0.0.1:1")
	t.Setenv("SGW_TOKEN", "tok")
	src, err = sourceFor(ctx, "", "", "", "")
	if err != nil {
		t.Fatalf("gateway from env: %v", err)
	}
	g, ok := src.(*source.Gateway)
	if !ok || g.Client.BaseURL != "http://127.0.0.1:1" || g.Client.Token != "tok" || !g.Pinnable() {
		t.Errorf("gateway source = %#v", src)
	}
	// The manifest's gateway URL overrides the environment's.
	src, err = gatewaySource("http://gateway.example")
	if err != nil || src.(*source.Gateway).Client.BaseURL != "http://gateway.example" {
		t.Errorf("gatewaySource(url) = %#v, %v", src, err)
	}
	// -ref and -dir without -git are silently ignored by sourceFor (the
	// path wins), which is documented behaviour of the flag set; but a
	// git URL that cannot be cloned is an error.
	if _, err := sourceFor(ctx, "", filepath.Join(t.TempDir(), "nope"), "main", "skills"); err == nil {
		t.Error("unclonable git source accepted")
	}
}
