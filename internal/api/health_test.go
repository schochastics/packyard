package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/schochastics/pakman/internal/api"
	"github.com/schochastics/pakman/internal/cas"
	"github.com/schochastics/pakman/internal/db"
)

func newTestDeps(t *testing.T) api.Deps {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	database, err := db.Open(ctx, filepath.Join(dir, "pakman.sqlite"))
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

	return api.Deps{DB: database, CAS: store}
}

func TestHealthOK(t *testing.T) {
	t.Parallel()

	mux := api.NewMux(newTestDeps(t))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body api.HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}

	// Request ID propagates through middleware.
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("X-Request-Id missing on /health response")
	}
}

func TestHealthDBDownReturns503(t *testing.T) {
	t.Parallel()

	deps := newTestDeps(t)
	if err := deps.DB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	mux := api.NewMux(deps)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
