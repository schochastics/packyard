package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/schochastics/packyard/internal/rds"
)

// rTarball builds a gzipped R package tarball whose DESCRIPTION holds
// the given extra fields (e.g. "Imports: fsdb").
func rTarball(t *testing.T, name, version, extra string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	desc := "Package: " + name + "\nVersion: " + version + "\nTitle: T\n" + extra
	if err := tw.WriteHeader(&tar.Header{Name: name + "/DESCRIPTION", Mode: 0o644, Size: int64(len(desc))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(desc)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSourcePACKAGESLatestOnlyOverHTTP(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.9.0", []byte("a 1.9"))
	publishSource(t, fx, "dev", "alpha", "1.10.0", []byte("a 1.10"))
	publishSource(t, fx, "dev", "alpha", "2.0.0", []byte("a 2.0"))
	if rec := doYank(t, fx, "dev", "alpha", "2.0.0", fx.token, `{"reason":"broken"}`); rec.Code != http.StatusOK {
		t.Fatalf("yank: %d %s", rec.Code, rec.Body.String())
	}

	rec := getURL(t, fx, "/dev/src/contrib/PACKAGES", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if want := "Package: alpha\nVersion: 1.10.0\n"; rec.Body.String() != want {
		t.Errorf("PACKAGES = %q, want %q", rec.Body.String(), want)
	}
}

func TestSourceArchiveServesEveryVersion(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a 1.0"))
	publishSource(t, fx, "dev", "alpha", "1.1.0", []byte("a 1.1"))
	if rec := doYank(t, fx, "dev", "alpha", "1.1.0", fx.token, `{"reason":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("yank: %d", rec.Code)
	}

	// Current, archived and yanked versions are all downloadable from
	// both src/contrib/ and src/contrib/Archive/<pkg>/.
	for path, want := range map[string]string{
		"/dev/src/contrib/alpha_1.0.0.tar.gz":               "a 1.0",
		"/dev/src/contrib/Archive/alpha/alpha_1.0.0.tar.gz": "a 1.0",
		"/dev/src/contrib/alpha_1.1.0.tar.gz":               "a 1.1",
		"/dev/src/contrib/Archive/alpha/alpha_1.1.0.tar.gz": "a 1.1",
	} {
		rec := getURL(t, fx, path, fx.token)
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s: status = %d body %q, want %q", path, rec.Code, rec.Body.String(), want)
		}
	}

	for _, path := range []string{
		"/dev/src/contrib/Archive/beta/alpha_1.0.0.tar.gz",  // pkg mismatch
		"/dev/src/contrib/Archive/alpha/alpha_9.9.9.tar.gz", // unknown version
		"/dev/src/contrib/Archive/alpha/not-a-tarball",
	} {
		if rec := getURL(t, fx, path, fx.token); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestDefaultAliasSourceArchive(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "prod", "alpha", "1.0.0", []byte("a 1.0"))
	rec := getURL(t, fx, "/src/contrib/Archive/alpha/alpha_1.0.0.tar.gz", fx.token)
	if rec.Code != http.StatusOK || rec.Body.String() != "a 1.0" {
		t.Errorf("status = %d body %q", rec.Code, rec.Body.String())
	}
}

func TestPublishedDescriptionFieldsReachPACKAGES(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "fsapi", "1.2.0", rTarball(t, "fsapi", "1.2.0",
		"Depends: R (>= 4.1.0)\nImports: fsdb (>= 2.1.16),\n    fsutils\nLicense: MIT\nNeedsCompilation: no\n"))

	want := "Package: fsapi\nVersion: 1.2.0\nDepends: R (>= 4.1.0)\n" +
		"Imports: fsdb (>= 2.1.16), fsutils\nLicense: MIT\nNeedsCompilation: no\n"
	rec := getURL(t, fx, "/dev/src/contrib/PACKAGES", fx.token)
	if rec.Body.String() != want {
		t.Errorf("source PACKAGES =\n%s\nwant\n%s", rec.Body.String(), want)
	}
	rec = getLinux(t, fx, devLinux+"/PACKAGES", fx.token, rUA("4.4.3"))
	if rec.Body.String() != want {
		t.Errorf("linux PACKAGES =\n%s\nwant\n%s", rec.Body.String(), want)
	}
}

func TestBinaryBuiltFieldFromTarball(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	src := rTarball(t, "alpha", "1.0.0", "")
	bin := rTarball(t, "alpha", "1.0.0", "Built: R 4.4.3; ; 2026-09-01 10:00:00 UTC; unix\n")
	reqBody, ct := buildPublishBody(t, map[string]any{
		"source":   "source",
		"binaries": []map[string]any{{"cell": "r-4.4", "part": "bin1"}},
	}, publishPart{name: "source", body: src}, publishPart{name: "bin1", body: bin})
	if rec := doPublish(t, fx, "dev", "alpha", "1.0.0", fx.token, reqBody, ct); rec.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}

	rec := getLinux(t, fx, devLinux+"/PACKAGES", fx.token, rUA("4.4.3"))
	want := "Package: alpha\nVersion: 1.0.0\nBuilt: R 4.4.3; ; 2026-09-01 10:00:00 UTC; unix\n"
	if rec.Body.String() != want {
		t.Errorf("linux PACKAGES = %q, want %q", rec.Body.String(), want)
	}
}

// archiveFixture publishes alpha 1.9.0, 1.10.0 (current) and 2.0.0
// (yanked) plus beta 1.0 (current, nothing archived) to dev.
func archiveFixture(t *testing.T) *publishFixture {
	t.Helper()
	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.9.0", []byte("a 1.9"))
	publishSource(t, fx, "dev", "alpha", "1.10.0", []byte("a 1.10"))
	publishSource(t, fx, "dev", "alpha", "2.0.0", []byte("a 2.0.0"))
	publishSource(t, fx, "dev", "beta", "1.0", []byte("b"))
	if rec := doYank(t, fx, "dev", "alpha", "2.0.0", fx.token, `{"reason":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("yank: %d", rec.Code)
	}
	return fx
}

func TestArchiveRDSListsNonCurrentVersions(t *testing.T) {
	t.Parallel()

	fx := archiveFixture(t)
	var published = map[string]time.Time{}
	rows, err := fx.deps.DB.QueryContext(context.Background(),
		`SELECT version, published_at FROM packages WHERE name = 'alpha'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v, at string
		if err := rows.Scan(&v, &at); err != nil {
			t.Fatal(err)
		}
		published[v], _ = time.Parse(time.RFC3339Nano, at)
	}
	_ = rows.Close()

	// 1.9.0 then 2.0.0 (yanked, but not current) in version order; beta
	// has nothing archived and is absent.
	want := rds.Archive([]rds.ArchivePackage{{Name: "alpha", Files: []rds.ArchiveFile{
		{Path: "alpha/alpha_1.9.0.tar.gz", Size: int64(len("a 1.9")), Time: published["1.9.0"]},
		{Path: "alpha/alpha_2.0.0.tar.gz", Size: int64(len("a 2.0.0")), Time: published["2.0.0"]},
	}}})
	var wantBytes bytes.Buffer
	if err := rds.Write(&wantBytes, want); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/dev/src/contrib/Meta/archive.rds",
		devLinux + "/Meta/archive.rds",
	} {
		rec := getLinux(t, fx, path, fx.token, rUA("4.4.3"))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d body %s", path, rec.Code, rec.Body.String())
		}
		if got := gunzip(t, rec.Body.Bytes()); !bytes.Equal(got, wantBytes.Bytes()) {
			t.Errorf("%s: archive.rds content differs from expected listing", path)
		}
	}
}

func TestArchiveRDSCacheInvalidatedOnPublish(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))
	empty := gunzip(t, getURL(t, fx, "/dev/src/contrib/Meta/archive.rds", fx.token).Body.Bytes())
	publishSource(t, fx, "dev", "alpha", "1.1.0", []byte("a2"))
	after := gunzip(t, getURL(t, fx, "/dev/src/contrib/Meta/archive.rds", fx.token).Body.Bytes())
	if bytes.Equal(empty, after) {
		t.Error("archive.rds not refreshed after publishing a newer version")
	}
}

func TestArchiveRDSHonorsReadScope(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	if rec := getURL(t, fx, "/dev/src/contrib/Meta/archive.rds", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon: status = %d, want 401", rec.Code)
	}
	if rec := getURL(t, fx, "/nope/src/contrib/Meta/archive.rds", fx.token); rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel: status = %d, want 404", rec.Code)
	}
}

// TestArchiveRDSReadableByR reads the served file with R's readRDS
// and checks the structure remotes::install_version() relies on.
// Skipped when Rscript isn't installed.
func TestArchiveRDSReadableByR(t *testing.T) {
	rscript, err := exec.LookPath("Rscript")
	if err != nil {
		t.Skip("Rscript not installed")
	}
	fx := archiveFixture(t)
	rec := getURL(t, fx, "/dev/src/contrib/Meta/archive.rds", fx.token)
	path := filepath.Join(t.TempDir(), "archive.rds")
	if err := os.WriteFile(path, rec.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `a <- readRDS(commandArgs(TRUE)[1])
x <- a[["alpha"]]
stopifnot(identical(names(a), "alpha"), is.data.frame(x),
          identical(rownames(x), c("alpha/alpha_1.9.0.tar.gz", "alpha/alpha_2.0.0.tar.gz")),
          identical(x$size, c(5, 7)), inherits(x$mtime, "POSIXct"),
          identical(names(x), names(file.info(tempdir()))))
cat("ok")`
	out, err := exec.Command(rscript, "-e", script, path).CombinedOutput()
	if err != nil || !strings.HasSuffix(string(out), "ok") {
		t.Fatalf("R could not read archive.rds: %v\n%s", err, out)
	}
}
