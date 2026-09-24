package importers

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTarGz(t *testing.T, entries []tar.Header) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bundle.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for _, h := range entries {
		h := h
		body := "x"
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []interface{ Close() error }{tw, zw, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// Hostile archives must fail without writing outside the target dir.
func TestExtractTarGzRejectsEscapes(t *testing.T) {
	t.Parallel()
	cases := map[string]tar.Header{
		"dot-dot":  {Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		"nested":   {Name: "a/../../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		"absolute": {Name: "/tmp/escape.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		"symlink":  {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"hardlink": {Name: "hard", Typeflag: tar.TypeLink, Linkname: "../x"},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			dst := filepath.Join(parent, "out")
			if err := os.Mkdir(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			err := extractTarGz(writeTarGz(t, []tar.Header{h}), dst)
			if err == nil {
				t.Fatal("hostile entry extracted without error")
			}
			if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
				t.Fatal("entry written outside the target dir")
			}
		})
	}
}

func TestExtractTarGzExtractsPlainEntries(t *testing.T) {
	t.Parallel()
	dst := t.TempDir()
	err := extractTarGz(writeTarGz(t, []tar.Header{
		{Name: "bundle/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "bundle/manifest.json", Typeflag: tar.TypeReg, Mode: 0o644},
	}), dst)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "bundle", "manifest.json"))
	if err != nil || !strings.EqualFold(string(b), "x") {
		t.Fatalf("extracted content = %q, %v", b, err)
	}
}
