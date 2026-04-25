package importers_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/importers"
)

// buildTestBundle writes a CRAN-shaped directory under root and
// returns the manifest. pkgs maps "<name>_<version>.tar.gz" to the
// raw tarball bytes so each test can dictate the on-disk content.
func buildTestBundle(t *testing.T, root string, pkgs map[string]string) *importers.BundleManifest {
	t.Helper()
	contrib := filepath.Join(root, "src", "contrib")
	if err := os.MkdirAll(contrib, 0o755); err != nil {
		t.Fatalf("mkdir contrib: %v", err)
	}

	m := &importers.BundleManifest{
		Schema:     importers.BundleSchema,
		SnapshotID: "test-snapshot",
		RVersion:   "4.4",
		SourceURL:  "https://cloud.r-project.org",
		Mode:       "subset",
		CreatedAt:  "2026-04-25T08:00:00Z",
		Tool:       "test",
	}

	for filename, body := range pkgs {
		full := filepath.Join(contrib, filename)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", filename, err)
		}
		name, version := splitDratFilename(filename)
		sum := sha256.Sum256([]byte(body))
		m.Packages = append(m.Packages, importers.BundleManifestPackage{
			Name:    name,
			Version: version,
			Path:    "src/contrib/" + filename,
			Sha256:  hex.EncodeToString(sum[:]),
			Size:    int64(len(body)),
		})
	}

	manBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), manBytes, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return m
}

// tarGzDir packs srcDir into a .tar.gz at archivePath.
func tarGzDir(t *testing.T, srcDir, archivePath string) {
	t.Helper()
	out, err := os.Create(archivePath) //nolint:gosec // test-only path
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer func() { _ = out.Close() }()

	gz := gzip.NewWriter(out)
	defer func() { _ = gz.Close() }()
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()

	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path) //nolint:gosec // walking test-only tree
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		_ = f.Close()
		return copyErr
	})
	if err != nil {
		t.Fatalf("tar walk: %v", err)
	}
}

func TestBundleImportDirectory(t *testing.T) {
	root := t.TempDir()
	buildTestBundle(t, root, map[string]string{
		"foo_1.0.0.tar.gz": "foo-bytes",
		"bar_2.1.0.tar.gz": "bar-bytes",
		"baz_0.3.4.tar.gz": "baz-bytes",
	})

	deps := newImportDeps(t, "cran-test", config.PolicyImmutable)
	imp := importers.NewBundleImporter(deps, "cran-test")

	res, err := imp.Run(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Imported) != 3 {
		t.Errorf("Imported = %v; want 3", res.Imported)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %v; want none", res.Failed)
	}
	if res.Manifest == nil || res.Manifest.SnapshotID != "test-snapshot" {
		t.Errorf("manifest snapshot id missing or wrong: %+v", res.Manifest)
	}
}

func TestBundleImportTarGzArchive(t *testing.T) {
	src := t.TempDir()
	buildTestBundle(t, src, map[string]string{
		"foo_1.0.0.tar.gz": "foo-bytes",
	})

	archDir := t.TempDir()
	archive := filepath.Join(archDir, "bundle.tar.gz")
	tarGzDir(t, src, archive)

	deps := newImportDeps(t, "cran-test", config.PolicyImmutable)
	imp := importers.NewBundleImporter(deps, "cran-test")

	res, err := imp.Run(context.Background(), archive, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Imported) != 1 {
		t.Errorf("Imported = %v; want 1", res.Imported)
	}
}

