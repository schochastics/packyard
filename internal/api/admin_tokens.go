package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/schochastics/packyard/internal/auth"
)

// scopeRE matches an individual scope entry. Validates shape at the
// API boundary so bad scopes can never reach the DB. Kept permissive
// enough to accept the ones we ship (publish:dev, read:*, yank:test,
// admin) plus hyphens and underscores for future-compat.
var scopeRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*(:([a-z0-9_*-]+))?$`)

// CreateTokenRequest is the JSON body of POST /api/v1/admin/tokens.
type CreateTokenRequest struct {
	Label  string   `json:"label"`
	Scopes []string `json:"scopes"`
}

// CreateTokenResponse returns the new token EXACTLY ONCE. Subsequent
// GETs on the token list do NOT include the plaintext — this is the
// only chance the operator has to copy it.
type CreateTokenResponse struct {
	ID        int64    `json:"id"`
	Label     string   `json:"label"`
	Scopes    []string `json:"scopes"`
	CreatedAt string   `json:"created_at"`
	Token     string   `json:"token"`
}

// TokenSummary is what list/revoke return. No plaintext, ever.
type TokenSummary struct {
	ID         int64    `json:"id"`
	Label      string   `json:"label"`
	Scopes     []string `json:"scopes"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at,omitempty"`
	RevokedAt  *string  `json:"revoked_at,omitempty"`
}

// ListTokensResponse wraps the slice so we can add paging fields later
// without a breaking change.
type ListTokensResponse struct {
	Tokens []TokenSummary `json:"tokens"`
}

// RevokeTokenResponse is the body of DELETE .../{id}. 200 + body
// rather than 204 matches every other mutation in the API.
type RevokeTokenResponse struct {
	ID      int64 `json:"id"`
	Revoked bool  `json:"revoked"`
}

// handleCreateToken mints a new API token. The plaintext is returned
// in the response body ONCE — never again. Requires admin scope
// because tokens can be created with arbitrary privileges, including
// admin itself; that's a privilege escalation surface we don't expose
// to non-admins.
func handleCreateToken(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireScope(w, r, auth.ScopeAdmin) {
			return
		}

		req, herr := decodeCreateTokenRequest(r.Body)
		if herr != nil {
			herr.write(w, r)
			return
		}

		// Storage is a single scopes_csv column, so we join on our way
		// in and split on our way out. ScopeSet.CSV gives stable order.
		scopeSet := auth.ParseScopes(strings.Join(req.Scopes, ","))
		csv := scopeSet.CSV()

		plaintext, err := auth.GenerateToken()
		if err != nil {
			internalErr("generate token", err).write(w, r)
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)

		res, err := deps.DB.ExecContext(r.Context(), `
			INSERT INTO tokens(token_sha256, scopes_csv, label, created_at)
			VALUES (?, ?, ?, ?)
		`, auth.HashToken(plaintext), csv, req.Label, now)
		if err != nil {
			internalErr("insert token", err).write(w, r)
			return
		}
		id, err := res.LastInsertId()
		if err != nil {
			internalErr("last insert id", err).write(w, r)
			return
		}

		_, _ = deps.DB.ExecContext(r.Context(), `
			INSERT INTO events(at, type, actor, note)
			VALUES (?, 'token_create', ?, ?)
		`, now, labelFromContext(r.Context()), req.Label)

		if deps.Metrics != nil {
			deps.Metrics.TokenCreateTotal.Inc()
		}

		writeJSON(w, r, http.StatusCreated, CreateTokenResponse{
			ID:        id,
			Label:     req.Label,
			Scopes:    splitCSV(csv),
			CreatedAt: now,
			Token:     plaintext,
		})
	}
}

// handleListTokens returns every row in the tokens table. Plaintext is
// never part of the response.
func handleListTokens(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireScope(w, r, auth.ScopeAdmin) {
			return
		}
		rows, err := deps.DB.QueryContext(r.Context(), `
			SELECT id, label, scopes_csv, created_at, last_used_at, revoked_at
			FROM tokens
			ORDER BY id ASC
		`)
		if err != nil {
			internalErr("list tokens", err).write(w, r)
			return
		}
		defer func() { _ = rows.Close() }()

		out := []TokenSummary{}
		for rows.Next() {
			var (
				id                  int64
				label               sql.NullString
				csv, createdAt      string
				lastUsed, revokedAt sql.NullString
			)
			if err := rows.Scan(&id, &label, &csv, &createdAt, &lastUsed, &revokedAt); err != nil {
				internalErr("scan token", err).write(w, r)
				return
			}
			out = append(out, TokenSummary{
				ID:         id,
				Label:      label.String,
				Scopes:     splitCSV(csv),
				CreatedAt:  createdAt,
				LastUsedAt: nullToPtr(lastUsed),
				RevokedAt:  nullToPtr(revokedAt),
			})
		}
		if err := rows.Err(); err != nil {
			internalErr("iterate tokens", err).write(w, r)
			return
		}
		writeJSON(w, r, http.StatusOK, ListTokensResponse{Tokens: out})
	}
}

