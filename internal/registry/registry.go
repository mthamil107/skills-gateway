// Package registry implements the gateway's use cases: publish, resolve,
// fetch, list, deprecate. Every call is authorized against the policy and
// every decision that matters is written to the audit log.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/mthamil107/skills-gateway/internal/auth"
	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/policy"
	"github.com/mthamil107/skills-gateway/internal/skill"
	"github.com/mthamil107/skills-gateway/internal/store"
)

var (
	// ErrForbidden is returned for denied mutations. Denied reads return
	// store.ErrNotFound so callers cannot enumerate hidden skills.
	ErrForbidden      = errors.New("forbidden")
	ErrInvalid        = errors.New("invalid request")
	ErrDigestMismatch = errors.New("digest mismatch")
)

// Service is the registry.
type Service struct {
	Store  store.Store
	Authz  policy.Authorizer
	Limits bundle.Limits
	Now    func() time.Time
}

// Meta is per-request context recorded in the audit log.
type Meta struct {
	RequestID  string
	RemoteAddr string
}

// New returns a Service with default limits.
func New(st store.Store, az policy.Authorizer) *Service {
	return &Service{Store: st, Authz: az, Limits: bundle.DefaultLimits, Now: time.Now}
}

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// ValidVersion reports whether v is a full semantic version without a
// leading "v" (e.g. 1.2.3, 1.0.0-rc.1).
func ValidVersion(v string) bool {
	return !strings.HasPrefix(v, "v") && semver.IsValid("v"+v) && semver.Canonical("v"+v) == "v"+strings.SplitN(v, "+", 2)[0]
}

func checkNames(ns, name string) error {
	if !skill.ValidName(ns) {
		return invalidf("namespace %q is invalid", ns)
	}
	if !skill.ValidName(name) {
		return invalidf("skill name %q is invalid", name)
	}
	return nil
}

func (s *Service) event(p auth.Principal, m Meta, action string, r policy.Resource, outcome, detail string) store.Event {
	return store.Event{
		Time: s.Now(), RequestID: m.RequestID, Subject: p.Subject, AgentType: p.AgentType,
		Action: action, Namespace: r.Namespace, Name: r.Name, Version: r.Version,
		Outcome: outcome, Detail: detail, RemoteAddr: m.RemoteAddr,
	}
}

func (s *Service) audit(ctx context.Context, e store.Event) {
	// Audit failures on reads are not fatal to the read; mutations write
	// their audit record inside the same transaction instead.
	_ = s.Store.AppendAudit(ctx, e)
}

