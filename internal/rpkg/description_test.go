package rpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"
)

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const sampleDescription = `Package: fsapi
Version: 1.2.0
Title: An API
Depends: R (>= 4.1.0)
Imports: fsdb (>= 2.1.16),
    fsutils,
	jsonlite
LinkingTo: cpp11
License: MIT + file LICENSE
NeedsCompilation: no
Built: R 4.4.3; ; 2026-09-01 10:00:00 UTC; unix
`

func TestReadDescription(t *testing.T) {
	data := tarball(t, map[string]string{
		"fsapi/DESCRIPTION":       sampleDescription,
		"fsapi/R/api.R":           "f <- function() 1\n",
		"fsapi/inst/DESCRIPTION":  "Package: decoy\n",
		"other/DESCRIPTION":       "Package: other\n",
		"fsapi/tests/DESCRIPTION": "Package: decoy2\n",
	})
	desc, err := ReadDescription(bytes.NewReader(data), "fsapi")
	if err != nil {
		t.Fatal(err)
	}
	if desc["Package"] != "fsapi" || desc["Version"] != "1.2.0" {
		t.Errorf("Package/Version = %q/%q", desc["Package"], desc["Version"])
	}
	if got, want := desc["Imports"], "fsdb (>= 2.1.16), fsutils, jsonlite"; got != want {
		t.Errorf("Imports = %q, want %q", got, want)
	}
	if !strings.HasPrefix(desc["Built"], "R 4.4.3;") {
		t.Errorf("Built = %q", desc["Built"])
	}
}

func TestReadDescriptionDotSlashPrefix(t *testing.T) {
	data := tarball(t, map[string]string{"./pkg/DESCRIPTION": "Package: pkg\nVersion: 1.0\n"})
	desc, err := ReadDescription(bytes.NewReader(data), "pkg")
	if err != nil {
		t.Fatal(err)
	}
	if desc["Version"] != "1.0" {
		t.Errorf("Version = %q", desc["Version"])
	}
}

func TestReadDescriptionMissing(t *testing.T) {
	data := tarball(t, map[string]string{"pkg/NAMESPACE": ""})
	if _, err := ReadDescription(bytes.NewReader(data), "pkg"); !errors.Is(err, ErrNoDescription) {
		t.Errorf("err = %v, want ErrNoDescription", err)
	}
}

func TestReadDescriptionNotGzip(t *testing.T) {
	if _, err := ReadDescription(strings.NewReader("not a tarball"), "pkg"); err == nil {
		t.Error("expected error for non-gzip input")
	}
}

func TestReadDescriptionTooLarge(t *testing.T) {
	big := "Package: pkg\nDescription: " + strings.Repeat("x", maxDescriptionBytes) + "\n"
	data := tarball(t, map[string]string{"pkg/DESCRIPTION": big})
	if _, err := ReadDescription(bytes.NewReader(data), "pkg"); err == nil {
		t.Error("expected size-limit error")
	}
}

func TestParseDCFRejectsMalformed(t *testing.T) {
	for _, s := range []string{"  leading continuation\n", "no colon here\n", "Bad Key: v\n"} {
		if _, err := ParseDCF(s); err == nil {
			t.Errorf("ParseDCF(%q) succeeded", s)
		}
	}
}

func TestSelectIndexFields(t *testing.T) {
	desc, err := ParseDCF(sampleDescription)
	if err != nil {
		t.Fatal(err)
	}
	got := SelectIndexFields(desc)
	for _, k := range []string{"Package", "Version", "Title", "Built"} {
		if _, ok := got[k]; ok {
			t.Errorf("SelectIndexFields kept %s", k)
		}
	}
	for _, k := range []string{"Depends", "Imports", "LinkingTo", "License", "NeedsCompilation"} {
		if got[k] == "" {
			t.Errorf("SelectIndexFields dropped %s", k)
		}
	}
}
