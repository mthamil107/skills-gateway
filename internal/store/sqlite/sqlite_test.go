package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/store"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sgw.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func files(contents map[string]string) []bundle.File {
	var out []bundle.File
	for p, c := range contents {
		out = append(out, bundle.File{Path: p, Content: []byte(c)})
	}
	return bundle.FromFiles(out).Files
}

func version(ns, name, ver string, fs []bundle.File) store.Version {
	return store.Version{
		Namespace: ns, Name: name, Version: ver, Status: store.Published,
		Digest: bundle.ManifestDigest(fs), Description: "desc of " + name, License: "MIT",
		Tags: []string{"go", name}, Metadata: map[string]string{"tags": "go, " + name, "owner": "team"},
		Publisher: "alice", PublishedAt: time.Date(2026, 9, 1, 10, 0, 0, 123456789, time.UTC),
		Files: fs,
	}
}

func TestVersionRoundtrip(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	fs := files(map[string]string{"SKILL.md": "---\nname: a\ndescription: d\n---\n", "scripts/x.sh": "echo\n", "bin/blob": "\x00\x01\xff"})
	v := version("ns", "a", "1.0.0", fs)
	if err := s.PutVersion(ctx, v, fs); err != nil {
		t.Fatalf("PutVersion: %v", err)
	}
	got, err := s.GetVersion(ctx, "ns", "a", "1.0.0")
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	// The manifest comes back without content, sorted by path.
	wantFiles := make([]bundle.File, len(fs))
	for i, f := range fs {
		f.Content = nil
		wantFiles[i] = f
	}
	want := v
	want.Files = wantFiles
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetVersion =\n%+v\nwant\n%+v", got, want)
	}
	for _, f := range fs {
		c, err := s.FileContent(ctx, "ns", "a", "1.0.0", f.Path)
		if err != nil || !bytes.Equal(c, f.Content) {
			t.Errorf("FileContent(%s) = %q, %v", f.Path, c, err)
		}
	}
	if _, err := s.FileContent(ctx, "ns", "a", "1.0.0", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FileContent(nope) = %v", err)
	}
	if _, err := s.GetVersion(ctx, "ns", "a", "9.9.9"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetVersion(missing) = %v", err)
	}
	if _, err := s.GetVersion(ctx, "NS", "a", "1.0.0"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetVersion is not case-sensitive on namespace: %v", err)
	}
	// Reopening the same file sees the data (durability, schema idempotent).
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetVersion(ctx, "ns", "a", "1.0.0"); err != nil {
		t.Errorf("after reopen: %v", err)
	}
}

func TestDuplicateVersion(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	fs := files(map[string]string{"SKILL.md": "a"})
	v := version("ns", "a", "1.0.0", fs)
	if err := s.PutVersion(ctx, v, fs); err != nil {
		t.Fatal(err)
	}
	fs2 := files(map[string]string{"SKILL.md": "changed"})
	v2 := version("ns", "a", "1.0.0", fs2)
	err := s.PutVersion(ctx, v2, fs2)
	if !errors.Is(err, store.ErrVersionExists) {
		t.Fatalf("second PutVersion = %v, want ErrVersionExists", err)
	}
	// The original is untouched and no orphan files were written.
	c, _ := s.FileContent(ctx, "ns", "a", "1.0.0", "SKILL.md")
	if string(c) != "a" {
		t.Errorf("content after failed re-put = %q", c)
	}
	got, _ := s.GetVersion(ctx, "ns", "a", "1.0.0")
	if got.Digest != v.Digest || len(got.Files) != 1 {
		t.Errorf("version after failed re-put = %+v", got)
	}
	// Other versions and same name in another namespace are fine.
	if err := s.PutVersion(ctx, version("ns", "a", "1.0.1", fs), fs); err != nil {
		t.Errorf("new version: %v", err)
	}
	if err := s.PutVersion(ctx, version("other", "a", "1.0.0", fs), fs); err != nil {
		t.Errorf("other namespace: %v", err)
	}
}

