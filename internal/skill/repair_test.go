package skill

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A document that parses as written must come back exactly as strict YAML
// reads it, colons or not: the repair may only run on a failed parse.
func TestRepairLeavesParsingDocumentsAlone(t *testing.T) {
	docs := map[string]string{
		"quoted colon":     "---\nname: demo\ndescription: \"Does everything: screens, menus\"\n---\n",
		"single quoted":    "---\nname: demo\ndescription: 'a: b'\nlicense: 'MIT: yes'\n---\n",
		"block scalar":     "---\nname: demo\ndescription: |\n  first: line\n  second: line\n---\n",
		"folded scalar":    "---\nname: demo\ndescription: >\n  first: line\n  more\n---\n",
		"nested mapping":   "---\nname: demo\ndescription: d\nmetadata:\n  owner: team\n  note: \"a: b\"\nagents:\n  cursor:\n    globs: \"*.go\"\n---\n",
		"colon no space":   "---\nname: demo\ndescription: http://example.com/a:b\n---\n",
		"flow collections": "---\nname: demo\ndescription: d\nagents:\n  cursor: {globs: [\"a: b\", \"c\"], alwaysApply: true}\n---\n",
		"anchors":          "---\nname: demo\ndescription: &d 'x: y'\nlicense: *d\n---\n",
		"list values":      "---\nname: demo\ndescription: d\nmetadata:\n  a: 'x: y'\ncustom:\n  - k: 'v: w'\n---\n",
		"comment":          "---\nname: demo # the name\ndescription: d # a: comment\n---\n",
	}
	for label, doc := range docs {
		t.Run(label, func(t *testing.T) {
			header, _, err := split([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			var strict map[string]any
			if err := yaml.Unmarshal([]byte(header), &strict); err != nil {
				t.Fatalf("fixture must parse strictly: %v", err)
			}
			raw, err := RawFrontmatter([]byte(doc))
			if err != nil {
				t.Fatalf("RawFrontmatter: %v", err)
			}
			if !reflect.DeepEqual(raw, strict) {
				t.Errorf("repair changed a valid document:\n got %#v\nwant %#v", raw, strict)
			}
			sk, err := Parse([]byte(doc))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if want, _ := strict["description"].(string); strings.TrimSpace(want) != sk.Description {
				t.Errorf("description = %q, want %q", sk.Description, want)
			}
		})
	}
}

// Several plain values with colons in one document are all repaired, at
// any indentation, without touching anything else.
func TestRepairHandlesSeveralColonValues(t *testing.T) {
	doc := "---\n" +
		"name: demo\n" +
		"description: Does everything: screens, menus and routes\n" +
		"license: Proprietary: internal use only\n" +
		"compatibility: Needs: go 1.26\n" +
		"allowed-tools: Bash(git: *) Read\n" +
		"metadata:\n" +
		"  owner: team: platform\n" +
		"  note: plain value\n" +
		"  count: 3\n" +
		"agents:\n" +
		"  cursor:\n" +
		"    globs: \"**/*.go\"\n" +
		"    hint: apply to: everything\n" +
		"    alwaysApply: true\n" +
		"---\nbody\n"
	sk, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Frontmatter{
		Name:          "demo",
		Description:   "Does everything: screens, menus and routes",
		License:       "Proprietary: internal use only",
		Compatibility: "Needs: go 1.26",
		AllowedTools:  "Bash(git: *) Read",
		Metadata:      map[string]string{"owner": "team: platform", "note": "plain value", "count": "3"},
		Agents:        map[string]map[string]any{"cursor": {"globs": "**/*.go", "hint": "apply to: everything", "alwaysApply": true}},
	}
	if !reflect.DeepEqual(sk.Frontmatter, want) {
		t.Errorf("frontmatter =\n%#v\nwant\n%#v", sk.Frontmatter, want)
	}
	if sk.Body != "body\n" {
		t.Errorf("body = %q", sk.Body)
	}
	raw, err := RawFrontmatter([]byte(doc))
	if err != nil {
		t.Fatalf("RawFrontmatter: %v", err)
	}
	if raw["license"] != want.License || raw["metadata"].(map[string]any)["owner"] != "team: platform" {
		t.Errorf("raw = %#v", raw)
	}
	if raw["metadata"].(map[string]any)["count"] != 3 {
		t.Errorf("repair must not quote values without a colon: count = %#v", raw["metadata"].(map[string]any)["count"])
	}
	// CRLF and a BOM do not defeat the repair.
	crlf := "\xef\xbb\xbf" + strings.ReplaceAll(doc, "\n", "\r\n")
	if sk2, err := Parse([]byte(crlf)); err != nil || sk2.Description != want.Description || sk2.Metadata["owner"] != "team: platform" {
		t.Errorf("CRLF: %+v, %v", sk2, err)
	}
	// A colon value whose repaired form is still an invalid name is
	// rejected by validation, not accepted by the repair.
	if _, err := Parse([]byte("---\nname: demo: x\ndescription: d\n---\n")); err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("colon in name: %v", err)
	}
}

// A repair must not rewrite a line that sits inside a block scalar.
func TestRepairDoesNotTouchBlockScalarLines(t *testing.T) {
	doc := "---\n" +
		"name: demo\n" +
		"description: Broken: on purpose\n" +
		"metadata:\n" +
		"  note: |\n" +
		"    see: this: thing\n" +
		"    plain line\n" +
		"---\n"
	sk, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sk.Description != "Broken: on purpose" {
		t.Errorf("description = %q", sk.Description)
	}
	if got := sk.Metadata["note"]; got != "see: this: thing\nplain line\n" {
		t.Errorf("block scalar content was rewritten by the repair: %q", got)
	}
	// Folded and chomped blocks, blank lines inside a block, and a key
	// after the block that itself needs repair.
	doc = "---\n" +
		"name: demo\n" +
		"description: >-\n" +
		"  folded: first\n" +
		"\n" +
		"  folded: second\n" +
		"license: Broken: on purpose\n" +
		"metadata:\n" +
		"  a: |+\n" +
		"    keep: this\n" +
		"  b: after: block\n" +
		"---\n"
	sk, err = Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse folded: %v", err)
	}
	if sk.Description != "folded: first\nfolded: second" || sk.License != "Broken: on purpose" {
		t.Errorf("folded block or following key wrong: %q %q", sk.Description, sk.License)
	}
	if sk.Metadata["a"] != "keep: this\n" || sk.Metadata["b"] != "after: block" {
		t.Errorf("metadata = %#v", sk.Metadata)
	}
}

