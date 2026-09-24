// Package syncer installs skills into a project, or into a developer's
// home directory, in each agent's native layout.
//
// The skills come from a source.Source: a governed gateway, a directory on
// disk, or a Git repository. The rest of the work is identical whichever
// it is, so a team can start with a repo and move to a gateway later
// without changing formats, output paths or the lock file.
//
// Integrity is end to end: files are verified against the digest the
// source advertises and against the lock file's pin when the source
// promises immutability, and translation happens locally from those bytes.
package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/skill"
	"github.com/mthamil107/skills-gateway/internal/source"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

// LockFile records what sync wrote, so later syncs can pin digests and
// remove files that are no longer wanted. Commit it.
const LockFile = "sgw-lock.json"

// Manifest is the sgw-sync.yaml file. Exactly one source may be named;
// with none, the gateway in $SGW_URL is used.
type Manifest struct {
	// Gateway is a Skills Gateway base URL. Empty means $SGW_URL.
	Gateway string `yaml:"gateway"`
	// Path is a directory of skill folders - no server involved.
	Path string `yaml:"path"`
	// Git is a repository of skill folders, with an optional ref and
	// subdirectory.
	Git    string `yaml:"git"`
	Ref    string `yaml:"ref"`
	GitDir string `yaml:"dir"`

	Formats []string `yaml:"formats"` // e.g. [claude, cursor]
	// Skills lists references, or a single "*" for everything the source
	// offers. A gateway reference is "ns/name[@version]"; a path or git
	// reference is just "name".
	Skills []string `yaml:"skills"`
}

// Source builds the source this manifest names. base is the directory the
// manifest was read from, so a relative path resolves against it.
func (m *Manifest) Source(ctx context.Context, base string, newGateway func(url string) (source.Source, error)) (source.Source, error) {
	named := 0
	for _, v := range []string{m.Gateway, m.Path, m.Git} {
		if v != "" {
			named++
		}
	}
	if named > 1 {
		return nil, fmt.Errorf("name only one of gateway, path or git")
	}
	switch {
	case m.Path != "":
		dir := expandHome(m.Path)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(base, filepath.FromSlash(dir))
		}
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("path source: %w", err)
		}
		return &source.Dir{Root: dir}, nil
	case m.Git != "":
		return source.NewGit(ctx, source.GitOptions{URL: m.Git, Ref: m.Ref, Dir: m.GitDir})
	default:
		return newGateway(m.Gateway)
	}
}

// LoadManifest reads and validates a sync manifest.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(m.Formats) == 0 || len(m.Skills) == 0 {
		return nil, fmt.Errorf("%s: formats and skills are both required", path)
	}
	if (m.Ref != "" || m.GitDir != "") && m.Git == "" {
		return nil, fmt.Errorf("%s: ref and dir apply to a git source", path)
	}
	return &m, nil
}

// expandHome turns a leading "~" into the user's home directory.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// Lock is the on-disk lock file.
type Lock struct {
	Skills []LockedSkill `json:"skills"`
}

// LockedSkill is one synced skill.
type LockedSkill struct {
	Ref     string       `json:"ref"` // as written in the manifest
	Source  string       `json:"source,omitempty"`
	Version string       `json:"version"`
	Digest  string       `json:"digest"`
	Files   []LockedFile `json:"files"`
}

// LockedFile is one file written into the project.
type LockedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Installed describes one installed skill.
type Installed struct {
	Version  string
	Digest   string
	Written  []LockedFile
	Warnings []string
}

type prepared struct {
	Installed
	files []translate.OutFile
}

// Install resolves, verifies, translates and writes one skill.
func Install(ctx context.Context, src source.Source, reg translate.Registry, root string, ref source.Ref, formats []string) (Installed, error) {
	p, err := prepare(ctx, src, reg, ref, nil, formats)
	if err != nil {
		return p.Installed, err
	}
	return p.Installed, writeAll(root, p.files)
}

