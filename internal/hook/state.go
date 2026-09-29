package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// State remembers, per install, when sync last ran and what the source
// looked like then. It is what lets a session hook cost nothing when
// nothing has changed.
type State struct {
	Runs map[string]Run `json:"runs"`
}

// Run is one install target's last outcome.
type Run struct {
	LastRun time.Time `json:"last_run"`
	Peek    string    `json:"peek,omitempty"`
	Version string    `json:"version,omitempty"`
}

// StatePath is where the state file lives.
func StatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".sgw", "state.json"), nil
}

// Key identifies one install: a manifest written into a root.
func Key(manifest, root string) string {
	sum := sha256.Sum256([]byte(manifest + "\x00" + root))
	return hex.EncodeToString(sum[:8])
}

// LoadState reads the state file. A missing or unreadable file is an empty
// state, never an error: the hook must not fail because of its own cache.
func LoadState() State {
	st := State{Runs: map[string]Run{}}
	path, err := StatePath()
	if err != nil {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if err := json.Unmarshal(data, &st); err != nil || st.Runs == nil {
		return State{Runs: map[string]Run{}}
	}
	return st
}

// Save writes the state file, ignoring failures for the same reason.
func (s State) Save() {
	path, err := StatePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, append(data, '\n'), 0o600)
}

// Due reports whether enough time has passed to do any work at all.
func (s State) Due(key string, every time.Duration) bool {
	run, ok := s.Runs[key]
	if !ok {
		return true
	}
	return time.Since(run.LastRun) >= every
}

// Record stores the outcome of a run.
func (s State) Record(key, peek, version string) {
	s.Runs[key] = Run{LastRun: time.Now().UTC(), Peek: peek, Version: version}
	s.Save()
}
