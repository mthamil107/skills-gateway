package source

import (
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		in            string
		ns, name, ver string
		ok            bool
	}{
		{"demo", "", "demo", "latest", true},
		{"platform/demo", "platform", "demo", "latest", true},
		{"platform/demo@1.2.3", "platform", "demo", "1.2.3", true},
		{"demo@latest", "", "demo", "latest", true},
		{"demo@1.0.0", "", "demo", "1.0.0", true},
		{"platform/demo@1.0.0-rc.1+build", "platform", "demo", "1.0.0-rc.1+build", true},
		{"a-b/c-d1", "a-b", "c-d1", "latest", true},
		{"", "", "", "", false},
		{"/demo", "", "", "", false},
		{"platform/", "", "", "", false},
		{"a/b/c", "", "", "", false},
		{"@1.0.0", "", "", "", false},
		{"platform/demo@", "", "", "", false},
		{"demo@", "", "", "", false},
		{"Platform/demo", "", "", "", false},
		{"platform/Demo", "", "", "", false},
		{"Bad Name", "", "", "", false},
		{"bad name", "", "", "", false},
		{"a--b", "", "", "", false},
		{"ns/a--b", "", "", "", false},
		{"-a", "", "", "", false},
		{"a-", "", "", "", false},
		{"a_b", "", "", "", false},
		{"ns/a b@1.0.0", "", "", "", false},
		{"@", "", "", "", false},
		{"/", "", "", "", false},
		{strings.Repeat("a", 65), "", "", "", false},
	}
	for _, c := range cases {
		r, err := ParseRef(c.in)
		if (err == nil) != c.ok {
			t.Errorf("ParseRef(%q) err = %v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if !c.ok {
			if r != (Ref{}) {
				t.Errorf("ParseRef(%q) returned %+v with an error", c.in, r)
			}
			continue
		}
		if r.Namespace != c.ns || r.Name != c.name || r.Version != c.ver {
			t.Errorf("ParseRef(%q) = %+v, want %q %q %q", c.in, r, c.ns, c.name, c.ver)
		}
	}
}

// String must round-trip through ParseRef and drop an implicit "latest".
func TestRefStringRoundTrip(t *testing.T) {
	for in, want := range map[string]string{
		"demo":                "demo",
		"demo@latest":         "demo",
		"platform/demo":       "platform/demo",
		"platform/demo@1.2.3": "platform/demo@1.2.3",
	} {
		r, err := ParseRef(in)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", in, err)
		}
		if got := r.String(); got != want {
			t.Errorf("ParseRef(%q).String() = %q, want %q", in, got, want)
		}
		again, err := ParseRef(r.String())
		if err != nil || again != r {
			t.Errorf("round trip of %q: %+v, %v", in, again, err)
		}
	}
	if got := (Ref{Name: "x"}).String(); got != "x" {
		t.Errorf("empty version renders as %q", got)
	}
}
