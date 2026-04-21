package ui

import (
	"context"
	"database/sql"
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
