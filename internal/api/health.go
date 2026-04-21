package api

import (
	"net/http"
)

// HealthResponse is the body of GET /health. Phase B3 extends this with
// per-subsystem state; v1 A4 only reports overall status so callers
// have something stable to poll during smoke tests.
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

// handleHealth returns a liveness check that simply pings the DB. A
// richer readiness check — CAS write probe, matrix loaded, etc — lands
// in B3 along with /metrics. For now this is enough to let a load
// balancer tell "the process exists" from "the process is hung".
func handleHealth(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := deps.DB.PingContext(r.Context()); err != nil {
			writeError(w, r, http.StatusServiceUnavailable,
				CodeUnavailable, "database ping failed",
				"check disk space and DB file permissions")
			return
		}
		writeJSON(w, r, http.StatusOK, HealthResponse{Status: "ok"})
	}
}
