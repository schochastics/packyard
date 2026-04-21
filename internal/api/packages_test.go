package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func doGet(t *testing.T, fx *publishFixture, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	return rec
}

func TestListPackagesShape(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))
	publishWithBinary(t, fx, "dev", "beta", "2.0.0", "ubuntu-22.04-amd64-r-4.4")

	rec := doGet(t, fx, "/api/v1/packages", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Total-Count"); got != "2" {
		t.Errorf("X-Total-Count = %q, want 2", got)
	}

	var resp ListPackagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Packages) != 2 {
		t.Fatalf("got %d packages, want 2", len(resp.Packages))
	}

	byName := map[string]PackageSummary{}
	for _, p := range resp.Packages {
		byName[p.Name] = p
	}
	alpha, ok := byName["alpha"]
	if !ok {
		t.Fatal("alpha missing")
	}
	if alpha.Binaries == nil {
		t.Error("Binaries should serialize as [], not null")
	}
	if len(alpha.Binaries) != 0 {
		t.Errorf("alpha Binaries = %d entries, want 0", len(alpha.Binaries))
	}
	beta, ok := byName["beta"]
	if !ok {
		t.Fatal("beta missing")
	}
	if len(beta.Binaries) != 1 {
		t.Fatalf("beta Binaries = %d, want 1", len(beta.Binaries))
	}
	if beta.Binaries[0].Cell != "ubuntu-22.04-amd64-r-4.4" {
		t.Errorf("beta binary cell = %q", beta.Binaries[0].Cell)
	}
}

func TestListPackagesFilterByChannel(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))
	publishSource(t, fx, "prod", "gamma", "1.0.0", []byte("g"))

	rec := doGet(t, fx, "/api/v1/packages?channel=prod", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if got := rec.Header().Get("X-Total-Count"); got != "1" {
		t.Errorf("X-Total-Count = %q, want 1", got)
	}
	var resp ListPackagesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Packages) != 1 || resp.Packages[0].Name != "gamma" {
		t.Errorf("filtered result = %+v, want only gamma", resp.Packages)
	}
}

func TestListPackagesFilterByName(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))
	publishSource(t, fx, "dev", "alpha", "1.1.0", []byte("a2"))
	publishSource(t, fx, "dev", "beta", "1.0.0", []byte("b"))

	rec := doGet(t, fx, "/api/v1/packages?package=alpha", fx.token)
	var resp ListPackagesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if len(resp.Packages) != 2 {
		t.Fatalf("got %d rows, want 2 (both alpha versions)", len(resp.Packages))
	}
	for _, p := range resp.Packages {
		if p.Name != "alpha" {
			t.Errorf("unexpected package %q", p.Name)
		}
	}
}

func TestListPackagesPagination(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	// Seed 7 rows.
	for i := 0; i < 7; i++ {
		publishSource(t, fx, "dev", "pkg"+strconv.Itoa(i), "1.0.0", []byte(fmt.Sprintf("src%d", i)))
	}

	// limit=3 → expect 3 rows + X-Total-Count=7
	rec := doGet(t, fx, "/api/v1/packages?limit=3", fx.token)
	if got := rec.Header().Get("X-Total-Count"); got != "7" {
		t.Errorf("X-Total-Count = %q, want 7", got)
	}
	var resp ListPackagesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Packages) != 3 {
		t.Errorf("limit=3 returned %d packages", len(resp.Packages))
	}

	// offset=5 limit=3 → expect only 2 rows (7-5=2)
	rec = doGet(t, fx, "/api/v1/packages?limit=3&offset=5", fx.token)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Packages) != 2 {
		t.Errorf("offset=5 limit=3 returned %d", len(resp.Packages))
	}
}

func TestListPackagesLimitCap(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	// Seed one package so the response isn't empty.
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))

	// limit above cap → clamped, not errored. Request succeeds.
	rec := doGet(t, fx, "/api/v1/packages?limit=99999", fx.token)
	if rec.Code != http.StatusOK {
		t.Errorf("clamped limit should succeed, got %d body %s", rec.Code, rec.Body.String())
	}
}

func TestListPackagesBadLimitOffset(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	cases := []string{
		"/api/v1/packages?limit=-1",
		"/api/v1/packages?limit=abc",
		"/api/v1/packages?offset=-5",
		"/api/v1/packages?offset=xyz",
	}
	for _, u := range cases {
		rec := doGet(t, fx, u, fx.token)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", u, rec.Code)
		}
	}
}

func TestListPackagesRequiresAdmin(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))

	// Anonymous → 401.
	rec := doGet(t, fx, "/api/v1/packages", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon: %d", rec.Code)
	}

	// Non-admin token → 403.
	tok := seedScopedToken(t, fx, "reader", "read:*,publish:dev")
	rec = doGet(t, fx, "/api/v1/packages", tok)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin: %d", rec.Code)
	}
}