func TestBundlePreflightRejectsSha256Mismatch(t *testing.T) {
	root := t.TempDir()
	buildTestBundle(t, root, map[string]string{
		"foo_1.0.0.tar.gz": "foo-bytes",
		"bar_2.1.0.tar.gz": "bar-bytes",
	})

	// Tamper with bar after the manifest is built.
	if err := os.WriteFile(filepath.Join(root, "src/contrib/bar_2.1.0.tar.gz"),
		[]byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := newImportDeps(t, "cran-test", config.PolicyImmutable)
	imp := importers.NewBundleImporter(deps, "cran-test")

	if _, err := imp.Run(context.Background(), root, nil); err == nil {
		t.Fatal("expected error on sha256 mismatch")
	} else if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("error = %v; want sha256 mismatch", err)
	}

	// Pre-flight aborts before any side effects: no packages should
	// have landed.
	var n int
	if err := deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM packages`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 packages after aborted import; got %d", n)
	}
}

func TestBundleRejectsBadSchema(t *testing.T) {
	root := t.TempDir()
	buildTestBundle(t, root, map[string]string{"foo_1.0.0.tar.gz": "x"})

	// Hand-edit the schema field.
	body, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(body), `"packyard-bundle/1"`, `"packyard-bundle/9999"`, 1)
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := newImportDeps(t, "cran-test", config.PolicyImmutable)
	imp := importers.NewBundleImporter(deps, "cran-test")

	if _, err := imp.Run(context.Background(), root, nil); err == nil {
		t.Fatal("expected schema rejection")
	} else if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error = %v; want schema rejection", err)
	}
}

func TestBundleIdempotentReimport(t *testing.T) {
	root := t.TempDir()
	buildTestBundle(t, root, map[string]string{
		"foo_1.0.0.tar.gz": "foo-bytes",
	})

	deps := newImportDeps(t, "cran-test", config.PolicyImmutable)
	imp := importers.NewBundleImporter(deps, "cran-test")

	res1, err := imp.Run(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(res1.Imported) != 1 || len(res1.Skipped) != 0 {
		t.Errorf("first run: imported=%v skipped=%v; want 1/0", res1.Imported, res1.Skipped)
	}

	res2, err := imp.Run(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res2.Imported) != 0 || len(res2.Skipped) != 1 {
		t.Errorf("second run: imported=%v skipped=%v; want 0/1", res2.Imported, res2.Skipped)
	}
}

func TestBundleRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	contrib := filepath.Join(root, "src", "contrib")
	if err := os.MkdirAll(contrib, 0o755); err != nil {
		t.Fatal(err)
	}

	// Hand-craft a manifest with a path-escape attempt.
	manifest := importers.BundleManifest{
		Schema:     importers.BundleSchema,
		SnapshotID: "evil",
		Mode:       "subset",
		Packages: []importers.BundleManifestPackage{
			{
				Name:    "evil",
				Version: "1.0.0",
				Path:    "../../../../etc/shadow",
				Sha256:  hex.EncodeToString(sha256.New().Sum(nil)),
				Size:    0,
			},
		},
	}
	body, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	deps := newImportDeps(t, "cran-test", config.PolicyImmutable)
	imp := importers.NewBundleImporter(deps, "cran-test")

	_, err := imp.Run(context.Background(), root, nil)
	if err == nil {
		t.Fatal("expected error on path traversal attempt")
	}
	if !strings.Contains(err.Error(), "escapes bundle root") {
		t.Errorf("error = %v; want path-escape rejection", err)
	}
}

// TestBundleProgressFires confirms the progress callback runs once
// per pre-flight + per import step. Useful for the CLI to wire a
// status line.
func TestBundleProgressFires(t *testing.T) {
	root := t.TempDir()
	buildTestBundle(t, root, map[string]string{
		"foo_1.0.0.tar.gz": "foo-bytes",
	})

	var lines []string
	progress := func(s string) { lines = append(lines, s) }

	deps := newImportDeps(t, "cran-test", config.PolicyImmutable)
	imp := importers.NewBundleImporter(deps, "cran-test")

	if _, err := imp.Run(context.Background(), root, progress); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := strings.Join(lines, "\n")
	for _, want := range []string{"manifest ok", "pre-flight", "importing foo@1.0.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("progress missing %q; got:\n%s", want, got)
		}
	}
}

// minimal helper used above when constructing manifest entries —
// re-uses splitDratFilename from drat_test.go but lives in the same
// package, so no import needed.
var _ = fmt.Sprintf // keep fmt import in case future tests need it
