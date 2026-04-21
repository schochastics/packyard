package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// The fixture makes "prod" the default channel (immutable). These tests
// exercise the /src/contrib/... and /bin/linux/... alias routes.

func TestDefaultAliasSourcePACKAGES(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "prod", "alpha", "1.0.0", []byte("x"))
	// Publish something to dev too, so we can verify the alias routes
	// strictly to prod rather than dumping everything.
	publishSource(t, fx, "dev", "beta", "1.0.0", []byte("y"))

	rec := getURL(t, fx, "/src/contrib/PACKAGES", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	s := rec.Body.String()
	if !strings.Contains(s, "Package: alpha") {
		t.Errorf("prod's alpha missing from default alias: %q", s)
	}
	if strings.Contains(s, "Package: beta") {
		t.Errorf("dev's beta leaked through default alias: %q", s)
	}
}

func TestDefaultAliasSourceTarball(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	content := []byte("default-channel tarball bytes")
	publishSource(t, fx, "prod", "alpha", "1.0.0", content)

	rec := getURL(t, fx, "/src/contrib/alpha_1.0.0.tar.gz", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Errorf("default-alias tarball bytes differ")
	}
}

func TestDefaultAliasBinaryPACKAGES(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishWithBinary(t, fx, "prod", "alpha", "1.0.0", "ubuntu-22.04-amd64-r-4.4")

	rec := getURL(t, fx, "/bin/linux/ubuntu-22.04-amd64-r-4.4/PACKAGES", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Package: alpha") {
		t.Errorf("alpha missing: %q", rec.Body.String())
	}
}
