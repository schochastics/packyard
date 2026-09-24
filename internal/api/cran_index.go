package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/rds"
	"github.com/schochastics/packyard/internal/rpkg"
	"github.com/schochastics/packyard/internal/rversion"
	"github.com/schochastics/packyard/internal/upstream"
)

// How the CRAN-protocol index is built for local channels:
//
//   - PACKAGES lists exactly one stanza per package: its highest
//     non-yanked version by R's package_version ordering. Older and
//     yanked versions stay downloadable (src/contrib/<file> and
//     src/contrib/Archive/<pkg>/<file>) and are listed in
//     Meta/archive.rds, as on CRAN.
//   - Stanzas carry the standard repository fields from the package's
//     DESCRIPTION (Depends, Imports, LinkingTo, …; see
//     rpkg.IndexFields) so install.packages() resolves dependencies
//     within the repository.
//   - The /__linux__/{distro}/latest/ index lists the same packages;
//     an entry describes the binary for the client's cell when one
//     exists (adding its Built field) and the source tarball
//     otherwise.
//   - Proxy channels pass the upstream index through unchanged.

// Index generates and caches PACKAGES-file bodies served from the
// CRAN-protocol routes. Entries are keyed by (kind, channel[, cell])
// and invalidated either on write (publish/yank/delete) or by TTL.
type Index struct {
	db  *sql.DB
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]indexEntry
	// gens counts invalidations per channel. A reader notes the
	// generation before querying the DB and stores its result only if
	// no invalidation happened meanwhile; otherwise a publish that
	// commits during a rebuild would be followed by the old body being
	// cached for a full TTL.
	gens map[string]uint64
}

type indexEntry struct {
	body    []byte
	expires time.Time
	// stale marks a proxy index kept past its TTL because upstream
	// failed; it is served until the retry backoff expires.
	stale bool
}

// proxyRead describes how a proxy index body was obtained.
type proxyRead int

const (
	proxyFresh        proxyRead = iota // from upstream or within TTL
	proxyStale                         // cached body served during an upstream outage
	proxyStaleStarted                  // first stale serve of this outage
)

// proxyRetryBackoff is how long a stale proxy index is served before
// upstream is tried again. Retrying on every request would make each
// one wait out the upstream timeout while upstream hangs, longer than
// R waits for PACKAGES.
const proxyRetryBackoff = 30 * time.Second

// NewIndex constructs an Index. A 5-minute TTL bounds staleness from
// any code path that bypasses InvalidateChannel (direct SQL, a future
// bug, etc.); the happy path is cache-then-invalidate.
func NewIndex(db *sql.DB) *Index {
	return &Index{
		db:      db,
		ttl:     5 * time.Minute,
		entries: map[string]indexEntry{},
		gens:    map[string]uint64{},
	}
}

// Cache keys start with the channel name and a NUL separator, so
// InvalidateChannel can drop every view of one channel by prefix.
// Channel names can't contain NUL.
func channelKeyPrefix(channel string) string { return channel + "\x00" }
func sourceKey(channel string) string        { return channelKeyPrefix(channel) + "src" }

// linuxKey keys the /__linux__/ index for a cell; cell == "" is the
// all-source view served to R versions without a cell.
func linuxKey(channel, cell string) string { return channelKeyPrefix(channel) + "linux:" + cell }

// GetSource returns the source PACKAGES body for channel.
//
// For local channels, it builds (and caches) from the DB on a miss
// or stale entry. For proxy channels, it fetches from upstream
// (with the channel's configured TTL) and caches the response; on
// upstream failure it serves a stale cached body with stale=true so
// the caller can emit a "proxy_index_stale_served" event.
//
// meta may be nil — in that case the channel is treated as local,
// matching the pre-proxy behavior. fetcher is consulted only on the
// proxy branch.
func (i *Index) GetSource(ctx context.Context, channel string, meta *channelMeta, fetcher *upstream.Fetcher) (body []byte, read proxyRead, err error) {
	if meta.IsProxy() {
		return i.getSourceProxy(ctx, channel, meta.Upstream, fetcher)
	}
	body, err = i.getSourceLocal(ctx, channel)
	return body, proxyFresh, err
}