func TestPutVersionAtomicInTx(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	fs := files(map[string]string{"SKILL.md": "a"})
	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx store.Store) error {
		if err := tx.PutVersion(ctx, version("ns", "a", "1.0.0", fs), fs); err != nil {
			return err
		}
		if err := tx.AppendAudit(ctx, store.Event{Time: time.Now(), Action: "publish", Outcome: "ok"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v", err)
	}
	if _, err := s.GetVersion(ctx, "ns", "a", "1.0.0"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("version survived a rolled-back tx: %v", err)
	}
	var buf bytes.Buffer
	s.ExportAudit(ctx, time.Time{}, &buf)
	if buf.Len() != 0 {
		t.Errorf("audit survived a rolled-back tx: %s", buf.String())
	}
	// Nested Tx reuses the outer transaction.
	err = s.Tx(ctx, func(tx store.Store) error {
		return tx.Tx(ctx, func(inner store.Store) error {
			return inner.PutVersion(ctx, version("ns", "a", "1.0.0", fs), fs)
		})
	})
	if err != nil {
		t.Fatalf("nested tx: %v", err)
	}
	if _, err := s.GetVersion(ctx, "ns", "a", "1.0.0"); err != nil {
		t.Errorf("after nested tx: %v", err)
	}
}

func TestSetStatus(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	fs := files(map[string]string{"SKILL.md": "a"})
	s.PutVersion(ctx, version("ns", "a", "1.0.0", fs), fs)
	if err := s.SetStatus(ctx, "ns", "a", "1.0.0", store.Deprecated); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetVersion(ctx, "ns", "a", "1.0.0")
	if got.Status != store.Deprecated {
		t.Errorf("status = %s", got.Status)
	}
	if err := s.SetStatus(ctx, "ns", "a", "2.0.0", store.Deprecated); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetStatus(missing) = %v", err)
	}
}

