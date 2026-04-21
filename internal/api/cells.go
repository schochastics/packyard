package api

import (
	"net/http"

	"github.com/schochastics/pakman/internal/auth"
)

// CellSummary is one row in the /api/v1/cells response. Mirrors the
// matrix.yaml entry exactly — no aggregate stats for v1. B4's
// dashboard can compute coverage from /packages + /cells client-side,
// which keeps this endpoint a pure-read / no-JOIN response.
type CellSummary struct {
	Name      string `json:"name"`
	OS        string `json:"os"`
	OSVersion string `json:"os_version"`
	Arch      string `json:"arch"`
	RMinor    string `json:"r_minor"`
}

// ListCellsResponse wraps the slice.
type ListCellsResponse struct {
	Cells []CellSummary `json:"cells"`
}

// handleListCells serves GET /api/v1/cells — essentially the
// matrix.yaml file, rendered as JSON for clients that prefer a
// programmatic handle over reading the YAML on disk.
//
// Admin-gated for v1 (same rationale as /channels). This endpoint is
// the weakest case for admin-only since CI workers need the cell
// list to decide what to build; Phase C may well loosen to "any
// valid token".
func handleListCells(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireScope(w, r, auth.ScopeAdmin) {
			return
		}

		out := []CellSummary{}
		if deps.Matrix != nil {
			for _, c := range deps.Matrix.Cells {
				out = append(out, CellSummary{
					Name:      c.Name,
					OS:        c.OS,
					OSVersion: c.OSVersion,
					Arch:      c.Arch,
					RMinor:    c.RMinor,
				})
			}
		}
		writeJSON(w, r, http.StatusOK, ListCellsResponse{Cells: out})
	}
}
