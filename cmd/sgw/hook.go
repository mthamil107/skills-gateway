package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mthamil107/skills-gateway/internal/hook"
	"github.com/mthamil107/skills-gateway/internal/source"
	"github.com/mthamil107/skills-gateway/internal/syncer"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

const hookUsage = `sgw hook — keep skills current when a session starts

  sgw hook install [-agent claude] [-file sgw-sync.yaml] [-out ~] [-every 4h]
  sgw hook status
  sgw hook remove  [-agent claude]

The hook checks cheaply whether the source has moved, and does nothing
when it has not. It never blocks a session: any failure is silent.
`

func hookCmd(args []string) error {
	if len(args) == 0 {
		fmt.Print(hookUsage)
		return nil
	}
	switch args[0] {
	case "install":
		return hookInstall(args[1:])
	case "status":
		return hookStatus(args[1:])
	case "remove", "uninstall":
		return hookRemove(args[1:])
	case "help", "-h", "--help":
		fmt.Print(hookUsage)
		return nil
	}
	return fmt.Errorf("unknown hook command %q; try: install, status, remove", args[0])
}

func hookInstall(args []string) error {
	fs := flag.NewFlagSet("hook install", flag.ExitOnError)
	agentID := fs.String("agent", "claude", "agent whose session hook to write")
	file := fs.String("file", "sgw-sync.yaml", "sync manifest")
	out := fs.String("out", "~", "root to install into")
	every := fs.Duration("every", 4*time.Hour, "minimum gap between real syncs")
	matcher := fs.String("matcher", "startup|resume", "when the hook fires")
	fs.Parse(args)

	agent, err := hook.Find(*agentID)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	manifest, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	if _, err := syncer.LoadManifest(manifest); err != nil {
		return fmt.Errorf("%w\n\nthe hook runs this manifest, so it must be valid first", err)
	}
	root, err := filepath.Abs(syncer.ExpandHome(*out))
	if err != nil {
		return err
	}

	// An author's unpublished work lives in the same place a sync writes.
	// Say so now, not after the first automatic overwrite.
	if err := warnLocalOnly(manifest, root); err != nil {
		return err
	}

	cmd := hook.Command{Exe: exe, Manifest: manifest, Out: root, Every: *every}
	path, err := hook.Install(agent, cmd, *matcher)
	if err != nil {
		return err
	}
	fmt.Printf("installed the %s session hook in %s\n", agent.Name, path)
	fmt.Printf("  runs   %s\n", cmd)
	fmt.Printf("  when   %s\n", *matcher)
	fmt.Printf("  checks at most once every %s, and does nothing when the source has not moved\n", *every)
	fmt.Printf("  remove with: sgw hook remove -agent %s\n", agent.ID)
	return nil
}

// warnLocalOnly reports skill directories the source does not offer.
func warnLocalOnly(manifest, root string) error {
	m, err := syncer.LoadManifest(manifest)
	if err != nil {
		return err
	}
	ctx := context.Background()
	src, err := m.Source(ctx, filepath.Dir(manifest), gatewaySource)
	if err != nil {
		// Not reachable now is not a reason to refuse: the hook tolerates
		// that too. Say what could not be checked.
		fmt.Fprintf(os.Stderr, "warning: could not read %s to check for local-only skills: %v\n", manifest, err)
		return nil
	}
	refs, err := src.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list %s: %v\n", src.Describe(), err)
		return nil
	}
	known := map[string]bool{}
	for _, r := range refs {
		known[r.Name] = true
	}
	local := hook.LocalOnly(root, m.Formats, known)
	if len(local) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "warning: %d skill folder(s) under %s are not in %s:\n", len(local), root, src.Describe())
	for _, d := range local {
		fmt.Fprintf(os.Stderr, "  %s\n", d)
	}
	fmt.Fprintf(os.Stderr, "sync leaves them alone, and never overwrites a file you edited,\n")
	fmt.Fprintf(os.Stderr, "but publish anything you want to keep before relying on automatic updates.\n\n")
	return nil
}

