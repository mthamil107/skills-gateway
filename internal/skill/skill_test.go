package skill

import (
	"strings"
	"testing"
)

const good = "---\nname: pdf-processing\ndescription: Extract, fill, and assemble PDF documents\nlicense: Apache-2.0\nmetadata:\n  tags: pdf, forms\nagents:\n  cursor:\n    globs: \"**/*.pdf\"\n    alwaysApply: true\n---\n\n# PDF processing\n\nBody text.\n"

func TestParseValid(t *testing.T) {
	sk, err := Parse([]byte(good))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sk.Name != "pdf-processing" {
		t.Errorf("name = %q", sk.Name)
	}
	if sk.Description != "Extract, fill, and assemble PDF documents" {
		t.Errorf("description = %q", sk.Description)
	}
	if sk.License != "Apache-2.0" {
		t.Errorf("license = %q", sk.License)
	}
	if sk.Metadata["tags"] != "pdf, forms" {
		t.Errorf("metadata.tags = %q", sk.Metadata["tags"])
	}
	if sk.Body != "\n# PDF processing\n\nBody text.\n" {
		t.Errorf("body = %q", sk.Body)
	}
	if g, _ := sk.Hint("cursor", "globs").(string); g != "**/*.pdf" {
		t.Errorf("hint globs = %v", sk.Hint("cursor", "globs"))
	}
	if a, _ := sk.Hint("cursor", "alwaysApply").(bool); !a {
		t.Errorf("hint alwaysApply = %v", sk.Hint("cursor", "alwaysApply"))
	}
	if sk.Hint("nope", "x") != nil {
		t.Error("hint for unknown agent should be nil")
	}
}

func TestParseBOMAndCRLF(t *testing.T) {
	in := "\xef\xbb\xbf" + strings.ReplaceAll(good, "\n", "\r\n")
	sk, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse with BOM+CRLF: %v", err)
	}
	if sk.Name != "pdf-processing" || sk.Description == "" {
		t.Errorf("unexpected parse result: %+v", sk.Frontmatter)
	}
	if strings.Contains(sk.Body, "\r") {
		t.Errorf("body should be LF-normalised, got %q", sk.Body)
	}
}

func TestParseInvalid(t *testing.T) {
	cases := map[string]string{
		"no frontmatter":        "# just markdown\n",
		"unterminated":          "---\nname: a\ndescription: b\n",
		"missing name":          "---\ndescription: b\n---\n",
		"missing description":   "---\nname: a\n---\n",
		"blank description":     "---\nname: a\ndescription: '   '\n---\n",
		"uppercase name":        "---\nname: Abc\ndescription: b\n---\n",
		"consecutive hyphens":   "---\nname: a--b\ndescription: b\n---\n",
		"leading hyphen":        "---\nname: -ab\ndescription: b\n---\n",
		"trailing hyphen":       "---\nname: ab-\ndescription: b\n---\n",
		"space in name":         "---\nname: a b\ndescription: b\n---\n",
		"underscore in name":    "---\nname: a_b\ndescription: b\n---\n",
		"too long name":         "---\nname: " + strings.Repeat("a", 65) + "\ndescription: b\n---\n",
		"invalid yaml":          "---\nname: [\ndescription: b\n---\n",
		"description too long":  "---\nname: a\ndescription: " + strings.Repeat("x", 1025) + "\n---\n",
		"leading blank line":    "\n---\nname: a\ndescription: b\n---\n",
		"name is a list":        "---\nname: [a]\ndescription: b\n---\n",
		"metadata not a map":    "---\nname: a\ndescription: b\nmetadata: [x]\n---\n",
		"metadata nested value": "---\nname: a\ndescription: b\nmetadata:\n  k:\n    nested: v\n---\n",
	}
	for label, in := range cases {
		t.Run(label, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Fatalf("expected error for %q", in)
			}
		})
	}
}

func TestParseNameLengthBoundary(t *testing.T) {
	name64 := strings.Repeat("a", 64)
	if _, err := Parse([]byte("---\nname: " + name64 + "\ndescription: b\n---\n")); err != nil {
		t.Errorf("64-char name should be valid: %v", err)
	}
	if _, err := Parse([]byte("---\nname: " + strings.Repeat("a", 1024) + "\ndescription: b\n---\n")); err == nil {
		t.Error("1024-char name should be rejected")
	}
	if _, err := Parse([]byte("---\nname: a\ndescription: " + strings.Repeat("x", 1024) + "\n---\n")); err != nil {
		t.Errorf("1024-char description should be valid: %v", err)
	}
}

