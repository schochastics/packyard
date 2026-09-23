package db_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/schochastics/packyard/internal/db"
)

func TestOpenAppliesPragmas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "packyard.sqlite")

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
	path := filepath.Join(t.TempDir(), "nope", "packyard.sqlite")

	if _, err := db.Open(ctx, path); err == nil {
		t.Fatal("expected error for missing parent directory, got nil")
	}
}

// TestConcurrentWritersSerialize proves the _txlock=immediate DSN
// flag takes effect. N goroutines each open a tx, INSERT a unique
// row, briefly hold the tx open, and commit. With BEGIN DEFERRED
// (the driver default) the first statement of a second goroutine
// that started its tx before the first goroutine committed races
// to escalate from SHARED to RESERVED and can hit
// SQLITE_BUSY_DEADLOCK that busy_timeout can't resolve. With BEGIN
// IMMEDIATE the second BeginTx blocks on the first's RESERVED lock
// and waits cleanly. We assert all N writes land without error.
func TestConcurrentWritersSerialize(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.sqlite")
	database, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if _, err := database.ExecContext(ctx, `CREATE TABLE t (x INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	start := make(chan struct{})

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(x int) {
			defer wg.Done()
			<-start // release all writers simultaneously to maximize contention
			tx, err := database.BeginTx(ctx, nil)
			if err != nil {
				errs <- err
				return
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO t(x) VALUES (?)`, x); err != nil {
				_ = tx.Rollback()
				errs <- err
				return
			}
			if err := tx.Commit(); err != nil {
				errs <- err
				return
			}
		}(i)
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent writer: %v", err)
	}

	var count int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("SELECT COUNT: %v", err)
	}
	if count != writers {
		t.Errorf("got %d rows, want %d", count, writers)
	}
}