func hookStatus(args []string) error {
	fs := flag.NewFlagSet("hook status", flag.ExitOnError)
	fs.Parse(args)
	state := hook.LoadState()
	for _, agent := range hook.Agents() {
		st, err := hook.Read(agent)
		if err != nil {
			fmt.Printf("%-12s %v\n", agent.ID, err)
			continue
		}
		switch {
		case st.Command == "":
			fmt.Printf("%-12s not installed  (%s)\n", agent.ID, st.Path)
		default:
			fmt.Printf("%-12s installed in %s\n", agent.ID, st.Path)
			fmt.Printf("             when %s\n", st.Matchers)
			fmt.Printf("             runs %s\n", st.Command)
		}
	}
	if len(state.Runs) == 0 {
		fmt.Println("\nno sync has run from a hook yet")
		return nil
	}
	fmt.Println("\nlast runs:")
	for key, run := range state.Runs {
		fmt.Printf("  %s  %s ago", key, time.Since(run.LastRun).Round(time.Minute))
		if run.Version != "" {
			fmt.Printf("  (%s)", run.Version)
		}
		fmt.Println()
	}
	for id, doc := range hook.Planned {
		fmt.Printf("\n%s documents hooks (%s); sgw does not write them yet\n", id, doc)
	}
	return nil
}

func hookRemove(args []string) error {
	fs := flag.NewFlagSet("hook remove", flag.ExitOnError)
	agentID := fs.String("agent", "claude", "agent whose session hook to remove")
	fs.Parse(args)
	agent, err := hook.Find(*agentID)
	if err != nil {
		return err
	}
	path, removed, err := hook.Remove(agent)
	if err != nil {
		return err
	}
	if !removed {
		fmt.Printf("no sgw hook in %s\n", path)
		return nil
	}
	fmt.Printf("removed the sgw hook from %s\n", path)
	return nil
}

// runHookSync is `sgw sync -hook`: the quiet, throttled, never-failing
// path a session hook runs.
//
// It prints nothing and exits 0 unless it actually updated something. A
// session must start normally whatever the network, the source or the
// state file is doing.
func runHookSync(ctx context.Context, m *syncer.Manifest, manifestPath, root string, every time.Duration) {
	key := hook.Key(manifestPath, root)
	state := hook.LoadState()
	if !state.Due(key, every) {
		return
	}
	src, err := m.Source(ctx, filepath.Dir(manifestPath), gatewaySource)
	if err != nil {
		return
	}
	// The cheap check: when the source has not moved, there is nothing to
	// do, and this costs one round trip at most.
	previous := state.Runs[key]
	peek := previous.Peek
	if p, ok := src.(source.Peeker); ok {
		now, err := p.Peek(ctx)
		if err != nil {
			return // unreachable: stay silent, try again next session
		}
		if now != "" && now == previous.Peek {
			state.Record(key, now, previous.Version)
			return
		}
		peek = now
	}
	rep, err := syncer.Sync(ctx, src, translate.Default(), root, m)
	if err != nil {
		// Keep the old fingerprint, so the next due session retries instead
		// of believing this failed run brought it up to date; keep the
		// throttle, so a persistently broken source costs one attempt per
		// interval and not one per session.
		state.Record(key, previous.Peek, previous.Version)
		return
	}
	state.Record(key, peek, describeRun(rep))
	if rep.Written == 0 && rep.Removed == 0 && rep.Kept == 0 {
		return
	}
	// One line, on stdout, which the agent reads as context.
	fmt.Printf("skills updated from %s: %d file(s) written", src.Describe(), rep.Written)
	if rep.Removed > 0 {
		fmt.Printf(", %d removed", rep.Removed)
	}
	if rep.Kept > 0 {
		fmt.Printf(", %d kept because you edited them", rep.Kept)
	}
	fmt.Println()
}

func describeRun(rep syncer.Report) string {
	if len(rep.Skills) == 0 {
		return ""
	}
	return fmt.Sprintf("%d skills, %s", len(rep.Skills), rep.Skills[0].Version)
}
