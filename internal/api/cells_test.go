package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListCellsMirrorsMatrix(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	rec := doGet(t, fx, "/api/v1/cells", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var resp ListCellsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	// Fixture seeds two cells (amd64 + arm64 for ubuntu 22.04 R 4.4).
	if len(resp.Cells) != 2 {
		t.Fatalf("got %d cells, want 2", len(resp.Cells))
	}

	byName := map[string]CellSummary{}
	for _, c := range resp.Cells {
		byName[c.Name] = c
	}
	amd64, ok := byName["ubuntu-22.04-amd64-r-4.4"]
	if !ok {
		t.Fatalf("amd64 cell missing: %+v", resp.Cells)
	}
	if amd64.OS != "linux" || amd64.Arch != "amd64" || amd64.RMinor != "4.4" {
		t.Errorf("amd64 fields = %+v", amd64)
	}
}

func TestListCellsNilMatrixReturnsEmptyArray(t *testing.T) {
	t.Parallel()

	deps := newAuthTestDeps(t)
	// deps.Matrix is nil from newAuthTestDeps; verify we serialize as
	// "[]", not null.
	tok := seedTokenRow(t, deps.DB.DB, "admin", "admin", false)
	mux := NewMux(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cells", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"cells":[]`)) {
		t.Errorf("expected empty array, got %s", rec.Body.String())
	}
}

func TestListCellsRequiresAdmin(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// Anonymous → 401.
	rec := doGet(t, fx, "/api/v1/cells", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon: %d", rec.Code)
	}

	// Non-admin → 403.
	tok := seedScopedToken(t, fx, "pub", "publish:*")
	rec = doGet(t, fx, "/api/v1/cells", tok)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin: %d", rec.Code)
	}
}
