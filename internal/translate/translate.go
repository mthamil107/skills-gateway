// Package translate maps a canonical skill bundle onto the files a given
// agent expects in a project.
//
// Every mainstream agent now reads SKILL.md directories natively, so most
// targets are a pass-through into that agent's skills directory. Real
// translation is only needed for legacy rule formats such as Cursor .mdc.
package translate

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/skill"
)

// OutFile is one file to write, relative to the project root.
type OutFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// Result is a translation plus any fidelity warnings.
type Result struct {
	Format   string    `json:"format"`
	Files    []OutFile `json:"files"`
	Warnings []string  `json:"warnings,omitempty"`
}

// Translator produces agent-native files for one skill.
type Translator interface {
	Format() string
	Description() string
	Translate(sk *skill.Skill, files []bundle.File) (Result, error)
}

// SkillDir places the unmodified skill directory under Root/<name>/.
type SkillDir struct {
	ID, Root, Agents string
}

func (d SkillDir) Format() string { return d.ID }
func (d SkillDir) Description() string {
	return fmt.Sprintf("SKILL.md directory in %s/<name>/ (%s)", d.Root, d.Agents)
}

func (d SkillDir) Translate(sk *skill.Skill, files []bundle.File) (Result, error) {
	r := Result{Format: d.ID}
	for _, f := range files {
		r.Files = append(r.Files, OutFile{Path: d.Root + "/" + sk.Name + "/" + f.Path, Content: f.Content})
	}
	return r, nil
}

// CursorRule renders a skill as a Cursor project rule (.cursor/rules/*.mdc).
// SKILL.md has no globs/alwaysApply, so they come from the optional
// agents.cursor frontmatter hint and default to an agent-requested rule.
type CursorRule struct{}

func (CursorRule) Format() string { return "cursor-rules" }
func (CursorRule) Description() string {
	return "Cursor project rule in .cursor/rules/<name>.mdc (legacy rules format; lossy)"
}

func (CursorRule) Translate(sk *skill.Skill, files []bundle.File) (Result, error) {
	r := Result{Format: "cursor-rules"}
	// Cursor's rule frontmatter is not general YAML: it expects a one-line
	// description, globs as a single comma-separated value, and a boolean.
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.WriteString("description: " + strings.Join(strings.Fields(sk.Description), " ") + "\n")
	buf.WriteString(strings.TrimSpace("globs: "+globsOf(sk.Hint("cursor", "globs"))) + "\n")
	always := false
	if a, ok := sk.Hint("cursor", "alwaysApply").(bool); ok {
		always = a
	}
	fmt.Fprintf(&buf, "alwaysApply: %t\n", always)
	buf.WriteString("---\n")
	buf.WriteString(sk.Body)
	r.Files = append(r.Files, OutFile{Path: ".cursor/rules/" + sk.Name + ".mdc", Content: buf.Bytes()})

	var extra []string
	for _, f := range files {
		if f.Path != "SKILL.md" {
			extra = append(extra, f.Path)
		}
	}
	if len(extra) > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"cursor-rules carries only SKILL.md; %d bundled file(s) dropped (%s). Use format \"cursor\" for the full skill directory.",
			len(extra), strings.Join(extra, ", ")))
	}
	return r, nil
}

// globsOf renders a globs hint (a string or a list) as Cursor's
// comma-separated form.
func globsOf(v any) string {
	switch g := v.(type) {
	case string:
		return strings.TrimSpace(g)
	case []any:
		var parts []string
		for _, e := range g {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
		return strings.Join(parts, ",")
	}
	return ""
}

// Registry holds the known translators by format id.
type Registry map[string]Translator

// Default returns the built-in translators. Directory locations follow each
// agent's published documentation as of September 2026.
func Default() Registry {
	reg := Registry{}
	for _, t := range []Translator{
		SkillDir{"claude", ".claude/skills", "Claude Code; also read by Copilot and Windsurf"},
		SkillDir{"codex", ".agents/skills", "OpenAI Codex; also read by Windsurf"},
		SkillDir{"cursor", ".cursor/skills", "Cursor"},
		SkillDir{"copilot", ".github/skills", "GitHub Copilot"},
		SkillDir{"gemini", ".gemini/skills", "Gemini CLI"},
		SkillDir{"kiro", ".kiro/skills", "Kiro"},
		SkillDir{"windsurf", ".windsurf/skills", "Windsurf"},
		CursorRule{},
	} {
		reg[t.Format()] = t
	}
	return reg
}

// Formats lists format ids in sorted order.
func (r Registry) Formats() []string {
	out := make([]string, 0, len(r))
	for k := range r {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
