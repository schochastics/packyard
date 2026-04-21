package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
)

// HealthResponse is the body of GET /health. Subsystem fields let
// operators tell "process is alive" from "process is alive but can't
// talk to its dependencies" at a glance.
type HealthResponse struct {
	Status     string            `json:"status"` // ok or degraded
	Version    string            `json:"version,omitempty"`
	Subsystems map[string]string `json:"subsystems,omitempty"` // subsystem → ok|down|<msg>
}

// handleHealth reports overall liveness plus the state of each
// subsystem pakman depends on. Returns 200 when everything is ok,
// 503 when anything fails — that's the signal load balancers read
// to pull the host out of rotation.
//
// Subsystems checked:
//
//	db       SQLite is ping-able.
//	cas      A temp file can be created under <cas_root>/tmp/ and
//	         removed. Catches filesystem-full, permissions-changed,
//	         and read-only-mount cases a DB ping wouldn't.
//	matrix   matrix.yaml loaded (non-nil on the Deps bundle). If
//	         absent, binary PACKAGES/tarball routes would 404
//	         everything even though the server is technically up.
func handleHealth(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subs := checkSubsystems(r.Context(), deps)

		overall := "ok"
		status := http.StatusOK
		for _, v := range subs {
			if v != "ok" {
				overall = "degraded"
				status = http.StatusServiceUnavailable
				break
			}
		}

		writeJSON(w, r, status, HealthResponse{
			Status:     overall,
			Subsystems: subs,
		})
	}
}

// checkSubsystems runs the individual probes and returns a map of
// subsystem name → status ("ok" on success, a short error string
// otherwise). Map values stay short so an ops dashboard can show
// them raw without truncation.
func checkSubsystems(ctx context.Context, deps Deps) map[string]string {
	return map[string]string{
		"db":     pingDB(ctx, deps),
		"cas":    probeCAS(deps),
		"matrix": checkMatrix(deps),
	}
}

func pingDB(ctx context.Context, deps Deps) string {
	if deps.DB == nil {
		return "not initialized"
	}
	if err := deps.DB.PingContext(ctx); err != nil {
		return "ping failed: " + err.Error()
	}
	return "ok"
}

// probeCAS opens a temp file under the CAS's tmp/ subdirectory and
// immediately removes it. Catches readonly mounts, permissions drift,
// and full disks — any of which would break publish before the DB
// noticed anything was wrong.
func probeCAS(deps Deps) string {
	if deps.CAS == nil {
		return "not initialized"
	}
	tmp, err := os.CreateTemp(filepath.Join(deps.CAS.Root(), "tmp"), "healthprobe-*")
	if err != nil {
		return "tmp write failed: " + err.Error()
	}
	path := tmp.Name()
	_ = tmp.Close()
	if err := os.Remove(path); err != nil {
		return "tmp cleanup failed: " + err.Error()
	}
	return "ok"
}

func checkMatrix(deps Deps) string {
	if deps.Matrix == nil {
		return "matrix.yaml not loaded"
	}
	if len(deps.Matrix.Cells) == 0 {
		return "matrix has no cells"
	}
	return "ok"
}
