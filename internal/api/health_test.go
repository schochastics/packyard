package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schochastics/pakman/internal/api"
	"github.com/schochastics/pakman/internal/cas"
	"github.com/schochastics/pakman/internal/config"
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

	matrix, err := config.DecodeMatrix(strings.NewReader(`
cells:
  - name: ubuntu-22.04-amd64-r-4.4
    os: linux
    os_version: ubuntu-22.04
    arch: amd64
    r_minor: "4.4"
`))
	if err != nil {
		t.Fatalf("matrix: %v", err)
	}

	return api.Deps{DB: database, CAS: store, Matrix: matrix}
}

func TestHealthOKWhenAllSubsystemsPass(t *testing.T) {
	t.Parallel()

	mux := api.NewMux(newTestDeps(t))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var body api.HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q", body.Status)
	}
	for _, sub := range []string{"db", "cas", "matrix"} {
		if got := body.Subsystems[sub]; got != "ok" {
			t.Errorf("subsystems[%s] = %q, want ok", sub, got)
		}
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
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body api.HealthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Status != "degraded" {
		t.Errorf("status = %q, want degraded", body.Status)
	}
	if body.Subsystems["db"] == "ok" {
		t.Errorf("db subsystem should not be ok after Close: %q",
			body.Subsystems["db"])
	}
	// Non-DB subsystems can still be ok and should be reported as such
	// so an operator can tell the DB is the specific problem.
	if body.Subsystems["cas"] != "ok" {
		t.Errorf("cas subsystem should still be ok: %q",
			body.Subsystems["cas"])
	}
}

func TestHealthMatrixAbsentDegrades(t *testing.T) {
	t.Parallel()

	deps := newTestDeps(t)
	deps.Matrix = nil

	mux := api.NewMux(deps)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body api.HealthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Subsystems["matrix"] == "ok" {
		t.Errorf("matrix subsystem should not be ok without a Matrix: %q",
			body.Subsystems["matrix"])
	}
}

func TestHealthResponseIsJSON(t *testing.T) {
	t.Parallel()

	mux := api.NewMux(newTestDeps(t))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	mux.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