func TestValidName(t *testing.T) {
	valid := []string{"a", "a1", "abc-def", "a-b-c", "123", strings.Repeat("z", 64)}
	invalid := []string{"", "A", "a--b", "-a", "a-", "a b", "a_b", "a.b", "a/b", strings.Repeat("z", 65), "ä"}
	for _, n := range valid {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	for _, n := range invalid {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

func TestRawFrontmatterPassesNestedMetadataThrough(t *testing.T) {
	in := "\xef\xbb\xbf---\r\nname: refunds\r\ndescription: Process refunds\r\nlicense: Apache-2.0\r\ncustom:\r\n  nested:\r\n    deep: [1, two, true]\r\n  flag: false\r\nmetadata:\r\n  owner: billing\r\n---\r\nbody\r\n"
	m, err := RawFrontmatter([]byte(in))
	if err != nil {
		t.Fatalf("RawFrontmatter: %v", err)
	}
	if m["name"] != "refunds" || m["description"] != "Process refunds" || m["license"] != "Apache-2.0" {
		t.Errorf("top-level fields not preserved: %v", m)
	}
	custom, ok := m["custom"].(map[string]any)
	if !ok {
		t.Fatalf("custom is %T, want map", m["custom"])
	}
	nested, ok := custom["nested"].(map[string]any)
	if !ok {
		t.Fatalf("custom.nested is %T, want map", custom["nested"])
	}
	deep, ok := nested["deep"].([]any)
	if !ok || len(deep) != 3 || deep[0] != 1 || deep[1] != "two" || deep[2] != true {
		t.Errorf("custom.nested.deep = %#v", nested["deep"])
	}
	if custom["flag"] != false {
		t.Errorf("custom.flag = %#v", custom["flag"])
	}
	if md, _ := m["metadata"].(map[string]any); md["owner"] != "billing" {
		t.Errorf("metadata = %#v", m["metadata"])
	}
	if _, ok := m["body"]; ok {
		t.Error("body text leaked into frontmatter")
	}
}

func TestRawFrontmatterErrors(t *testing.T) {
	for _, in := range []string{"no frontmatter", "---\nname: a\n", "---\nname: [\n---\n"} {
		if _, err := RawFrontmatter([]byte(in)); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestParseDashesInsideYAMLValue(t *testing.T) {
	md := "---\nname: demo\ndescription: |\n  first\n  ---- not a terminator\n---\nbody\n"
	sk, err := Parse([]byte(md))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sk.Description, "---- not a terminator") || sk.Body != "body\n" {
		t.Fatalf("description %q body %q", sk.Description, sk.Body)
	}
	if _, err := Parse([]byte("---\n---\n")); err == nil || strings.Contains(err.Error(), "terminated") {
		t.Fatalf("empty frontmatter should fail validation, not termination: %v", err)
	}
}

// Real skills often carry an unquoted description with a colon in it.
// Agents accept those, so the gateway must too.
func TestParseRepairsUnquotedColonInDescription(t *testing.T) {
	md := "---\nname: demo\ndescription: Does everything: screens, menus and routes\n---\nbody\n"
	sk, err := Parse([]byte(md))
	if err != nil {
		t.Fatal(err)
	}
	if sk.Description != "Does everything: screens, menus and routes" {
		t.Fatalf("description = %q", sk.Description)
	}
	raw, err := RawFrontmatter([]byte(md))
	if err != nil || raw["description"] != sk.Description {
		t.Fatalf("raw = %v, %v", raw, err)
	}
	// A nested block still parses, and a genuinely broken document still fails.
	nested := "---\nname: demo\ndescription: a: b\nagents:\n  cursor:\n    globs: [\"*.go\"]\n---\n"
	sk2, err := Parse([]byte(nested))
	if err != nil {
		t.Fatal(err)
	}
	if sk2.Hint("cursor", "globs") == nil {
		t.Error("nested agents hint lost during repair")
	}
	if _, err := Parse([]byte("---\nname: demo\n  bad-indent: x\n   worse: y\n---\n")); err == nil {
		t.Error("a genuinely malformed document should still fail")
	}
}
