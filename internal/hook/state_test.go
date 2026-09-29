package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatePathIsUnderHome(t *testing.T) {
	h := home(t)
	p, err := StatePath()
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(h, ".sgw", "state.json") {
		t.Errorf("StatePath = %s", p)
	}
}

func TestKeyIsStableAndDistinct(t *testing.T) {
	a := Key("/w/sgw-sync.yaml", "/home/dev")
	if a != Key("/w/sgw-sync.yaml", "/home/dev") || len(a) != 16 {
		t.Errorf("Key is not stable: %q", a)
	}
	for _, other := range []string{Key("/w/sgw-sync.yaml", "/home/other"), Key("/x/sgw-sync.yaml", "/home/dev"), Key("/w/sgw-sync.yaml/home", "/dev")} {
		if other == a {
			t.Errorf("distinct installs share key %q", a)
		}
	}
}

func TestLoadStateMissingOrCorruptIsEmpty(t *testing.T) {
	home(t)
	check := func(label string) {
		t.Helper()
		st := LoadState()
		if st.Runs == nil || len(st.Runs) != 0 {
			t.Errorf("%s: LoadState = %+v, want empty", label, st)
		}
		// An empty state must still be usable: Due and Record must not
		// panic on it.
		if !st.Due("k", time.Hour) {
			t.Errorf("%s: an empty state is not due", label)
		}
	}
	check("missing file")
	p, _ := StatePath()
	os.MkdirAll(filepath.Dir(p), 0o755)
	for _, body := range []string{"{not json", `[]`, `{"runs": null}`, `{"runs": "x"}`, ""} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		check("body " + body)
	}
	// A state path that cannot exist (home is a file) is also just empty.
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, []byte("x"), 0o600)
	t.Setenv("HOME", f)
	t.Setenv("USERPROFILE", f)
	check("home is a file")
	State{Runs: map[string]Run{}}.Record("k", "p", "v") // must not panic or fail
}

func TestRecordRoundTrips(t *testing.T) {
	h := home(t)
	key := Key("/w/sgw-sync.yaml", "/home/dev")
	before := time.Now().UTC().Add(-time.Second)
	st := LoadState()
	st.Record(key, "git:abc123", "3 skills, abc123")
	st.Record("other", "dir:def", "")

	got := LoadState()
	run, ok := got.Runs[key]
	if !ok || run.Peek != "git:abc123" || run.Version != "3 skills, abc123" {
		t.Fatalf("round trip = %+v, %v", run, ok)
	}
	if run.LastRun.Before(before) || run.LastRun.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("LastRun = %v, want about now", run.LastRun)
	}
	if run.LastRun.Location() != time.UTC {
		t.Errorf("LastRun is not UTC: %v", run.LastRun)
	}
	if len(got.Runs) != 2 || got.Runs["other"].Peek != "dir:def" {
		t.Errorf("state = %+v", got.Runs)
	}
	data, err := os.ReadFile(filepath.Join(h, ".sgw", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), key) || !strings.HasSuffix(string(data), "\n") {
		t.Errorf("state file = %s", data)
	}
	// Recording again replaces the run rather than accumulating.
	got.Record(key, "git:abc124", "")
	if again := LoadState(); len(again.Runs) != 2 || again.Runs[key].Peek != "git:abc124" || again.Runs[key].Version != "" {
		t.Errorf("after re-record = %+v", again.Runs)
	}
}

func TestDueHonoursInterval(t *testing.T) {
	st := State{Runs: map[string]Run{}}
	if !st.Due("new", 4*time.Hour) {
		t.Error("a key that never ran must be due")
	}
	st.Runs["k"] = Run{LastRun: time.Now().UTC()}
	if st.Due("k", 4*time.Hour) {
		t.Error("just ran: must not be due")
	}
	if !st.Due("k", 0) {
		t.Error("every=0 must always be due")
	}
	st.Runs["k"] = Run{LastRun: time.Now().UTC().Add(-3*time.Hour - 59*time.Minute)}
	if st.Due("k", 4*time.Hour) {
		t.Error("3h59m ago: must not be due for a 4h interval")
	}
	st.Runs["k"] = Run{LastRun: time.Now().UTC().Add(-4*time.Hour - time.Second)}
	if !st.Due("k", 4*time.Hour) {
		t.Error("over 4h ago: must be due")
	}
	if !st.Due("k", time.Hour) {
		t.Error("over 4h ago: must be due for a 1h interval")
	}
	// A clock set to the future (a bad timestamp in the file) still
	// resolves rather than being due forever or never.
	st.Runs["k"] = Run{LastRun: time.Now().UTC().Add(time.Hour)}
	if st.Due("k", 4*time.Hour) {
		t.Error("a future LastRun should not be due yet")
	}
}
