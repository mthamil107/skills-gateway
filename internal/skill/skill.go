// Package skill parses and validates SKILL.md files as defined by the
// Agent Skills open standard (https://agentskills.io/specification).
package skill

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter is the YAML header of a SKILL.md file. Only name and
// description are required by the standard; everything else is optional.
type Frontmatter struct {
	Name          string            `yaml:"name" json:"name"`
	Description   string            `yaml:"description" json:"description"`
	License       string            `yaml:"license,omitempty" json:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty" json:"compatibility,omitempty"`
	AllowedTools  string            `yaml:"allowed-tools,omitempty" json:"allowed_tools,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`

	// Agents carries optional per-agent hints used by format translators,
	// e.g. agents.cursor.globs. It is a Skills Gateway extension and is
	// ignored by agents that read SKILL.md natively.
	Agents map[string]map[string]any `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// Skill is a parsed SKILL.md: its frontmatter plus the markdown body.
type Skill struct {
	Frontmatter
	Body string `json:"-"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidName reports whether s is a valid skill or namespace name:
// 1-64 characters, lowercase alphanumerics and single hyphens, no leading
// or trailing hyphen.
func ValidName(s string) bool {
	return len(s) >= 1 && len(s) <= 64 && nameRe.MatchString(s)
}

// Parse reads a SKILL.md document and validates the required fields.
func Parse(data []byte) (*Skill, error) {
	header, body, err := split(data)
	if err != nil {
		return nil, err
	}
	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(header), &fm); err != nil {
		return nil, fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	if !ValidName(fm.Name) {
		return nil, fmt.Errorf("SKILL.md name %q is invalid: use 1-64 lowercase letters, digits and single hyphens", fm.Name)
	}
	fm.Description = strings.TrimSpace(fm.Description)
	if fm.Description == "" {
		return nil, errors.New("SKILL.md description is required")
	}
	if len(fm.Description) > 1024 {
		return nil, errors.New("SKILL.md description exceeds 1024 characters")
	}
	return &Skill{Frontmatter: fm, Body: body}, nil
}

// Hint returns an agent-specific frontmatter hint, or nil.
func (s *Skill) Hint(agent, key string) any {
	if s.Agents == nil {
		return nil
	}
	return s.Agents[agent][key]
}

// RawFrontmatter returns the SKILL.md frontmatter exactly as authored,
// decoded into generic JSON-compatible values (MCP SEP-2640 requires the
// frontmatter to be passed through verbatim).
func RawFrontmatter(data []byte) (map[string]any, error) {
	header, _, err := split(data)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(header), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// split separates the YAML frontmatter from the body. The frontmatter opens
// with a first line of exactly "---" and closes at the next such line, so a
// "---" inside a YAML value does not end it early.
func split(data []byte) (header, body string, err error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.SplitAfter(text, "\n")
	if strings.TrimRight(lines[0], " \t\n") != "---" {
		return "", "", errors.New("SKILL.md must start with a YAML frontmatter block (---)")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\n") == "---" {
			return strings.Join(lines[1:i], ""), strings.Join(lines[i+1:], ""), nil
		}
	}
	return "", "", errors.New("SKILL.md frontmatter is not terminated by ---")
}
