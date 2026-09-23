package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/schochastics/packyard/internal/auth"
	"github.com/schochastics/packyard/internal/cas"
	"github.com/schochastics/packyard/internal/db"
)

func newAuthTestDeps(t *testing.T) Deps {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	database, err := db.Open(ctx, filepath.Join(dir, "packyard.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	store, err := cas.New(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	return Deps{DB: database, CAS: store}
}

func seedTokenRow(t *testing.T, db *sql.DB, label, scopes string, revoked bool) string {
	t.Helper()
	plaintext, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	var revAt sql.NullString
	if revoked {
		revAt = sql.NullString{String: "2020-01-01T00:00:00Z", Valid: true}
	}
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO tokens(token_sha256, scopes_csv, label, revoked_at)
		VALUES (?, ?, ?, ?)
	`, auth.HashToken(plaintext), scopes, label, revAt)
	if err != nil {
		t.Fatalf("seed token: %v", err)
	}
	return plaintext
}

// probeHandler returns 200 if the caller holds required, otherwise
// writes the standard 401/403 envelope via requireScope.
func probeHandler(required string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireScope(w, r, required) {
			return
		}
		id, _ := IdentityFromContext(r.Context())
		writeJSON(w, r, http.StatusOK, map[string]any{"label": id.Label})
	}
}

// buildProbeHandler wraps probeHandler with the auth middleware (only
// auth — we don't need request-id or logging for the probe).
func buildProbeHandler(deps Deps, required string) http.Handler {
	return authMiddleware(deps)(probeHandler(required))
}

func TestAuthAnonymousRequestGets401(t *testing.T) {
	t.Parallel()

	deps := newAuthTestDeps(t)
	h := buildProbeHandler(deps, "publish:dev")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/probe", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ErrorCode != CodeUnauthorized {
		t.Errorf("error_code = %q, want %q", body.ErrorCode, CodeUnauthorized)
	}
}

func TestAuthValidTokenInsufficientScopeGets403(t *testing.T) {
	t.Parallel()

	deps := newAuthTestDeps(t)
	tok := seedTokenRow(t, deps.DB.DB, "ci-dev", "publish:dev", false)

	h := buildProbeHandler(deps, "publish:prod") // required scope differs
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ErrorCode != CodeInsufficientScope {
		t.Errorf("error_code = %q, want %q", body.ErrorCode, CodeInsufficientScope)
	}
}

func TestAuthValidTokenSufficientScopeGets200(t *testing.T) {
	t.Parallel()

	deps := newAuthTestDeps(t)
	tok := seedTokenRow(t, deps.DB.DB, "ci-all", "publish:*,read:*", false)

	h := buildProbeHandler(deps, "publish:prod") // satisfied by publish:*
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["label"] != "ci-all" {
		t.Errorf("label = %v, want ci-all", body["label"])
	}
}

func TestAuthRevokedTokenGets401(t *testing.T) {
	t.Parallel()

	deps := newAuthTestDeps(t)
	tok := seedTokenRow(t, deps.DB.DB, "rev", "publish:*", true)

	h := buildProbeHandler(deps, "publish:dev")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked token: status = %d, want 401", rec.Code)
	}
}

func TestAuthNonBearerHeaderIgnored(t *testing.T) {
	t.Parallel()

	deps := newAuthTestDeps(t)

	// Basic auth should neither match nor crash us.
	h := buildProbeHandler(deps, "publish:dev")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/probe", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("non-bearer: status = %d, want 401", rec.Code)
	}
}
