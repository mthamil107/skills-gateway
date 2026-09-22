package translate

import (
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/skill"
)

func parse(t *testing.T, md string) *skill.Skill {
	t.Helper()
	sk, err := skill.Parse([]byte(md))
	if err != nil {
		t.Fatal(err)
	}
	return sk
}

func TestDefaultRegistry(t *testing.T) {
	reg := Default()
	want := []string{"claude", "codex", "copilot", "cursor", "cursor-rules", "gemini", "kiro", "windsurf"}
	if got := reg.Formats(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("Formats = %v", got)
	}
	for id, tr := range reg {
		if tr.Format() != id || tr.Description() == "" {
			t.Errorf("%s: Format()=%q Description()=%q", id, tr.Format(), tr.Description())
		}
	}
	if reg["claude"].(SkillDir).Root != ".claude/skills" || reg["codex"].(SkillDir).Root != ".agents/skills" || reg["cursor"].(SkillDir).Root != ".cursor/skills" {
		t.Errorf("unexpected roots: %+v", reg)
	}
}

func TestSkillDirPassThrough(t *testing.T) {
	md := "---\nname: demo\ndescription: d\n---\nbody\n"
	files := []bundle.File{{Path: "SKILL.md", Content: []byte(md)}, {Path: "scripts/x.sh", Content: []byte("echo")}}
	res, err := SkillDir{"claude", ".claude/skills", "Claude Code"}.Translate(parse(t, md), files)
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != "claude" || len(res.Files) != 2 || len(res.Warnings) != 0 {
		t.Errorf("result = %+v", res)
	}
	if res.Files[0].Path != ".claude/skills/demo/SKILL.md" || string(res.Files[0].Content) != md {
		t.Errorf("SKILL.md = %+v", res.Files[0])
	}
	if res.Files[1].Path != ".claude/skills/demo/scripts/x.sh" || string(res.Files[1].Content) != "echo" {
		t.Errorf("x.sh = %+v", res.Files[1])
	}
}

func TestCursorRule(t *testing.T) {
	md := "---\nname: demo\ndescription: \"Review: code\"\nagents:\n  cursor:\n    globs: [\"**/*.go\", \"**/*.ts\"]\n    alwaysApply: true\n---\n\n# Demo\n\nBody.\n"
	files := []bundle.File{{Path: "SKILL.md", Content: []byte(md)}, {Path: "a.md", Content: []byte("a")}, {Path: "b/c.txt", Content: []byte("c")}}
	res, err := CursorRule{}.Translate(parse(t, md), files)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != ".cursor/rules/demo.mdc" {
		t.Fatalf("files = %+v", res.Files)
	}
	got := string(res.Files[0].Content)
	want := "---\ndescription: Review: code\nglobs: **/*.go,**/*.ts\nalwaysApply: true\n---\n\n# Demo\n\nBody.\n"
	if got != want {
		t.Errorf(".mdc =\n%q\nwant\n%q", got, want)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "2 bundled file(s) dropped (a.md, b/c.txt)") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	// Without hints: agent-requested rule, no globs, no warning for a lone SKILL.md.
	plain := "---\nname: demo\ndescription: d\n---\nbody"
	res, _ = CursorRule{}.Translate(parse(t, plain), []bundle.File{{Path: "SKILL.md", Content: []byte(plain)}})
	if got := string(res.Files[0].Content); got != "---\ndescription: d\nglobs:\nalwaysApply: false\n---\nbody" || len(res.Warnings) != 0 {
		t.Errorf("plain .mdc = %q, warnings %v", got, res.Warnings)
	}
	// A non-boolean alwaysApply hint is ignored rather than trusted.
	odd := "---\nname: demo\ndescription: d\nagents:\n  cursor:\n    alwaysApply: \"yes\"\n---\n"
	res, _ = CursorRule{}.Translate(parse(t, odd), nil)
	if !strings.Contains(string(res.Files[0].Content), "alwaysApply: false") {
		t.Errorf("string alwaysApply honoured: %q", res.Files[0].Content)
	}
}
