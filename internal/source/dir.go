package source

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/skill"
)

// Dir serves skills from a directory tree: every folder containing a
// SKILL.md is one skill, named by its frontmatter. This is the
// no-server path — point it at a checkout of the team's skills repo.
type Dir struct {
	Root string
	// Label overrides how the source is described (git uses this).
	Label string
	// Version is recorded in the lock file for every skill, e.g. a commit.
	Version string
	// Pinned marks a source that cannot move: a git tag or commit. Digests
	// and versions recorded against it are then binding.
	Pinned bool
}

// Describe implements Source.
func (d *Dir) Describe() string {
	if d.Label != "" {
		return d.Label
	}
	return "path:" + filepath.ToSlash(d.Root)
}

// Pinnable implements Source.
func (d *Dir) Pinnable() bool { return d.Pinned }

// Fixed implements Source.
func (d *Dir) Fixed() bool { return d.Pinned }

// skillDirs finds every directory holding a SKILL.md, skipping hidden and
// vendor directories.
func (d *Dir) skillDirs() (map[string]string, error) {
	out := map[string]string{}
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
		if e.Name() != "SKILL.md" {
			return nil
		}
		dir := filepath.Dir(p)
		name := filepath.Base(dir)
		if prev, dup := out[name]; dup {
			return fmt.Errorf("two skills are both named %q: %s and %s", name, prev, dir)
		}
		out[name] = dir
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no SKILL.md found under %s", d.Root)
	}
	return out, nil
}

// List implements Source.
func (d *Dir) List(context.Context) ([]Ref, error) {
	dirs, err := d.skillDirs()
	if err != nil {
		return nil, err
	}
	refs := make([]Ref, 0, len(dirs))
	for name := range dirs {
		refs = append(refs, Ref{Name: name, Version: "latest"})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// Resolve implements Source. A namespace in the reference is accepted and
// ignored: a folder has no namespaces, so "platform/x" and "x" are the
// same skill, which keeps one manifest working against either source.
func (d *Dir) Resolve(_ context.Context, ref Ref) (Skill, error) {
	dirs, err := d.skillDirs()
	if err != nil {
		return Skill{}, err
	}
	if ref.Version != "" && ref.Version != "latest" {
		return Skill{}, fmt.Errorf("%s: a folder or repository has no per-skill versions, so %q cannot be honoured; pin the source instead (a git ref, or a gateway)", ref.Name, ref.Version)
	}
	dir, ok := dirs[ref.Name]
	if !ok {
		return Skill{}, fmt.Errorf("no skill named %q under %s", ref.Name, d.Root)
	}
	files, err := readSkillDir(dir)
	if err != nil {
		return Skill{}, fmt.Errorf("%s: %w", ref.Name, err)
	}
	b := bundle.FromFiles(files)
	md, _ := b.File("SKILL.md")
	sk, err := skill.Parse(md.Content)
	if err != nil {
		return Skill{}, fmt.Errorf("%s: %w", filepath.Join(dir, "SKILL.md"), err)
	}
	if sk.Name != ref.Name {
		return Skill{}, fmt.Errorf("%s: SKILL.md name %q does not match its folder %q", dir, sk.Name, ref.Name)
	}
	v := d.Version
	if v == "" {
		v = "-"
	}
	return Skill{Namespace: ref.Namespace, Name: sk.Name, Version: v, Digest: b.Digest, Files: b.Files}, nil
}

// readSkillDir loads a skill folder, refusing links and hidden files and
// applying the same limits as a published bundle.
func readSkillDir(dir string) ([]bundle.File, error) {
	var files []bundle.File
	lim := bundle.DefaultLimits
	var total int64
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(e.Name(), ".") {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; skills may only contain regular files", rel)
		}
		if e.IsDir() {
			return nil
		}
		if _, err := bundle.CleanPath(rel); err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Size() > lim.MaxFileBytes {
			return fmt.Errorf("%s exceeds %d bytes", rel, lim.MaxFileBytes)
		}
		total += info.Size()
		if total > lim.MaxTotal || len(files) >= lim.MaxFiles {
			return fmt.Errorf("skill exceeds %d files or %d bytes", lim.MaxFiles, lim.MaxTotal)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, bundle.File{Path: rel, Content: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.Path == "SKILL.md" {
			return files, nil
		}
	}
	return nil, fmt.Errorf("SKILL.md missing in %s", dir)
}
