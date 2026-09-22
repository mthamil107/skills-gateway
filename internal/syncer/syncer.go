// Package syncer installs skills from a gateway into a project directory
// in each agent's native layout ("push/sync" delivery).
//
// Integrity is end to end: the bundle is downloaded, its digest verified
// against the server's advertised digest (and the lock file's pin, when
// present), and translation happens locally from the verified bytes.
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
	"github.com/mthamil107/skills-gateway/internal/client"
	"github.com/mthamil107/skills-gateway/internal/skill"
	"github.com/mthamil107/skills-gateway/internal/translate"
)

// LockFile records what sync wrote, so later syncs can pin digests and
// remove files that are no longer wanted. Commit it.
const LockFile = "sgw-lock.json"

// Manifest is the sgw-sync.yaml file.
type Manifest struct {
	Formats []string `yaml:"formats"` // e.g. [claude, cursor]
	Skills  []string `yaml:"skills"`  // "ns/name" or "ns/name@1.2.3"
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
	return &m, nil
}

// Lock is the on-disk lock file.
type Lock struct {
	Skills []LockedSkill `json:"skills"`
}

// LockedSkill is one synced skill.
type LockedSkill struct {
	Ref     string       `json:"ref"` // as written in the manifest
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

// Install downloads, verifies, translates and writes one skill.
func Install(ctx context.Context, c *client.Client, reg translate.Registry, root, ns, name, version string, formats []string) (Installed, error) {
	p, err := prepare(ctx, c, reg, ns, name, version, nil, formats)
	if err != nil {
		return p.Installed, err
	}
	return p.Installed, writeAll(root, p.files)
}

// prepare downloads, verifies and translates one skill without writing.
func prepare(ctx context.Context, c *client.Client, reg translate.Registry, ns, name, version string, pin *LockedSkill, formats []string) (prepared, error) {
	var res prepared
	// Resolve "latest" once, then fetch that exact version, so a publish
	// between the two calls cannot mix versions.
	meta, err := c.Get(ctx, ns, name, version)
	if err != nil {
		return res, fmt.Errorf("%s/%s@%s: %w", ns, name, version, err)
	}
	b, err := c.Bundle(ctx, ns, name, meta.Version)
	if err != nil {
		return res, fmt.Errorf("%s/%s@%s: %w", ns, name, meta.Version, err)
	}
	if meta.Digest != b.Digest {
		return res, fmt.Errorf("%s/%s: metadata digest %s differs from bundle digest %s", ns, name, meta.Digest, b.Digest)
	}
	// Versions are immutable: if the lock recorded this exact version, its
	// digest must not have changed. A "latest" ref may move to a new
	// version, which is then pinned afresh.
	if pin != nil && pin.Version == meta.Version && pin.Digest != b.Digest {
		return res, fmt.Errorf("%s/%s@%s: digest %s does not match the pinned %s in %s; published versions are immutable, so this indicates tampering or a changed server",
			ns, name, meta.Version, b.Digest, pin.Digest, LockFile)
	}
	md, _ := b.File("SKILL.md")
	sk, err := skill.Parse(md.Content)
	if err != nil {
		return res, err
	}
	res.Version, res.Digest = meta.Version, b.Digest
	for _, f := range formats {
		tr, ok := reg[f]
		if !ok {
			return res, fmt.Errorf("unknown format %q", f)
		}
		out, err := tr.Translate(sk, b.Files)
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
func Sync(ctx context.Context, c *client.Client, reg translate.Registry, root string, m *Manifest) (Report, error) {
	var rep Report
	old, err := readLock(root)
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
	for _, ref := range m.Skills {
		ns, name, ver, err := parseRef(ref)
		if err != nil {
			return rep, err
		}
		var pin *LockedSkill
		if p, ok := pins[ref]; ok {
			pin = &p
		}
		res, err := prepare(ctx, c, reg, ns, name, ver, pin, m.Formats)
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
		lock.Skills = append(lock.Skills, LockedSkill{Ref: ref, Version: res.Version, Digest: res.Digest, Files: res.Written})
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

func parseRef(ref string) (ns, name, ver string, err error) {
	ver = "latest"
	orig := ref
	if i := strings.LastIndexByte(ref, '@'); i > 0 {
		ref, ver = ref[:i], ref[i+1:]
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || ver == "" {
		return "", "", "", fmt.Errorf("skill reference must be <namespace>/<name>[@version], got %q", orig)
	}
	return parts[0], parts[1], ver, nil
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
