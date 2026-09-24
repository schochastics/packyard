package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/schochastics/packyard/internal/auth"
	"github.com/schochastics/packyard/internal/config"
)

func publishSource(t *testing.T, fx *publishFixture, channel, name, version string, body []byte) {
	t.Helper()
	reqBody, ct := buildPublishBody(t, map[string]any{"source": "source"},
		publishPart{name: "source", body: body})
	rec := doPublish(t, fx, channel, name, version, fx.token, reqBody, ct)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("seed publish %s@%s: status %d body %s", name, version, rec.Code, rec.Body.String())
	}
}

func getURL(t *testing.T, fx *publishFixture, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	return rec
}

func TestSourcePACKAGESLists(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("alpha source"))
	publishSource(t, fx, "dev", "beta", "2.0.0", []byte("beta source"))

	rec := getURL(t, fx, "/dev/src/contrib/PACKAGES", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain…", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Package: alpha\nVersion: 1.0.0\n") {
		t.Errorf("alpha stanza missing: %q", body)
	}
	if !strings.Contains(body, "Package: beta\nVersion: 2.0.0\n") {
		t.Errorf("beta stanza missing: %q", body)
	}
}

func TestSourcePACKAGESGzRoundTrip(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("x"))

	rec := getURL(t, fx, "/dev/src/contrib/PACKAGES.gz", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q", ct)
	}

	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = zr.Close() }()
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gz: %v", err)
	}
	if !strings.Contains(string(plain), "Package: alpha") {
		t.Errorf("decompressed body missing alpha stanza: %q", plain)
	}
}

func TestSourceTarballServesCASBytes(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	content := []byte("real tarball bytes")
	publishSource(t, fx, "dev", "alpha", "1.0.0", content)

	rec := getURL(t, fx, "/dev/src/contrib/alpha_1.0.0.tar.gz", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Errorf("body bytes differ from published source")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-gzip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("ETag header missing")
	}
}

func TestSourceTarballUnknownVersionReturns404(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("x"))

	rec := getURL(t, fx, "/dev/src/contrib/alpha_9.9.9.tar.gz", fx.token)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSourceTarballMalformedFilenameReturns404(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	cases := []string{
		"/dev/src/contrib/not-a-tarball",
		"/dev/src/contrib/without_underscore.tar.gz",
		"/dev/src/contrib/missing-ext.tar",
		"/dev/src/contrib/_noName.tar.gz",
		"/dev/src/contrib/noVersion_.tar.gz",
	}
	for _, u := range cases {
		rec := getURL(t, fx, u, fx.token)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", u, rec.Code)
		}
	}
}

func TestSourcePACKAGESRequiresReadScope(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// Anonymous fails 401.
	rec := getURL(t, fx, "/dev/src/contrib/PACKAGES", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon: status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}

	// Token without read scope fails 403.
	noRead, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fx.deps.DB.ExecContext(context.Background(), `
		INSERT INTO tokens(token_sha256, scopes_csv, label) VALUES (?, ?, ?)
	`, auth.HashToken(noRead), "publish:dev", "pub-only")
	if err != nil {
		t.Fatal(err)
	}
	rec = getURL(t, fx, "/dev/src/contrib/PACKAGES", noRead)
	if rec.Code != http.StatusForbidden {
		t.Errorf("wrong-scope: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAnonymousReadsPerChannel(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	fx.deps.Channels = &config.ChannelsConfig{Channels: []config.Channel{
		{Name: "dev", OverwritePolicy: config.PolicyMutable, Kind: config.KindLocal},
		{Name: "prod", OverwritePolicy: config.PolicyImmutable, Kind: config.KindLocal, Default: true, AnonymousReads: true},
	}}
	fx.mux = NewMux(fx.deps)
	for _, ch := range []string{"dev", "prod"} {
		publishSource(t, fx, ch, "alpha", "1.0.0", []byte("x "+ch))
	}

	for _, c := range []struct {
		path string
		want int
	}{
		{"/prod/src/contrib/PACKAGES", http.StatusOK},
		{"/prod/src/contrib/alpha_1.0.0.tar.gz", http.StatusOK},
		{"/src/contrib/PACKAGES", http.StatusOK}, // default alias → prod
		{"/prod/__linux__/jammy/latest/src/contrib/PACKAGES", http.StatusOK},
		{"/dev/src/contrib/PACKAGES", http.StatusUnauthorized},
		{"/dev/src/contrib/alpha_1.0.0.tar.gz", http.StatusUnauthorized},
		{"/dev/__linux__/jammy/latest/src/contrib/PACKAGES", http.StatusUnauthorized},
	} {
		if rec := getURL(t, fx, c.path, ""); rec.Code != c.want {
			t.Errorf("anon %s: status = %d, want %d", c.path, rec.Code, c.want)
		}
	}

	// Anonymous reads cover the CRAN-protocol surface only.
	if rec := getURL(t, fx, "/api/v1/packages", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon JSON API: status = %d, want 401", rec.Code)
	}
}

func TestPublishInvalidatesPACKAGESCache(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// First fetch: empty channel body.
	first := getURL(t, fx, "/dev/src/contrib/PACKAGES", fx.token)
	if first.Code != http.StatusOK {
		t.Fatalf("first fetch: %d", first.Code)
	}
	if len(first.Body.Bytes()) != 0 {
		t.Errorf("expected empty body, got %q", first.Body.String())
	}

	// Publish, then re-fetch immediately; cache must reflect.
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("x"))
	second := getURL(t, fx, "/dev/src/contrib/PACKAGES", fx.token)
	if !strings.Contains(second.Body.String(), "Package: alpha") {
		t.Errorf("PACKAGES did not refresh after publish: %q", second.Body.String())
	}
}

func TestUnknownChannelPACKAGES404(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	rec := getURL(t, fx, "/nosuchchannel/src/contrib/PACKAGES", fx.token)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// Tarballs honor conditional and range requests (resumed downloads,
// caching proxies) and HEAD.
func TestSourceTarballConditionalAndRange(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	content := []byte("0123456789abcdef")
	publishSource(t, fx, "dev", "alpha", "1.0.0", content)
	url := "/dev/src/contrib/alpha_1.0.0.tar.gz"
	etag := getURL(t, fx, url, fx.token).Header().Get("ETag")

	do := func(method string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, url, nil)
		req.Header.Set("Authorization", "Bearer "+fx.token)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		fx.mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(http.MethodGet, map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: status %d, want 304", rec.Code)
	}
	rec := do(http.MethodGet, map[string]string{"Range": "bytes=10-"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "abcdef" {
		t.Errorf("Range: status %d body %q", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodHead, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "16" {
		t.Errorf("HEAD: status %d len %d CL %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
}
