package api

import (
	"log/slog"
	"net/http"

	"github.com/schochastics/packyard/internal/cas"
	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/db"
	"github.com/schochastics/packyard/internal/metrics"
	"github.com/schochastics/packyard/internal/ui"
)

// Deps is the set of services API handlers reach for. Assembled once at
// server startup and passed through NewMux. Handlers hold pointers to
// the same values — no defensive copies, no hidden state.
type Deps struct {
	DB              *db.DB
	CAS             *cas.Store
	Matrix          *config.MatrixConfig
	Server          *config.ServerConfig
	Index           *Index           // optional; NewMux fills in if nil
	Metrics         *metrics.Metrics // optional; NewMux fills in if nil
	UISessionKey    []byte           // HMAC key for /ui/ session cookies; empty disables the UI
	UISecureCookies bool             // mark /ui/ cookies Secure (production)
}

// NewMux builds the top-level HTTP handler: the http.ServeMux of
// packyard's routes wrapped in middleware. Callers pass the result to
// http.Server{Handler: ...}.
//
// Middleware order (outermost first):
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

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth(deps))
	mux.Handle("GET /metrics", handleMetrics(deps))
	mux.HandleFunc("POST /api/v1/packages/{channel}/{name}/{version}", handlePublish(deps))
	mux.HandleFunc("POST /api/v1/packages/{channel}/{name}/{version}/yank", handleYank(deps))
	mux.HandleFunc("DELETE /api/v1/packages/{channel}/{name}/{version}", handleDelete(deps))

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

	// CRAN-protocol source surface. {channel} is the first path segment
	// so `repos = "http://packyard/<channel>"` Just Works with vanilla R —
	// R's contrib.url() appends "/src/contrib/PACKAGES" on its own.
	mux.HandleFunc("GET /{channel}/src/contrib/PACKAGES", handleSourcePackages(deps))
	mux.HandleFunc("GET /{channel}/src/contrib/PACKAGES.gz", handleSourcePackagesGz(deps))
	mux.HandleFunc("GET /{channel}/src/contrib/{file}", handleSourceTarball(deps))

	// CRAN-protocol binary surface. Only Linux is served directly in
	// the URL shape; macOS and Windows binaries weren't on packyard's
	// v1 target platforms.
	mux.HandleFunc("GET /{channel}/bin/linux/{cell}/PACKAGES", handleBinaryPackages(deps))
	mux.HandleFunc("GET /{channel}/bin/linux/{cell}/PACKAGES.gz", handleBinaryPackagesGz(deps))
	mux.HandleFunc("GET /{channel}/bin/linux/{cell}/{file}", handleBinaryTarball(deps))

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

	// Default-channel aliases: `repos = "http://packyard/"` works the
	// same as naming the default channel explicitly.
	mux.HandleFunc("GET /src/contrib/PACKAGES", handleDefaultSourcePackages(deps))
	mux.HandleFunc("GET /src/contrib/PACKAGES.gz", handleDefaultSourcePackagesGz(deps))
	mux.HandleFunc("GET /src/contrib/{file}", handleDefaultSourceTarball(deps))
	mux.HandleFunc("GET /bin/linux/{cell}/PACKAGES", handleDefaultBinaryPackages(deps))
	mux.HandleFunc("GET /bin/linux/{cell}/PACKAGES.gz", handleDefaultBinaryPackagesGz(deps))
	mux.HandleFunc("GET /bin/linux/{cell}/{file}", handleDefaultBinaryTarball(deps))

	return chain(mux,
		requestIDMiddleware,
		metricsMiddleware(deps),
		accessLogMiddleware,
		recoveryMiddleware,
		authMiddleware(deps),
	)
}
