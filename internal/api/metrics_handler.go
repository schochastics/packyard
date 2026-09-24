package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// handleMetrics returns the Prometheus scrape endpoint for the packyard
// registry. No auth: /metrics is a standard unauthenticated internal
// endpoint, same convention as /health. Operators who need to
// restrict scraping do so at the network layer (firewall, VPC peering,
// reverse proxy allowlist).
func handleMetrics(deps Deps) http.Handler {
	return promhttp.HandlerFor(deps.Metrics.Registry, promhttp.HandlerOpts{
		// Surface errors in the Prometheus response rather than 500'ing;
		// degraded metrics are better than no metrics during a scrape.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// metricsMiddleware records method/status counters and a duration
// histogram for every request. Cardinality is intentionally bounded:
// no path or route labels — see internal/metrics for the rationale.
//
// /metrics itself is excluded from its own observations so a scraping
// Prometheus doesn't skew the duration histogram upward.
func metricsMiddleware(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			labels := []string{methodLabel(r.Method), strconv.Itoa(status)}

			deps.Metrics.HTTPRequestsTotal.WithLabelValues(labels...).Inc()
			deps.Metrics.HTTPRequestDuration.WithLabelValues(labels...).
				Observe(time.Since(start).Seconds())
		})
	}
}

// MetricsHandler serves /metrics for a separate listener
// (server.yaml metrics_listen). deps.Metrics must be the same
// instance passed to NewMux, or the scrape sees none of its counters.
func MetricsHandler(deps Deps) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", handleMetrics(deps))
	return mux
}

// methodLabel folds the request method into a fixed set. net/http
// accepts any token as a method, so using it verbatim would let an
// unauthenticated client create unbounded metric series.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "other"
}
