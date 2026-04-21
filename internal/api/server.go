package api

import (
	"net/http"

	"github.com/schochastics/pakman/internal/cas"
	"github.com/schochastics/pakman/internal/config"
	"github.com/schochastics/pakman/internal/db"
)

// Deps is the set of services API handlers reach for. Assembled once at
// server startup and passed through NewMux. Handlers hold pointers to
// the same values — no defensive copies, no hidden state.
type Deps struct {
	DB     *db.DB
	CAS    *cas.Store
	Matrix *config.MatrixConfig
	Server *config.ServerConfig
	Index  *Index // optional; callers may leave nil and NewMux fills it in
}

// NewMux builds the top-level HTTP handler: the http.ServeMux of
// pakman's routes wrapped in middleware. Callers pass the result to
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

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth(deps))
	mux.HandleFunc("POST /api/v1/packages/{channel}/{name}/{version}", handlePublish(deps))
	mux.HandleFunc("POST /api/v1/packages/{channel}/{name}/{version}/yank", handleYank(deps))
	mux.HandleFunc("DELETE /api/v1/packages/{channel}/{name}/{version}", handleDelete(deps))

	return chain(mux,
		requestIDMiddleware,
		accessLogMiddleware,
		recoveryMiddleware,
		authMiddleware(deps),
	)
}
