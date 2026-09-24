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

// ErrSchemaTooNew is returned when the database has migrations applied
// that this binary doesn't know: a newer packyard ran against it, or a
// backup from a newer version was restored. Running against a schema
// the code doesn't understand is how data gets corrupted, and there is
// no downgrade path.
var ErrSchemaTooNew = errors.New("database schema is newer than this packyard binary")

// ErrSchemaBehind is returned by [CheckEmbedded] when migrations are
// pending.
var ErrSchemaBehind = errors.New("database schema is older than this packyard binary")

// LatestEmbeddedVersion is the highest migration version shipped in the
// binary.
func LatestEmbeddedVersion() (int, error) {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return 0, err
	}
	ms, err := readMigrations(sub)
	if err != nil {
		return 0, err
	}
	if len(ms) == 0 {
		return 0, nil
	}
	return ms[len(ms)-1].version, nil
}

// CheckEmbedded reports whether db's schema matches the migrations
// shipped in the binary exactly, without changing anything. Commands
// that only read or copy an existing repository (admin verbs, backup)
// use it instead of migrating, so an admin container on a newer image
// never migrates the DB underneath an older running server.
func CheckEmbedded(ctx context.Context, db *DB) error {
	latest, err := LatestEmbeddedVersion()
	if err != nil {
		return err
	}
	current, err := currentVersion(ctx, db)
	if err != nil {
		return err
	}
	switch {
	case current > latest:
		return fmt.Errorf("%w (database at migration %d, binary knows up to %d)", ErrSchemaTooNew, current, latest)
	case current < latest:
		return fmt.Errorf("%w (database at migration %d, binary expects %d); start the server once to migrate", ErrSchemaBehind, current, latest)
	}
	return nil
}

func currentVersion(ctx context.Context, db *DB) (int, error) {
	return SchemaVersion(ctx, db.DB)
}

// Querier is the read surface shared by *sql.DB and *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SchemaVersion returns the highest applied migration, 0 for a DB
// that has none.
func SchemaVersion(ctx context.Context, q Querier) (int, error) {
	var v sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return int(v.Int64), nil
}

// ReferencedBlobs returns every CAS sha256 a package or binary row
// references, sorted and deduplicated. Yanked packages are included:
// yanking hides a version, its bytes stay reachable. This is the live
// set for gc and the blob set a backup must contain.
func ReferencedBlobs(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT source_sha256 FROM packages
		UNION SELECT binary_sha256 FROM binaries
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("list referenced blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
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
	known := map[int]struct{}{}
	for _, m := range migrations {
		known[m.version] = struct{}{}
	}
	for v := range applied {
		if _, ok := known[v]; !ok {
			return fmt.Errorf("%w (migration %d is applied but not shipped in this binary)", ErrSchemaTooNew, v)
		}
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

	// Another process (a server restart racing an admin command) may
	// have applied this migration since the applied set was read. The
	// check runs under the write lock BEGIN IMMEDIATE took, so it is
	// authoritative.
	var done int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&done); err != nil {
		return fmt.Errorf("re-check version: %w", err)
	}
	if done > 0 {
		return nil
	}

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
