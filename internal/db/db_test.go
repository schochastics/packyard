package db_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/schochastics/pakman/internal/db"
)

func TestOpenAppliesPragmas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pakman.sqlite")

	database, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	cases := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
		{"synchronous", "1"}, // NORMAL == 1
	}
	for _, tc := range cases {
		var got string
		if err := database.QueryRowContext(ctx, "PRAGMA "+tc.pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", tc.pragma, err)
		}
		if got != tc.want {
			t.Errorf("PRAGMA %s = %q, want %q", tc.pragma, got, tc.want)
		}
	}
}

func TestOpenCreatesFileAndRoundTrips(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rt.sqlite")

	database, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if _, err := database.ExecContext(ctx, `CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO t(x) VALUES (42)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	var x int
	if err := database.QueryRowContext(ctx, `SELECT x FROM t`).Scan(&x); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if x != 42 {
		t.Errorf("got %d, want 42", x)
	}
}

func TestOpenMissingDirFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	// Path under a directory that doesn't exist — SQLite should fail on open.
	path := filepath.Join(t.TempDir(), "nope", "pakman.sqlite")

	if _, err := db.Open(ctx, path); err == nil {
		t.Fatal("expected error for missing parent directory, got nil")
	}
}