// GetLinux returns the /__linux__/ PACKAGES body for channel as seen
// by a client resolved to cell (nil: no binaries for the client's R
// version, so every entry is a source entry). See [Index.GetSource]
// for the proxy/stale semantics; a proxy channel serves the upstream
// binary index configured for the cell, else its source index.
func (i *Index) GetLinux(ctx context.Context, channel string, cell *config.Cell, arch string, meta *channelMeta, fetcher *upstream.Fetcher) (body []byte, read proxyRead, err error) {
	if meta.IsProxy() {
		if cell != nil {
			if base := meta.Upstream.BinaryURLs[cell.Name]; base != "" {
				return i.getProxyIndex(ctx, linuxKey(channel, cell.Name), base, rUserAgent(cell, arch),
					meta.Upstream.IndexTTL, meta.Upstream.Timeout, fetcher)
			}
		}
		return i.getSourceProxy(ctx, channel, meta.Upstream, fetcher)
	}
	cellName := ""
	if cell != nil {
		cellName = cell.Name
	}
	key := linuxKey(channel, cellName)
	if body, ok := i.lookup(key); ok {
		return body, proxyFresh, nil
	}
	gen := i.generation(channel)
	rows, err := i.latestRows(ctx, channel, cellName)
	if err != nil {
		return nil, proxyFresh, err
	}
	body = formatPackages(rows, cell, arch)
	i.storeIfCurrent(key, channel, gen, body)
	return body, proxyFresh, nil
}

func (i *Index) getSourceLocal(ctx context.Context, channel string) ([]byte, error) {
	key := sourceKey(channel)
	if body, ok := i.lookup(key); ok {
		return body, nil
	}
	gen := i.generation(channel)
	body, err := i.buildSource(ctx, channel)
	if err != nil {
		return nil, err
	}
	i.storeIfCurrent(key, channel, gen, body)
	return body, nil
}

func (i *Index) getSourceProxy(ctx context.Context, channel string, up config.UpstreamConfig, fetcher *upstream.Fetcher) ([]byte, proxyRead, error) {
	return i.getProxyIndex(ctx, sourceKey(channel), up.SourceURL, "", up.IndexTTL, up.Timeout, fetcher)
}

// getProxyIndex is the shared "fetch from upstream with TTL +
// stale-while-error" core used by both source and binary proxy paths.
// userAgent is forwarded upstream ("" for the default).
//
// When a refresh fails and a previous body exists, that body is served
// and kept for proxyRetryBackoff, so requests during an outage get the
// stale index immediately instead of each waiting on upstream.
func (i *Index) getProxyIndex(ctx context.Context, key, baseURL, userAgent string, ttl, timeout time.Duration, fetcher *upstream.Fetcher) ([]byte, proxyRead, error) {
	if fetcher == nil {
		return nil, proxyFresh, fmt.Errorf("proxy channel %q needs a fetcher; none configured", key)
	}
	prev, hasPrev := i.peek(key)
	if hasPrev && time.Now().Before(prev.expires) {
		if prev.stale {
			return prev.body, proxyStale, nil
		}
		return prev.body, proxyFresh, nil
	}
	body, err := fetcher.FetchIndex(ctx, baseURL, userAgent, timeout)
	if err != nil {
		if hasPrev {
			read := proxyStale
			if !prev.stale {
				read = proxyStaleStarted
			}
			i.mu.Lock()
			i.entries[key] = indexEntry{body: prev.body, expires: time.Now().Add(proxyRetryBackoff), stale: true}
			i.mu.Unlock()
			return prev.body, read, nil
		}
		return nil, proxyFresh, err
	}
	i.storeWithTTL(key, body, ttl)
	return body, proxyFresh, nil
}

