package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/schochastics/packyard/internal/api"
	"github.com/schochastics/packyard/internal/cas"
	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/db"
)

func newGCTestDeps(t *testing.T) api.Deps {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(dir, "packyard.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store, err := cas.New(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	return api.Deps{DB: database, CAS: store}
}

// writeOrphan drops a fake blob under <root>/aa/<rest>.
func writeOrphan(t *testing.T, root, hex, body string) {
	t.Helper()
	dir := filepath.Join(root, hex[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, hex[2:]), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestLiveBlobSetCollectsPackagesAndBinaries(t *testing.T) {
	deps := newGCTestDeps(t)
	ctx := context.Background()

	if _, err := deps.DB.ExecContext(ctx,
		`INSERT INTO channels(name, overwrite_policy) VALUES ('dev', 'mutable')`,
	); err != nil {
		t.Fatal(err)
	}
	res, err := deps.DB.ExecContext(ctx,
		`INSERT INTO packages(channel, name, version, source_sha256, source_size)
		 VALUES ('dev', 'foo', '1.0.0', `+
			`'abc0000000000000000000000000000000000000000000000000000000000001', 1)`,
	)
	if err != nil {
		t.Fatal(err)
	}
	pkgID, _ := res.LastInsertId()
	if _, err := deps.DB.ExecContext(ctx,
		`INSERT INTO binaries(package_id, cell, binary_sha256, size)
		 VALUES (?, 'linux', `+
			`'abc0000000000000000000000000000000000000000000000000000000000002', 1)`,
		pkgID,
	); err != nil {
		t.Fatal(err)
	}

	live, err := liveBlobSet(deps)
	if err != nil {
		t.Fatalf("liveBlobSet: %v", err)
	}
	if len(live) != 2 {
		t.Errorf("live size = %d; want 2 (one source + one binary)", len(live))
	}
}

func TestVerifyBlobsReportsMissing(t *testing.T) {
	deps := newGCTestDeps(t)
	root := deps.CAS.Root()
	ctx := context.Background()

	if _, err := deps.DB.ExecContext(ctx,
		`INSERT INTO channels(name, overwrite_policy) VALUES ('dev', 'mutable')`,
	); err != nil {
		t.Fatal(err)
	}

	const liveSum = "ccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc1"
	const missingSum = "ddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd2"

	// Seed two packages: one with a CAS blob, one without.
	if _, err := deps.DB.ExecContext(ctx,
		`INSERT INTO packages(channel, name, version, source_sha256, source_size)
		 VALUES ('dev', 'ok', '1.0.0', ?, 1), ('dev', 'gone', '1.0.0', ?, 1)`,
		liveSum, missingSum,
	); err != nil {
		t.Fatal(err)
	}
	writeOrphan(t, root, liveSum, "x")
	// Don't write the missingSum blob.

	missing, err := verifyBlobs(deps)
	if err != nil {
		t.Fatalf("verifyBlobs: %v", err)
	}
	if len(missing) != 1 {
		t.Fatalf("missing = %d; want 1", len(missing))
	}
	if missing[0].Package != "gone" || missing[0].Column != "source" {
		t.Errorf("unexpected missing entry: %+v", missing[0])
	}
}

// TestAdminImportBundleSourceThenBinary exercises the full CLI path:
// resolveConfig -> adminImportBundle -> openAdminDeps -> BundleImporter,
// for a source bundle followed by a binary bundle on the same channel.
// Verifies the final DB state has both source and binary rows linked.
func TestAdminImportBundleSourceThenBinary(t *testing.T) {
	const (
		channelName = "cran-r4.4-test"
		cell        = "rhel9-amd64-r-4.4"
	)

	dataDir := t.TempDir()

	// Seed matrix.yaml with the cell we'll import binaries for.
	matrixYAML := []byte(`cells:
  - name: ` + cell + `
    os: linux
    os_version: rhel9
    arch: amd64
    r_minor: "4.4"
`)
	if err := os.WriteFile(filepath.Join(dataDir, "matrix.yaml"), matrixYAML, 0o644); err != nil {
		t.Fatalf("write matrix: %v", err)
	}

	// Seed channels.yaml so the operator-visible config matches the DB.
	channelsYAML := []byte(`channels:
  - name: ` + channelName + `
    overwrite_policy: immutable
`)
	if err := os.WriteFile(filepath.Join(dataDir, "channels.yaml"), channelsYAML, 0o644); err != nil {
		t.Fatalf("write channels: %v", err)
	}

	cfg := config.DefaultServerConfig()
	cfg.DataDir = dataDir

	// Open the DB once to migrate + seed the channel row, since
	// adminImportBundle doesn't reconcile channels.yaml.
	database, err := db.Open(context.Background(), filepath.Join(dataDir, "db.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.MigrateEmbedded(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := database.ExecContext(context.Background(),
		`INSERT INTO channels(name, overwrite_policy) VALUES (?, 'immutable')`, channelName,
	); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	_ = database.Close()

	// Build a source bundle on disk.
	srcDir := t.TempDir()
	writeSourceBundle(t, srcDir, map[string]string{
		"foo_1.0.0.tar.gz": "foo-source-bytes",
	})
	srcArchive := filepath.Join(t.TempDir(), "src.tar.gz")
	tarGzDir(t, srcDir, srcArchive)

	// Build a matching binary bundle on disk.
	binDir := t.TempDir()
	writeBinaryBundle(t, binDir, cell, map[string]string{
		"foo_1.0.0.tar.gz": "foo-rhel9-binary-bytes",
	})
	binArchive := filepath.Join(t.TempDir(), "bin.tar.gz")
	tarGzDir(t, binDir, binArchive)

	if err := adminImportBundle(&cfg, []string{"-channel", channelName, srcArchive}); err != nil {
		t.Fatalf("source import: %v", err)
	}
	if err := adminImportBundle(&cfg, []string{"-channel", channelName, binArchive}); err != nil {
		t.Fatalf("binary import: %v", err)
	}

	// Reopen DB and verify state.
	database, err = db.Open(context.Background(), filepath.Join(dataDir, "db.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	var (
		pkgID     int64
		sourceSHA string
	)
	if err := database.QueryRowContext(context.Background(),
		`SELECT id, source_sha256 FROM packages WHERE channel = ? AND name = ? AND version = ?`,
		channelName, "foo", "1.0.0",
	).Scan(&pkgID, &sourceSHA); err != nil {
		t.Fatalf("read package row: %v", err)
	}
	wantSourceSum := sha256Sum("foo-source-bytes")
	if sourceSHA != wantSourceSum {
		t.Errorf("source_sha256 = %s; want %s", sourceSHA, wantSourceSum)
	}

	var binSHA string
	if err := database.QueryRowContext(context.Background(),
		`SELECT binary_sha256 FROM binaries WHERE package_id = ? AND cell = ?`,
		pkgID, cell,
	).Scan(&binSHA); err != nil {
		t.Fatalf("read binary row: %v", err)
	}
	if binSHA != sha256Sum("foo-rhel9-binary-bytes") {
		t.Errorf("binary_sha256 = %s; want sha256(foo-rhel9-binary-bytes)", binSHA)
	}
}

// writeSourceBundle writes a v2 source-shaped CRAN bundle directory
// at root with a manifest.json. Mirrors the helper in importers but
// duplicated here because the importers test helper is in a _test
// package and not importable.
func writeSourceBundle(t *testing.T, root string, pkgs map[string]string) {
	t.Helper()
	contrib := filepath.Join(root, "src", "contrib")
	if err := os.MkdirAll(contrib, 0o755); err != nil {
		t.Fatalf("mkdir contrib: %v", err)
	}

	type blob struct {
		Path   string `json:"path"`
		Sha256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	type pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Source  *blob  `json:"source"`
	}
	manifest := map[string]any{
		"schema":      "packyard-bundle/2",
		"snapshot_id": "cran-r4.4-test",
		"r_version":   "4.4",
		"mode":        "subset",
		"kind":        "source",
		"created_at":  "2026-04-25T08:00:00Z",
		"tool":        "test",
	}
	var pkgList []pkg
	for filename, body := range pkgs {
		full := filepath.Join(contrib, filename)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", filename, err)
		}
		name, version := splitTarballFilename(filename)
		pkgList = append(pkgList, pkg{
			Name:    name,
			Version: version,
			Source: &blob{
				Path:   "src/contrib/" + filename,
				Sha256: sha256Sum(body),
				Size:   int64(len(body)),
			},
		})
	}
	manifest["packages"] = pkgList

	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), out, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func writeBinaryBundle(t *testing.T, root, cell string, pkgs map[string]string) {
	t.Helper()
	cellDir := filepath.Join(root, "bin", "linux", cell)
	if err := os.MkdirAll(cellDir, 0o755); err != nil {
		t.Fatalf("mkdir cellDir: %v", err)
	}

	type bin struct {
		Cell   string `json:"cell"`
		Path   string `json:"path"`
		Sha256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	type pkg struct {
		Name     string `json:"name"`
		Version  string `json:"version"`
		Binaries []bin  `json:"binaries"`
	}
	manifest := map[string]any{
		"schema":      "packyard-bundle/2",
		"snapshot_id": "cran-r4.4-test",
		"r_version":   "4.4",
		"mode":        "subset",
		"kind":        "binary",
		"cell":        cell,
		"created_at":  "2026-04-25T08:00:00Z",
		"tool":        "test",
	}
	var pkgList []pkg
	for filename, body := range pkgs {
		full := filepath.Join(cellDir, filename)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", filename, err)
		}
		name, version := splitTarballFilename(filename)
		pkgList = append(pkgList, pkg{
			Name:    name,
			Version: version,
			Binaries: []bin{{
				Cell:   cell,
				Path:   "bin/linux/" + cell + "/" + filename,
				Sha256: sha256Sum(body),
				Size:   int64(len(body)),
			}},
		})
	}
	manifest["packages"] = pkgList

	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), out, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func splitTarballFilename(fn string) (string, string) {
	base := fn[:len(fn)-len(".tar.gz")]
	for i := 0; i < len(base); i++ {
		if base[i] == '_' {
			return base[:i], base[i+1:]
		}
	}
	return base, ""
}

func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
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

func TestAdminGCReclaimsOrphan(t *testing.T) {
	deps := newGCTestDeps(t)
	root := deps.CAS.Root()

	const liveSum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
	const orphanSum = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2"

	ctx := context.Background()
	if _, err := deps.DB.ExecContext(ctx,
		`INSERT INTO channels(name, overwrite_policy) VALUES ('dev', 'mutable')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.DB.ExecContext(ctx,
		`INSERT INTO packages(channel, name, version, source_sha256, source_size)
		 VALUES ('dev', 'foo', '1.0.0', ?, 1)`, liveSum,
	); err != nil {
		t.Fatal(err)
	}

	writeOrphan(t, root, liveSum, "live-body")
	writeOrphan(t, root, orphanSum, "orphan-body")

	live, err := liveBlobSet(deps)
	if err != nil {
		t.Fatal(err)
	}
	report, err := deps.CAS.GC(live)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if report.Removed != 1 {
		t.Errorf("Removed = %d; want 1", report.Removed)
	}
	if report.FreedBytes != int64(len("orphan-body")) {
		t.Errorf("FreedBytes = %d; want %d", report.FreedBytes, len("orphan-body"))
	}
	// Live blob still there.
	if _, err := os.Stat(filepath.Join(root, liveSum[:2], liveSum[2:])); err != nil {
		t.Errorf("live blob was removed: %v", err)
	}
}
