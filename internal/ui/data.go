package ui

import (
	"context"
	"database/sql"

	"github.com/schochastics/pakman/internal/config"
)

// channelCard is one card on the dashboard. Mirrors the shape of
// api.ChannelSummary but lives here to keep the UI free of JSON-tag
// concerns.
type channelCard struct {
	Name            string
	OverwritePolicy string
	IsDefault       bool
	PackageCount    int64
	LatestPublishAt string // empty when the channel has no packages yet
}

// eventRow is one entry in the recent-events feed. All optional fields
// are empty strings when the underlying column was NULL, so templates
// can use {{if}} without importing a "null" type.
type eventRow struct {
	ID      int64
	At      string
	Type    string
	Actor   string
	Channel string
	Package string
	Version string
	Note    string
}

// dashboardData is everything the /ui/ template consumes. Every list is
// pre-sorted server-side so the template is dumb.
type dashboardData struct {
	Totals       dashboardTotals
	Channels     []channelCard
	RecentEvents []eventRow
	RecentLimit  int // how many events we asked for; used to caption the list
}

type dashboardTotals struct {
	Channels int64
	Packages int64
	Events   int64
}

// loadDashboardData runs the three read queries the overview page
// needs. They're issued sequentially rather than in parallel — SQLite
// serializes writes but readers are already cheap, and keeping this
// single-threaded avoids fanning out goroutines for a page that
// renders in under a millisecond.
func loadDashboardData(ctx context.Context, d *sql.DB, eventLimit int) (*dashboardData, error) {
	if eventLimit <= 0 {
		eventLimit = 20
	}

	out := &dashboardData{RecentLimit: eventLimit}

	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels`).Scan(&out.Totals.Channels); err != nil {
		return nil, err
	}
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM packages`).Scan(&out.Totals.Packages); err != nil {
		return nil, err
	}
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&out.Totals.Events); err != nil {
		return nil, err
	}

	channels, err := loadChannelCards(ctx, d)
	if err != nil {
		return nil, err
	}
	out.Channels = channels

	events, err := loadRecentEvents(ctx, d, eventLimit)
	if err != nil {
		return nil, err
	}
	out.RecentEvents = events

	return out, nil
}

