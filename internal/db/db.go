// Package db opens the packyard SQLite database and runs migrations.
//
// The driver is modernc.org/sqlite — a pure-Go port — so the final binary
// can stay CGO-free and ship as a single static artifact (see Dockerfile,
// which uses distroless/static). That matters more for the ops story than
// the marginal speed difference versus mattn/go-sqlite3.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DB wraps *sql.DB with packyard-specific helpers.
type DB struct {
	*sql.DB
}

// Open opens (or creates) the packyard SQLite database at path with pragmas
// tuned for a low-concurrency server: WAL journalling, foreign keys on,
// a 5 s busy timeout so writers don't immediately fail under contention,
// and synchronous=NORMAL — the WAL-recommended setting that trades a
// theoretical loss of the last transaction on power failure for ~2x writes.
//
// Open pings the connection before returning so a dead file or permission
// error surfaces at startup rather than on the first query.
func Open(ctx context.Context, path string) (*DB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn := "file:" + path + "?" + q.Encode()

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	// Conservative pool sizing: SQLite serializes writes, so a large pool
	// buys us nothing on writes and just wastes FDs. Reads scale fine up to
	// ~10 concurrent connections in WAL mode.
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return &DB{DB: sqlDB}, nil
}
