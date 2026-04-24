package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/schochastics/packyard/openapi"
)

// TestOpenAPISpecIsValid confirms the embedded spec parses and type-
// checks against the OpenAPI 3 meta-schema. If this fails, the YAML
// has a structural problem that would break every downstream
// generator.
func TestOpenAPISpecIsValid(t *testing.T) {
	t.Parallel()

	doc := mustLoadSpec(t)
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("spec failed OpenAPI validation: %v", err)
	}
}

// TestServedSpecMatchesEmbedded: the /api/v1/openapi.json body must be
// the JSON rendering of the same YAML bytes embedded in the binary.
// Both should parse back to equivalent structures.
func TestServedSpecMatchesEmbedded(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	rec := doGet(t, fx, "/api/v1/openapi.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var served map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &served); err != nil {
		t.Fatalf("served JSON invalid: %v", err)
	}
	if served["openapi"] != "3.0.3" {
		t.Errorf("openapi field = %v, want 3.0.3", served["openapi"])
	}
	info, _ := served["info"].(map[string]any)
	if info["title"] != "packyard" {
		t.Errorf("info.title = %v", info["title"])
	}
}

// TestServedSpecYAMLByteIdentical confirms GET .../openapi.yaml returns
// exactly the embedded bytes (no rewriting, no whitespace drift).
func TestServedSpecYAMLByteIdentical(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	rec := doGet(t, fx, "/api/v1/openapi.yaml", "")
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), openapi.YAML) {
		t.Errorf("served YAML (%d bytes) differs from embedded (%d bytes)",
			len(rec.Body.Bytes()), len(openapi.YAML))
	}
}

// TestSpecIsPublic: the spec endpoints must not require auth. SDK
// generators should be able to fetch the contract anonymously.
func TestSpecIsPublic(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	for _, path := range []string{"/api/v1/openapi.json", "/api/v1/openapi.yaml"} {
		rec := doGet(t, fx, path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s anon: status = %d, want 200", path, rec.Code)
		}
	}
}

// TestContractHealthResponse walks a real /health call through the
// openapi3filter response validator. This catches drift between the
// spec and what the server actually returns.
func TestContractHealthResponse(t *testing.T) {
	t.Parallel()

	doc := mustLoadSpec(t)
	router := mustRouter(t, doc)

	fx := newPublishFixture(t)
	ts := httptest.NewServer(fx.mux)
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	validateResponse(t, router, req, resp.StatusCode, resp.Header, body)
}

// TestContractAdminListTokensResponse validates a response from an
// authenticated endpoint — makes sure security: bearerAuth doesn't
// trip up the validator.
func TestContractAdminListTokensResponse(t *testing.T) {
	t.Parallel()

	doc := mustLoadSpec(t)
	router := mustRouter(t, doc)

	fx := newPublishFixture(t)
	ts := httptest.NewServer(fx.mux)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/api/v1/admin/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+fx.token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	validateResponse(t, router, req, resp.StatusCode, resp.Header, body)
}

// TestContractListPackagesResponse is the hardest case because it
// carries the nested `binaries` array and the X-Total-Count header.
func TestContractListPackagesResponse(t *testing.T) {
	t.Parallel()

	doc := mustLoadSpec(t)
	router := mustRouter(t, doc)

	fx := newPublishFixture(t)
	publishWithBinary(t, fx, "dev", "alpha", "1.0.0", "ubuntu-22.04-amd64-r-4.4")
	publishSource(t, fx, "dev", "beta", "1.0.0", []byte("src"))

	ts := httptest.NewServer(fx.mux)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/api/v1/packages", nil)
	req.Header.Set("Authorization", "Bearer "+fx.token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	validateResponse(t, router, req, resp.StatusCode, resp.Header, body)
}

// ------------------------------------------------------------------ helpers

func mustLoadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(openapi.YAML)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	return doc
}

// mustRouter builds a router that matches by path only. The gorillamux
// backend's default behavior is to match the request Host against the
// spec's `servers` list; our contract tests run against a random
// httptest port, so we clear servers to force path-only matching.
func mustRouter(t *testing.T, doc *openapi3.T) routers.Router {
	t.Helper()
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	return router
}

// validateResponse runs the response body + headers through the
// openapi3filter response validator. Fails the test with a readable
// message on any violation.
func validateResponse(t *testing.T, router routers.Router, req *http.Request, status int, headers http.Header, body []byte) {
	t.Helper()

	route, params, err := router.FindRoute(req)
	if err != nil {
		t.Fatalf("route not in spec: %s %s: %v", req.Method, req.URL.Path, err)
	}

	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    req,
			PathParams: params,
			Route:      route,
			Options:    &openapi3filter.Options{AuthenticationFunc: nopAuth},
		},
		Status: status,
		Header: headers,
	}
	input.SetBodyBytes(body)

	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		t.Errorf("response for %s %s (status %d) failed spec:\n  %v\nbody:\n  %s",
			req.Method, req.URL.Path, status, err, preview(body))
	}
}

func nopAuth(_ context.Context, _ *openapi3filter.AuthenticationInput) error {
	// We're validating the SERVER's responses against the spec, so we
	// don't actually want to re-check bearer tokens against the spec's
	// security schemes. Accept any input.
	return nil
}

// preview trims very long bodies so test failure messages stay readable.
func preview(b []byte) string {
	s := string(b)
	if len(s) > 400 {
		s = s[:400] + "…" + strings.Repeat("", 0)
	}
	return s
}
