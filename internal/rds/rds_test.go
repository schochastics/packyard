package rds

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"io"
	"os"
	"testing"
	"time"
)

// headerLen is "X\n" plus three int32s. The second int32 is the
// writer's R version, which differs between the machine that
// generated the fixtures and this package's constant.
const headerLen = 14

func goldenCompare(t *testing.T, fixture string, obj Object) {
	t.Helper()
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := Write(&got, obj); err != nil {
		t.Fatal(err)
	}
	g, w := got.Bytes(), want
	if !bytes.Equal(g[:6], w[:6]) || !bytes.Equal(g[10:headerLen], w[10:headerLen]) {
		t.Fatalf("header mismatch:\ngot  %x\nwant %x", g[:headerLen], w[:headerLen])
	}
	if !bytes.Equal(g[headerLen:], w[headerLen:]) {
		t.Fatalf("body mismatch (regenerate with Rscript internal/rds/testdata/gen-archive.R if the fixture is stale)\ngot:\n%s\nwant:\n%s",
			hex.Dump(g[headerLen:]), hex.Dump(w[headerLen:]))
	}
}

func TestArchiveMatchesR(t *testing.T) {
	obj := Archive([]ArchivePackage{
		{Name: "alpha", Files: []ArchiveFile{
			{Path: "alpha/alpha_1.0.0.tar.gz", Size: 100, Time: time.Unix(1700000000, 0)},
			{Path: "alpha/alpha_1.1.0.tar.gz", Size: 200, Time: time.Unix(1700086400, 500_000_000)},
		}},
		{Name: "beta", Files: []ArchiveFile{
			{Path: "beta/beta_0.1.tar.gz", Size: 50, Time: time.Unix(1600000000, 0)},
		}},
	})
	goldenCompare(t, "testdata/archive.rds", obj)
}

func TestEmptyArchiveMatchesR(t *testing.T) {
	goldenCompare(t, "testdata/archive-empty.rds", Archive(nil))
}

func TestWriteGzipRoundTrips(t *testing.T) {
	obj := Archive([]ArchivePackage{{Name: "x", Files: []ArchiveFile{{Path: "x/x_1.0.tar.gz", Size: 1}}}})
	var plain, gz bytes.Buffer
	if err := Write(&plain, obj); err != nil {
		t.Fatal(err)
	}
	if err := WriteGzip(&gz, obj); err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(&gz)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain.Bytes()) {
		t.Error("gzip payload differs from uncompressed serialization")
	}
}

func TestCharsxpEncodingFlags(t *testing.T) {
	var buf bytes.Buffer
	e := &encoder{w: bufio.NewWriter(&buf), symbols: map[string]int32{}}
	e.charsxp("abc")
	e.charsxp("é")
	_ = e.w.Flush()
	b := buf.Bytes()
	if got := hex.EncodeToString(b[:4]); got != "00040009" {
		t.Errorf("ASCII CHARSXP flags = %s, want 00040009", got)
	}
	if got := hex.EncodeToString(b[11:15]); got != "00008009" {
		t.Errorf("UTF-8 CHARSXP flags = %s, want 00008009", got)
	}
}