// Publish validates and stores a new immutable version. expectDigest, if
// non-empty, must equal the computed bundle digest.
func (s *Service) Publish(ctx context.Context, p auth.Principal, m Meta, ns, name, version string, body io.Reader, expectDigest string) (store.Version, error) {
	if err := checkNames(ns, name); err != nil {
		return store.Version{}, err
	}
	if !ValidVersion(version) {
		return store.Version{}, invalidf("version %q is not a semantic version like 1.2.3", version)
	}
	res := policy.Resource{Namespace: ns, Name: name, Version: version}
	if d := s.Authz.Decide(ctx, p, policy.Publish, res); !d.Allow {
		s.audit(ctx, s.event(p, m, "publish", res, "denied", d.RuleID))
		return store.Version{}, ErrForbidden
	}
	b, err := bundle.Read(body, s.Limits)
	if err != nil {
		return store.Version{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if expectDigest != "" && expectDigest != b.Digest {
		return store.Version{}, fmt.Errorf("%w: client sent %s, bundle is %s", ErrDigestMismatch, expectDigest, b.Digest)
	}
	md, _ := b.File("SKILL.md")
	sk, err := skill.Parse(md.Content)
	if err != nil {
		return store.Version{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if sk.Name != name {
		return store.Version{}, invalidf("SKILL.md name %q does not match URL name %q", sk.Name, name)
	}
	v := store.Version{
		Namespace: ns, Name: name, Version: version, Status: store.Published,
		Digest: b.Digest, Description: sk.Description, License: sk.License,
		Tags: tagsOf(sk), Metadata: sk.Metadata, Publisher: p.Subject,
		PublishedAt: s.Now().UTC(), Files: b.Files,
	}
	err = s.Store.Tx(ctx, func(tx store.Store) error {
		if err := tx.PutVersion(ctx, v, b.Files); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, s.event(p, m, "publish", res, "ok", v.Digest))
	})
	if err != nil {
		return store.Version{}, err
	}
	return v, nil
}

func tagsOf(sk *skill.Skill) []string {
	var tags []string
	for _, t := range strings.Split(sk.Metadata["tags"], ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// Resolve maps "latest" to the highest published (non-deprecated) version
// the principal may fetch, preferring stable releases over pre-releases.
// Other versions pass through. It agrees with List by construction.
func (s *Service) Resolve(ctx context.Context, p auth.Principal, ns, name, version string) (string, error) {
	if version != "latest" {
		return version, nil
	}
	vs, err := s.Store.ListVersions(ctx, ns, name)
	if err != nil {
		return "", err
	}
	if best := latest(s.fetchable(ctx, p, vs)); best != nil {
		return best.Version, nil
	}
	if best := latest(vs); best != nil {
		return best.Version, errHidden // the version that was withheld, for the audit record
	}
	return "", store.ErrNotFound
}

// errHidden means published versions exist but the policy hides all of
// them. It is reported as not found, but audited as a denial.
var errHidden = fmt.Errorf("%w (hidden by policy)", store.ErrNotFound)

// fetchable keeps the versions the principal may fetch.
func (s *Service) fetchable(ctx context.Context, p auth.Principal, vs []store.Version) []store.Version {
	var out []store.Version
	for _, v := range vs {
		if s.Authz.Decide(ctx, p, policy.Fetch, policy.Resource{Namespace: v.Namespace, Name: v.Name, Version: v.Version}).Allow {
			out = append(out, v)
		}
	}
	return out
}

func latest(vs []store.Version) *store.Version {
	var best *store.Version
	for i := range vs {
		v := &vs[i]
		if v.Status != store.Published {
			continue
		}
		if best == nil || better(v.Version, best.Version) {
			best = v
		}
	}
	return best
}

func better(a, b string) bool {
	pa, pb := semver.Prerelease("v"+a) != "", semver.Prerelease("v"+b) != ""
	if pa != pb {
		return !pa
	}
	return semver.Compare("v"+a, "v"+b) > 0
}

// Get resolves and authorizes a fetch, returning the version with manifest.
// A denied fetch is reported as not found.
func (s *Service) Get(ctx context.Context, p auth.Principal, m Meta, ns, name, version, what string) (store.Version, error) {
	if err := checkNames(ns, name); err != nil {
		return store.Version{}, err
	}
	ver, err := s.Resolve(ctx, p, ns, name, version)
	if errors.Is(err, errHidden) {
		s.audit(ctx, s.event(p, m, "fetch", policy.Resource{Namespace: ns, Name: name, Version: ver}, "denied", ""))
		return store.Version{}, store.ErrNotFound
	}
	if err != nil {
		return store.Version{}, err
	}
	res := policy.Resource{Namespace: ns, Name: name, Version: ver}
	if d := s.Authz.Decide(ctx, p, policy.Fetch, res); !d.Allow {
		s.audit(ctx, s.event(p, m, "fetch", res, "denied", d.RuleID))
		return store.Version{}, store.ErrNotFound
	}
	v, err := s.Store.GetVersion(ctx, ns, name, ver)
	if err != nil {
		return v, err
	}
	if what != "" {
		s.audit(ctx, s.event(p, m, "fetch", res, "ok", what))
	}
	return v, nil
}

// Files loads the content of every file in v.
func (s *Service) Files(ctx context.Context, v store.Version) ([]bundle.File, error) {
	out := make([]bundle.File, 0, len(v.Files))
	for _, f := range v.Files {
		c, err := s.Store.FileContent(ctx, v.Namespace, v.Name, v.Version, f.Path)
		if err != nil {
			return nil, err
		}
		f.Content = c
		out = append(out, f)
	}
	return out, nil
}

// Versions lists the versions of a skill the principal may fetch.
func (s *Service) Versions(ctx context.Context, p auth.Principal, ns, name string) ([]store.Version, error) {
	if err := checkNames(ns, name); err != nil {
		return nil, err
	}
	vs, err := s.Store.ListVersions(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	var out []store.Version
	for _, v := range vs {
		if s.Authz.Decide(ctx, p, policy.Fetch, policy.Resource{Namespace: ns, Name: name, Version: v.Version}).Allow {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, store.ErrNotFound
	}
	sort.Slice(out, func(i, j int) bool { return semver.Compare("v"+out[i].Version, "v"+out[j].Version) > 0 })
	return out, nil
}

// Page is one page of List results.
type Page struct {
	Items      []store.Version `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// List returns the latest visible published version of each skill, sorted
// by namespace/name, with keyset pagination on "namespace/name".
func (s *Service) List(ctx context.Context, p auth.Principal, f store.ListFilter, limit int, cursor string) (Page, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	all, err := s.Store.ListAll(ctx, f)
	if err != nil {
		return Page{}, err
	}
	groups := map[string][]store.Version{}
	var keys []string
	for _, v := range all {
		k := v.Namespace + "/" + v.Name
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], v)
	}
	sort.Strings(keys)
	var page Page
	for _, k := range keys {
		if cursor != "" && k <= cursor {
			continue
		}
		// Only versions this principal may fetch are candidates for "latest".
		best := latest(s.fetchable(ctx, p, groups[k]))
		if best == nil {
			continue
		}
		if len(page.Items) == limit {
			page.NextCursor = page.Items[len(page.Items)-1].Namespace + "/" + page.Items[len(page.Items)-1].Name
			break
		}
		page.Items = append(page.Items, *best)
	}
	return page, nil
}

// Deprecate marks a version deprecated. It is idempotent.
func (s *Service) Deprecate(ctx context.Context, p auth.Principal, m Meta, ns, name, version string) (store.Version, error) {
	if err := checkNames(ns, name); err != nil {
		return store.Version{}, err
	}
	res := policy.Resource{Namespace: ns, Name: name, Version: version}
	if d := s.Authz.Decide(ctx, p, policy.Deprecate, res); !d.Allow {
		s.audit(ctx, s.event(p, m, "deprecate", res, "denied", d.RuleID))
		return store.Version{}, ErrForbidden
	}
	var out store.Version
	err := s.Store.Tx(ctx, func(tx store.Store) error {
		v, err := tx.GetVersion(ctx, ns, name, version)
		if err != nil {
			return err
		}
		if v.Status != store.Deprecated {
			if err := tx.SetStatus(ctx, ns, name, version, store.Deprecated); err != nil {
				return err
			}
			if err := tx.AppendAudit(ctx, s.event(p, m, "deprecate", res, "ok", "")); err != nil {
				return err
			}
			v.Status = store.Deprecated
		}
		out = v
		return nil
	})
	return out, err
}

// ExportAudit streams the audit log; it requires the admin action.
func (s *Service) ExportAudit(ctx context.Context, p auth.Principal, since time.Time, w io.Writer) error {
	if !s.Authz.Decide(ctx, p, policy.Admin, policy.Resource{Namespace: "*", Name: "*"}).Allow {
		return ErrForbidden
	}
	return s.Store.ExportAudit(ctx, since, w)
}
