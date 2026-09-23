package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

// rUA returns the User-Agent R sends for the given version.
func rUA(version string) string {
	return "R (" + version + " x86_64-pc-linux-gnu x86_64 linux-gnu)"
}

// getLinux GETs path with an R User-Agent (ua == "" sends none).
func getLinux(t *testing.T, fx *publishFixture, path, token, ua string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", ua)
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	return rec
}

const devLinux = "/dev/__linux__/jammy/latest/src/contrib"

func TestRMinorFromUserAgent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ua   string
		want string
		ok   bool
	}{
		{"R (4.4.3 x86_64-pc-linux-gnu x86_64 linux-gnu)", "4.4", true},
		{"R (4.5.0 aarch64-unknown-linux-gnu aarch64 linux-gnu)", "4.5", true},
		{"R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu) pkgcache/2.2.0", "4.6", true},
		{"libcurl/8.5.0 R (4.4.1 x86_64-pc-linux-gnu x86_64 linux-gnu)", "4.4", true},
		{"R (4.10.0)", "4.10", true},
		// Collected by make e2e: Posit's R builds (R/x.y.z (<os>) prefix),
		// renv on top of them; pak downloads with R's own User-Agent.
		{"R/4.4.3 (ubuntu-22.04) R (4.4.3 x86_64-pc-linux-gnu x86_64 linux-gnu)", "4.4", true},
		{"R/4.5.3 (almalinux-9.8) R (4.5.3 x86_64-pc-linux-gnu x86_64 linux-gnu)", "4.5", true},
		{"renv (1.2.4); R/4.4.3 (ubuntu-22.04) R (4.4.3 x86_64-pc-linux-gnu x86_64 linux-gnu)", "4.4", true},
		{"curl/8.5.0", "", false},
		{"", "", false},
		{"RStudio (2024.12)", "", false},
	}
	for _, c := range cases {
		got, ok := rMinorFromUserAgent(c.ua)
		if got != c.want || ok != c.ok {
			t.Errorf("rMinorFromUserAgent(%q) = %q, %v; want %q, %v", c.ua, got, ok, c.want, c.ok)
		}
	}
}

func TestLinuxPACKAGESMixesBinaryAndSource(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishWithBinary(t, fx, "dev", "alpha", "1.0.0", "r-4.4")
	publishSource(t, fx, "dev", "beta", "1.0.0", []byte("src only"))

	rec := getLinux(t, fx, devLinux+"/PACKAGES", fx.token, rUA("4.4.3"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	want := "Package: alpha\nVersion: 1.0.0\nBuilt: R 4.4.0; x86_64-pc-linux-gnu; ; unix\n\n" +
		"Package: beta\nVersion: 1.0.0\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("PACKAGES =\n%s\nwant\n%s", got, want)
	}
	if v := rec.Header().Get("Vary"); v != "User-Agent" {
		t.Errorf("Vary = %q, want User-Agent", v)
	}

	// R 4.5 has a cell but alpha has no binary for it: source entries.
	rec = getLinux(t, fx, devLinux+"/PACKAGES.gz", fx.token, rUA("4.5.1"))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("gz: status = %d, content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestLinuxTarballPicksCellFromUserAgent(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	srcBody, binBody := publishWithBinary(t, fx, "dev", "alpha", "1.0.0", "r-4.4")

	cases := []struct {
		name string
		ua   string
		want []byte
	}{
		{"R 4.4 gets the binary", rUA("4.4.3"), binBody},
		{"no R version falls back to default_r_minor 4.4", "curl/8.5.0", binBody},
		{"R 4.5 has a cell but no binary: source", rUA("4.5.0"), srcBody},
		{"R 4.6 has no cell: source", rUA("4.6.1"), srcBody},
	}
	for _, c := range cases {
		for _, path := range []string{
			devLinux + "/alpha_1.0.0.tar.gz",
			devLinux + "/Archive/alpha/alpha_1.0.0.tar.gz",
		} {
			rec := getLinux(t, fx, path, fx.token, c.ua)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s: status = %d body %s", c.name, path, rec.Code, rec.Body.String())
			}
			if !bytes.Equal(rec.Body.Bytes(), c.want) {
				t.Errorf("%s %s: body = %q, want %q", c.name, path, rec.Body.Bytes(), c.want)
			}
		}
	}
}

func TestLinuxRejectsWrongDistroAndSnapshot(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishWithBinary(t, fx, "dev", "alpha", "1.0.0", "r-4.4")

	rec := getLinux(t, fx, "/dev/__linux__/noble/latest/src/contrib/PACKAGES", fx.token, rUA("4.4.3"))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `this repository serves \"jammy\"`) {
		t.Errorf("wrong distro: status = %d body %s", rec.Code, rec.Body.String())
	}
	rec = getLinux(t, fx, "/dev/__linux__/jammy/2026-01-01/src/contrib/alpha_1.0.0.tar.gz", fx.token, rUA("4.4.3"))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "snapshots are not supported") {
		t.Errorf("dated snapshot: status = %d body %s", rec.Code, rec.Body.String())
	}
}

func TestLinuxArchiveRejectsPackageMismatch(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishWithBinary(t, fx, "dev", "alpha", "1.0.0", "r-4.4")

	rec := getLinux(t, fx, devLinux+"/Archive/beta/alpha_1.0.0.tar.gz", fx.token, rUA("4.4.3"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestLinuxUnknownChannel404(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	rec := getLinux(t, fx, "/nope/__linux__/jammy/latest/src/contrib/PACKAGES", fx.token, rUA("4.4.3"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestLinuxRoutesHonorReadScope(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishWithBinary(t, fx, "dev", "alpha", "1.0.0", "r-4.4")

	rec := getLinux(t, fx, devLinux+"/PACKAGES", "", rUA("4.4.3"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon PACKAGES: status = %d, want 401", rec.Code)
	}
	rec = getLinux(t, fx, devLinux+"/alpha_1.0.0.tar.gz", "", rUA("4.4.3"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon tarball: status = %d, want 401", rec.Code)
	}
}