// Genuinely malformed YAML still fails, and the error names the original
// problem rather than a side effect of the repair.
func TestRepairStillRejectsMalformedYAML(t *testing.T) {
	cases := map[string]string{
		"bad indent":          "---\nname: demo\n  bad-indent: x\n   worse: y\n---\n",
		"unclosed flow":       "---\nname: demo\ndescription: [a: b\n---\n",
		"unclosed quote":      "---\nname: demo\ndescription: \"a: b\n---\n",
		"tab indentation":     "---\nname: demo\ndescription: d\nmetadata:\n\towner: a: b\n---\n",
		"duplicate colon key": "---\nname: demo\ndescription: a: b\ndescription: c: d\n---\n",
		"mapping and scalar":  "---\nname: demo\ndescription: a: b\nmetadata: x\n  y: z\n---\n",
		"colon then nested":   "---\nname: demo\ndescription: a: b\n  c: d\n---\n",
		"not a mapping":       "---\n- a: b\n- c: d\n---\n",
	}
	for label, in := range cases {
		t.Run(label, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Fatalf("accepted %q", in)
			}
			if _, err := RawFrontmatter([]byte(in)); err == nil {
				t.Fatalf("RawFrontmatter accepted %q", in)
			}
		})
	}
	// The reported error is the original YAML error, so the user sees the
	// real line, not a repaired one.
	_, err := Parse([]byte("---\nname: demo\ndescription: a: b\nmetadata: [\n---\n"))
	if err == nil || !strings.Contains(err.Error(), "frontmatter") {
		t.Errorf("error = %v", err)
	}
}
