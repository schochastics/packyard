package api

import (
	"log/slog"
	"net/http"
	"net/netip"

	"github.com/schochastics/packyard/internal/cas"
	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/db"
	"github.com/schochastics/packyard/internal/metrics"
	"github.com/schochastics/packyard/internal/store"
	"github.com/schochastics/packyard/internal/ui"
	"github.com/schochastics/packyard/internal/upstream"
)

// Deps is the set of services API handlers reach for. Assembled once at
// server startup and passed through NewMux. Handlers hold pointers to
// the same values — no defensive copies, no hidden state.
type Deps struct {
	DB              *db.DB
	CAS             *cas.Store
	Matrix          *config.MatrixConfig
	Channels        *config.ChannelsConfig // optional in tests; required for proxy channels
	Server          *config.ServerConfig
	Index           *Index            // optional; NewMux fills in if nil
	Metrics         *metrics.Metrics  // optional; NewMux fills in if nil
	Store           *store.Service    // optional; NewMux fills in if nil
	Upstream        *upstream.Fetcher // optional; NewMux fills in if nil (proxy channels need it)
	UISessionKey    []byte            // HMAC key for /ui/ session cookies; empty disables the UI
	UISecureCookies bool              // mark /ui/ cookies Secure (production)
	PublicURL       string            // external base URL (server.yaml public_url); UI snippets
	TrustedProxies  []netip.Prefix    // peers whose X-Forwarded-For is believed
	SeparateMetrics bool              // /metrics is served by MetricsHandler on its own listener
}

