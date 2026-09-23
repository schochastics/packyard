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

	if resp.Distro != "jammy" || resp.Arch != "amd64" || resp.DefaultRMinor != "4.4" {
		t.Errorf("top-level fields = %+v", resp)
	}
	// Fixture seeds two cells: R 4.4 and R 4.5.
	if len(resp.Cells) != 2 {
		t.Fatalf("got %d cells, want 2", len(resp.Cells))
	}
	if resp.Cells[0] != (CellSummary{Name: "r-4.4", RMinor: "4.4"}) ||
		resp.Cells[1] != (CellSummary{Name: "r-4.5", RMinor: "4.5"}) {
		t.Errorf("cells = %+v", resp.Cells)
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

func TestListCellsRequiresAnyToken(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// Anonymous → 401.
	rec := doGet(t, fx, "/api/v1/cells", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon: %d", rec.Code)
	}

	// A publish-only CI token is enough.
	tok := seedScopedToken(t, fx, "pub", "publish:dev")
	rec = doGet(t, fx, "/api/v1/cells", tok)
	if rec.Code != http.StatusOK {
		t.Errorf("publish token: %d %s", rec.Code, rec.Body.String())
	}
}
