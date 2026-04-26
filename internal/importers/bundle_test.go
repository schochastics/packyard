package importers_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schochastics/packyard/internal/api"
	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/importers"
)

// buildTestBundle writes a v2 source-shaped CRAN bundle under root and
// returns the in-memory manifest. pkgs maps "<name>_<version>.tar.gz"
// to the raw tarball bytes so each test can dictate the on-disk
// content.
func buildTestBundle(t *testing.T, root string, pkgs map[string]string) *importers.BundleManifest {
	t.Helper()
	contrib := filepath.Join(root, "src", "contrib")
	if err := os.MkdirAll(contrib, 0o755); err != nil {
		t.Fatalf("mkdir contrib: %v", err)
	}

	m := &importers.BundleManifest{
		Schema:     importers.BundleSchemaV2,
		SnapshotID: "test-snapshot",
		RVersion:   "4.4",
		SourceURL:  "https://cloud.r-project.org",
		Mode:       "subset",
		Kind:       importers.BundleKindSource,
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
			Source: &importers.BundleManifestBlob{
				Path:   "src/contrib/" + filename,
				Sha256: hex.EncodeToString(sum[:]),
				Size:   int64(len(body)),
			},
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

// buildTestBundleV1Raw writes a legacy packyard-bundle/1 manifest by
// hand. Used to keep regression coverage that v1 archives still
// import unchanged after the schema bump.
func buildTestBundleV1Raw(t *testing.T, root string, pkgs map[string]string) {
	t.Helper()
	contrib := filepath.Join(root, "src", "contrib")
	if err := os.MkdirAll(contrib, 0o755); err != nil {
		t.Fatalf("mkdir contrib: %v", err)
	}

	type v1Pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Path    string `json:"path"`
		Sha256  string `json:"sha256"`
		Size    int64  `json:"size"`
	}
	type v1Manifest struct {
		Schema     string  `json:"schema"`
		SnapshotID string  `json:"snapshot_id"`
		RVersion   string  `json:"r_version"`
		Mode       string  `json:"mode"`
		CreatedAt  string  `json:"created_at"`
		Tool       string  `json:"tool"`
		Packages   []v1Pkg `json:"packages"`
	}

	m := v1Manifest{
		Schema:     importers.BundleSchemaV1,
		SnapshotID: "test-snapshot-v1",
		RVersion:   "4.4",
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
		m.Packages = append(m.Packages, v1Pkg{
			Name:    name,
			Version: version,
			Path:    "src/contrib/" + filename,
			Sha256:  hex.EncodeToString(sum[:]),
			Size:    int64(len(body)),
		})
	}

	manBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal v1 manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), manBytes, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// buildTestBinaryBundle writes a v2 binary-shaped bundle under root.
// pkgs maps "<name>_<version>.tar.gz" → raw bytes; tarballs land at
// bin/linux/<cell>/<filename> matching the bundler's output layout.
func buildTestBinaryBundle(t *testing.T, root, cell string, pkgs map[string]string) *importers.BundleManifest {
	t.Helper()
	cellDir := filepath.Join(root, "bin", "linux", cell)
	if err := os.MkdirAll(cellDir, 0o755); err != nil {
		t.Fatalf("mkdir cellDir: %v", err)
	}

	m := &importers.BundleManifest{
		Schema:     importers.BundleSchemaV2,
		SnapshotID: "test-snapshot-bin",
		RVersion:   "4.4",
		SourceURL:  "https://packagemanager.posit.co/cran/__linux__/rhel9/2026-04-01",
		Mode:       "subset",
		Kind:       importers.BundleKindBinary,
		Cell:       cell,
		CreatedAt:  "2026-04-25T08:00:00Z",
		Tool:       "test",
	}

	for filename, body := range pkgs {
		full := filepath.Join(cellDir, filename)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", filename, err)
		}
		name, version := splitDratFilename(filename)
		sum := sha256.Sum256([]byte(body))
		m.Packages = append(m.Packages, importers.BundleManifestPackage{
			Name:    name,
			Version: version,
			Binaries: []importers.BundleManifestBinary{
				{
					Cell: cell,
					BundleManifestBlob: importers.BundleManifestBlob{
						Path:   "bin/linux/" + cell + "/" + filename,
						Sha256: hex.EncodeToString(sum[:]),
						Size:   int64(len(body)),
					},
				},
			},
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

// withMatrix returns deps with a matrix containing the named cell so
// binary bundle imports validate. Reused across binary-mode tests.
func withMatrix(deps api.Deps, cell string) api.Deps {
	deps.Matrix = &config.MatrixConfig{
		Cells: []config.Cell{
			{
				Name:      cell,
				OS:        "linux",
				OSVersion: "rhel9",
				Arch:      "amd64",
				RMinor:    "4.4",
			},
		},
	}
	return deps
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

	body, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(body), `"packyard-bundle/2"`, `"packyard-bundle/9999"`, 1)
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

	manifest := importers.BundleManifest{
		Schema:     importers.BundleSchemaV2,
		SnapshotID: "evil",
		Mode:       "subset",
		Kind:       importers.BundleKindSource,
		Packages: []importers.BundleManifestPackage{
			{
				Name:    "evil",
				Version: "1.0.0",
				Source: &importers.BundleManifestBlob{
					Path:   "../../../../etc/shadow",
					Sha256: hex.EncodeToString(sha256.New().Sum(nil)),
					Size:   0,
				},
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

// TestBundleV1ManifestStillImports is the regression test that proves
// pre-existing packyard-bundle/1 archives don't break after the v2
// schema bump.
func TestBundleV1ManifestStillImports(t *testing.T) {
	root := t.TempDir()
	buildTestBundleV1Raw(t, root, map[string]string{
		"foo_1.0.0.tar.gz": "foo-bytes",
	})

	deps := newImportDeps(t, "cran-test", config.PolicyImmutable)
	imp := importers.NewBundleImporter(deps, "cran-test")

	res, err := imp.Run(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Imported) != 1 {
		t.Errorf("Imported = %v; want 1", res.Imported)
	}
	if res.Manifest.Kind != importers.BundleKindSource {
		t.Errorf("v1 manifest should normalise to Kind=source; got %q", res.Manifest.Kind)
	}
	if res.Manifest.Schema != importers.BundleSchemaV1 {
		t.Errorf("schema should be preserved as v1; got %q", res.Manifest.Schema)
	}
}

// TestBundleBinaryRoundTrip is the happy path for the new flow:
// import a source bundle to seed packages rows, then import a binary
// bundle on the same channel and confirm binaries land.
func TestBundleBinaryRoundTrip(t *testing.T) {
	const cell = "rhel9-amd64-r-4.4"

	srcRoot := t.TempDir()
	buildTestBundle(t, srcRoot, map[string]string{
		"foo_1.0.0.tar.gz": "foo-source-bytes",
		"bar_2.1.0.tar.gz": "bar-source-bytes",
	})
	binRoot := t.TempDir()
	buildTestBinaryBundle(t, binRoot, cell, map[string]string{
		"foo_1.0.0.tar.gz": "foo-rhel9-binary-bytes",
		"bar_2.1.0.tar.gz": "bar-rhel9-binary-bytes",
	})

	deps := withMatrix(newImportDeps(t, "cran-r4.4-2026q2", config.PolicyImmutable), cell)
	imp := importers.NewBundleImporter(deps, "cran-r4.4-2026q2")

	if res, err := imp.Run(context.Background(), srcRoot, nil); err != nil {
		t.Fatalf("source Run: %v", err)
	} else if len(res.Imported) != 2 || len(res.Failed) != 0 {
		t.Fatalf("source import: imported=%v failed=%v", res.Imported, res.Failed)
	}

	res, err := imp.Run(context.Background(), binRoot, nil)
	if err != nil {
		t.Fatalf("binary Run: %v", err)
	}
	if len(res.Imported) != 2 {
		t.Errorf("binary Imported = %v; want 2", res.Imported)
	}
	if len(res.Failed) != 0 {
		t.Errorf("binary Failed = %v; want none", res.Failed)
	}

	var n int
	if err := deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM binaries WHERE cell = ?`, cell).Scan(&n); err != nil {
		t.Fatalf("count binaries: %v", err)
	}
	if n != 2 {
		t.Errorf("binaries rows for %s = %d; want 2", cell, n)
	}
}

// TestBundleBinaryFailsWithoutSource confirms binary bundles surface
// ErrSourceRowMissing per package when the matching source bundle
// hasn't been imported yet, without aborting the whole run.
func TestBundleBinaryFailsWithoutSource(t *testing.T) {
	const cell = "rhel9-amd64-r-4.4"

	binRoot := t.TempDir()
	buildTestBinaryBundle(t, binRoot, cell, map[string]string{
		"foo_1.0.0.tar.gz": "foo-rhel9-binary-bytes",
		"bar_2.1.0.tar.gz": "bar-rhel9-binary-bytes",
	})

	deps := withMatrix(newImportDeps(t, "cran-r4.4-2026q2", config.PolicyImmutable), cell)
	imp := importers.NewBundleImporter(deps, "cran-r4.4-2026q2")

	res, err := imp.Run(context.Background(), binRoot, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Imported) != 0 {
		t.Errorf("Imported = %v; want none", res.Imported)
	}
	if len(res.Failed) != 2 {
		t.Fatalf("Failed = %v; want 2", res.Failed)
	}
	for _, f := range res.Failed {
		if !errors.Is(f.Err, api.ErrSourceRowMissing) {
			t.Errorf("failure %s@%s: want ErrSourceRowMissing; got %v", f.Package, f.Version, f.Err)
		}
	}
}

// TestBundleBinaryRejectsUnknownCell aborts before pre-flight if the
// bundle's cell isn't in matrix.yaml.
func TestBundleBinaryRejectsUnknownCell(t *testing.T) {
	const cell = "rhel9-amd64-r-4.4"

	binRoot := t.TempDir()
	buildTestBinaryBundle(t, binRoot, cell, map[string]string{
		"foo_1.0.0.tar.gz": "x",
	})

	// Matrix declares a different cell; the bundle's cell is unknown.
	deps := withMatrix(newImportDeps(t, "cran-test", config.PolicyImmutable), "ubuntu-24.04-amd64-r-4.4")
	imp := importers.NewBundleImporter(deps, "cran-test")

	_, err := imp.Run(context.Background(), binRoot, nil)
	if err == nil {
		t.Fatal("expected unknown-cell rejection")
	}
	if !strings.Contains(err.Error(), "not declared in matrix.yaml") {
		t.Errorf("error = %v; want matrix rejection", err)
	}
}

// TestBundleBinaryPreflightMismatch checks that a tampered binary
// tarball aborts the whole import before any binaries land.
func TestBundleBinaryPreflightMismatch(t *testing.T) {
	const cell = "rhel9-amd64-r-4.4"

	srcRoot := t.TempDir()
	buildTestBundle(t, srcRoot, map[string]string{
		"foo_1.0.0.tar.gz": "foo-source-bytes",
	})
	binRoot := t.TempDir()
	buildTestBinaryBundle(t, binRoot, cell, map[string]string{
		"foo_1.0.0.tar.gz": "foo-rhel9-binary-bytes",
	})

	if err := os.WriteFile(filepath.Join(binRoot, "bin/linux", cell, "foo_1.0.0.tar.gz"),
		[]byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := withMatrix(newImportDeps(t, "cran-test", config.PolicyImmutable), cell)
	imp := importers.NewBundleImporter(deps, "cran-test")

	if _, err := imp.Run(context.Background(), srcRoot, nil); err != nil {
		t.Fatalf("source Run: %v", err)
	}
	if _, err := imp.Run(context.Background(), binRoot, nil); err == nil {
		t.Fatal("expected sha256 mismatch on binary preflight")
	} else if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("error = %v; want sha256 mismatch", err)
	}

	var n int
	if err := deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM binaries`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 binary rows after aborted import; got %d", n)
	}
}