// NewMux builds the top-level HTTP handler: the http.ServeMux of
// packyard's routes wrapped in middleware. Callers pass the result to
// http.Server{Handler: ...}.
//
// Middleware order (outermost first):
//  0. clientIPMiddleware  — RemoteAddr from X-Forwarded-For, trusted peers only
//  1. requestIDMiddleware — tag every request with an X-Request-Id
//  2. accessLogMiddleware — one structured log line per request
//  3. recoveryMiddleware  — convert panics into 500 JSON envelopes
//  4. authMiddleware      — resolve bearer tokens to an Identity
//
// Note the deviation from the more conventional "access-log outside
// recovery" ordering: we want the access log to still fire even if a
// handler panics, so recovery sits inside it. The panic itself is
// logged separately by recoveryMiddleware at ERROR level.
//
// authMiddleware does NOT reject anonymous requests. Endpoints that
// require auth call requireScope(); endpoints that don't (/health,
// anon reads on the default channel when enabled) stay simple.
func NewMux(deps Deps) http.Handler {
	if deps.Index == nil {
		deps.Index = NewIndex(deps.DB.DB)
	}
	if deps.Metrics == nil {
		deps.Metrics = metrics.New()
	}
	if deps.Store == nil {
		deps.Store = store.New(deps.DB.DB, deps.CAS)
	}
	if deps.Upstream == nil {
		deps.Upstream = upstream.New(nil, deps.Store)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth(deps))
	if !deps.SeparateMetrics {
		mux.Handle("GET /metrics", handleMetrics(deps))
	}
	mux.HandleFunc("POST /api/v1/packages/{channel}/{name}/{version}", handlePublish(deps))
	mux.HandleFunc("POST /api/v1/packages/{channel}/{name}/{version}/yank", handleYank(deps))
	mux.HandleFunc("DELETE /api/v1/packages/{channel}/{name}/{version}", handleDelete(deps))
	mux.HandleFunc("POST /api/v1/packages/{channel}/{name}/{version}/binaries/{cell}", handleAttachBinary(deps))
	mux.HandleFunc("GET /api/v1/channels/{channel}/missing-binaries", handleListMissingBinaries(deps))

	// Admin surface. All routes require the admin scope; tokens created
	// here can grant arbitrary privileges including admin itself, so
	// this is a privilege-escalation surface by design.
	mux.HandleFunc("POST /api/v1/admin/tokens", handleCreateToken(deps))
	mux.HandleFunc("GET /api/v1/admin/tokens", handleListTokens(deps))
	mux.HandleFunc("DELETE /api/v1/admin/tokens/{id}", handleRevokeToken(deps))

	// JSON read surface. All admin-gated for v1; see individual
	// handlers for the rationale / future loosening notes.
	mux.HandleFunc("GET /api/v1/channels", handleListChannels(deps))
	mux.HandleFunc("GET /api/v1/packages", handleListPackages(deps))
	mux.HandleFunc("GET /api/v1/cells", handleListCells(deps))
	mux.HandleFunc("GET /api/v1/events", handleListEvents(deps))

	// OpenAPI spec. No auth: the contract is public so SDK generators
	// can pull it.
	mux.HandleFunc("GET /api/v1/openapi.json", handleOpenAPIJSON(deps))
	mux.HandleFunc("GET /api/v1/openapi.yaml", handleOpenAPIYAML(deps))

	// CRAN-protocol surface. {channel} is the first path segment so
	// `repos = "http://packyard/<channel>"` Just Works with vanilla R —
	// R's contrib.url() appends "/src/contrib/PACKAGES" on its own.
	// Binaries live under /__linux__/{distro}/{snapshot}/, the URL shape
	// Posit Package Manager uses and Workbench/Connect images expect.
	// Every route also exists without {channel} for the default
	// channel; the handlers tell the two apart via PathValue.
	for _, prefix := range []string{"/{channel}", ""} {
		src := prefix + "/src/contrib"
		mux.HandleFunc("GET "+src+"/PACKAGES", handleSourcePackages(deps, false))
		mux.HandleFunc("GET "+src+"/PACKAGES.gz", handleSourcePackages(deps, true))
		mux.HandleFunc("GET "+src+"/{file}", handleSourceTarball(deps))
		mux.HandleFunc("GET "+src+"/Archive/{pkg}/{file}", handleSourceArchiveTarball(deps))
		mux.HandleFunc("GET "+src+"/Meta/archive.rds", handleArchiveRDS(deps, false))

		linux := prefix + "/__linux__/{distro}/{snapshot}/src/contrib"
		mux.HandleFunc("GET "+linux+"/PACKAGES", handleLinuxPackages(deps, false))
		mux.HandleFunc("GET "+linux+"/PACKAGES.gz", handleLinuxPackages(deps, true))
		mux.HandleFunc("GET "+linux+"/{file}", handleLinuxTarball(deps))
		mux.HandleFunc("GET "+linux+"/Archive/{pkg}/{file}", handleLinuxArchiveTarball(deps))
		mux.HandleFunc("GET "+linux+"/Meta/archive.rds", handleArchiveRDS(deps, true))
	}

	// Operator dashboard. Mounted under /ui/ so an operator can point a
	// browser at the same host that serves the API. Disabled when no
	// session key was supplied — keeps tests and CLI-only deployments
	// from having to generate a key they won't use.
	if len(deps.UISessionKey) > 0 {
		uiHandler, err := ui.NewHandler(ui.Deps{
			DB:            deps.DB,
			Matrix:        deps.Matrix,
			SessionKey:    deps.UISessionKey,
			SecureCookies: deps.UISecureCookies,
			PublicURL:     deps.PublicURL,
		})
		if err != nil {
			// Unreachable in practice: only SessionKey emptiness and
			// template-parse bugs fail here, and we've just gated on the
			// former. Log loudly and keep serving the API anyway.
			slog.Default().Error("ui: handler init failed; /ui/ disabled", "err", err)
		} else {
			// Each UI route registered explicitly rather than as a
			// "/ui/" subtree: the channel wildcard in
			// /{channel}/src/contrib/... makes any /ui/ prefix
			// ambiguous, which Go 1.22's pattern mux refuses at
			// registration time. Per-route lets us keep the R-
			// compatible CRAN URL shape intact.
			stripped := http.StripPrefix("/ui", uiHandler)
			mux.Handle("GET /ui/{$}", stripped)
			mux.Handle("GET /ui/login", stripped)
			mux.Handle("POST /ui/login", stripped)
			mux.Handle("POST /ui/logout", stripped)
			mux.Handle("GET /ui/events", stripped)
			mux.Handle("GET /ui/cells", stripped)
			mux.Handle("GET /ui/storage", stripped)
			mux.Handle("GET /ui/channels/{name}", stripped)
			mux.Handle("GET /ui/static/", stripped)
		}
	}

	return chain(mux,
		clientIPMiddleware(deps.TrustedProxies),
		requestIDMiddleware,
		metricsMiddleware(deps),
		accessLogMiddleware,
		recoveryMiddleware,
		authMiddleware(deps),
	)
}
