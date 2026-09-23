package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/schochastics/packyard/internal/auth"
)

// seedPublished inserts a single package on (channel, name, version)
// with a trivial source blob. Returns nothing; tests do their own
// assertions against fx.deps.DB afterwards.
func seedPublished(t *testing.T, fx *publishFixture, channel, name, version string) {
	t.Helper()
	body, ct := buildPublishBody(t, map[string]any{"source": "source"},
		publishPart{name: "source", body: []byte("seed-" + channel + "-" + name + "-" + version)})
	rec := doPublish(t, fx, channel, name, version, fx.token, body, ct)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed publish: status %d body %s", rec.Code, rec.Body.String())
	}
}

func doYank(t *testing.T, fx *publishFixture, channel, name, version, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/v1/packages/" + channel + "/" + name + "/" + version + "/yank"
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(http.MethodPost, url, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	return rec
}

func doDelete(t *testing.T, fx *publishFixture, channel, name, version, token string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/v1/packages/" + channel + "/" + name + "/" + version
	req := httptest.NewRequest(http.MethodDelete, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	return rec
}

func seedScopedToken(t *testing.T, fx *publishFixture, label, scopes string) string {
	t.Helper()
	tok, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fx.deps.DB.ExecContext(context.Background(), `
		INSERT INTO tokens(token_sha256, scopes_csv, label) VALUES (?, ?, ?)
	`, auth.HashToken(tok), scopes, label)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestYankMarksRowAndEmitsEvent(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	seedPublished(t, fx, "prod", "mypkg", "1.0.0")

	rec := doYank(t, fx, "prod", "mypkg", "1.0.0", fx.token, `{"reason": "cve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var resp YankResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Yanked || resp.Reason != "cve" {
		t.Errorf("yank response = %+v", resp)
	}

	// DB reflects yank.
	var yanked int
	var reason *string
	if err := fx.deps.DB.QueryRowContext(context.Background(),
		`SELECT yanked, yank_reason FROM packages WHERE channel='prod' AND name='mypkg' AND version='1.0.0'`,
	).Scan(&yanked, &reason); err != nil {
		t.Fatal(err)
	}
	if yanked != 1 || reason == nil || *reason != "cve" {
		t.Errorf("DB state: yanked=%d reason=%v", yanked, reason)
	}

	// Event present.
	var eventType string
	if err := fx.deps.DB.QueryRowContext(context.Background(),
		`SELECT type FROM events ORDER BY id DESC LIMIT 1`).Scan(&eventType); err != nil {
		t.Fatal(err)
	}
	if eventType != "yank" {
		t.Errorf("latest event = %q, want yank", eventType)
	}
}

func TestYankAcceptsEmptyBody(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	seedPublished(t, fx, "dev", "mypkg", "0.1.0")

	rec := doYank(t, fx, "dev", "mypkg", "0.1.0", fx.token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
}

func TestYankUnknownPackageReturns404(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	rec := doYank(t, fx, "dev", "nopkg", "9.9.9", fx.token, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestYankRequiresYankScope(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	seedPublished(t, fx, "dev", "mypkg", "1.0.0")

	// Token has publish but not yank on dev.
	tok := seedScopedToken(t, fx, "pub-only", "publish:dev")
	rec := doYank(t, fx, "dev", "mypkg", "1.0.0", tok, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteOnMutableSucceeds(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	seedPublished(t, fx, "dev", "mypkg", "1.0.0")

	rec := doDelete(t, fx, "dev", "mypkg", "1.0.0", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}

	var count int
	if err := fx.deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM packages WHERE channel='dev' AND name='mypkg' AND version='1.0.0'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("package row count = %d, want 0 after delete", count)
	}

	// Event emitted.
	var eventType string
	if err := fx.deps.DB.QueryRowContext(context.Background(),
		`SELECT type FROM events ORDER BY id DESC LIMIT 1`).Scan(&eventType); err != nil {
		t.Fatal(err)
	}
	if eventType != "delete" {
		t.Errorf("latest event = %q, want delete", eventType)
	}
}

func TestDeleteOnImmutableRefused(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	seedPublished(t, fx, "prod", "mypkg", "1.0.0")

	rec := doDelete(t, fx, "prod", "mypkg", "1.0.0", fx.token)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body %s; want 409", rec.Code, rec.Body.String())
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ErrorCode != CodeChannelImmutable {
		t.Errorf("error_code = %q, want %q", body.ErrorCode, CodeChannelImmutable)
	}

	// Row must still exist.
	var count int
	if err := fx.deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM packages WHERE channel='prod' AND name='mypkg' AND version='1.0.0'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("row vanished despite 409 refusal")
	}
}

func TestDeleteUnknownPackageReturns404(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	rec := doDelete(t, fx, "dev", "nopkg", "9.9.9", fx.token)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
