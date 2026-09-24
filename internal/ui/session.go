// Package ui serves the operator dashboard at /ui/ — a server-rendered
// HTML interface for inspecting channels, packages, events, cells, and
// storage. No JavaScript framework, no client build step; everything
// renders server-side from html/template.
//
// Auth is a server-side session: logging in with an admin token
// creates a ui_sessions row, and the cookie carries only a random
// session id (the DB stores its sha256). The bearer token never
// leaves the login form, logout deletes the row, and a session ends at
// its expiry or when its token is revoked.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/schochastics/packyard/internal/auth"
)

// sessionCookieName is the cookie that carries the session id. The
// name is deliberately distinct from anything else so an existing
// "auth" or "token" cookie from another service on the same host can't
// collide.
const sessionCookieName = "packyard_ui"

// defaultSessionTTL is how long an operator stays logged in before the
// UI prompts for a token again. Enforced server-side.
const defaultSessionTTL = 24 * time.Hour

func hashSessionID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// createSession stores a session for tokenID and returns the cookie
// value. Expired sessions are swept first, so the table stays small.
func (h *Handler) createSession(ctx context.Context, tokenID int64) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("session id: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	expires := now.Add(defaultSessionTTL)

	d := h.deps.DB.DB
	if _, err := d.ExecContext(ctx, `DELETE FROM ui_sessions WHERE expires_at < ?`,
		now.Format(time.RFC3339Nano)); err != nil {
		return "", time.Time{}, fmt.Errorf("sweep sessions: %w", err)
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO ui_sessions(id_sha256, token_id, expires_at) VALUES (?, ?, ?)`,
		hashSessionID(id), tokenID, expires.Format(time.RFC3339Nano)); err != nil {
		return "", time.Time{}, fmt.Errorf("store session: %w", err)
	}
	return id, expires, nil
}

// sessionIdentity resolves the request's session cookie to the token
// it was created for. Returns (id, true) on success; (zero, false) for
// a missing, unknown or expired session, a revoked token, and a token
// that lost the admin scope since login.
func (h *Handler) sessionIdentity(r *http.Request) (auth.Identity, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return auth.Identity{}, false
	}
	var (
		tokenID int64
		expires string
	)
	err = h.deps.DB.QueryRowContext(r.Context(),
		`SELECT token_id, expires_at FROM ui_sessions WHERE id_sha256 = ?`,
		hashSessionID(c.Value)).Scan(&tokenID, &expires)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("ui: session lookup failed", "err", err)
		}
		return auth.Identity{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, expires); err != nil || time.Now().After(t) {
		return auth.Identity{}, false
	}
	id, err := auth.LookupByID(r.Context(), h.deps.DB.DB, tokenID)
	if err != nil || !id.Scopes.Has(auth.ScopeAdmin) {
		return auth.Identity{}, false
	}
	return id, true
}

// deleteSession removes the request's session, if any.
func (h *Handler) deleteSession(r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return
	}
	if _, err := h.deps.DB.ExecContext(r.Context(),
		`DELETE FROM ui_sessions WHERE id_sha256 = ?`, hashSessionID(c.Value)); err != nil {
		slog.Warn("ui: session delete failed", "err", err)
	}
}