// prepare resolves, verifies and translates one skill without writing.
func prepare(ctx context.Context, src source.Source, reg translate.Registry, ref source.Ref, pin *LockedSkill, formats []string) (prepared, error) {
	var res prepared
	sk, err := src.Resolve(ctx, ref)
	if err != nil {
		return res, err
	}
	b := bundle.FromFiles(sk.Files)
	if b.Digest != sk.Digest {
		return res, fmt.Errorf("%s: digest %s does not match the received files (%s)", ref, sk.Digest, b.Digest)
	}
	// A fixed source (a git tag or commit) must keep resolving to the same
	// version. If it does not, the tag was moved under us.
	if pin != nil && src.Fixed() && pin.Source == src.Describe() && pin.Version != sk.Version {
		return res, fmt.Errorf("%s: %s is pinned, but it now resolves to %s while %s records %s; the tag or ref was rewritten",
			ref, src.Describe(), sk.Version, LockFile, pin.Version)
	}
	// A source that promises immutability must never change a recorded
	// version's content. A folder or a moving branch legitimately does.
	if pin != nil && src.Pinnable() && pin.Version == sk.Version && pin.Digest != b.Digest {
		return res, fmt.Errorf("%s@%s: digest %s does not match the pinned %s in %s; this version was published as immutable, so this indicates tampering or a changed source",
			ref, sk.Version, b.Digest, pin.Digest, LockFile)
	}
	md, _ := b.File("SKILL.md")
	parsed, err := skill.Parse(md.Content)
	if err != nil {
		return res, fmt.Errorf("%s: %w", ref, err)
	}
	res.Version, res.Digest = sk.Version, b.Digest
	for _, f := range formats {
		tr, ok := reg[f]
		if !ok {
			return res, fmt.Errorf("unknown format %q", f)
		}
		out, err := tr.Translate(parsed, b.Files)
		if err != nil {
			return res, err
		}
		res.Warnings = append(res.Warnings, out.Warnings...)
		for _, of := range out.Files {
			sum := sha256.Sum256(of.Content)
			res.files = append(res.files, of)
			res.Written = append(res.Written, LockedFile{Path: of.Path, SHA256: hex.EncodeToString(sum[:])})
		}
	}
	return res, nil
}

func writeAll(root string, files []translate.OutFile) error {
	// Validate every path before touching the disk.
	for _, f := range files {
		if _, err := safeJoin(root, f.Path); err != nil {
			return err
		}
	}
	for _, f := range files {
		if err := writeFile(root, f.Path, f.Content); err != nil {
			return err
		}
	}
	return nil
}

// safeJoin resolves a translator output path under root, refusing anything
// that would land outside it.
func safeJoin(root, rel string) (string, error) {
	if _, err := bundle.CleanPath(rel); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	base, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if r, err := filepath.Rel(base, abs); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to write outside %s: %s", root, rel)
	}
	return abs, nil
}

// writeFile writes through os.Root, which refuses to follow a symbolic
// link, in the file or any parent directory, that points outside root.
func writeFile(root, rel string, data []byte) error {
	if _, err := safeJoin(root, rel); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	name := filepath.FromSlash(rel)
	if fi, err := r.Lstat(name); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to overwrite symlink %s", rel)
	}
	if dir := filepath.Dir(name); dir != "." {
		if err := r.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	tmp := name + ".sgw-tmp"
	if err := r.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	return r.Rename(tmp, name)
}

// removeIfUnchanged deletes a file sync wrote earlier, through os.Root,
// unless it was modified since.
func removeIfUnchanged(root string, f LockedFile) (removed, modified bool) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return false, false
	}
	defer r.Close()
	name := filepath.FromSlash(f.Path)
	data, err := r.ReadFile(name)
	if err != nil {
		return false, false
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != f.SHA256 {
		return false, true
	}
	return r.Remove(name) == nil, false
}

// Report summarises a sync run.
type Report struct {
	Skills   []LockedSkill
	Written  int
	Removed  int
	Warnings []string
}

