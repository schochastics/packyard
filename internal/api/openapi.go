package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"

	"github.com/schochastics/packyard/openapi"
	"gopkg.in/yaml.v3"
)

var (
	specJSONOnce sync.Once
	specJSONBody []byte
	specJSONErr  error
)

// specJSON returns the JSON-rendered OpenAPI spec, converting from the
// embedded YAML on first call and caching the result. Conversion is
// deterministic — the file never changes at runtime — so we build it
// once and serve it forever.
func specJSON() ([]byte, error) {
	specJSONOnce.Do(func() {
		var m any
		if err := yaml.Unmarshal(openapi.YAML, &m); err != nil {
			specJSONErr = err
			return
		}
		specJSONBody, specJSONErr = json.MarshalIndent(m, "", "  ")
	})
	return specJSONBody, specJSONErr
}

// handleOpenAPIJSON serves GET /api/v1/openapi.json.
//
// No auth: the spec is the advertised contract; any caller — including
// SDK generators — must be able to fetch it without credentials. The
// spec itself says what's authenticated.
func handleOpenAPIJSON(_ Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := specJSON()
		if err != nil {
			writeError(w, r, http.StatusInternalServerError,
				CodeInternal, "render OpenAPI spec: "+err.Error(), "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}
}

// handleOpenAPIYAML serves GET /api/v1/openapi.yaml — same content, as
// the YAML file shipped in the repo. Some SDK generators prefer YAML.
func handleOpenAPIYAML(_ Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Content-Length", strconv.Itoa(len(openapi.YAML)))
		_, _ = w.Write(openapi.YAML)
	}
}
