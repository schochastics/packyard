package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCRANProtocolEndToEnd exercises the full read/write cycle the way
// an R client would: publish a source package that actually looks like
// an R package (real DESCRIPTION + NAMESPACE inside a gzipped tar),
// then fetch PACKAGES, parse it, and download the tarball over HTTP
// from a live httptest.Server. Success means every URL R would hit
// for `install.packages("pakmantest", repos = "http://pakman/<channel>/")`
// returns sensible bytes.
//
// We don't actually untar-and-install — that would require R in the
// test environment. The goal here is protocol compliance at the HTTP
// and tarball-byte level, which is what reviewers will break first.
func TestCRANProtocolEndToEnd(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	ts := httptest.NewServer(fx.mux)
	t.Cleanup(ts.Close)

	// 1. Build a real R source tarball.
	const pkgName = "pakmantest"
	const pkgVersion = "0.1.0"
	tarball := buildRPackageTarball(t, pkgName, pkgVersion)

	// 2. Publish it via the real HTTP surface.
	publishBody, publishCT := buildPublishBody(t, map[string]any{
		"source":              "source",
		"description_version": pkgVersion,
	}, publishPart{name: "source", body: tarball})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/api/v1/packages/dev/"+pkgName+"/"+pkgVersion, publishBody)
	if err != nil {
		t.Fatalf("new publish req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+fx.token)
	req.Header.Set("Content-Type", publishCT)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("publish status = %d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// 3. Fetch PACKAGES exactly as R would. R hits PACKAGES.gz first
	//    then falls back to PACKAGES.
	gzResp := getWithToken(t, ts, "/dev/src/contrib/PACKAGES.gz", fx.token)
	plain := gunzip(t, gzResp)
	entries := parseDCF(t, plain)

	var entry map[string]string
	for _, e := range entries {
		if e["Package"] == pkgName {
			entry = e
			break
		}
	}
	if entry == nil {
		t.Fatalf("package %q not in PACKAGES: %s", pkgName, plain)
	}
	if entry["Version"] != pkgVersion {
		t.Errorf("PACKAGES Version = %q, want %q", entry["Version"], pkgVersion)
	}

	// 4. Download the source tarball R would pick (bytes match publish).
	tarURL := fmt.Sprintf("/dev/src/contrib/%s_%s.tar.gz", pkgName, pkgVersion)
	downloaded := getWithToken(t, ts, tarURL, fx.token)
	if !bytes.Equal(downloaded, tarball) {
		t.Fatalf("downloaded tarball differs from published (%d vs %d bytes)",
			len(downloaded), len(tarball))
	}

	// 5. The tarball we got back must untar and have a real DESCRIPTION
	//    at <pkg>/DESCRIPTION — that's R's first check post-download.
	desc := extractDescriptionFromTarball(t, downloaded, pkgName)
	if !strings.Contains(desc, "Package: "+pkgName) {
		t.Errorf("extracted DESCRIPTION missing Package line: %q", desc)
	}
	if !strings.Contains(desc, "Version: "+pkgVersion) {
		t.Errorf("extracted DESCRIPTION missing Version line: %q", desc)
	}

	// 6. Default-channel alias: prod is default in the fixture. Publish
	//    the same bytes there and verify /src/contrib/PACKAGES resolves
	//    to prod without mentioning channel in the URL.
	prodBody, prodCT := buildPublishBody(t, map[string]any{
		"source":              "source",
		"description_version": pkgVersion,
	}, publishPart{name: "source", body: tarball})
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/api/v1/packages/prod/"+pkgName+"/"+pkgVersion, prodBody)
	req2.Header.Set("Authorization", "Bearer "+fx.token)
	req2.Header.Set("Content-Type", prodCT)
	resp2, err := ts.Client().Do(req2)
	if err != nil {
		t.Fatalf("publish prod: %v", err)
	}
	_ = resp2.Body.Close()

	plain2 := getWithToken(t, ts, "/src/contrib/PACKAGES", fx.token)
	entries2 := parseDCF(t, plain2)
	foundOnAlias := false
	for _, e := range entries2 {
		if e["Package"] == pkgName {
			foundOnAlias = true
			break
		}
	}
	if !foundOnAlias {
		t.Errorf("default-channel alias PACKAGES missing %q: %q", pkgName, plain2)
	}
}

// buildRPackageTarball creates a minimal-but-valid R source tarball
// containing the usual DESCRIPTION + NAMESPACE + a hello.R.
func buildRPackageTarball(t *testing.T, name, version string) []byte {
	t.Helper()

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(zw)

	addFile := func(path, body string) {
		hdr := &tar.Header{
			Name: path,
			Mode: 0o644,
			Size: int64(len(body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar write header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write body: %v", err)
		}
	}

	desc := fmt.Sprintf(
		"Package: %s\nVersion: %s\nTitle: A pakman smoke-test package\n"+
			"License: MIT + file LICENSE\nEncoding: UTF-8\n",
		name, version)
	addFile(name+"/DESCRIPTION", desc)
	addFile(name+"/NAMESPACE", "exportPattern(\"^[^.]\")\n")
	addFile(name+"/R/hello.R", "hello <- function() \"hi from pakman\"\n")

	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return gz.Bytes()
}

// extractDescriptionFromTarball reads the top-level DESCRIPTION from a
// gzipped tarball. Mirrors how R's untar + DESCRIPTION parse would
// start.
func extractDescriptionFromTarball(t *testing.T, data []byte, pkgName string) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Name == pkgName+"/DESCRIPTION" {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read DESCRIPTION: %v", err)
			}
			return string(body)
		}
	}
	t.Fatalf("DESCRIPTION missing from tarball")
	return ""
}

// parseDCF parses a PACKAGES-style DCF into a slice of stanzas. Each
// stanza is a map of field → value. Simplified parser: no folded
// continuation lines, which our current PACKAGES output doesn't use.
func parseDCF(t *testing.T, body []byte) []map[string]string {
	t.Helper()
	var out []map[string]string
	stanza := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" {
			if len(stanza) > 0 {
				out = append(out, stanza)
				stanza = map[string]string{}
			}
			continue
		}
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		field := strings.TrimSpace(line[:i])
		value := strings.TrimSpace(line[i+1:])
		stanza[field] = value
	}
	if len(stanza) > 0 {
		out = append(out, stanza)
	}
	return out
}

func getWithToken(t *testing.T, ts *httptest.Server, path, token string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d body %s", path, resp.StatusCode, body)
	}
	return body
}

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gunzip: %v", err)
	}
	return out
}