// rUserAgent is the User-Agent R itself would send for cell, e.g.
// "R (4.4.0 x86_64-pc-linux-gnu x86_64 linux-gnu)". Upstreams such as
// Posit Package Manager pick binary or source from it.
func rUserAgent(cell *config.Cell, arch string) string {
	triple := archTriples[arch]
	if triple == "" {
		triple = archTriples["amd64"]
	}
	cpu, _, _ := strings.Cut(triple, "-")
	return fmt.Sprintf("R (%s.0 %s %s linux-gnu)", cell.RMinor, triple, cpu)
}

// InvalidateChannel drops every cached view of channel: source
// PACKAGES, each /__linux__/ variant and Meta/archive.rds. Called by
// publish, yank, delete and attach on success so the next read
// reflects the new state without waiting for TTL.
func (i *Index) InvalidateChannel(channel string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.gens[channel]++
	prefix := channelKeyPrefix(channel)
	for k := range i.entries {
		if strings.HasPrefix(k, prefix) {
			delete(i.entries, k)
		}
	}
}

// generation returns channel's invalidation count; see Index.gens.
func (i *Index) generation(channel string) uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.gens[channel]
}

// storeIfCurrent caches a locally built body unless channel was
// invalidated since gen was read.
func (i *Index) storeIfCurrent(key, channel string, gen uint64, body []byte) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.gens[channel] != gen {
		return
	}
	i.entries[key] = indexEntry{body: body, expires: time.Now().Add(i.ttl)}
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

// peek returns the entry for key without evicting on expiry. The
// proxy path needs this for stale-while-error: a stale entry is the
// fallback when an upstream refresh fails.
func (i *Index) peek(key string) (indexEntry, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.entries[key]
	return e, ok
}

