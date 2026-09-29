package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// Peeker reports a cheap fingerprint of what the source is currently
// offering, without downloading anything. A caller that holds the same
// fingerprint from last time knows there is no work to do.
//
// The fingerprint is opaque: compare it, do not parse it.
type Peeker interface {
	Peek(ctx context.Context) (string, error)
}

// Peek implements Peeker for a directory by hashing each skill's path,
// size and modification time. It reads no file contents.
func (d *Dir) Peek(context.Context) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(d.Root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			n := e.Name()
			if p != d.Root && (strings.HasPrefix(n, ".") || n == "node_modules" || n == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(d.Root, p)
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return "", err
	}
	return "dir:" + hex.EncodeToString(h.Sum(nil))[:32], nil
}

// Peek implements Peeker for a git source: one `git ls-remote` against the
// ref, which is a single round trip and no clone.
func (g *Git) Peek(ctx context.Context) (string, error) {
	return PeekGit(ctx, g.opts)
}

// PeekGit resolves the commit a ref currently points at, without cloning.
// It is the cheap check a session hook runs before doing any real work.
func PeekGit(ctx context.Context, o GitOptions) (string, error) {
	ref := o.Ref
	if ref == "" {
		ref = "HEAD"
	}
	// A commit cannot move, and ls-remote lists refs, not objects.
	if isObjectName(ref) {
		return "git:" + ref, nil
	}
	// ls-remote only prints a tag's peeled "^{}" line when a pattern asks
	// for it, so ask for both; the peeled line is the commit an annotated
	// tag actually points at.
	out, err := run(ctx, "", "git", "ls-remote", "--", o.URL, ref, ref+"^{}")
	if err != nil {
		return "", fmt.Errorf("git ls-remote %s: %w", redact(o.URL), err)
	}
	// Prefer the peeled line over the exact ref match.
	var first, peeled string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if strings.HasSuffix(f[1], "^{}") {
			peeled = f[0]
			continue
		}
		if first == "" {
			first = f[0]
		}
	}
	if peeled != "" {
		return "git:" + peeled, nil
	}
	if first == "" {
		return "", fmt.Errorf("git ls-remote %s: ref %q not found", redact(o.URL), ref)
	}
	return "git:" + first, nil
}

// isObjectName reports whether ref is a full SHA-1 or SHA-256 object name.
func isObjectName(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for _, c := range ref {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Peek implements Peeker for a gateway: one list request, whose digests
// change whenever any visible skill changes.
func (g *Gateway) Peek(ctx context.Context) (string, error) {
	h := sha256.New()
	cursor := ""
	for {
		page, err := g.Client.List(ctx, "", "", "", cursor, 200)
		if err != nil {
			return "", err
		}
		for _, v := range page.Items {
			fmt.Fprintf(h, "%s/%s\x00%s\x00%s\n", v.Namespace, v.Name, v.Version, v.Digest)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return "gw:" + hex.EncodeToString(h.Sum(nil))[:32], nil
}
