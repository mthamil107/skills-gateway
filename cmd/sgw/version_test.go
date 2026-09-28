package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

// `go install` cannot pass -ldflags, so the version has to come from the
// module the toolchain recorded. Only a hand-built binary reports the
// development default.
func TestBuildVersion(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })

	version = "0.1.0"
	if got := buildVersion(); got != "0.1.0" {
		t.Errorf("stamped build = %q, want the ldflags value", got)
	}

	version = original
	got := buildVersion()
	info, ok := debug.ReadBuildInfo()
	switch {
	case ok && info.Main.Version != "" && info.Main.Version != "(devel)":
		if got != strings.TrimPrefix(info.Main.Version, "v") {
			t.Errorf("build-info build = %q, want %q", got, info.Main.Version)
		}
		if strings.HasPrefix(got, "v") {
			t.Errorf("version must not carry a leading v: %q", got)
		}
	default:
		if got != original {
			t.Errorf("development build = %q, want %q", got, original)
		}
	}
}