// handleRevokeToken stamps revoked_at on the given token. Idempotent:
// revoking an already-revoked token is a no-op and still returns 200.
// Unknown ids 404.
func handleRevokeToken(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireScope(w, r, auth.ScopeAdmin) {
			return
		}
		idStr := r.PathValue("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, r, http.StatusBadRequest,
				CodeBadRequest, "invalid token id", "path id must be a positive integer")
			return
		}

		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err := deps.DB.ExecContext(r.Context(), `
			UPDATE tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?
		`, now, id)
		if err != nil {
			internalErr("revoke token", err).write(w, r)
			return
		}
		n, err := res.RowsAffected()
		if err != nil {
			internalErr("rows affected", err).write(w, r)
			return
		}
		if n == 0 {
			writeError(w, r, http.StatusNotFound,
				CodeNotFound, fmt.Sprintf("token id %d not found", id), "")
			return
		}

		_, _ = deps.DB.ExecContext(r.Context(), `
			INSERT INTO events(at, type, actor, note)
			VALUES (?, 'token_revoke', ?, ?)
		`, now, labelFromContext(r.Context()), strconv.FormatInt(id, 10))

		if deps.Metrics != nil {
			deps.Metrics.TokenRevokeTotal.Inc()
		}

		writeJSON(w, r, http.StatusOK, RevokeTokenResponse{ID: id, Revoked: true})
	}
}

// decodeCreateTokenRequest reads + validates the JSON body.
func decodeCreateTokenRequest(body io.Reader) (CreateTokenRequest, *httpError) {
	buf, err := io.ReadAll(io.LimitReader(body, 64*1024+1))
	if err != nil {
		return CreateTokenRequest{}, &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "failed to read request body",
			hint:   err.Error(),
		}
	}
	if len(buf) > 64*1024 {
		return CreateTokenRequest{}, &httpError{
			status: http.StatusRequestEntityTooLarge,
			code:   CodePayloadTooLarge,
			msg:    "request body too large",
		}
	}

	var req CreateTokenRequest
	dec := json.NewDecoder(bytesReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return CreateTokenRequest{}, &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "body is not valid JSON",
			hint:   err.Error(),
		}
	}

	req.Label = strings.TrimSpace(req.Label)
	if req.Label == "" {
		return CreateTokenRequest{}, &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "label is required",
			hint:   "use a short human-readable name like 'ci-github-prod' so audit logs make sense",
		}
	}
	if len(req.Scopes) == 0 {
		return CreateTokenRequest{}, &httpError{
			status: http.StatusBadRequest,
			code:   CodeBadRequest,
			msg:    "scopes is required and must be non-empty",
			hint:   "e.g. [\"publish:dev\", \"read:*\"]",
		}
	}
	for i, s := range req.Scopes {
		s = strings.TrimSpace(s)
		req.Scopes[i] = s
		if !scopeRE.MatchString(s) {
			return CreateTokenRequest{}, &httpError{
				status: http.StatusBadRequest,
				code:   CodeBadRequest,
				msg:    fmt.Sprintf("invalid scope %q", s),
				hint:   "scopes are 'verb:target' or a bare keyword like 'admin'",
			}
		}
	}
	return req, nil
}

// labelFromContext pulls the acting token's label for audit events.
// Returns NULL (nil) if the caller's identity is somehow missing —
// the admin-scope check above makes that unreachable in practice but
// defense in depth keeps the column honest.
func labelFromContext(ctx context.Context) any {
	id, ok := IdentityFromContext(ctx)
	if !ok || id.Label == "" {
		return nil
	}
	return id.Label
}

// splitCSV is the inverse of ScopeSet.CSV — it is NOT ParseScopes
// because we want to preserve the exact stored order for display.
func splitCSV(csv string) []string {
	if csv == "" {
		return []string{}
	}
	parts := strings.Split(csv, ",")
	out := parts[:0]
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// nullToPtr converts a nullable SQL column into a *string so the JSON
// response shows `null` for absent values (consistent with the rest
// of our schemas where optional fields are pointer types).
func nullToPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}
