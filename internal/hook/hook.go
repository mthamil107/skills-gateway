// Package hook installs a session-start hook in an agent's own settings,
// so skills refresh themselves when work begins instead of going stale
// until someone remembers to sync.
//
// The rules the hook must obey are the reason this is careful work: a
// session hook runs constantly, so it must cost nothing when the source
// has not moved, it must never delay or block a session when the network
// is down, and it must never surprise the developer by touching anything
// it was not asked to.
package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Marker identifies the hook entry this tool owns. Anything else in the
// settings file is left exactly as it was found.
//
// It deliberately excludes the program name: the command carries an
// absolute path, which is "sgw.exe" on Windows and may be a renamed or
// wrapped binary anywhere. The flag pair is what makes it ours.
const Marker = "sync -hook"

// Agent is one agent whose session hook can be managed.
type Agent struct {
	ID       string // claude, cursor, ...
	Name     string // for messages
	Settings string // path to the settings file, relative to the home dir
	Docs     string
}

// Agents lists the agents supported today. Claude Code is implemented;
// the others document a hook mechanism and are listed so `hook install`
// can say so rather than pretend they do not exist.
func Agents() []Agent {
	return []Agent{
		{ID: "claude", Name: "Claude Code", Settings: filepath.Join(".claude", "settings.json"),
			Docs: "https://code.claude.com/docs/en/hooks"},
	}
}

// Planned names agents with a documented hook mechanism that this tool
// does not write yet.
var Planned = map[string]string{
	"cursor": "https://cursor.com/docs/hooks",
	"codex":  "https://developers.openai.com/codex/hooks",
	"gemini": "https://geminicli.com/docs/hooks",
}

// Find returns the agent with the given id.
func Find(id string) (Agent, error) {
	for _, a := range Agents() {
		if a.ID == id {
			return a, nil
		}
	}
	if doc, ok := Planned[id]; ok {
		return Agent{}, fmt.Errorf("%s documents hooks (%s) but sgw does not write them yet; install the hook by hand, running: %s", id, doc, Marker)
	}
	return Agent{}, fmt.Errorf("unknown agent %q; known: claude", id)
}

// SettingsPath is the absolute path to the agent's user settings file.
func (a Agent) SettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, a.Settings), nil
}

// Command is the command line the hook runs.
type Command struct {
	Exe      string        // absolute path to sgw
	Manifest string        // absolute path to the sync manifest
	Out      string        // absolute path to the install root
	Every    time.Duration // minimum gap between real syncs
}

// Args is the exec form: one argument per element, run with no shell at
// all, so a path containing spaces or backslashes passes through exactly
// as written on every platform.
func (c Command) Args() []string {
	return []string{"sync", "-hook", "-file", c.Manifest, "-out", c.Out, "-every", c.Every.String()}
}

// String renders the command for display only.
func (c Command) String() string {
	parts := []string{quote(c.Exe)}
	for _, a := range c.Args() {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " ")
}

// quote renders a path for the hook's shell. Claude Code runs shell-form
// hooks under Git Bash on Windows, where an unquoted backslash is an
// escape character, so paths always use forward slashes, which every
// shell there (Git Bash, PowerShell, cmd) and Go itself accept.
func quote(p string) string {
	p = filepath.ToSlash(p)
	if !strings.ContainsAny(p, " \t") {
		return p
	}
	return `"` + p + `"`
}

// Status describes what is installed.
type Status struct {
	Agent    Agent
	Path     string
	Exists   bool   // the settings file exists
	Command  string // the installed hook command, if any
	Matchers string
}

// Read reports the hook currently installed for the agent.
func Read(a Agent) (Status, error) {
	path, err := a.SettingsPath()
	if err != nil {
		return Status{}, err
	}
	st := Status{Agent: a, Path: path}
	doc, err := load(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Exists = true
	for _, entry := range sessionStart(doc) {
		m, _ := entry.(map[string]any)
		for _, h := range commands(m) {
			if isOurs(h) {
				st.Command = display(h)
				st.Matchers, _ = m["matcher"].(string)
			}
		}
	}
	return st, nil
}

// Install writes (or replaces) the hook entry, leaving every other part of
// the settings file untouched. It returns the path it wrote.
func Install(a Agent, c Command, matcher string) (string, error) {
	path, err := a.SettingsPath()
	if err != nil {
		return "", err
	}
	doc, err := load(path)
	if os.IsNotExist(err) {
		doc = map[string]any{}
	} else if err != nil {
		return "", err
	}
	if err := backup(path); err != nil {
		return "", err
	}
	entries := removeOurs(sessionStart(doc))
	entries = append(entries, map[string]any{
		"matcher": matcher,
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": c.Exe,
			"args":    toAny(c.Args()),
			// Seconds. The hook exits immediately when nothing changed,
			// so this only bounds a genuinely slow network.
			"timeout": 60,
		}},
	})
	setSessionStart(doc, entries)
	return path, save(path, doc)
}

