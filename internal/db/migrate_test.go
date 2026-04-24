package db_test

import (
	"context"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/schochastics/packyard/internal/db"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "packyard.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestMigrateAppliesInOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := openTestDB(t)

	fsys := fstest.MapFS{
		"001_first.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"002_second.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (y INTEGER);`)},
	}

	if err := db.Migrate(ctx, database, fsys); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, tbl := range []string{"a", "b"} {
		var got string
		err := database.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&got)
		if err != nil {
			t.Errorf("table %q missing: %v", tbl, err)
		}
	}

	var count int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("schema_migrations count = %d, want 2", count)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := openTestDB(t)

	fsys := fstest.MapFS{
		"001_one.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE only_once (x INTEGER);`)},
	}

	if err := db.Migrate(ctx, database, fsys); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	// Second run must not attempt to re-CREATE the table (that would error).
	if err := db.Migrate(ctx, database, fsys); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var count int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version=1`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_migrations rows for v1 = %d, want 1", count)
	}
}

func TestMigrateRollsBackOnError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := openTestDB(t)

	// The first statement should succeed; the second should fail. Because we
	// run each migration inside a single tx, the partial effect must be
	// rolled back.
	fsys := fstest.MapFS{
		"001_broken.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE partial (x INTEGER); NOT A VALID STATEMENT;`,
		)},
	}

	if err := db.Migrate(ctx, database, fsys); err == nil {
		t.Fatal("Migrate succeeded but migration contained invalid SQL")
	}

	var name string
	err := database.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='partial'`).Scan(&name)
	if err == nil {
		t.Error("partial table survived a failed migration")
	}

	var count int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("schema_migrations count = %d after rollback, want 0", count)
	}
}

func TestMigrateRejectsBadFilename(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := openTestDB(t)

	fsys := fstest.MapFS{
		"no-leading-number.sql": &fstest.MapFile{Data: []byte(`-- empty`)},
	}

	if err := db.Migrate(ctx, database, fsys); err == nil {
		t.Fatal("Migrate accepted malformed filename")
	}
}

func TestMigrateEmbeddedBootstrapsSchema(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := openTestDB(t)

	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatalf("MigrateEmbedded: %v", err)
	}

	// All five design-mandated tables plus schema_migrations must exist.
	wantTables := []string{
		"channels", "packages", "binaries", "events", "tokens",
		"schema_migrations",
	}
	for _, tbl := range wantTables {
		var got string
		err := database.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&got)
		if err != nil {
			t.Errorf("table %q missing: %v", tbl, err)
		}
	}

	// The partial-unique-index guard: two default channels must fail.
	if _, err := database.ExecContext(ctx, `
		INSERT INTO channels(name, overwrite_policy, is_default) VALUES ('a', 'mutable', 1);
		INSERT INTO channels(name, overwrite_policy, is_default) VALUES ('b', 'immutable', 1);
	`); err == nil {
		t.Error("two default channels were accepted; channels_one_default index is missing")
	}

	// Second embedded run is a no-op.
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatalf("MigrateEmbedded (rerun): %v", err)
	}
}

func TestMigrateRejectsDuplicateVersion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := openTestDB(t)

	fsys := fstest.MapFS{
		"001_a.sql": &fstest.MapFile{Data: []byte(`-- a`)},
		"001_b.sql": &fstest.MapFile{Data: []byte(`-- b`)},
	}

	if err := db.Migrate(ctx, database, fsys); err == nil {
		t.Fatal("Migrate accepted duplicate version")
	}
}
