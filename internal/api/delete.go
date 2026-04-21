package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/schochastics/pakman/internal/config"
)

// DeleteResponse is the success body of a hard-delete.
type DeleteResponse struct {
	Channel string `json:"channel"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Deleted bool   `json:"deleted"`
}

// handleDelete removes a published version on mutable channels only.
// Immutable channels explicitly refuse delete — the whole point of
// that policy is that downstream consumers can trust a version string.
// Operators who need to force-remove a prod package should either mark
// the channel mutable temporarily, or yank it (which leaves the bytes
// reachable for in-flight installs).
func handleDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.PathValue("channel")
		name := r.PathValue("name")
		version := r.PathValue("version")

		if !packageNameRE.MatchString(name) || !versionRE.MatchString(version) {
			writeError(w, r, http.StatusBadRequest,
				CodeBadRequest, "invalid package name or version", "")
			return
		}
		if !requireScope(w, r, "publish:"+channel) {
			return
		}

		id, _ := IdentityFromContext(r.Context())
		resp, herr := persistDelete(r.Context(), deps.DB.DB, channel, name, version, id.Label)
		if herr != nil {
			herr.write(w, r)
			return
		}
		if deps.Index != nil {
			deps.Index.InvalidateChannel(channel)
		}
		if deps.Metrics != nil {
			deps.Metrics.DeleteTotal.WithLabelValues(channel).Inc()
		}
		refreshCASBytes(r.Context(), deps)
		writeJSON(w, r, http.StatusOK, resp)
	}
}

func persistDelete(ctx context.Context, db *sql.DB, channel, name, version, actor string) (*DeleteResponse, *httpError) {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, internalErr("begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Check channel policy first so we reject immutable deletes before
	// we mutate anything.
	var policy string
	err = tx.QueryRowContext(ctx,
		`SELECT overwrite_policy FROM channels WHERE name = ?`, channel,
	).Scan(&policy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &httpError{
			status: http.StatusNotFound,
			code:   CodeNotFound,
			msg:    fmt.Sprintf("channel %q not found", channel),
		}
	}
	if err != nil {
		return nil, internalErr("lookup channel", err)
	}
	if policy == config.PolicyImmutable {
		return nil, &httpError{
			status: http.StatusConflict,
			code:   CodeChannelImmutable,
			msg:    fmt.Sprintf("channel %s is immutable; delete is not allowed", channel),
			hint:   "yank the version instead, or change the channel to mutable and retry",
		}
	}

	// Delete the package row. ON DELETE CASCADE on binaries.package_id
	// takes care of the child rows for us.
	res, err := tx.ExecContext(ctx, `
		DELETE FROM packages WHERE channel = ? AND name = ? AND version = ?
	`, channel, name, version)
	if err != nil {
		return nil, internalErr("delete package", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, internalErr("rows affected", err)
	}
	if n == 0 {
		return nil, &httpError{
			status: http.StatusNotFound,
			code:   CodeNotFound,
			msg:    fmt.Sprintf("%s@%s not found on channel %s", name, version, channel),
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events(at, type, actor, channel, package, version, note)
		VALUES (?, 'delete', ?, ?, ?, ?, NULL)
	`, now, nullIfEmpty(actor), channel, name, version); err != nil {
		return nil, internalErr("append event", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, internalErr("commit", err)
	}
	return &DeleteResponse{
		Channel: channel, Name: name, Version: version, Deleted: true,
	}, nil
}
