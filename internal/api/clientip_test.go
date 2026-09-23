package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/schochastics/packyard/internal/metrics"
)

func TestForwardedClientIP(t *testing.T) {
	t.Parallel()
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}

	cases := []struct {
		name, remote, xff, realIP, want string
	}{
		{"untrusted peer ignored", "203.0.113.9:4000", "198.51.100.1", "", ""},
		{"single hop", "10.0.0.2:4000", "198.51.100.1", "", "198.51.100.1"},
		{"spoofed left entry skipped", "10.0.0.2:4000", "1.2.3.4, 198.51.100.1, 10.0.0.3", "", "198.51.100.1"},
		{"all hops trusted", "10.0.0.2:4000", "10.1.1.1, 10.0.0.3", "", "10.1.1.1"},
		{"x-real-ip fallback", "[::1]:4000", "", "198.51.100.7", "198.51.100.7"},
		{"garbage", "10.0.0.2:4000", "not-an-ip", "", ""},
		{"no headers", "10.0.0.2:4000", "", "", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.realIP != "" {
			r.Header.Set("X-Real-IP", c.realIP)
		}
		got, ok := forwardedClientIP(r, trusted)
		if (c.want == "") == ok || (ok && got.String() != c.want) {
			t.Errorf("%s: got %v, %v; want %q", c.name, got, ok, c.want)
		}
	}
}

func TestClientIPMiddlewareRewritesRemoteAddr(t *testing.T) {
	t.Parallel()
	var seen string
	h := clientIPMiddleware([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr }))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.2:4000"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "198.51.100.1" {
		t.Errorf("RemoteAddr = %q", seen)
	}
}

func TestSeparateMetricsRemovesMainRoute(t *testing.T) {
	t.Parallel()
	fx := newPublishFixture(t)
	deps := fx.deps
	deps.SeparateMetrics = true
	deps.Metrics = metrics.New()
	mux := NewMux(deps)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("/metrics on main mux: status %d, want not served", rec.Code)
	}
	rec = httptest.NewRecorder()
	MetricsHandler(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("MetricsHandler: status %d", rec.Code)
	}
}