func loadChannelCards(ctx context.Context, d *sql.DB) ([]channelCard, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT c.name, c.overwrite_policy, c.is_default,
		       COUNT(p.id) AS package_count,
		       COALESCE(MAX(p.published_at), '') AS latest
		FROM channels c
		LEFT JOIN packages p ON p.channel = c.name
		GROUP BY c.name, c.overwrite_policy, c.is_default
		ORDER BY c.is_default DESC, c.name
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []channelCard{}
	for rows.Next() {
		var (
			c         channelCard
			isDefault int
		)
		if err := rows.Scan(&c.Name, &c.OverwritePolicy, &isDefault, &c.PackageCount, &c.LatestPublishAt); err != nil {
			return nil, err
		}
		c.IsDefault = isDefault == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// channelDetailData backs /ui/channels/{name}. Meta is the channel row
// itself; Packages is one row per (name, version) with a precomputed
// binary count so the template doesn't need another query per package.
type channelDetailData struct {
	Meta     channelMeta
	Packages []packageRow
}

type channelMeta struct {
	Name            string
	OverwritePolicy string
	IsDefault       bool
	CreatedAt       string
	PackageCount    int64
	TotalSourceSize int64
}

type packageRow struct {
	ID           int64
	Name         string
	Version      string
	SourceSHA256 string
	SourceSize   int64
	PublishedAt  string
	PublishedBy  string
	Yanked       bool
	YankReason   string
	BinaryCount  int64
}

// loadChannelDetail returns ErrNoRows when the channel doesn't exist,
// letting the handler render a 404.
func loadChannelDetail(ctx context.Context, d *sql.DB, name string) (*channelDetailData, error) {
	meta := channelMeta{Name: name}
	var isDefault int
	err := d.QueryRowContext(ctx, `
		SELECT overwrite_policy, is_default, created_at
		FROM channels WHERE name = ?
	`, name).Scan(&meta.OverwritePolicy, &isDefault, &meta.CreatedAt)
	if err != nil {
		return nil, err
	}
	meta.IsDefault = isDefault == 1

	// Totals alongside the detail rows: kept separate so a channel with
	// thousands of packages doesn't pay to recount per row.
	if err := d.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(source_size), 0)
		FROM packages WHERE channel = ?
	`, name).Scan(&meta.PackageCount, &meta.TotalSourceSize); err != nil {
		return nil, err
	}

	rows, err := d.QueryContext(ctx, `
		SELECT p.id, p.name, p.version, p.source_sha256, p.source_size,
		       p.published_at,
		       COALESCE(p.published_by, ''),
		       p.yanked,
		       COALESCE(p.yank_reason, ''),
		       (SELECT COUNT(*) FROM binaries b WHERE b.package_id = p.id) AS binary_count
		FROM packages p
		WHERE p.channel = ?
		ORDER BY p.name, p.published_at DESC
	`, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	pkgs := []packageRow{}
	for rows.Next() {
		var (
			p      packageRow
			yanked int
		)
		if err := rows.Scan(&p.ID, &p.Name, &p.Version, &p.SourceSHA256, &p.SourceSize,
			&p.PublishedAt, &p.PublishedBy, &yanked, &p.YankReason, &p.BinaryCount); err != nil {
			return nil, err
		}
		p.Yanked = yanked == 1
		pkgs = append(pkgs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &channelDetailData{Meta: meta, Packages: pkgs}, nil
}

// eventsPageData backs /ui/events. page is 1-indexed; total is the
// full match count so the template can render "showing X of Y".
// HasPrev / HasNext are precomputed for convenience.
type eventsPageData struct {
	Rows       []eventRow
	Page       int
	PageSize   int
	Total      int64
	HasPrev    bool
	HasNext    bool
	Filter     eventFilter
	Channels   []string // for the channel <select>
	EventTypes []string // for the type <select>
}

type eventFilter struct {
	Channel string
	Type    string
	Package string
}

// loadEventsPage runs the paginated audit-log query used by /ui/events.
// Offset pagination is appropriate here — an operator clicking through
// a few hundred rows isn't going to race new events landing faster
// than they can scroll.
func loadEventsPage(ctx context.Context, d *sql.DB, page, pageSize int, f eventFilter) (*eventsPageData, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 500 {
		pageSize = 500
	}

	where := "WHERE 1=1"
	args := []any{}
	if f.Channel != "" {
		where += " AND channel = ?"
		args = append(args, f.Channel)
	}
	if f.Type != "" {
		where += " AND type = ?"
		args = append(args, f.Type)
	}
	if f.Package != "" {
		where += " AND package = ?"
		args = append(args, f.Package)
	}

	var total int64
	if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM events "+where, args...).Scan(&total); err != nil {
		return nil, err
	}

	offset := (page - 1) * pageSize
	rowArgs := append([]any{}, args...)
	rowArgs = append(rowArgs, pageSize, offset)

	//nolint:gosec // `where` is a fixed template plus "? AND ..." fragments; user values go in args.
	rows, err := d.QueryContext(ctx, `
		SELECT id, at, type,
		       COALESCE(actor,   ''),
		       COALESCE(channel, ''),
		       COALESCE(package, ''),
		       COALESCE(version, ''),
		       COALESCE(note,    '')
		FROM events
	`+where+" ORDER BY id DESC LIMIT ? OFFSET ?", rowArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []eventRow{}
	for rows.Next() {
		var e eventRow
		if err := rows.Scan(&e.ID, &e.At, &e.Type,
			&e.Actor, &e.Channel, &e.Package, &e.Version, &e.Note); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	channels, err := listDistinct(ctx, d, "SELECT name FROM channels ORDER BY name")
	if err != nil {
		return nil, err
	}
	// Event types come from the event log itself rather than an enum —
	// new types (eg. token_revoke, overwrite) get picked up
	// automatically as they start appearing.
	types, err := listDistinct(ctx, d, "SELECT DISTINCT type FROM events ORDER BY type")
	if err != nil {
		return nil, err
	}

	return &eventsPageData{
		Rows:       out,
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		HasPrev:    page > 1,
		HasNext:    int64(page*pageSize) < total,
		Filter:     f,
		Channels:   channels,
		EventTypes: types,
	}, nil
}

// cellsPageData backs /ui/cells. Rows is one entry per declared cell;
// each row carries the cell's matrix metadata plus aggregate coverage
// numbers (how many binaries, across how many distinct packages).
type cellsPageData struct {
	Rows        []cellCoverageRow
	TotalPkgs   int64 // packages rowcount — cells with this BinariesFor value are 100% covered
	DeclaredSet bool  // true when Matrix was non-nil; false means cells list shows placeholder
}

type cellCoverageRow struct {
	Name         string
	OS           string
	OSVersion    string
	Arch         string
	RMinor       string
	BinaryCount  int64 // total binaries uploaded for this cell
	DistinctPkgs int64 // distinct (channel,name,version) tuples covered
	SizeBytes    int64 // sum of binaries.size for this cell
}

func loadCellsPage(ctx context.Context, d *sql.DB, matrix *config.MatrixConfig) (*cellsPageData, error) {
	out := &cellsPageData{DeclaredSet: matrix != nil}

	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM packages`).Scan(&out.TotalPkgs); err != nil {
		return nil, err
	}

	// Pull aggregates once for every cell present in the binaries table,
	// keyed by cell name. Cells declared in matrix.yaml but never
	// uploaded to show up as zeros.
	agg, err := aggregateByCell(ctx, d)
	if err != nil {
		return nil, err
	}

	if matrix == nil {
		return out, nil
	}
	for _, c := range matrix.Cells {
		row := cellCoverageRow{
			Name: c.Name, OS: c.OS, OSVersion: c.OSVersion, Arch: c.Arch, RMinor: c.RMinor,
		}
		if a, ok := agg[c.Name]; ok {
			row.BinaryCount = a.BinaryCount
			row.DistinctPkgs = a.DistinctPkgs
			row.SizeBytes = a.SizeBytes
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

type cellAggregate struct {
	BinaryCount, DistinctPkgs, SizeBytes int64
}

func aggregateByCell(ctx context.Context, d *sql.DB) (map[string]cellAggregate, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT cell,
		       COUNT(*) AS binary_count,
		       COUNT(DISTINCT package_id) AS distinct_pkgs,
		       COALESCE(SUM(size), 0) AS size_bytes
		FROM binaries
		GROUP BY cell
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]cellAggregate{}
	for rows.Next() {
		var (
			cell string
			a    cellAggregate
		)
		if err := rows.Scan(&cell, &a.BinaryCount, &a.DistinctPkgs, &a.SizeBytes); err != nil {
			return nil, err
		}
		out[cell] = a
	}
	return out, rows.Err()
}

// storagePageData backs /ui/storage. Reports logical bytes per the DB —
// physical CAS on-disk size is what admin gc will expose in B7.
type storagePageData struct {
	SourceBytes int64
	BinaryBytes int64
	TotalBytes  int64 // Source + Binary
	SourceCount int64
	BinaryCount int64
	ByChannel   []channelStorageRow
	TopSource   []storageItem
	TopBinary   []storageItem
}

type channelStorageRow struct {
	Channel     string
	PkgCount    int64
	SourceBytes int64
	BinaryBytes int64
}

type storageItem struct {
	Channel string
	Name    string
	Version string
	Cell    string // empty for source tarballs
	Size    int64
}

func loadStoragePage(ctx context.Context, d *sql.DB) (*storagePageData, error) {
	out := &storagePageData{}

	if err := d.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(source_size), 0) FROM packages
	`).Scan(&out.SourceCount, &out.SourceBytes); err != nil {
		return nil, err
	}
	if err := d.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size), 0) FROM binaries
	`).Scan(&out.BinaryCount, &out.BinaryBytes); err != nil {
		return nil, err
	}
	out.TotalBytes = out.SourceBytes + out.BinaryBytes

	chRows, err := d.QueryContext(ctx, `
		SELECT p.channel,
		       COUNT(*) AS pkgs,
		       COALESCE(SUM(p.source_size), 0) AS src,
		       COALESCE(SUM((SELECT COALESCE(SUM(size), 0) FROM binaries b WHERE b.package_id = p.id)), 0) AS bins
		FROM packages p
		GROUP BY p.channel
		ORDER BY (src + bins) DESC
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = chRows.Close() }()
	for chRows.Next() {
		var r channelStorageRow
		if err := chRows.Scan(&r.Channel, &r.PkgCount, &r.SourceBytes, &r.BinaryBytes); err != nil {
			return nil, err
		}
		out.ByChannel = append(out.ByChannel, r)
	}
	if err := chRows.Err(); err != nil {
		return nil, err
	}

	topSrc, err := topItems(ctx, d, `
		SELECT channel, name, version, '' AS cell, source_size AS size
		FROM packages ORDER BY source_size DESC LIMIT 10
	`)
	if err != nil {
		return nil, err
	}
	out.TopSource = topSrc

	topBin, err := topItems(ctx, d, `
		SELECT p.channel, p.name, p.version, b.cell, b.size
		FROM binaries b JOIN packages p ON p.id = b.package_id
		ORDER BY b.size DESC LIMIT 10
	`)
	if err != nil {
		return nil, err
	}
	out.TopBinary = topBin

	return out, nil
}

func topItems(ctx context.Context, d *sql.DB, query string) ([]storageItem, error) {
	rows, err := d.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []storageItem{}
	for rows.Next() {
		var it storageItem
		if err := rows.Scan(&it.Channel, &it.Name, &it.Version, &it.Cell, &it.Size); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func listDistinct(ctx context.Context, d *sql.DB, query string) ([]string, error) {
	rows, err := d.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// loadRecentEvents returns the N newest events in reverse-chronological
// order (newest first). Uses id DESC rather than at DESC since id is
// AUTOINCREMENT and therefore monotonic — at can collide at the
// millisecond boundary on fast publish bursts.
func loadRecentEvents(ctx context.Context, d *sql.DB, limit int) ([]eventRow, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT id, at, type,
		       COALESCE(actor,   ''),
		       COALESCE(channel, ''),
		       COALESCE(package, ''),
		       COALESCE(version, ''),
		       COALESCE(note,    '')
		FROM events
		ORDER BY id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []eventRow{}
	for rows.Next() {
		var e eventRow
		if err := rows.Scan(&e.ID, &e.At, &e.Type,
			&e.Actor, &e.Channel, &e.Package, &e.Version, &e.Note); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
