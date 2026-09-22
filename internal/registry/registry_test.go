package registry

import (
	"testing"

	"github.com/mthamil107/skills-gateway/internal/skill"
	"github.com/mthamil107/skills-gateway/internal/store"
)

func TestValidVersion(t *testing.T) {
	valid := []string{"0.0.0", "1.2.3", "1.0.0-rc.1", "1.0.0-alpha.beta.1", "1.0.0+build.7", "1.0.0-rc.1+meta", "10.20.30"}
	invalid := []string{"", "v1.2.3", "1.2", "1", "1.2.3.4", "01.2.3", "1.2.3-", "latest", "1.2.3 ", " 1.2.3", "1.2.x", "1..3", "-1.2.3"}
	for _, v := range valid {
		if !ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = false", v)
		}
	}
	for _, v := range invalid {
		if ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = true", v)
		}
	}
}

func TestLatestPrefersStableAndSkipsDeprecated(t *testing.T) {
	vs := func(specs ...string) []store.Version {
		var out []store.Version
		for _, s := range specs {
			st := store.Published
			if s[0] == '!' {
				st, s = store.Deprecated, s[1:]
			}
			out = append(out, store.Version{Version: s, Status: st})
		}
		return out
	}
	cases := []struct {
		in   []store.Version
		want string
	}{
		{vs("1.0.0", "1.1.0", "2.0.0-rc.1"), "1.1.0"},
		{vs("2.0.0-rc.1", "1.1.0", "1.0.0"), "1.1.0"},
		{vs("!1.1.0", "1.0.0", "2.0.0-rc.1"), "1.0.0"},
		{vs("!1.1.0", "!1.0.0", "2.0.0-rc.1", "2.0.0-rc.2"), "2.0.0-rc.2"},
		{vs("1.0.0-alpha", "1.0.0-beta"), "1.0.0-beta"},
		{vs("!1.0.0"), ""},
		{vs(), ""},
		{vs("1.0.0+b2", "1.0.0+b1"), "1.0.0+b2"}, // equal precedence: first wins
		{vs("1.10.0", "1.9.0"), "1.10.0"},
	}
	for _, c := range cases {
		got := ""
		if b := latest(c.in); b != nil {
			got = b.Version
		}
		if got != c.want {
			t.Errorf("latest(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTagsOfSplitsAndTrims(t *testing.T) {
	sk := &skill.Skill{}
	sk.Metadata = map[string]string{"tags": " go , review,,  , ci"}
	if got := tagsOf(sk); len(got) != 3 || got[0] != "go" || got[1] != "review" || got[2] != "ci" {
		t.Errorf("tagsOf = %v", got)
	}
	sk.Metadata = nil
	if got := tagsOf(sk); got != nil {
		t.Errorf("tagsOf(nil metadata) = %v", got)
	}
}
