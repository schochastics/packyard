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
}

// NewMux builds the top-level HTTP handler: the http.ServeMux of
// pakman's routes wrapped in middleware. Callers pass the result to
// http.Server{Handler: ...}.
//
// Middleware order (outermost first):
//  1. requestIDMiddleware — tag every request with an X-Request-Id
//  2. recoveryMiddleware  — convert panics into 500 JSON envelopes
//  3. accessLogMiddleware — one structured log line per request
//
// Note the deviation from the more conventional "access-log then
// recovery" ordering: we want the access log to still fire even if a
// handler panics, so recovery sits inside it. We log the panic
// separately inside recovery at ERROR level.
func NewMux(deps Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth(deps))

	return chain(mux,
		requestIDMiddleware,
		accessLogMiddleware,
		recoveryMiddleware,
	)
}
