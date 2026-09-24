package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mthamil107/skills-gateway/internal/bundle"
)

// GitOptions configures a Git-backed source.
type GitOptions struct {
	URL string // any URL git can clone; credentials are git's own
	Ref string // branch, tag or commit; defaults to the remote's HEAD
	Dir string // subdirectory holding the skills, relative to the repo root
	// Cache is where checkouts live. Defaults to the user's cache dir.
	Cache string
}

// Git serves skills from a Git repository. It shells out to the user's own
// git, so private repositories work with whatever credentials that git is
// already configured with, and no token is handled here.
type Git struct {
	opts GitOptions
	dir  *Dir
}

// NewGit clones or updates the repository and returns a source over it.
func NewGit(ctx context.Context, o GitOptions) (*Git, error) {
	if o.URL == "" {
		return nil, fmt.Errorf("git source: url is required")
	}
	// Nothing that git could read as an option, and no escaping the checkout.
	for name, v := range map[string]string{"url": o.URL, "ref": o.Ref, "dir": o.Dir} {
		if strings.HasPrefix(v, "-") {
			return nil, fmt.Errorf("git source: %s must not start with %q", name, "-")
		}
	}
	if o.Dir != "" {
		if _, err := bundle.CleanPath(strings.TrimSuffix(filepath.ToSlash(o.Dir), "/")); err != nil {
			return nil, fmt.Errorf("git source: dir %q must be a relative path inside the repository", o.Dir)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git source: git is not installed or not on PATH")
	}
	cache := o.Cache
	if cache == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		cache = filepath.Join(base, "sgw", "git")
	}
	sum := sha256.Sum256([]byte(o.URL))
	work := filepath.Join(cache, hex.EncodeToString(sum[:8]))

	if _, err := os.Stat(filepath.Join(work, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(work), 0o755); err != nil {
			return nil, err
		}
		// Files must reach the project byte for byte, whatever the user's
		// line-ending settings are, or the same tag would hash differently
		// on Windows and Linux.
		if out, err := run(ctx, "", "git", "clone", "--quiet", "--config", "core.autocrlf=false", "--", o.URL, work); err != nil {
			return nil, fmt.Errorf("git clone %s: %w: %s", redact(o.URL), err, out)
		}
	}
	// --force lets a moved tag update in the cache, so sync compares it
	// against the lock and reports it clearly, instead of git failing here
	// with "would clobber existing tag".
	if out, err := run(ctx, work, "git", "fetch", "--quiet", "--tags", "--force", "origin"); err != nil {
		return nil, fmt.Errorf("git fetch: %w: %s", err, out)
	}
	ref := o.Ref
	if ref == "" {
		head, err := run(ctx, work, "git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			ref = "origin/HEAD"
		} else {
			ref = strings.TrimSpace(head)
		}
	} else if isBranch(ctx, work, ref) {
		// A branch is read through its remote-tracking ref, which the
		// fetch above just updated; the local branch the clone created
		// would stay at the first commit forever.
		ref = "origin/" + ref
	}
	// The cache is ours: --force discards anything that drifted in it,
	// including files an older checkout converted to CRLF.
	if out, err := run(ctx, work, "git", "-c", "core.autocrlf=false", "checkout", "--quiet", "--force", "--detach", ref); err != nil {
		return nil, fmt.Errorf("git checkout %s: %w: %s", ref, err, out)
	}
	commit, err := run(ctx, work, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("git rev-parse: %w", err)
	}
	commit = strings.TrimSpace(commit)

	root := work
	if o.Dir != "" {
		root = filepath.Join(work, filepath.FromSlash(o.Dir))
		if _, err := os.Stat(root); err != nil {
			return nil, fmt.Errorf("git source: %s does not exist in %s at %s", o.Dir, redact(o.URL), ref)
		}
	}
	// A commit or tag is a fixed point, so neither its content nor the
	// commit it resolves to may change; a branch legitimately moves.
	pinned := o.Ref != "" && !isBranch(ctx, work, o.Ref)
	// The label goes into the committed lock file, so it carries the ref as
	// written and never the credentials or the resolved commit.
	label := "git:" + redact(o.URL)
	if o.Ref != "" {
		label += "@" + o.Ref
	}
	return &Git{opts: o, dir: &Dir{Root: root, Label: label, Version: commit[:min(12, len(commit))], Pinned: pinned}}, nil
}

// redact removes any credentials embedded in a clone URL, so they cannot
// reach the lock file or an error message.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func isBranch(ctx context.Context, work, ref string) bool {
	_, err := run(ctx, work, "git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+ref)
	return err == nil
}

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Describe implements Source.
func (g *Git) Describe() string { return g.dir.Describe() }

// Pinnable implements Source.
func (g *Git) Pinnable() bool { return g.dir.Pinnable() }

// Fixed implements Source.
func (g *Git) Fixed() bool { return g.dir.Fixed() }

// List implements Source.
func (g *Git) List(ctx context.Context) ([]Ref, error) { return g.dir.List(ctx) }

// Resolve implements Source.
func (g *Git) Resolve(ctx context.Context, ref Ref) (Skill, error) { return g.dir.Resolve(ctx, ref) }
