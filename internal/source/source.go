// Package source supplies skills to the syncer.
//
// A gateway is one source, not the only one. A team can start with skills
// in a folder or a Git repository and move to a governed gateway later
// without changing the manifest's formats, the files that land in a
// project, or the lock file format.
//
//	path    — a directory of skill folders on disk
//	git     — a Git repository, cloned into a local cache
//	gateway — a Skills Gateway server (identity, policy, audit)
package source

import (
	"context"
	"fmt"

	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/skill"
)

// Skill is one resolved skill, ready to translate.
type Skill struct {
	Namespace string
	Name      string
	Version   string // semver from a gateway, commit or "-" from a repo
	Digest    string // "sha256:..." over the manifest of files
	Files     []bundle.File
}

// Ref is "<name>" or "<namespace>/<name>", optionally "@<version>".
type Ref struct {
	Namespace string
	Name      string
	Version   string // "latest" when unset
}

// String renders the reference the way a manifest writes it.
func (r Ref) String() string {
	s := r.Name
	if r.Namespace != "" {
		s = r.Namespace + "/" + r.Name
	}
	if r.Version != "" && r.Version != "latest" {
		s += "@" + r.Version
	}
	return s
}

// Source resolves references to skills.
type Source interface {
	// Describe names the source for messages and the lock file.
	Describe() string
	// List returns every skill the source offers, for a "*" manifest entry.
	List(ctx context.Context) ([]Ref, error)
	// Resolve returns one skill's files, verified as far as the source can.
	Resolve(ctx context.Context, ref Ref) (Skill, error)
	// Pinnable reports whether a recorded (version, digest) pair must stay
	// identical on a later sync. True for immutable gateway versions; false
	// for a folder or a moving branch, whose content legitimately changes.
	Pinnable() bool
	// Fixed reports whether the source itself is pinned to one point, such
	// as a git tag or commit. The resolved version must then not move
	// either: if it does, the tag was rewritten.
	Fixed() bool
}

// ParseRef parses "[namespace/]name[@version]".
func ParseRef(s string) (Ref, error) {
	r := Ref{Version: "latest"}
	if i := lastIndexByte(s, '@'); i > 0 {
		r.Version = s[i+1:]
		s = s[:i]
		if r.Version == "" {
			return Ref{}, fmt.Errorf("skill reference %q has an empty version", s)
		}
	}
	switch parts := splitSlash(s); len(parts) {
	case 1:
		r.Name = parts[0]
	case 2:
		if parts[0] == "" {
			return Ref{}, fmt.Errorf("skill reference has an empty namespace: %q", s)
		}
		r.Namespace, r.Name = parts[0], parts[1]
	default:
		return Ref{}, fmt.Errorf("skill reference must be <name> or <namespace>/<name>, got %q", s)
	}
	if !skill.ValidName(r.Name) || (r.Namespace != "" && !skill.ValidName(r.Namespace)) {
		return Ref{}, fmt.Errorf("skill reference must be <name> or <namespace>/<name> in lowercase, got %q", s)
	}
	return r, nil
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func splitSlash(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
