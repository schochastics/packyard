package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListChannelsShapeAndStats(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	// dev and prod exist from the fixture; seed two packages on dev.
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))
	publishSource(t, fx, "dev", "beta", "2.0.0", []byte("b"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/channels", nil)
	req.Header.Set("Authorization", "Bearer "+fx.token)
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var resp ListChannelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	byName := map[string]ChannelSummary{}
	for _, c := range resp.Channels {
		byName[c.Name] = c
	}
	dev, ok := byName["dev"]
	if !ok {
		t.Fatalf("dev missing: %+v", resp.Channels)
	}
	if dev.PackageCount != 2 {
		t.Errorf("dev PackageCount = %d, want 2", dev.PackageCount)
	}
	if dev.LatestPublishAt == nil || *dev.LatestPublishAt == "" {
		t.Error("dev LatestPublishAt not populated after seed")
	}
	if dev.OverwritePolicy != "mutable" {
		t.Errorf("dev OverwritePolicy = %q", dev.OverwritePolicy)
	}
	if dev.Default {
		t.Error("dev unexpectedly marked default (prod is default in fixture)")
	}

	prod, ok := byName["prod"]
	if !ok {
		t.Fatalf("prod missing: %+v", resp.Channels)
	}
	if !prod.Default {
		t.Error("prod should be default")
	}
	if prod.PackageCount != 0 {
		t.Errorf("prod PackageCount = %d, want 0", prod.PackageCount)
	}
	if prod.LatestPublishAt != nil {
		t.Errorf("prod LatestPublishAt = %v, want nil on empty channel", *prod.LatestPublishAt)
	}
}

func TestListChannelsRequiresAdmin(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// Anonymous → 401.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/channels", nil)
	fx.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon: status = %d, want 401", rec.Code)
	}

	// Token with read:* but no admin → 403.
	tok := seedScopedToken(t, fx, "reader", "read:*,publish:dev")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/channels", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	fx.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin: status = %d, want 403", rec.Code)
	}
}

func TestListChannelsEmptyDB(t *testing.T) {
	t.Parallel()

	// Build Deps with an empty channels table (no fixture reconcile).
	deps := newAuthTestDeps(t)
	tok := seedTokenRow(t, deps.DB.DB, "admin", "admin", false)

	mux := NewMux(deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/channels", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var resp ListChannelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Channels) != 0 {
		t.Errorf("empty DB returned %d channels", len(resp.Channels))
	}
	// Must be "channels: []" not null — clients parsing JSON should
	// not have to special-case this.
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"channels":[]`)) {
		t.Errorf("response should serialize as empty array, not null: %s", rec.Body.String())
	}
}
