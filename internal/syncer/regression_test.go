package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mthamil107/skills-gateway/internal/gatewaytest"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

// A version-constrained policy must make "latest" resolve to the newest
// version the caller may fetch, consistently for list, get and sync.
func TestLatestRespectsVersionConstrainedPolicy(t *testing.T) {
	g := gatewaytest.New(t, gatewaytest.Options{Policy: `
version: 1
rules:
  - id: admin-all
    effect: allow
    subject: {roles: [admin]}
    actions: [fetch, publish, deprecate, admin]
  - id: readers-v1-only
    effect: allow
    subject: {any: true}
    actions: [fetch]
    resource: {namespace: platform, version: "<2.0.0"}
`})
	g.Publish(t, gatewaytest.AdminToken, "platform", "demo", "1.0.0", gatewaytest.Files("demo", nil))
	g.Publish(t, gatewaytest.AdminToken, "platform", "demo", "2.0.0", gatewaytest.Files("demo", nil))

	reader := g.Client(gatewaytest.ReaderToken)
	page, err := reader.List(ctx, "", "", "", "", 0)
	if err != nil || len(page.Items) != 1 || page.Items[0].Version != "1.0.0" {
		t.Fatalf("list = %+v, %v", page.Items, err)
	}
	v, err := reader.Get(ctx, "platform", "demo", "latest")
	if err != nil || v.Version != "1.0.0" {
		t.Fatalf("get latest = %s, %v", v.Version, err)
	}
	root := t.TempDir()
	rep, err := Sync(ctx, reader, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}})
	if err != nil || rep.Skills[0].Version != "1.0.0" {
		t.Fatalf("sync = %+v, %v", rep.Skills, err)
	}
	if admin, _ := g.Client(gatewaytest.AdminToken).Get(ctx, "platform", "demo", "latest"); admin.Version != "2.0.0" {
		t.Errorf("admin latest = %s", admin.Version)
	}
}

// The lock pin applies to "latest" refs too: same resolved version with a
// different digest is tampering.
func TestPinAppliesToLatestRefs(t *testing.T) {
	_, c, root := setup(t)
	m := &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}}
	if _, err := Sync(ctx, c, translate.Default(), root, m); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, LockFile)
	var l Lock
	data, _ := os.ReadFile(lockPath)
	json.Unmarshal(data, &l)
	l.Skills[0].Digest = "sha256:" + strings.Repeat("0", 64)
	data, _ = json.Marshal(l)
	os.WriteFile(lockPath, data, 0o644)
	if _, err := Sync(ctx, c, translate.Default(), root, m); err == nil || !strings.Contains(err.Error(), "does not match the pinned") {
		t.Fatalf("expected pin mismatch for latest ref, got %v", err)
	}
}

// A symlinked directory inside the project must not redirect writes
// outside it.
func TestSyncRefusesSymlinkedDirectory(t *testing.T) {
	_, c, root := setup(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".claude")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	_, err := Sync(ctx, c, translate.Default(), root, &Manifest{Formats: []string{"claude"}, Skills: []string{"platform/demo"}})
	if err == nil {
		t.Fatal("sync wrote through a symlinked directory")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("files escaped the project: %v", entries)
	}
}
