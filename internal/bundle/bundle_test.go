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
	"strings"
	"testing"
	"time"
)

const skillMD = "---\nname: demo\ndescription: A demo skill\n---\n# Demo\n"

// entry is one raw tar entry for building adversarial archives.
type entry struct {
	name  string
	typ   byte
	body  []byte
	link  string
	mtime time.Time
	size  int64 // overrides len(body) when > 0 (used to lie in headers)
}

func tgz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, ModTime: e.mtime, Linkname: e.link, Size: int64(len(e.body))}
		if e.size > 0 {
			h.Size = e.size
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("WriteHeader %s: %v", e.name, err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("Write %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func reg(name string, body string) entry { return entry{name: name, body: []byte(body)} }

func mustInvalid(t *testing.T, label string, data []byte, lim Limits) error {
	t.Helper()
	b, err := Read(bytes.NewReader(data), lim)
	if err == nil {
		t.Fatalf("%s: expected error, got bundle with %d files", label, len(b.Files))
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("%s: error %v is not ErrInvalid", label, err)
	}
	return err
}

func TestCleanPath(t *testing.T) {
	ok := []string{"SKILL.md", "a/b", "a/b/c.txt", "./SKILL.md", ".hidden", "a/.b", "a.b/c", "..a", "a.."}
	for _, p := range ok {
		got, err := CleanPath(p)
		if err != nil {
			t.Errorf("CleanPath(%q) = error %v, want ok", p, err)
		} else if got != strings.TrimPrefix(p, "./") {
			t.Errorf("CleanPath(%q) = %q", p, got)
		}
	}
	bad := []string{"", ".", "..", "../x", "a/../b", "a/..", "/abs", "//x", "a\\b", "C:x", "C:/x", "a:b", "a//b", "a/", "a/./b", "./", "./../x", "a/b/", "x\x00y", "./."}
	for _, p := range bad {
		if got, err := CleanPath(p); err == nil {
			t.Errorf("CleanPath(%q) = %q, want error", p, got)
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanPath(%q) error %v is not ErrInvalid", p, err)
		}
	}
}

func TestReadValidBundle(t *testing.T) {
	data := tgz(t, entry{name: "scripts/", typ: tar.TypeDir}, reg("scripts/run.sh", "echo hi\n"), reg("SKILL.md", skillMD), reg("./README.md", "readme"))
	b, err := Read(bytes.NewReader(data), DefaultLimits)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var paths []string
	for _, f := range b.Files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "README.md,SKILL.md,scripts/run.sh" {
		t.Errorf("files sorted = %v", paths)
	}
	f, ok := b.File("scripts/run.sh")
	if !ok || string(f.Content) != "echo hi\n" || f.Size != 8 {
		t.Errorf("run.sh = %+v", f)
	}
	sum := sha256.Sum256([]byte("echo hi\n"))
	if f.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %s", f.SHA256)
	}
	if !strings.HasPrefix(b.Digest, "sha256:") || len(b.Digest) != 7+64 {
		t.Errorf("digest = %q", b.Digest)
	}
	if _, ok := b.File("nope"); ok {
		t.Error("File(nope) should be absent")
	}
}

func TestReadRejectsUnsafeArchives(t *testing.T) {
	md := reg("SKILL.md", skillMD)
	cases := []struct {
		label string
		data  []byte
		want  string
	}{
		{"symlink", tgz(t, md, entry{name: "evil", typ: tar.TypeSymlink, link: "/etc/passwd"}), "not a regular file"},
		{"hardlink", tgz(t, md, entry{name: "evil", typ: tar.TypeLink, link: "SKILL.md"}), "not a regular file"},
		{"char device", tgz(t, md, entry{name: "dev", typ: tar.TypeChar}), "not a regular file"},
		{"fifo", tgz(t, md, entry{name: "fifo", typ: tar.TypeFifo}), "not a regular file"},
		{"dir traversal", tgz(t, md, reg("../escape.txt", "x")), "unsafe path"},
		{"nested traversal", tgz(t, md, reg("a/../../escape.txt", "x")), "unsafe path"},
		{"absolute", tgz(t, md, reg("/etc/x", "x")), "absolute path"},
		{"backslash", tgz(t, md, reg("a\\b.txt", "x")), "backslash"},
		{"drive letter", tgz(t, md, reg("C:x.txt", "x")), "colon"},
		{"double slash", tgz(t, md, reg("a//b.txt", "x")), "unsafe path"},
		{"dir entry traversal", tgz(t, md, entry{name: "../x/", typ: tar.TypeDir}), "unsafe path"},
		{"duplicate path", tgz(t, md, reg("a.txt", "1"), reg("a.txt", "2")), "duplicate path"},
		{"duplicate via ./", tgz(t, md, reg("a.txt", "1"), reg("./a.txt", "2")), "duplicate path"},
		{"missing SKILL.md", tgz(t, reg("README.md", "x")), "SKILL.md missing"},
		{"SKILL.md not at root", tgz(t, reg("sub/SKILL.md", skillMD)), "SKILL.md missing"},
		{"empty archive", tgz(t), "SKILL.md missing"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			err := mustInvalid(t, c.label, c.data, DefaultLimits)
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestReadNotGzip(t *testing.T) {
	// A plain (uncompressed) tar and random bytes must both be rejected.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	tw.WriteHeader(&tar.Header{Name: "SKILL.md", Size: int64(len(skillMD)), Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Write([]byte(skillMD))
	tw.Close()
	for label, data := range map[string][]byte{"plain tar": raw.Bytes(), "garbage": []byte("hello world"), "empty": {}} {
		err := mustInvalid(t, label, data, DefaultLimits)
		if !strings.Contains(err.Error(), "not gzip") {
			t.Errorf("%s: %v", label, err)
		}
	}
}

func TestReadLimits(t *testing.T) {
	lim := Limits{MaxFiles: 3, MaxFileBytes: 100, MaxTotal: 260}
	md := reg("SKILL.md", skillMD) // 52 bytes

	t.Run("too many files", func(t *testing.T) {
		data := tgz(t, md, reg("a", "1"), reg("b", "2"), reg("c", "3"))
		err := mustInvalid(t, "files", data, lim)
		if !strings.Contains(err.Error(), "more than 3 files") {
			t.Error(err)
		}
	})
	t.Run("exactly max files ok", func(t *testing.T) {
		data := tgz(t, md, reg("a", "1"), reg("b", "2"))
		if _, err := Read(bytes.NewReader(data), lim); err != nil {
			t.Error(err)
		}
	})
	t.Run("per-file cap", func(t *testing.T) {
		data := tgz(t, md, reg("big", strings.Repeat("x", 101)))
		err := mustInvalid(t, "file", data, lim)
		if !strings.Contains(err.Error(), `"big" exceeds 100 bytes`) {
			t.Error(err)
		}
	})
	t.Run("per-file cap boundary ok", func(t *testing.T) {
		data := tgz(t, md, reg("big", strings.Repeat("x", 100)))
		if _, err := Read(bytes.NewReader(data), lim); err != nil {
			t.Error(err)
		}
	})
	t.Run("total cap", func(t *testing.T) {
		data := tgz(t, md, reg("a", strings.Repeat("x", 100)), reg("b", strings.Repeat("x", 100)))
		// 52 + 100 + 100 = 252 < 260 ok; add 9 more bytes to cross it.
		if _, err := Read(bytes.NewReader(data), lim); err != nil {
			t.Fatalf("under total cap: %v", err)
		}
		data = tgz(t, md, reg("a", strings.Repeat("x", 100)), reg("b", strings.Repeat("x", 100)), reg("c", strings.Repeat("x", 9)))
		err := mustInvalid(t, "total", data, Limits{MaxFiles: 10, MaxFileBytes: 100, MaxTotal: 260})
		if !strings.Contains(err.Error(), "exceeds 260 bytes uncompressed") {
			t.Error(err)
		}
	})
}

func TestReadGzipBomb(t *testing.T) {
	lim := Limits{MaxFiles: 8, MaxFileBytes: 64 << 10, MaxTotal: 128 << 10}

	t.Run("single huge zero file", func(t *testing.T) {
		zeros := make([]byte, 8<<20) // 8 MiB of zeros compresses to ~8 KiB
		data := tgz(t, reg("SKILL.md", skillMD), entry{name: "bomb", body: zeros})
		if len(data) > 64<<10 {
			t.Fatalf("test bomb did not compress: %d bytes", len(data))
		}
		mustInvalid(t, "bomb", data, lim)
	})
	t.Run("many files under per-file cap exceeding total", func(t *testing.T) {
		zeros := make([]byte, 60<<10)
		data := tgz(t, reg("SKILL.md", skillMD), entry{name: "a", body: zeros}, entry{name: "b", body: zeros}, entry{name: "c", body: zeros})
		mustInvalid(t, "many", data, lim)
	})
	t.Run("raw zero stream that is not a tar", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		io.Copy(gz, io.LimitReader(zeroReader{}, 32<<20))
		gz.Close()
		mustInvalid(t, "zeros", buf.Bytes(), lim)
	})
	t.Run("truncated archive", func(t *testing.T) {
		data := tgz(t, reg("SKILL.md", skillMD), reg("a", strings.Repeat("y", 200)))
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gr, _ := gzip.NewReader(bytes.NewReader(data))
		raw, _ := io.ReadAll(gr)
		gz.Write(raw[:len(raw)-1424]) // drop the trailer and half of the last file
		gz.Close()
		err := mustInvalid(t, "truncated", buf.Bytes(), lim)
		if !strings.Contains(err.Error(), "corrupt tar") && !strings.Contains(err.Error(), "reading") {
			t.Error(err)
		}
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestDigestStableAcrossMetadataAndOrder(t *testing.T) {
	t1 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	a := tgz(t, entry{name: "SKILL.md", body: []byte(skillMD), mtime: t1}, entry{name: "b.txt", body: []byte("b"), mtime: t1}, entry{name: "a.txt", body: []byte("a"), mtime: t1})
	b := tgz(t, entry{name: "a.txt", body: []byte("a"), mtime: t2}, entry{name: "SKILL.md", body: []byte(skillMD), mtime: t2}, entry{name: "b.txt", body: []byte("b"), mtime: t2})
	c := tgz(t, entry{name: "./a.txt", body: []byte("a"), mtime: t2}, entry{name: "SKILL.md", body: []byte(skillMD), mtime: t2}, entry{name: "b.txt", body: []byte("b"), mtime: t2})
	if bytes.Equal(a, b) {
		t.Fatal("test archives should differ in bytes")
	}
	ba, err := Read(bytes.NewReader(a), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := Read(bytes.NewReader(b), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := Read(bytes.NewReader(c), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if ba.Digest != bb.Digest || ba.Digest != bc.Digest {
		t.Errorf("digests differ: %s %s %s", ba.Digest, bb.Digest, bc.Digest)
	}
	// Content changes must change the digest; so must a rename.
	d := tgz(t, entry{name: "SKILL.md", body: []byte(skillMD)}, entry{name: "b.txt", body: []byte("B")}, entry{name: "a.txt", body: []byte("a")})
	bd, _ := Read(bytes.NewReader(d), DefaultLimits)
	if bd.Digest == ba.Digest {
		t.Error("content change did not change digest")
	}
	e := tgz(t, entry{name: "SKILL.md", body: []byte(skillMD)}, entry{name: "c.txt", body: []byte("b")}, entry{name: "a.txt", body: []byte("a")})
	be, _ := Read(bytes.NewReader(e), DefaultLimits)
	if be.Digest == ba.Digest {
		t.Error("rename did not change digest")
	}
	// ManifestDigest is order-independent and matches FromFiles.
	rev := []File{ba.Files[2], ba.Files[1], ba.Files[0]}
	if ManifestDigest(rev) != ba.Digest {
		t.Error("ManifestDigest depends on input order")
	}
	if FromFiles(rev).Digest != ba.Digest {
		t.Error("FromFiles digest differs")
	}
}

func TestManifestDigestFormat(t *testing.T) {
	f := newFile("SKILL.md", []byte(skillMD))
	h := sha256.New()
	fmt.Fprintf(h, "SKILL.md\x00%s\n", f.SHA256)
	want := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got := ManifestDigest([]File{f}); got != want {
		t.Errorf("ManifestDigest = %s, want %s", got, want)
	}
}

func TestEncodeReadRoundtrip(t *testing.T) {
	files := []File{
		{Path: "scripts/run.sh", Content: []byte("#!/bin/sh\necho hi\n")},
		{Path: "SKILL.md", Content: []byte(skillMD)},
		{Path: "bin/blob", Content: []byte{0, 1, 2, 255, 0}},
		{Path: "empty.txt", Content: []byte{}},
	}
	data, err := Encode(files)
	if err != nil {
		t.Fatal(err)
	}
	data2, _ := Encode([]File{files[1], files[3], files[0], files[2]})
	if !bytes.Equal(data, data2) {
		t.Error("Encode is not deterministic across input order")
	}
	b, err := Read(bytes.NewReader(data), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if b.Digest != FromFiles(files).Digest {
		t.Errorf("roundtrip digest %s != %s", b.Digest, FromFiles(files).Digest)
	}
	if len(b.Files) != 4 {
		t.Fatalf("got %d files", len(b.Files))
	}
	for _, in := range files {
		out, ok := b.File(in.Path)
		if !ok {
			t.Errorf("%s missing after roundtrip", in.Path)
			continue
		}
		if !bytes.Equal(out.Content, in.Content) || out.Size != int64(len(in.Content)) {
			t.Errorf("%s content differs after roundtrip", in.Path)
		}
	}
	// Re-encoding the decoded bundle reproduces identical bytes.
	data3, _ := Encode(b.Files)
	if !bytes.Equal(data, data3) {
		t.Error("Encode(Read(Encode(x))) != Encode(x)")
	}
}

func TestFromFilesDoesNotMutateInput(t *testing.T) {
	in := []File{{Path: "b", Content: []byte("b")}, {Path: "a", Content: []byte("a")}}
	FromFiles(in)
	if in[0].Path != "b" || in[0].SHA256 != "" {
		t.Error("FromFiles mutated its input")
	}
}