// Sync installs every skill in the manifest, pins digests in the lock file
// and removes files a previous sync wrote that are no longer produced.
func Sync(ctx context.Context, src source.Source, reg translate.Registry, root string, m *Manifest) (Report, error) {
	var rep Report
	old, err := readLock(root)
	if err != nil {
		return rep, err
	}
	wanted, err := refs(ctx, src, m.Skills)
	if err != nil {
		return rep, err
	}
	pins := map[string]LockedSkill{}
	for _, s := range old.Skills {
		pins[s.Ref] = s
	}
	owner := map[string]string{} // output path -> skill ref, to catch collisions
	var lock Lock
	var all []translate.OutFile
	for _, r := range wanted {
		ref := r.String()
		var pin *LockedSkill
		if p, ok := pins[ref]; ok {
			pin = &p
		}
		res, err := prepare(ctx, src, reg, r, pin, m.Formats)
		if err != nil {
			return rep, err
		}
		for _, f := range res.Written {
			if prev, ok := owner[f.Path]; ok {
				return rep, fmt.Errorf("%s and %s both write %s; skill names must be unique across namespaces within one project", prev, ref, f.Path)
			}
			owner[f.Path] = ref
		}
		rep.Warnings = append(rep.Warnings, res.Warnings...)
		rep.Written += len(res.Written)
		all = append(all, res.files...)
		lock.Skills = append(lock.Skills, LockedSkill{
			Ref: ref, Source: src.Describe(), Version: res.Version,
			Digest: res.Digest, Files: res.Written,
		})
	}
	// Nothing is written until every skill has been fetched, verified and
	// checked for collisions.
	if err := writeAll(root, all); err != nil {
		return rep, err
	}
	// Remove files we wrote last time that this sync no longer produces,
	// but only if they are unchanged since we wrote them.
	for _, s := range old.Skills {
		for _, f := range s.Files {
			if _, still := owner[f.Path]; still {
				continue
			}
			p, err := safeJoin(root, f.Path)
			if err != nil {
				continue
			}
			removed, modified := removeIfUnchanged(root, f)
			if modified {
				rep.Warnings = append(rep.Warnings, "kept locally modified stale file "+f.Path)
			}
			if removed {
				rep.Removed++
				pruneEmpty(root, filepath.Dir(p))
			}
		}
	}
	sort.Slice(lock.Skills, func(i, j int) bool { return lock.Skills[i].Ref < lock.Skills[j].Ref })
	rep.Skills = lock.Skills
	return rep, writeLock(root, lock)
}

func pruneEmpty(root, dir string) {
	base, _ := filepath.Abs(root)
	for {
		d, _ := filepath.Abs(dir)
		if d == base || !strings.HasPrefix(d, base) {
			return
		}
		if os.Remove(d) != nil { // fails when not empty
			return
		}
		dir = filepath.Dir(d)
	}
}

// refs turns the manifest's skill list into concrete references. A single
// "*" means everything the source offers.
func refs(ctx context.Context, src source.Source, list []string) ([]source.Ref, error) {
	if len(list) == 1 && strings.TrimSpace(list[0]) == "*" {
		all, err := src.List(ctx)
		if err != nil {
			return nil, err
		}
		if len(all) == 0 {
			return nil, fmt.Errorf("%s offers no skills", src.Describe())
		}
		return all, nil
	}
	out := make([]source.Ref, 0, len(list))
	for _, item := range list {
		if strings.TrimSpace(item) == "*" {
			return nil, fmt.Errorf(`"*" must be the only entry in skills`)
		}
		r, err := source.ParseRef(item)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func readLock(root string) (Lock, error) {
	var l Lock
	data, err := os.ReadFile(filepath.Join(root, LockFile))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(data, &l); err != nil {
		return l, fmt.Errorf("%s: %w", LockFile, err)
	}
	return l, nil
}

func writeLock(root string, l Lock) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(root, LockFile, append(data, '\n'))
}
