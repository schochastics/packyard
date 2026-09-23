package store_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"path/filepath"
	"testing"

	"github.com/schochastics/packyard/internal/cas"
	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/db"
	"github.com/schochastics/packyard/internal/store"
)

func tarball(t *testing.T, name, desc string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
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

func setup(t *testing.T) (*db.DB, *store.Service) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO channels(name, overwrite_policy, is_default) VALUES ('dev', 'mutable', 1)`); err != nil {
		t.Fatal(err)
	}
	c, err := cas.New(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	return database, store.New(database.DB, c)
}

func TestMaterializeRecordsDescriptionFields(t *testing.T) {
	database, svc := setup(t)
	ctx := context.Background()

	src, err := svc.WriteBlob(bytes.NewReader(tarball(t, "alpha", "Package: alpha\nVersion: 1.0\nImports: beta\nTitle: x\n")))
	if err != nil {
		t.Fatal(err)
	}
	bin, err := svc.WriteBlob(bytes.NewReader(tarball(t, "alpha", "Package: alpha\nVersion: 1.0\nBuilt: R 4.4.3; ; d; unix\n")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Materialize(ctx, store.Input{
		Channel: "dev", Name: "alpha", Version: "1.0", Policy: config.PolicyMutable,
		Source: src, Binaries: []store.BinaryInput{{Cell: "r-4.4", Blob: bin}},
	}); err != nil {
		t.Fatal(err)
	}

	var fields, built string
	if err := database.QueryRowContext(ctx,
		`SELECT p.index_fields, b.built FROM packages p JOIN binaries b ON b.package_id = p.id`).Scan(&fields, &built); err != nil {
		t.Fatal(err)
	}
	if fields != `{"Imports":"beta"}` {
		t.Errorf("index_fields = %s", fields)
	}
	if built != "R 4.4.3; ; d; unix" {
		t.Errorf("built = %q", built)
	}
}

func TestBackfillMetadataFillsNullRowsOnly(t *testing.T) {
	database, svc := setup(t)
	ctx := context.Background()

	good, err := svc.WriteBlob(bytes.NewReader(tarball(t, "alpha", "Package: alpha\nVersion: 1.0\nDepends: R (>= 4.0)\n")))
	if err != nil {
		t.Fatal(err)
	}
	junk, err := svc.WriteBlob(bytes.NewReader([]byte("not a tarball")))
	if err != nil {
		t.Fatal(err)
	}
	// Rows as they looked before migration 003: metadata NULL.
	for _, r := range []struct{ name, sum string }{{"alpha", good.SHA256}, {"junk", junk.SHA256}} {
		if _, err := database.ExecContext(ctx, `
			INSERT INTO packages(channel, name, version, source_sha256, source_size)
			VALUES ('dev', ?, '1.0', ?, 1)`, r.name, r.sum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO binaries(package_id, cell, binary_sha256, size)
		SELECT id, 'r-4.4', source_sha256, 1 FROM packages WHERE name = 'alpha'`); err != nil {
		t.Fatal(err)
	}

	res, err := svc.BackfillMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Packages != 2 || res.Binaries != 1 {
		t.Errorf("backfill = %+v, want 2 packages, 1 binary", res)
	}

	got := map[string]string{}
	rows, err := database.QueryContext(ctx, `SELECT name, index_fields FROM packages`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n, f string
		if err := rows.Scan(&n, &f); err != nil {
			t.Fatal(err)
		}
		got[n] = f
	}
	if got["alpha"] != `{"Depends":"R (>= 4.0)"}` {
		t.Errorf("alpha index_fields = %s", got["alpha"])
	}
	if got["junk"] != "{}" {
		t.Errorf("junk index_fields = %s, want {}", got["junk"])
	}

	again, err := svc.BackfillMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.Packages != 0 || again.Binaries != 0 {
		t.Errorf("second backfill = %+v, want nothing to do", again)
	}
}
