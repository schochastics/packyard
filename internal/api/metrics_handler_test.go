package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsEndpointServesPrometheus(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// Drive a few requests so counters have something to show.
	_ = doGet(t, fx, "/health", "")
	_ = doGet(t, fx, "/health", "")
	_ = doGet(t, fx, "/api/v1/channels", fx.token)

	rec := doGet(t, fx, "/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The custom HTTP counter should be present and non-zero.
	if !strings.Contains(body, "pakman_http_requests_total") {
		t.Errorf("pakman_http_requests_total missing: %s", body)
	}
	// Histogram emits _count / _sum / _bucket lines.
	if !strings.Contains(body, "pakman_http_request_duration_seconds") {
		t.Errorf("duration histogram missing: %s", body)
	}
	// Process + go runtime collectors should be there too.
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("go runtime collector not wired: %s", body)
	}
}

func TestMetricsEndpointExcludesItself(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	_ = doGet(t, fx, "/metrics", "")
	_ = doGet(t, fx, "/metrics", "")

	rec := doGet(t, fx, "/metrics", "")
	body := rec.Body.String()

	// Paths are deliberately not a label — check that /metrics itself
	// doesn't leak into any exposition line.
	if strings.Contains(body, "/metrics") {
		t.Errorf("/metrics path leaked into exposition (cardinality risk): %s",
			truncate(body, 400))
	}
}

func TestMetricsMiddlewareRecordsStatus(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// 401 on an unauth admin request.
	unauthReq := httptest.NewRequest(http.MethodGet, "/api/v1/channels", nil)
	unauthRec := httptest.NewRecorder()
	fx.mux.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("seed request: unexpected status %d", unauthRec.Code)
	}

	scrape := doGet(t, fx, "/metrics", "")
	body := scrape.Body.String()

	if !strings.Contains(body, `status="401"`) {
		t.Errorf("status=\"401\" label missing from counter: %s",
			truncate(body, 600))
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
