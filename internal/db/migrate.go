package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// MigrateEmbedded applies the migrations shipped inside the binary. It's a
// thin wrapper over Migrate that is the right entry point for production
// code; tests use Migrate directly with an in-memory fs.FS.
func MigrateEmbedded(ctx context.Context, db *DB) error {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return fmt.Errorf("locate embedded migrations: %w", err)
	}
	return Migrate(ctx, db, sub)
}

// migrationFilename matches "NNN_some-name.sql" where NNN is one or more digits.
// The leading number is the migration's version; filenames without this shape
// are rejected so we never silently skip a file.
var migrationFilename = regexp.MustCompile(`^(\d+)_[^/]+\.sql$`)

type migration struct {
	version int
	name    string
	body    string
}

// Migrate applies any migrations in fsys whose version is higher than the
// highest version recorded in the schema_migrations table. Each migration
// runs in its own transaction together with the INSERT into
// schema_migrations, so a SQL error leaves the database unchanged.
//
// fsys is expected to contain *.sql files at its root. Callers with an
// embed.FS should pass fs.Sub(embedFS, "migrations") or similar.
//
// Running Migrate repeatedly is a no-op once all files have been applied.
func Migrate(ctx context.Context, db *DB, fsys fs.FS) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadAppliedVersions(ctx, db)
	if err != nil {
		return err
	}

	migrations, err := readMigrations(fsys)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("apply %03d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

func loadAppliedVersions(ctx context.Context, db *DB) (map[int]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := map[int]struct{}{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return applied, nil
}

func readMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	seen := map[int]string{}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := migrationFilename.FindStringSubmatch(e.Name())
		if match == nil {
			return nil, fmt.Errorf("migration filename %q does not match NNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("parse version from %q: %w", e.Name(), err)
		}
		if prev, ok := seen[v]; ok {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", v, prev, e.Name())
		}
		seen[v] = e.Name()

		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Name(), err)
		}
		out = append(out, migration{
			version: v,
			name:    strings.TrimSuffix(e.Name(), ".sql"),
			body:    string(body),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func applyOne(ctx context.Context, db *DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// If anything below errors we roll back; if the commit succeeds the
	// rollback here is a harmless no-op. errors.Is check keeps lint happy
	// by acknowledging the rollback may report "already committed".
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			// Can't return this; the caller already has the primary error.
			// Best effort — log would be ideal but we keep db free of logging deps.
			_ = rbErr
		}
	}()

	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, name) VALUES (?, ?)`, m.version, m.name); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
