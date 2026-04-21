package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// YankRequest is the JSON body for POST .../yank. Reason is optional
// but recommended — it lands in packages.yank_reason and in the event
// log where ops folks can see it later.
type YankRequest struct {
	Reason string `json:"reason,omitempty"`
}

// YankResponse is the success body.
type YankResponse struct {
	Channel string `json:"channel"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Yanked  bool   `json:"yanked"`
	Reason  string `json:"reason,omitempty"`
}

// handleYank marks a published version as yanked. Unlike delete, yank
// works on any channel (including immutable): the bytes stay addressable
// so running installs don't break, but the CRAN-protocol PACKAGES
// listing flags the row and new installs get the previous version.
func handleYank(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channel := r.PathValue("channel")
		name := r.PathValue("name")
		version := r.PathValue("version")

		if !packageNameRE.MatchString(name) || !versionRE.MatchString(version) {
			writeError(w, r, http.StatusBadRequest,
				CodeBadRequest, "invalid package name or version", "")
			return
		}
		if !requireScope(w, r, "yank:"+channel) {
			return
		}

		req, herr := decodeYankRequest(r.Body)
		if herr != nil {
			herr.write(w, r)
			return
		}

		id, _ := IdentityFromContext(r.Context())
		resp, herr := persistYank(r.Context(), deps.DB.DB, channel, name, version, req.Reason, id.Label)
		if herr != nil {
			herr.write(w, r)
			return
		}
		writeJSON(w, r, http.StatusOK, resp)
	}
}

// decodeYankRequest tolerates an empty body (→ zero YankRequest) so
// callers who don't want to provide a reason can just POST with no body.
// A non-empty body must be valid JSON and must not include unknown
// fields — typoed "reson" should fail loudly.
func decodeYankRequest(body io.Reader) (YankRequest, *httpError) {
	// Limit the body size. A yank request body should be tiny.
	buf, err := io.ReadAll(io.LimitReader(body, 64*1024+1))
	if err != nil {
		return YankRequest{}, &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "failed to read request body",
			hint:   err.Error(),
		}
	}
	if len(buf) > 64*1024 {
		return YankRequest{}, &httpError{
			status: http.StatusRequestEntityTooLarge,
			code:   CodePayloadTooLarge,
			msg:    "yank request body too large",
			hint:   "reason is a short string; trim it",
		}
	}
	var req YankRequest
	if len(buf) == 0 {
		return req, nil
	}
	dec := json.NewDecoder(bytesReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return YankRequest{}, &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "yank request is not valid JSON",
			hint:   err.Error(),
		}
	}
	return req, nil
}

func persistYank(ctx context.Context, db *sql.DB, channel, name, version, reason, actor string) (*YankResponse, *httpError) {
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, internalErr("begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM packages
		WHERE channel = ? AND name = ? AND version = ?
	`, channel, name, version).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &httpError{
			status: http.StatusNotFound,
			code:   CodeNotFound,
			msg:    fmt.Sprintf("%s@%s not found on channel %s", name, version, channel),
		}
	}
	if err != nil {
		return nil, internalErr("lookup package", err)
	}

	var reasonArg any
	if reason == "" {
		reasonArg = nil
	} else {
		reasonArg = reason
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE packages SET yanked = 1, yank_reason = ? WHERE id = ?
	`, reasonArg, id); err != nil {
		return nil, internalErr("mark yanked", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events(at, type, actor, channel, package, version, note)
		VALUES (?, 'yank', ?, ?, ?, ?, ?)
	`, now, nullIfEmpty(actor), channel, name, version, nullIfEmpty(reason)); err != nil {
		return nil, internalErr("append event", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, internalErr("commit", err)
	}
	return &YankResponse{
		Channel: channel, Name: name, Version: version,
		Yanked: true, Reason: reason,
	}, nil
}
