// Package bundle reads, validates and digests skill bundles.
//
// A bundle is a gzip-compressed tar archive of one skill directory, with
// SKILL.md at its root. The gateway never trusts archive metadata: it
// extracts regular files only, rejects unsafe paths, enforces size limits,
// and derives a manifest of per-file SHA-256 digests. The bundle digest is
// the SHA-256 of the canonical manifest, so it is stable no matter how the
// archive was produced (mtimes, owners, ordering).
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// Limits bounds what a bundle may contain.
type Limits struct {
	MaxFiles     int   // maximum number of regular files
	MaxFileBytes int64 // maximum size of any single file
	MaxTotal     int64 // maximum total uncompressed size
}

// DefaultLimits are conservative defaults for text-first skills.
// They match the MCP Skills Extension (SEP-2640) per-skill limits.
var DefaultLimits = Limits{MaxFiles: 512, MaxFileBytes: 5 << 20, MaxTotal: 16 << 20}

// File is one file inside a bundle.
type File struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Content []byte `json:"-"`
}

// Bundle is a validated, in-memory skill directory.
type Bundle struct {
	Files  []File // sorted by Path
	Digest string // "sha256:<hex>" of the canonical manifest
}

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid bundle")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// CleanPath validates a relative bundle path and returns it unchanged if it
// is safe. It rejects absolute paths, backslashes, drive letters, empty and
// dot segments, and anything that would escape the skill directory.
func CleanPath(p string) (string, error) {
	p = strings.TrimPrefix(p, "./")
	switch {
	case p == "":
		return "", invalid("empty path")
	case strings.HasPrefix(p, "/"):
		return "", invalid("absolute path %q", p)
	case strings.ContainsRune(p, '\\'):
		return "", invalid("backslash in path %q", p)
	case strings.ContainsRune(p, ':'):
		return "", invalid("colon in path %q", p)
	case strings.ContainsRune(p, 0):
		return "", invalid("NUL in path")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", invalid("unsafe path %q", p)
		}
	}
	if path.Clean(p) != p {
		return "", invalid("non-canonical path %q", p)
	}
	return p, nil
}

// Read decodes a tar.gz bundle from r and validates it against lim.
func Read(r io.Reader, lim Limits) (*Bundle, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, invalid("not gzip: %v", err)
	}
	defer gz.Close()
	// Bound total decompressed bytes to defeat gzip bombs. The allowance on
	// top of MaxTotal covers tar headers, including PAX long-name records.
	lr := &io.LimitedReader{R: gz, N: lim.MaxTotal + int64(lim.MaxFiles+16)*4096}
	tr := tar.NewReader(lr)

	seen := map[string]bool{}
	var files []File
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if lr.N <= 0 {
				return nil, invalid("bundle exceeds %d bytes uncompressed", lim.MaxTotal)
			}
			return nil, invalid("corrupt tar: %v", err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			// Directories are implied by file paths; validate and skip.
			if d := strings.TrimSuffix(h.Name, "/"); d != "." {
				if _, err := CleanPath(d); err != nil {
					return nil, err
				}
			}
			continue
		case tar.TypeReg:
		case tar.TypeXGlobalHeader:
			continue
		default:
			return nil, invalid("entry %q is not a regular file (links and devices are not allowed)", h.Name)
		}
		p, err := CleanPath(h.Name)
		if err != nil {
			return nil, err
		}
		if seen[p] {
			return nil, invalid("duplicate path %q", p)
		}
		seen[p] = true
		if len(files) >= lim.MaxFiles {
			return nil, invalid("more than %d files", lim.MaxFiles)
		}
		if h.Size > lim.MaxFileBytes {
			return nil, invalid("file %q exceeds %d bytes", p, lim.MaxFileBytes)
		}
		data, err := io.ReadAll(io.LimitReader(tr, lim.MaxFileBytes+1))
		if err != nil {
			return nil, invalid("reading %q: %v", p, err)
		}
		if int64(len(data)) > lim.MaxFileBytes {
			return nil, invalid("file %q exceeds %d bytes", p, lim.MaxFileBytes)
		}
		total += int64(len(data))
		if total > lim.MaxTotal {
			return nil, invalid("bundle exceeds %d bytes uncompressed", lim.MaxTotal)
		}
		files = append(files, newFile(p, data))
	}
	if !seen["SKILL.md"] {
		return nil, invalid("SKILL.md missing at bundle root")
	}
	return FromFiles(files), nil
}

func newFile(p string, data []byte) File {
	sum := sha256.Sum256(data)
	return File{Path: p, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Content: data}
}

// FromFiles builds a bundle from already-validated files, sorting them and
// computing the digest.
func FromFiles(files []File) *Bundle {
	sorted := make([]File, len(files))
	copy(sorted, files)
	for i := range sorted {
		if sorted[i].SHA256 == "" {
			sorted[i] = newFile(sorted[i].Path, sorted[i].Content)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	return &Bundle{Files: sorted, Digest: ManifestDigest(sorted)}
}

// ManifestDigest returns "sha256:<hex>" over the canonical manifest: one
// line per file, sorted by path, formatted as "<path>\x00<sha256-hex>\n".
func ManifestDigest(files []File) string {
	sorted := make([]File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		io.WriteString(h, f.Path)
		h.Write([]byte{0})
		io.WriteString(h, f.SHA256)
		h.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// File returns the named file, if present.
func (b *Bundle) File(p string) (File, bool) {
	for _, f := range b.Files {
		if f.Path == p {
			return f, true
		}
	}
	return File{}, false
}

// Write encodes files as a deterministic tar.gz (fixed mtimes and modes).
func Write(w io.Writer, files []File) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	sorted := FromFiles(files).Files
	epoch := time.Unix(0, 0).UTC()
	for _, f := range sorted {
		hdr := &tar.Header{Name: f.Path, Mode: 0o644, Size: int64(len(f.Content)), ModTime: epoch, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(f.Content); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// Encode is Write into a byte slice.
func Encode(files []File) ([]byte, error) {
	var buf bytes.Buffer
	if err := Write(&buf, files); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
