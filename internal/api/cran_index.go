package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Ways the CRAN-protocol read surface differs from strict CRAN:
//
//   - All versions of a package appear in PACKAGES, not just the latest.
//     CRAN keeps older versions in /Archive/; packyard is simpler. R's
//     available.packages() will dedupe (keeping the lex-max version),
//     which works for the common case of monotonically increasing
//     versions and is documented as a known quirk otherwise.
//   - Yanked rows are included with "Yanked: yes". Base R ignores the
//     field and will still install them if they happen to be the
//     lex-max; pak/renv-aware tooling can honor the flag. Yanking is
//     meant for safety flagging, not removal — use delete for that.
//   - We ship PACKAGES only. PACKAGES.gz is derived on request; we
//     skip PACKAGES.rds entirely, which base R doesn't need.

// Index generates and caches PACKAGES-file bodies served from the
// CRAN-protocol routes. Entries are keyed by (kind, channel[, cell])
// and invalidated either on write (publish/yank/delete) or by TTL.
type Index struct {
	db  *sql.DB
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]indexEntry
}

type indexEntry struct {
	body    []byte
	expires time.Time
}

// NewIndex constructs an Index. A 5-minute TTL bounds staleness from
// any code path that bypasses InvalidateChannel (direct SQL, a future
// bug, etc.); the happy path is cache-then-invalidate.
func NewIndex(db *sql.DB) *Index {
	return &Index{
		db:      db,
		ttl:     5 * time.Minute,
		entries: map[string]indexEntry{},
	}
}

// SourceKey / BinaryKey shape a cache key. Kept unexported so the only
// way to populate the index is via Get* methods.
func sourceKey(channel string) string        { return "src:" + channel }
func binaryKey(channel, cell string) string  { return "bin:" + channel + ":" + cell }
func keysForChannel(channel string) []string { return []string{sourceKey(channel)} }
func binaryKeyPrefix(channel string) string  { return "bin:" + channel + ":" }

// GetSource returns the source PACKAGES body for channel, building
// (and caching) it from the DB on a miss or stale entry.
func (i *Index) GetSource(ctx context.Context, channel string) ([]byte, error) {
	key := sourceKey(channel)
	if body, ok := i.lookup(key); ok {
		return body, nil
	}
	body, err := i.buildSource(ctx, channel)
	if err != nil {
		return nil, err
	}
	i.store(key, body)
	return body, nil
}

// GetBinary returns the binary PACKAGES body for (channel, cell).
func (i *Index) GetBinary(ctx context.Context, channel, cell, rMinor string) ([]byte, error) {
	key := binaryKey(channel, cell)
	if body, ok := i.lookup(key); ok {
		return body, nil
	}
	body, err := i.buildBinary(ctx, channel, cell, rMinor)
	if err != nil {
		return nil, err
	}
	i.store(key, body)
	return body, nil
}

// InvalidateChannel drops all cached entries for channel — both the
// source PACKAGES and every (channel, *) binary PACKAGES. Called by
// publish, yank, and delete handlers on success so the next read
// reflects the new state without waiting for TTL.
func (i *Index) InvalidateChannel(channel string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, k := range keysForChannel(channel) {
		delete(i.entries, k)
	}
	prefix := binaryKeyPrefix(channel)
	for k := range i.entries {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(i.entries, k)
		}
	}
}

// InvalidateAll wipes the cache. Exposed for future /admin/reindex
// use and for tests.
func (i *Index) InvalidateAll() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.entries = map[string]indexEntry{}
}

func (i *Index) lookup(key string) ([]byte, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		delete(i.entries, key)
		return nil, false
	}
	return e.body, true
}

func (i *Index) store(key string, body []byte) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.entries[key] = indexEntry{
		body:    body,
		expires: time.Now().Add(i.ttl),
	}
}

// indexRow is one (name, version, yanked) triple used during body
// generation.
type indexRow struct {
	Name    string
	Version string
	Yanked  bool
}

