package api

import (
	"database/sql"
	"net/http"
	"strconv"

	"github.com/schochastics/packyard/internal/auth"
)

// EventSummary is one row of /api/v1/events. Channel / package /
// version / actor / note are all nullable in the DB because some
// event types (e.g. token_create) don't have a package or channel,
// so they come back as omitempty pointer fields.
type EventSummary struct {
	ID      int64   `json:"id"`
	At      string  `json:"at"`
	Type    string  `json:"type"`
	Actor   *string `json:"actor,omitempty"`
	Channel *string `json:"channel,omitempty"`
	Package *string `json:"package,omitempty"`
	Version *string `json:"version,omitempty"`
	Note    *string `json:"note,omitempty"`
}

// ListEventsResponse wraps the slice.
type ListEventsResponse struct {
	Events []EventSummary `json:"events"`
}

// handleListEvents serves GET /api/v1/events.
//
// Cursor pagination via ?since_id=: the server returns events whose id
// is STRICTLY GREATER than since_id, in ascending id order. Cursors
// beat limit/offset here because new events stream in constantly —
// an offset-based feed would double-count rows that land between
// page N and page N+1. Clients poll by remembering the max id they
// saw and passing it back as since_id on the next call.
//
// Optional filters: channel, package, type. All exact-match.
//
// limit defaults to 100, caps at 500. X-Total-Count is still set
// (total matching rows irrespective of since_id) so UIs can show
// "N events total, streaming new ones from id X".
//
// Admin-gated — the event log is an audit trail.
func handleListEvents(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireScope(w, r, auth.ScopeAdmin) {
			return
		}

		limit, _, herr := parseLimitOffset(r)
		if herr != nil {
			herr.write(w, r)
			return
		}

		sinceID, herr := parseSinceID(r)
		if herr != nil {
			herr.write(w, r)
			return
		}

		where, args := buildEventWhere(r, sinceID)
		totalWhere, totalArgs := buildEventWhere(r, 0)

		var total int64
		if err := deps.DB.QueryRowContext(r.Context(),
			"SELECT COUNT(*) FROM events "+totalWhere, totalArgs...,
		).Scan(&total); err != nil {
			internalErr("count events", err).write(w, r)
			return
		}

		rowArgs := append([]any{}, args...)
		rowArgs = append(rowArgs, limit)
		rows, err := deps.DB.QueryContext(r.Context(), `
			SELECT id, at, type, actor, channel, package, version, note
			FROM events
		`+where+" ORDER BY id ASC LIMIT ?", rowArgs...)
		if err != nil {
			internalErr("list events", err).write(w, r)
			return
		}
		defer func() { _ = rows.Close() }()

		out := []EventSummary{}
		for rows.Next() {
			var (
				e                                  EventSummary
				actor, channel, pkg, version, note sql.NullString
			)
			if err := rows.Scan(&e.ID, &e.At, &e.Type,
				&actor, &channel, &pkg, &version, &note,
			); err != nil {
				internalErr("scan event", err).write(w, r)
				return
			}
			e.Actor = nullToPtr(actor)
			e.Channel = nullToPtr(channel)
			e.Package = nullToPtr(pkg)
			e.Version = nullToPtr(version)
			e.Note = nullToPtr(note)
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			internalErr("iterate events", err).write(w, r)
			return
		}

		w.Header().Set("X-Total-Count", strconv.FormatInt(total, 10))
		writeJSON(w, r, http.StatusOK, ListEventsResponse{Events: out})
	}
}

// parseSinceID reads ?since_id= with a default of 0 (no lower bound).
// Rejects negative values and non-integers with 400.
func parseSinceID(r *http.Request) (int64, *httpError) {
	raw := r.URL.Query().Get("since_id")
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "since_id must be a non-negative integer",
		}
	}
	return v, nil
}

// buildEventWhere assembles the shared WHERE clause. since_id > 0
// adds the cursor predicate; 0 means "no cursor" (used by the
// COUNT(*) so X-Total-Count reflects the full match set).
func buildEventWhere(r *http.Request, sinceID int64) (string, []any) {
	clause := "WHERE 1=1"
	args := []any{}
	if sinceID > 0 {
		clause += " AND id > ?"
		args = append(args, sinceID)
	}
	if ch := r.URL.Query().Get("channel"); ch != "" {
		clause += " AND channel = ?"
		args = append(args, ch)
	}
	if pkg := r.URL.Query().Get("package"); pkg != "" {
		clause += " AND package = ?"
		args = append(args, pkg)
	}
	if typ := r.URL.Query().Get("type"); typ != "" {
		clause += " AND type = ?"
		args = append(args, typ)
	}
	return clause, args
}
