package db_test

import (
	"context"
	"errors"
	"os"
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

func TestMigrateRefusesNewerSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestDB(t)
	newer := fstest.MapFS{
		"001_first.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"002_second.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (y INTEGER);`)},
	}
	if err := db.Migrate(ctx, database, newer); err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{"001_first.sql": newer["001_first.sql"]}
	if err := db.Migrate(ctx, database, older); !errors.Is(err, db.ErrSchemaTooNew) {
		t.Fatalf("older binary against newer schema: err = %v", err)
	}
}

func TestCheckEmbedded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestDB(t)
	if err := db.CheckEmbedded(ctx, database); !errors.Is(err, db.ErrSchemaBehind) {
		t.Fatalf("fresh DB: err = %v", err)
	}
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckEmbedded(ctx, database); err != nil {
		t.Fatalf("migrated DB: err = %v", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO schema_migrations(version, name) VALUES (999, '999_future')`); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckEmbedded(ctx, database); !errors.Is(err, db.ErrSchemaTooNew) {
		t.Fatalf("future DB: err = %v", err)
	}
}

// Two processes starting at once both see a migration as pending; the
// second must skip it rather than fail on e.g. a duplicate column.
func TestMigrateConcurrentOpenersBothSucceed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "packyard.sqlite")
	fsys := fstest.MapFS{
		"001_first.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"002_alter.sql":  &fstest.MapFile{Data: []byte(`ALTER TABLE a ADD COLUMN y INTEGER;`)},
		"003_create.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE c (z INTEGER);`)},
	}
	// Create the file first (WAL mode persists), as a server that has
	// run before would have; the race of interest is on migrations.
	first, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	errs := make(chan error, 4)
	for range 4 {
		go func() {
			d, err := db.Open(ctx, path)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = d.Close() }()
			errs <- db.Migrate(ctx, d, fsys)
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent migrate: %v", err)
		}
	}
}

// A path with URI metacharacters must open exactly that file.
func TestOpenPathWithURIMetacharacters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "a#b?c%d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "packyard.sqlite")
	database, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.ExecContext(ctx, `CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at %s: %v", path, err)
	}
	var mode string
	if err := database.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("pragmas not applied: journal_mode = %q, %v", mode, err)
	}
}