// rawDB opens a second, independent connection to the same database file so
// tests can attempt the writes the application layer never issues.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAuditIsAppendOnly(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	if err := s.AppendAudit(ctx, store.Event{Time: time.Now(), Subject: "alice", Action: "publish", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, path)
	for _, stmt := range []string{
		"UPDATE audit SET subject='mallory'",
		"UPDATE audit SET outcome='denied' WHERE id=1",
		"DELETE FROM audit",
		"DELETE FROM audit WHERE id=1",
	} {
		_, err := db.ExecContext(ctx, stmt)
		if err == nil {
			t.Errorf("%q succeeded; the audit log must be append-only", stmt)
			continue
		}
		if !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q: unexpected error %v", stmt, err)
		}
	}
	var buf bytes.Buffer
	if err := s.ExportAudit(ctx, time.Time{}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"subject":"alice"`) || strings.Contains(buf.String(), "mallory") {
		t.Errorf("audit changed: %s", buf.String())
	}
	// Inserts through the raw connection still work (it is append-only, not read-only).
	if _, err := db.ExecContext(ctx, "INSERT INTO audit (time, action, outcome) VALUES ('2026-01-01T00:00:00.000000000Z','x','ok')"); err != nil {
		t.Errorf("insert: %v", err)
	}
}

func TestPublishedContentIsImmutable(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	fs := files(map[string]string{"SKILL.md": "original"})
	if err := s.PutVersion(ctx, version("ns", "a", "1.0.0", fs), fs); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, path)
	blocked := []string{
		"UPDATE skill_files SET content=X'41' WHERE path='SKILL.md'",
		"UPDATE skill_files SET sha256='0' WHERE path='SKILL.md'",
		"UPDATE skill_files SET path='OTHER.md'",
		"UPDATE skill_versions SET digest='sha256:0'",
		"UPDATE skill_versions SET description='x'",
		"UPDATE skill_versions SET publisher='mallory'",
		"UPDATE skill_versions SET published_at='1999-01-01T00:00:00.000000000Z'",
		"UPDATE skill_versions SET tags='[]', metadata='{}', license='WTFPL'",
	}
	for _, stmt := range blocked {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Errorf("%q succeeded; published content must be immutable", stmt)
		} else if !strings.Contains(err.Error(), "immutable") {
			t.Errorf("%q: unexpected error %v", stmt, err)
		}
	}
	// Status is the one mutable column.
	if _, err := db.ExecContext(ctx, "UPDATE skill_versions SET status='deprecated'"); err != nil {
		t.Errorf("status update blocked: %v", err)
	}
	c, _ := s.FileContent(ctx, "ns", "a", "1.0.0", "SKILL.md")
	if string(c) != "original" {
		t.Errorf("content changed to %q", c)
	}
}

func TestExportAuditSinceAndOrder(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		e := store.Event{Time: base.Add(time.Duration(i) * time.Hour), RequestID: "r" + string(rune('0'+i)),
			Subject: "alice", AgentType: "bot", Action: "fetch", Namespace: "ns", Name: "a", Version: "1.0.0",
			Outcome: "ok", Detail: "bundle", RemoteAddr: "127.0.0.1:1"}
		if err := s.AppendAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	decode := func(since time.Time) []store.Event {
		var buf bytes.Buffer
		if err := s.ExportAudit(ctx, since, &buf); err != nil {
			t.Fatal(err)
		}
		var out []store.Event
		for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var e store.Event
			if err := json.Unmarshal(line, &e); err != nil {
				t.Fatalf("bad JSON line %q: %v", line, err)
			}
			out = append(out, e)
		}
		return out
	}
	all := decode(time.Time{})
	if len(all) != 5 {
		t.Fatalf("got %d events, want 5", len(all))
	}
	for i, e := range all {
		if e.ID != int64(i+1) || e.RequestID != "r"+string(rune('0'+i)) || !e.Time.Equal(base.Add(time.Duration(i)*time.Hour)) {
			t.Errorf("event %d out of order or corrupted: %+v", i, e)
		}
	}
	if all[0].Subject != "alice" || all[0].AgentType != "bot" || all[0].Action != "fetch" || all[0].Detail != "bundle" || all[0].RemoteAddr != "127.0.0.1:1" {
		t.Errorf("fields lost: %+v", all[0])
	}
	// since is inclusive and honours a non-UTC location.
	since := decode(base.Add(2 * time.Hour).In(time.FixedZone("IST", 5*3600+1800)))
	if len(since) != 3 || since[0].RequestID != "r2" {
		t.Errorf("since filter: %+v", since)
	}
	if got := decode(base.Add(2*time.Hour + time.Nanosecond)); len(got) != 2 {
		t.Errorf("since just after r2: %d events", len(got))
	}
	if got := decode(base.Add(24 * time.Hour)); len(got) != 0 {
		t.Errorf("future since: %d events", len(got))
	}
}

func TestListAllFilters(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	fs := files(map[string]string{"SKILL.md": "a"})
	put := func(ns, name, ver string, tags []string, desc string) {
		v := version(ns, name, ver, fs)
		v.Tags = tags
		v.Description = desc
		if err := s.PutVersion(ctx, v, fs); err != nil {
			t.Fatal(err)
		}
	}
	put("platform", "code-review", "1.0.0", []string{"go", "review"}, "Review Go code")
	put("platform", "code-review", "1.1.0", []string{"go"}, "Review Go code v2")
	put("payments", "refunds", "1.0.0", []string{"billing"}, "Process refunds")
	put("payments", "empty-tags", "1.0.0", nil, "No tags at all")
	put("zeta", "aaa", "1.0.0", []string{"go"}, "Sorted last by namespace")

	names := func(vs []store.Version) string {
		var out []string
		for _, v := range vs {
			out = append(out, v.Namespace+"/"+v.Name+"@"+v.Version)
		}
		return strings.Join(out, " ")
	}
	cases := []struct {
		f    store.ListFilter
		want string
	}{
		{store.ListFilter{}, "payments/empty-tags@1.0.0 payments/refunds@1.0.0 platform/code-review@1.0.0 platform/code-review@1.1.0 zeta/aaa@1.0.0"},
		{store.ListFilter{Namespace: "payments"}, "payments/empty-tags@1.0.0 payments/refunds@1.0.0"},
		{store.ListFilter{Tag: "go"}, "platform/code-review@1.0.0 platform/code-review@1.1.0 zeta/aaa@1.0.0"},
		{store.ListFilter{Tag: "review"}, "platform/code-review@1.0.0"},
		{store.ListFilter{Tag: "g"}, ""},      // tags are exact, not substring
		{store.ListFilter{Tag: "\"go\""}, ""}, // no JSON injection
		{store.ListFilter{Query: "REFUND"}, "payments/refunds@1.0.0"},
		{store.ListFilter{Query: "go code"}, "platform/code-review@1.0.0 platform/code-review@1.1.0"},
		{store.ListFilter{Query: "%"}, ""},
		{store.ListFilter{Namespace: "platform", Tag: "go", Query: "v2"}, "platform/code-review@1.1.0"},
		{store.ListFilter{Namespace: "nope"}, ""},
	}
	for _, c := range cases {
		vs, err := s.ListAll(ctx, c.f)
		if err != nil {
			t.Errorf("%+v: %v", c.f, err)
			continue
		}
		if got := names(vs); got != c.want {
			t.Errorf("ListAll(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
	vs, _ := s.ListVersions(ctx, "platform", "code-review")
	if names(vs) != "platform/code-review@1.0.0 platform/code-review@1.1.0" {
		t.Errorf("ListVersions = %q", names(vs))
	}
	vs, _ = s.ListVersions(ctx, "platform", "missing")
	if len(vs) != 0 {
		t.Errorf("ListVersions(missing) = %v", vs)
	}
	// Tags round-trip; nil tags come back empty, not as garbage.
	v, _ := s.GetVersion(ctx, "payments", "empty-tags", "1.0.0")
	if len(v.Tags) != 0 {
		t.Errorf("empty tags = %v", v.Tags)
	}
}
