package api

import (
	"database/sql"
	"net/http"

	"github.com/schochastics/pakman/internal/auth"
)

// ChannelSummary is one row in the /api/v1/channels response. Flat
// shape matches design.md §7's general rule; the "package_count" stat
// is a COUNT aggregate, not a nested list.
type ChannelSummary struct {
	Name            string  `json:"name"`
	OverwritePolicy string  `json:"overwrite_policy"`
	Default         bool    `json:"default"`
	CreatedAt       string  `json:"created_at"`
	PackageCount    int64   `json:"package_count"`
	LatestPublishAt *string `json:"latest_publish_at,omitempty"`
}

// ListChannelsResponse wraps the slice so later fields (filters
// applied, generation time) don't require a breaking change.
type ListChannelsResponse struct {
	Channels []ChannelSummary `json:"channels"`
}

// handleListChannels serves GET /api/v1/channels.
//
// Admin-gated for v1. Channels are operational metadata; a read:<ch>
// caller doesn't need to know what other channels exist to do their
// work. If CI tooling starts wanting a programmatic channel list we
// can loosen this in Phase C.
func handleListChannels(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireScope(w, r, auth.ScopeAdmin) {
			return
		}

		rows, err := deps.DB.QueryContext(r.Context(), `
			SELECT c.name, c.overwrite_policy, c.is_default, c.created_at,
			       COUNT(p.id) AS package_count,
			       MAX(p.published_at) AS latest_publish_at
			FROM channels c
			LEFT JOIN packages p ON p.channel = c.name
			GROUP BY c.name, c.overwrite_policy, c.is_default, c.created_at
			ORDER BY c.name
		`)
		if err != nil {
			internalErr("list channels", err).write(w, r)
			return
		}
		defer func() { _ = rows.Close() }()

		out := []ChannelSummary{}
		for rows.Next() {
			var (
				name, policy, createdAt string
				isDefault               int
				count                   int64
				latest                  sql.NullString
			)
			if err := rows.Scan(&name, &policy, &isDefault, &createdAt, &count, &latest); err != nil {
				internalErr("scan channel", err).write(w, r)
				return
			}
			out = append(out, ChannelSummary{
				Name:            name,
				OverwritePolicy: policy,
				Default:         isDefault == 1,
				CreatedAt:       createdAt,
				PackageCount:    count,
				LatestPublishAt: nullToPtr(latest),
			})
		}
		if err := rows.Err(); err != nil {
			internalErr("iterate channels", err).write(w, r)
			return
		}

		writeJSON(w, r, http.StatusOK, ListChannelsResponse{Channels: out})
	}
}
