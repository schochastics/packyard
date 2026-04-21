// Package metrics owns the pakman Prometheus registry and the metric
// handles that everything else (middleware, domain handlers, admin
// operations) increments.
//
// Shape conventions:
//
//   - All metric names prefixed "pakman_" except the stdlib runtime
//     metrics the process collector contributes on its own.
//   - HTTP metrics (cross-cutting) labeled only by method + status to
//     keep cardinality bounded. Per-route metrics are not derived from
//     paths because Go 1.22's ServeMux doesn't expose the matched
//     route pattern; domain counters (publish_total, yank_total, ...)
//     fill that gap at the semantic layer.
//   - Domain counters carry one low-cardinality label each — usually
//     the channel name, occasionally a result enum.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics bundles every metric handle pakman emits. Construct one
// instance in main and pass to the API/mutation layers; do not
// register individual handles anywhere else.
type Metrics struct {
	Registry *prometheus.Registry

	// HTTP layer.
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec

	// Domain.
	PublishTotal *prometheus.CounterVec // labels: channel, result
	YankTotal    *prometheus.CounterVec // labels: channel
	DeleteTotal  *prometheus.CounterVec // labels: channel
	CASBytes     prometheus.Gauge

	// Token admin.
	TokenCreateTotal prometheus.Counter
	TokenRevokeTotal prometheus.Counter
}

// New builds and registers every pakman metric on a fresh registry.
// Using our own registry (not the default one) keeps the surface
// hermetic — a stray prometheus.DefaultRegisterer.MustRegister in
// another dep can't pollute the /metrics output.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		Registry: reg,
		HTTPRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "pakman",
				Name:      "http_requests_total",
				Help:      "Total HTTP requests handled, labeled by method and status code.",
			},
			[]string{"method", "status"},
		),
		HTTPRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "pakman",
				Name:      "http_request_duration_seconds",
				Help:      "HTTP request duration in seconds, labeled by method and status code.",
				// Buckets chosen for pakman's mix: fast admin/read requests
				// in the low-ms range, publish requests that can run for
				// seconds over a slow link.
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
			},
			[]string{"method", "status"},
		),
		PublishTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "pakman",
				Name:      "publish_total",
				Help:      "Publish attempts by channel and outcome (created, overwrote, already_existed, rejected).",
			},
			[]string{"channel", "result"},
		),
		YankTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "pakman",
				Name:      "yank_total",
				Help:      "Successful yanks by channel.",
			},
			[]string{"channel"},
		),
		DeleteTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "pakman",
				Name:      "delete_total",
				Help:      "Successful hard-deletes by channel.",
			},
			[]string{"channel"},
		),
		CASBytes: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "pakman",
				Name:      "cas_bytes",
				Help:      "Total bytes stored in the content-addressable blob store.",
			},
		),
		TokenCreateTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "pakman",
				Name:      "token_create_total",
				Help:      "Tokens minted via the admin endpoint.",
			},
		),
		TokenRevokeTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "pakman",
				Name:      "token_revoke_total",
				Help:      "Tokens revoked via the admin endpoint.",
			},
		),
	}

	reg.MustRegister(
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.PublishTotal,
		m.YankTotal,
		m.DeleteTotal,
		m.CASBytes,
		m.TokenCreateTotal,
		m.TokenRevokeTotal,
	)
	return m
}