func (i *Index) storeWithTTL(key string, body []byte, ttl time.Duration) {
	if ttl <= 0 {
		ttl = i.ttl
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.entries[key] = indexEntry{
		body:    body,
		expires: time.Now().Add(ttl),
	}
}

// indexRow is one package version as the index sees it.
type indexRow struct {
	Name    string
	Version string
	Yanked  bool
	// Fields are the DESCRIPTION index fields (rpkg.IndexFields).
	Fields map[string]string
	// HasBinary / Built describe the binary for the requested cell,
	// when one was asked for and exists.
	HasBinary bool
	Built     string
}

// buildSource formats the source PACKAGES body for a local channel.
func (i *Index) buildSource(ctx context.Context, channel string) ([]byte, error) {
	rows, err := i.latestRows(ctx, channel, "")
	if err != nil {
		return nil, err
	}
	return formatPackages(rows, nil, ""), nil
}

// channelRows returns every version of every package in channel,
// joined to the binary for cell ("" joins nothing).
func (i *Index) channelRows(ctx context.Context, channel, cell string) ([]indexRow, error) {
	rows, err := i.db.QueryContext(ctx, `
		SELECT p.name, p.version, p.yanked, p.index_fields,
		       b.id IS NOT NULL, b.built
		FROM packages p
		LEFT JOIN binaries b ON b.package_id = p.id AND b.cell = ?
		WHERE p.channel = ?
	`, cell, channel)
	if err != nil {
		return nil, fmt.Errorf("index: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []indexRow{}
	for rows.Next() {
		var (
			r      indexRow
			yanked int
			fields sql.NullString
			built  sql.NullString
		)
		if err := rows.Scan(&r.Name, &r.Version, &yanked, &fields, &r.HasBinary, &built); err != nil {
			return nil, fmt.Errorf("index: scan: %w", err)
		}
		r.Yanked = yanked == 1
		r.Built = built.String
		if fields.Valid && fields.String != "" {
			// A malformed value (hand-edited DB) degrades to a
			// Package/Version-only stanza rather than failing the
			// whole index.
			_ = json.Unmarshal([]byte(fields.String), &r.Fields)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: iterate: %w", err)
	}
	return out, nil
}

// latestRows keeps, per package, the highest non-yanked version.
// Packages whose every version is yanked drop out of the index.
func (i *Index) latestRows(ctx context.Context, channel, cell string) ([]indexRow, error) {
	all, err := i.channelRows(ctx, channel, cell)
	if err != nil {
		return nil, err
	}
	best := map[string]indexRow{}
	for _, r := range all {
		if r.Yanked {
			continue
		}
		if cur, ok := best[r.Name]; !ok || rversion.Compare(r.Version, cur.Version) > 0 {
			best[r.Name] = r
		}
	}
	out := make([]indexRow, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// archTriples maps matrix.yaml arch values to the platform triple R
// writes into a binary's Built field.
var archTriples = map[string]string{
	"amd64": "x86_64-pc-linux-gnu",
	"arm64": "aarch64-unknown-linux-gnu",
}

// formatPackages writes one DCF stanza per row. cell is the cell the
// client resolved to (nil for the plain source index); rows with a
// binary for it get a Built field so R treats the tarball as built.
func formatPackages(rows []indexRow, cell *config.Cell, arch string) []byte {
	var buf bytes.Buffer
	for n, r := range rows {
		if n > 0 {
			buf.WriteByte('\n')
		}
		fmt.Fprintf(&buf, "Package: %s\n", r.Name)
		fmt.Fprintf(&buf, "Version: %s\n", r.Version)
		for _, f := range rpkg.IndexFields {
			if v := r.Fields[f]; v != "" {
				fmt.Fprintf(&buf, "%s: %s\n", f, v)
			}
		}
		if cell != nil && r.HasBinary {
			built := r.Built
			if built == "" {
				// Binary uploaded without a readable DESCRIPTION: say
				// what we know — the R minor it was published for.
				built = fmt.Sprintf("R %s.0; %s; ; unix", cell.RMinor, archTriples[arch])
			}
			fmt.Fprintf(&buf, "Built: %s\n", built)
		}
	}
	return buf.Bytes()
}

func archiveKey(channel string) string { return channelKeyPrefix(channel) + "archive" }

// GetArchive returns the gzipped Meta/archive.rds for a local channel:
// every stored version not listed in PACKAGES (older and yanked ones),
// in CRAN's file.info() layout.
func (i *Index) GetArchive(ctx context.Context, channel string) ([]byte, error) {
	key := archiveKey(channel)
	if body, ok := i.lookup(key); ok {
		return body, nil
	}
	gen := i.generation(channel)
	body, err := i.buildArchive(ctx, channel)
	if err != nil {
		return nil, err
	}
	i.storeIfCurrent(key, channel, gen, body)
	return body, nil
}

func (i *Index) buildArchive(ctx context.Context, channel string) ([]byte, error) {
	latest, err := i.latestRows(ctx, channel, "")
	if err != nil {
		return nil, err
	}
	current := make(map[string]string, len(latest))
	for _, r := range latest {
		current[r.Name] = r.Version
	}

	rows, err := i.db.QueryContext(ctx, `
		SELECT name, version, source_size, published_at
		FROM packages WHERE channel = ?
	`, channel)
	if err != nil {
		return nil, fmt.Errorf("archive: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type archived struct {
		version string
		file    rds.ArchiveFile
	}
	byName := map[string][]archived{}
	for rows.Next() {
		var (
			name, version, published string
			size                     int64
		)
		if err := rows.Scan(&name, &version, &size, &published); err != nil {
			return nil, fmt.Errorf("archive: scan: %w", err)
		}
		if current[name] == version {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, published)
		byName[name] = append(byName[name], archived{version, rds.ArchiveFile{
			Path: name + "/" + name + "_" + version + ".tar.gz",
			Size: size,
			Time: at,
		}})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("archive: iterate: %w", err)
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names) // byte order == R's C-locale order
	pkgs := make([]rds.ArchivePackage, len(names))
	for k, n := range names {
		vs := byName[n]
		sort.Slice(vs, func(a, b int) bool { return rversion.Compare(vs[a].version, vs[b].version) < 0 })
		files := make([]rds.ArchiveFile, len(vs))
		for j, v := range vs {
			files[j] = v.file
		}
		pkgs[k] = rds.ArchivePackage{Name: n, Files: files}
	}

	var buf bytes.Buffer
	if err := rds.WriteGzip(&buf, rds.Archive(pkgs)); err != nil {
		return nil, fmt.Errorf("archive: serialize: %w", err)
	}
	return buf.Bytes(), nil
}
