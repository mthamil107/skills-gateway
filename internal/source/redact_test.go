package source

import (
	"strings"
	"testing"
)

// Credentials in a clone URL must never reach the lock file, which is
// committed, or an error message, which is pasted into issues.
func TestRedactStripsCredentials(t *testing.T) {
	cases := map[string]string{
		"https://user:token@github.com/acme/skills.git": "https://github.com/acme/skills.git",
		"https://github.com/acme/skills.git":            "https://github.com/acme/skills.git",
		"git@github.com:acme/skills.git":                "git@github.com:acme/skills.git",
		"/plain/local/path":                             "/plain/local/path",
	}
	for in, want := range cases {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(redact(in), "token") {
			t.Errorf("redact(%q) leaked the credential", in)
		}
	}
}
