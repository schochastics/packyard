package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/schochastics/packyard/internal/api"
	"github.com/schochastics/packyard/internal/cas"
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
