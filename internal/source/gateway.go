package source

import (
	"context"
	"fmt"

	"github.com/mthamil107/skills-gateway/internal/client"
)

// Gateway serves skills from a Skills Gateway server: identity on every
// request, policy per fetch, immutable versions and an audit trail.
type Gateway struct {
	Client *client.Client
}

// Describe implements Source.
func (g *Gateway) Describe() string { return "gateway:" + g.Client.BaseURL }

// Pinnable implements Source: published versions never change.
func (g *Gateway) Pinnable() bool { return true }

// Fixed implements Source: a reference to "latest" may legitimately move
// to a newer published version.
func (g *Gateway) Fixed() bool { return false }

// List implements Source, returning every skill this caller may fetch.
func (g *Gateway) List(ctx context.Context) ([]Ref, error) {
	var refs []Ref
	cursor := ""
	for {
		page, err := g.Client.List(ctx, "", "", "", cursor, 200)
		if err != nil {
			return nil, err
		}
		for _, v := range page.Items {
			refs = append(refs, Ref{Namespace: v.Namespace, Name: v.Name, Version: "latest"})
		}
		if page.NextCursor == "" {
			return refs, nil
		}
		cursor = page.NextCursor
	}
}

// Resolve implements Source. It resolves the version first, then downloads
// that exact version, so a publish between the two calls cannot mix
// versions, and it checks the bundle against the advertised digest.
func (g *Gateway) Resolve(ctx context.Context, ref Ref) (Skill, error) {
	if ref.Namespace == "" {
		return Skill{}, fmt.Errorf("a gateway reference needs a namespace: <namespace>/%s", ref.Name)
	}
	version := ref.Version
	if version == "" {
		version = "latest"
	}
	meta, err := g.Client.Get(ctx, ref.Namespace, ref.Name, version)
	if err != nil {
		return Skill{}, fmt.Errorf("%s/%s@%s: %w", ref.Namespace, ref.Name, version, err)
	}
	b, err := g.Client.Bundle(ctx, ref.Namespace, ref.Name, meta.Version)
	if err != nil {
		return Skill{}, fmt.Errorf("%s/%s@%s: %w", ref.Namespace, ref.Name, meta.Version, err)
	}
	if meta.Digest != b.Digest {
		return Skill{}, fmt.Errorf("%s/%s: metadata digest %s differs from bundle digest %s",
			ref.Namespace, ref.Name, meta.Digest, b.Digest)
	}
	return Skill{
		Namespace: ref.Namespace, Name: ref.Name, Version: meta.Version,
		Digest: b.Digest, Files: b.Files,
	}, nil
}