// Remove takes the hook out again, leaving the file as it was found. It
// reports whether anything was removed.
func Remove(a Agent) (string, bool, error) {
	path, err := a.SettingsPath()
	if err != nil {
		return "", false, err
	}
	doc, err := load(path)
	if os.IsNotExist(err) {
		return path, false, nil
	}
	if err != nil {
		return path, false, err
	}
	before := sessionStart(doc)
	after := removeOurs(before)
	if len(after) == len(before) {
		return path, false, nil
	}
	if err := backup(path); err != nil {
		return path, false, err
	}
	setSessionStart(doc, after)
	return path, true, save(path, doc)
}

// ---------------------------------------------------------------------------
// settings.json handling
// ---------------------------------------------------------------------------

func load(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON, so it will not be edited: %w", path, err)
	}
	return doc, nil
}

// backup keeps one copy of the file as it was before the first edit, so a
// mistake is recoverable without git.
func backup(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path+".sgw-backup", data, 0o600)
}

func save(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".sgw-tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func hooksMap(doc map[string]any) map[string]any {
	h, _ := doc["hooks"].(map[string]any)
	if h == nil {
		h = map[string]any{}
		doc["hooks"] = h
	}
	return h
}

func sessionStart(doc map[string]any) []any {
	list, _ := hooksMap(doc)["SessionStart"].([]any)
	return list
}

func setSessionStart(doc map[string]any, entries []any) {
	h := hooksMap(doc)
	if len(entries) == 0 {
		delete(h, "SessionStart")
		if len(h) == 0 {
			delete(doc, "hooks")
		}
		return
	}
	h["SessionStart"] = entries
}

// removeOurs drops the commands that carry the marker, and any entry left
// with nothing else in it. An entry holding none of ours is passed through
// untouched, whatever shape it is in: it is not ours to tidy.
func removeOurs(entries []any) []any {
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok || !hasOurs(m) {
			out = append(out, e)
			continue
		}
		raw, _ := m["hooks"].([]any)
		var kept []any
		for _, h := range raw {
			if hm, ok := h.(map[string]any); ok && isOurs(hm) {
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			continue
		}
		m["hooks"] = kept
		out = append(out, m)
	}
	return out
}

func hasOurs(entry map[string]any) bool {
	for _, h := range commands(entry) {
		if isOurs(h) {
			return true
		}
	}
	return false
}

// isOurs reports whether one hook entry is one this tool wrote, in either
// the exec form (args) or the shell form an older version installed.
func isOurs(h map[string]any) bool {
	if cmd, _ := h["command"].(string); strings.Contains(cmd, Marker) {
		return true
	}
	return strings.Contains(strings.Join(stringsOf(h["args"]), " "), Marker)
}

// display renders a hook entry the way status shows it.
func display(h map[string]any) string {
	cmd, _ := h["command"].(string)
	parts := []string{quote(cmd)}
	for _, a := range stringsOf(h["args"]) {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " ")
}

func stringsOf(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func commands(entry map[string]any) []map[string]any {
	raw, _ := entry["hooks"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, h := range raw {
		if m, ok := h.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Unpublished local skills
// ---------------------------------------------------------------------------

// LocalOnly lists skill directories under root that the source does not
// offer. They are almost always an author's work in progress, and an
// automatic sync is exactly what would lose them.
func LocalOnly(root string, formats []string, known map[string]bool) []string {
	dirs := map[string]bool{}
	for _, f := range formats {
		base := formatRoot(f)
		if base == "" {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(base)))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() && !known[e.Name()] {
				dirs[base+"/"+e.Name()] = true
			}
		}
	}
	out := make([]string, 0, len(dirs))
	for d := range dirs {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// formatRoot maps a format id to the directory it installs into. Only the
// directory formats can hold an author's separate work.
func formatRoot(format string) string {
	switch format {
	case "claude":
		return ".claude/skills"
	case "codex":
		return ".agents/skills"
	case "cursor":
		return ".cursor/skills"
	case "copilot":
		return ".github/skills"
	case "gemini":
		return ".gemini/skills"
	case "kiro":
		return ".kiro/skills"
	case "windsurf":
		return ".windsurf/skills"
	}
	return ""
}

// Shell reports the shell the hook command will run under, for messages.
func Shell() string {
	if runtime.GOOS == "windows" {
		return "the default Windows shell"
	}
	return "sh"
}
