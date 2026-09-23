package api

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"

	"github.com/schochastics/packyard/internal/auth"
)

// defaults + caps for list pagination. Kept as consts so any future
// change ripples through every list endpoint consistently.
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// PackageSummary is one row in the /api/v1/packages response. This
// is the one endpoint that carries a nested array — the "binaries"
// list — per design.md §7's documented exception. Everything else
// stays flat.
type PackageSummary struct {
	ID           int64           `json:"id"`
	Channel      string          `json:"channel"`
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	SourceSHA256 string          `json:"source_sha256"`
	SourceSize   int64           `json:"source_size"`
	PublishedAt  string          `json:"published_at"`
	PublishedBy  *string         `json:"published_by,omitempty"`
	Yanked       bool            `json:"yanked"`
	YankReason   *string         `json:"yank_reason,omitempty"`
	Binaries     []BinarySummary `json:"binaries"`
}

// BinarySummary is one entry in PackageSummary.Binaries.
type BinarySummary struct {
	Cell       string `json:"cell"`
	SHA256     string `json:"binary_sha256"`
	Size       int64  `json:"size"`
	UploadedAt string `json:"uploaded_at"`
}

// ListPackagesResponse wraps the slice (same convention as every
// other list endpoint).
type ListPackagesResponse struct {
	Packages []PackageSummary `json:"packages"`
}

// handleListPackages serves GET /api/v1/packages with filters and
// limit/offset pagination.
//
// Filters (all optional, all exact match):
//   - channel  restrict to one channel
//   - package  restrict to one package name
//   - limit    page size, default 100, max 500
//   - offset   skip first N rows
//
// Returns an X-Total-Count header with the number of matching rows
// before paging. Admin-gated for v1 — see the channel handler for
// the scope rationale.
func handleListPackages(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireScope(w, r, auth.ScopeAdmin) {
			return
		}

		limit, offset, herr := parseLimitOffset(r)
		if herr != nil {
			herr.write(w, r)
			return
		}

		channel := r.URL.Query().Get("channel")
		pkg := r.URL.Query().Get("package")

		// COUNT(*) and the paginated SELECT use the same WHERE clause.
		// Build it once, substitute into both statements.
		where, args := buildPackageWhere(channel, pkg)

		var total int64
		if err := deps.DB.QueryRowContext(r.Context(),
			"SELECT COUNT(*) FROM packages "+where, args...,
		).Scan(&total); err != nil {
			internalErr("count packages", err).write(w, r)
			return
		}

		rowArgs := append([]any{}, args...)
		rowArgs = append(rowArgs, limit, offset)
		rows, err := deps.DB.QueryContext(r.Context(), `
			SELECT id, channel, name, version, source_sha256, source_size,
			       published_at, published_by, yanked, yank_reason
			FROM packages
		`+where+" ORDER BY id DESC LIMIT ? OFFSET ?", rowArgs...)
		if err != nil {
			internalErr("list packages", err).write(w, r)
			return
		}
		defer func() { _ = rows.Close() }()

		pkgs, pkgIDs, herr := scanPackageRows(rows)
		if herr != nil {
			herr.write(w, r)
			return
		}

		if len(pkgIDs) > 0 {
			binsByPkg, herr := loadBinariesForPackages(r.Context(), deps.DB.DB, pkgIDs)
			if herr != nil {
				herr.write(w, r)
				return
			}
			for i := range pkgs {
				if b, ok := binsByPkg[pkgs[i].ID]; ok {
					pkgs[i].Binaries = b
				} else {
					pkgs[i].Binaries = []BinarySummary{}
				}
			}
		}

		w.Header().Set("X-Total-Count", strconv.FormatInt(total, 10))
		writeJSON(w, r, http.StatusOK, ListPackagesResponse{Packages: pkgs})
	}
}

// parseLimitOffset reads ?limit= and ?offset= with defaults and caps.
// Rejects negative values; over-cap values are clamped (not errored)
// because a UI that always asks for 1000 shouldn't break on
// introduction of a new cap.
func parseLimitOffset(r *http.Request) (limit, offset int, herr *httpError) {
	limit = defaultListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			return 0, 0, &httpError{
				status: http.StatusBadRequest,
				code:   CodeBadRequest,
				msg:    "limit must be a non-negative integer",
			}
		}
		limit = v
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			return 0, 0, &httpError{
				status: http.StatusBadRequest,
				code:   CodeBadRequest,
				msg:    "offset must be a non-negative integer",
			}
		}
		offset = v
	}
	return limit, offset, nil
}

// buildPackageWhere assembles the shared WHERE clause for both the
// COUNT and the paginated SELECT. Returns a clause that always starts
// with "WHERE 1=1" so callers can safely append " ORDER BY ..." or
// concatenate additional predicates without re-checking emptiness.
func buildPackageWhere(channel, pkg string) (string, []any) {
	clause := "WHERE 1=1"
	args := []any{}
	if channel != "" {
		clause += " AND channel = ?"
		args = append(args, channel)
	}
	if pkg != "" {
		clause += " AND name = ?"
		args = append(args, pkg)
	}
	return clause, args
}

// scanPackageRows pulls the flat fields; binaries are loaded in a
// second query below. Returning the id slice lets the caller do a
// single IN(...) query for child rows.
func scanPackageRows(rows *sql.Rows) ([]PackageSummary, []int64, *httpError) {
	out := []PackageSummary{}
	ids := []int64{}
	for rows.Next() {
		var (
			p           PackageSummary
			yanked      int
			publishedBy sql.NullString
			yankReason  sql.NullString
		)
		if err := rows.Scan(
			&p.ID, &p.Channel, &p.Name, &p.Version,
			&p.SourceSHA256, &p.SourceSize, &p.PublishedAt,
			&publishedBy, &yanked, &yankReason,
		); err != nil {
			return nil, nil, internalErr("scan package", err)
		}
		p.Yanked = yanked == 1
		p.PublishedBy = nullToPtr(publishedBy)
		p.YankReason = nullToPtr(yankReason)
		p.Binaries = []BinarySummary{} // avoid "null" JSON for rows with no binaries
		out = append(out, p)
		ids = append(ids, p.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, internalErr("iterate packages", err)
	}
	return out, ids, nil
}

// loadBinariesForPackages runs one IN() query for all child rows to
// avoid the N+1 a naive per-package lookup would produce.
func loadBinariesForPackages(ctx context.Context, db *sql.DB, ids []int64) (map[int64][]BinarySummary, *httpError) {
	placeholders := make([]byte, 0, 2*len(ids)-1)
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}
	// The only dynamic portion is a sequence of '?' placeholders we built
	// above; no user input flows into the SQL text. Values go through
	// args as prepared parameters.
	q := "SELECT package_id, cell, binary_sha256, size, uploaded_at " + //nolint:gosec // placeholders only, not user data
		"FROM binaries WHERE package_id IN (" + string(placeholders) + ") ORDER BY cell"
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, internalErr("list binaries", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[int64][]BinarySummary{}
	for rows.Next() {
		var (
			pkgID int64
			b     BinarySummary
		)
		if err := rows.Scan(&pkgID, &b.Cell, &b.SHA256, &b.Size, &b.UploadedAt); err != nil {
			return nil, internalErr("scan binary", err)
		}
		out[pkgID] = append(out[pkgID], b)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("iterate binaries", err)
	}
	return out, nil
}
