package api

import (
	"net/http"
)

// CellSummary is one row in the /api/v1/cells response.
type CellSummary struct {
	Name   string `json:"name"`
	RMinor string `json:"r_minor"`
}

// ListCellsResponse is matrix.yaml rendered as JSON: the one distro
// this deployment serves plus one cell per R minor version.
type ListCellsResponse struct {
	Distro         string        `json:"distro"`
	Arch           string        `json:"arch"`
	DefaultRMinor  string        `json:"default_r_minor"`
	BuildImageHint string        `json:"build_image_hint,omitempty"`
	Cells          []CellSummary `json:"cells"`
}

// handleListCells serves GET /api/v1/cells. CI reads it to decide
// which R versions to build binaries for, so any valid token may call
// it — a publish-only CI token included.
func handleListCells(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAuthenticated(w, r) {
			return
		}

		resp := ListCellsResponse{Cells: []CellSummary{}}
		if m := deps.Matrix; m != nil {
			resp.Distro = m.Distro
			resp.Arch = m.Arch
			resp.DefaultRMinor = m.DefaultRMinor
			resp.BuildImageHint = m.BuildImageHint
			for _, c := range m.Cells {
				resp.Cells = append(resp.Cells, CellSummary{Name: c.Name, RMinor: c.RMinor})
			}
		}
		writeJSON(w, r, http.StatusOK, resp)
	}
}
