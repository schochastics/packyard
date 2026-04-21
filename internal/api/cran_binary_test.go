package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// publishWithBinary publishes a package with exactly one binary for
// the given cell. Returns the (srcBytes, binBytes) pair actually used.
func publishWithBinary(t *testing.T, fx *publishFixture, channel, name, version, cell string) ([]byte, []byte) {
	t.Helper()
	srcBody := []byte("src " + name + " " + version)
	binBody := []byte("bin " + name + " " + version + " " + cell)
	manifest := map[string]any{
		"source": "source",
		"binaries": []map[string]any{
			{"cell": cell, "part": "bin1"},
		},
	}
	reqBody, ct := buildPublishBody(t, manifest,
		publishPart{name: "source", body: srcBody},
		publishPart{name: "bin1", body: binBody},
	)
	rec := doPublish(t, fx, channel, name, version, fx.token, reqBody, ct)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed publish: %d %s", rec.Code, rec.Body.String())
	}
	return srcBody, binBody
}

func TestBinaryPACKAGESListsOnlyRowsWithThatCell(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishWithBinary(t, fx, "dev", "alpha", "1.0.0", "ubuntu-22.04-amd64-r-4.4")
	// beta published source-only (no binary).
	publishSource(t, fx, "dev", "beta", "1.0.0", []byte("src only"))

	rec := getURL(t, fx, "/dev/bin/linux/ubuntu-22.04-amd64-r-4.4/PACKAGES", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Package: alpha") {
		t.Errorf("alpha missing from binary PACKAGES: %q", body)
	}
	if strings.Contains(body, "Package: beta") {
		t.Errorf("beta (source-only) should not appear in binary PACKAGES: %q", body)
	}
	if !strings.Contains(body, "Built: R 4.4.0;") {
		t.Errorf("Built field missing: %q", body)
	}
}

func TestBinaryTarballServesCASBytes(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	cell := "ubuntu-22.04-amd64-r-4.4"
	_, binBody := publishWithBinary(t, fx, "dev", "alpha", "1.0.0", cell)

	rec := getURL(t, fx, "/dev/bin/linux/"+cell+"/alpha_1.0.0.tar.gz", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), binBody) {
		t.Errorf("binary bytes differ from published content")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-gzip" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestBinaryTarballUnknownCellReturns404(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishWithBinary(t, fx, "dev", "alpha", "1.0.0", "ubuntu-22.04-amd64-r-4.4")

	// Package has a binary for the amd64 cell, not arm64.
	rec := getURL(t, fx, "/dev/bin/linux/ubuntu-22.04-arm64-r-4.4/alpha_1.0.0.tar.gz", fx.token)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestBinaryPACKAGESUnknownCellReturns404(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	rec := getURL(t, fx, "/dev/bin/linux/not-in-matrix/PACKAGES", fx.token)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBinaryRoutesHonorReadScope(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishWithBinary(t, fx, "dev", "alpha", "1.0.0", "ubuntu-22.04-amd64-r-4.4")

	rec := getURL(t, fx, "/dev/bin/linux/ubuntu-22.04-amd64-r-4.4/PACKAGES", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon PACKAGES: status = %d, want 401", rec.Code)
	}
	rec = getURL(t, fx, "/dev/bin/linux/ubuntu-22.04-amd64-r-4.4/alpha_1.0.0.tar.gz", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon tarball: status = %d, want 401", rec.Code)
	}
}