// buildSource runs the DB query and formats a PACKAGES body for a
// channel's source packages.
func (i *Index) buildSource(ctx context.Context, channel string) ([]byte, error) {
	rows, err := i.db.QueryContext(ctx, `
		SELECT name, version, yanked
		FROM packages
		WHERE channel = ?
		ORDER BY name, version
	`, channel)
	if err != nil {
		return nil, fmt.Errorf("index: query source: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []indexRow{}
	for rows.Next() {
		var r indexRow
		var yanked int
		if err := rows.Scan(&r.Name, &r.Version, &yanked); err != nil {
			return nil, fmt.Errorf("index: scan source: %w", err)
		}
		r.Yanked = yanked == 1
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: iterate source: %w", err)
	}
	return formatSourcePackages(out), nil
}

// buildBinary is buildSource's binary-per-cell counterpart. A row
// appears in the binary PACKAGES only if a binary for (package, cell)
// exists.
func (i *Index) buildBinary(ctx context.Context, channel, cell, rMinor string) ([]byte, error) {
	rows, err := i.db.QueryContext(ctx, `
		SELECT p.name, p.version, p.yanked
		FROM packages p
		JOIN binaries b ON b.package_id = p.id AND b.cell = ?
		WHERE p.channel = ?
		ORDER BY p.name, p.version
	`, cell, channel)
	if err != nil {
		return nil, fmt.Errorf("index: query binary: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []indexRow{}
	for rows.Next() {
		var r indexRow
		var yanked int
		if err := rows.Scan(&r.Name, &r.Version, &yanked); err != nil {
			return nil, fmt.Errorf("index: scan binary: %w", err)
		}
		r.Yanked = yanked == 1
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: iterate binary: %w", err)
	}
	return formatBinaryPackages(out, rMinor), nil
}

// formatSourcePackages writes one DCF stanza per row.
//
// Minimum-viable stanza for v1: Package, Version, Yanked (when set).
// Richer fields (Depends, Imports, MD5sum, etc.) require parsing
// DESCRIPTION files from tarballs and land in v1.x. Base R tolerates
// missing fields — install.packages() still works, and dependency
// resolution for packyard→CRAN deps works when CRAN is in the repos
// list. packyard→packyard dep resolution requires the richer PACKAGES
// file; document this trade-off in docs/quickstart.md.
func formatSourcePackages(rows []indexRow) []byte {
	// Stable output: already ordered by SQL, but defensively sort so
	// tests don't rely on SQLite's ordering semantics.
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].Name != rows[b].Name {
			return rows[a].Name < rows[b].Name
		}
		return rows[a].Version < rows[b].Version
	})

	var buf bytes.Buffer
	for i, r := range rows {
		if i > 0 {
			buf.WriteByte('\n')
		}
		fmt.Fprintf(&buf, "Package: %s\n", r.Name)
		fmt.Fprintf(&buf, "Version: %s\n", r.Version)
		if r.Yanked {
			// "yes"/"no" is DCF's canonical boolean. See R's
			// tools::.read_description() for how PACKAGES is parsed.
			buf.WriteString("Yanked: yes\n")
		}
	}
	return buf.Bytes()
}

// formatBinaryPackages includes a "Built:" field so R recognizes the
// tarball as a pre-built binary for the given R minor. The OS/arch
// portion is approximate (a single generic "x86_64-pc-linux-gnu")
// because R's own heuristic is loose here; we leave exact matching to
// v1.x once we care about cross-distro binaries.
func formatBinaryPackages(rows []indexRow, rMinor string) []byte {
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].Name != rows[b].Name {
			return rows[a].Name < rows[b].Name
		}
		return rows[a].Version < rows[b].Version
	})

	var buf bytes.Buffer
	for i, r := range rows {
		if i > 0 {
			buf.WriteByte('\n')
		}
		fmt.Fprintf(&buf, "Package: %s\n", r.Name)
		fmt.Fprintf(&buf, "Version: %s\n", r.Version)
		fmt.Fprintf(&buf, "Built: R %s.0; x86_64-pc-linux-gnu; 2026-01-01 00:00:00; unix\n", rMinor)
		if r.Yanked {
			buf.WriteString("Yanked: yes\n")
		}
	}
	return buf.Bytes()
}
