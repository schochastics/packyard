package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schochastics/pakman/internal/auth"
	"github.com/schochastics/pakman/internal/cas"
	"github.com/schochastics/pakman/internal/config"
	"github.com/schochastics/pakman/internal/db"
)

// publishTestFixture seeds a DB, CAS, matrix and two channels: "dev"
// (mutable) and "prod" (immutable). Returns a server handler and a
// token granting publish:*.
type publishFixture struct {
	deps  Deps
	mux   http.Handler
	token string
}

func newPublishFixture(t *testing.T) *publishFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	database, err := db.Open(ctx, filepath.Join(dir, "pakman.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	store, err := cas.New(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}

	// Seed channels.
	cfg, err := config.DecodeChannels(strings.NewReader(`
channels:
  - name: dev
    overwrite_policy: mutable
  - name: prod
    overwrite_policy: immutable
    default: true
`))
	if err != nil {
		t.Fatalf("decode channels: %v", err)
	}
	if _, err := config.ReconcileChannels(ctx, database.DB, cfg); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Seed matrix with two cells (one for tests that need a binary).
	matrix, err := config.DecodeMatrix(strings.NewReader(`
cells:
  - name: ubuntu-22.04-amd64-r-4.4
    os: linux
    os_version: ubuntu-22.04
    arch: amd64
    r_minor: "4.4"
  - name: ubuntu-22.04-arm64-r-4.4
    os: linux
    os_version: ubuntu-22.04
    arch: arm64
    r_minor: "4.4"
`))
	if err != nil {
		t.Fatalf("decode matrix: %v", err)
	}

	deps := Deps{DB: database, CAS: store, Matrix: matrix}

	// Seed a token with broad publish scope.
	plaintext, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	_, err = database.ExecContext(ctx, `
		INSERT INTO tokens(token_sha256, scopes_csv, label) VALUES (?, ?, ?)
	`, auth.HashToken(plaintext), "publish:*,yank:*,read:*,admin", "tests")
	if err != nil {
		t.Fatalf("insert token: %v", err)
	}

	return &publishFixture{deps: deps, mux: NewMux(deps), token: plaintext}
}

// publishPart is a tiny helper for building a multipart body.
type publishPart struct {
	name string
	body []byte
}

func buildPublishBody(t *testing.T, manifest any, parts ...publishPart) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = new(bytes.Buffer)
	mw := multipart.NewWriter(body)

	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	mp, err := mw.CreateFormField("manifest")
	if err != nil {
		t.Fatalf("CreateFormField manifest: %v", err)
	}
	if _, err := mp.Write(manifestBytes); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	for _, p := range parts {
		pw, err := mw.CreateFormFile(p.name, p.name+".bin")
		if err != nil {
			t.Fatalf("CreateFormFile %s: %v", p.name, err)
		}
		if _, err := pw.Write(p.body); err != nil {
			t.Fatalf("write %s: %v", p.name, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return body, mw.FormDataContentType()
}

func doPublish(t *testing.T, fx *publishFixture, channel, name, version, token string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/v1/packages/" + channel + "/" + name + "/" + version
	req := httptest.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	return rec
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPublishHappyPathInsertsAndEmitsEvent(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	srcBody := []byte("fake R source tarball bytes")
	binBody := []byte("fake binary tarball bytes")

	manifest := map[string]any{
		"source": "source",
		"binaries": []map[string]any{
			{"cell": "ubuntu-22.04-amd64-r-4.4", "part": "bin_amd64"},
		},
	}
	body, ct := buildPublishBody(t, manifest,
		publishPart{name: "source", body: srcBody},
		publishPart{name: "bin_amd64", body: binBody},
	)

	rec := doPublish(t, fx, "dev", "mypkg", "1.0.0", fx.token, body, ct)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	var resp PublishResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SourceSHA256 != sha256Hex(srcBody) {
		t.Errorf("SourceSHA256 = %s, want %s", resp.SourceSHA256, sha256Hex(srcBody))
	}
	if resp.AlreadyExisted || resp.Overwritten {
		t.Errorf("unexpected flags: %+v", resp)
	}
	if len(resp.Binaries) != 1 || resp.Binaries[0].SHA256 != sha256Hex(binBody) {
		t.Errorf("binaries mismatch: %+v", resp.Binaries)
	}

	// Verify blobs landed in CAS.
	if !fx.deps.CAS.Has(resp.SourceSHA256) {
		t.Error("source blob missing from CAS")
	}
	if !fx.deps.CAS.Has(resp.Binaries[0].SHA256) {
		t.Error("binary blob missing from CAS")
	}

	// Verify DB rows.
	var count int
	if err := fx.deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM packages WHERE channel='dev' AND name='mypkg' AND version='1.0.0'`).Scan(&count); err != nil {
		t.Fatalf("count packages: %v", err)
	}
	if count != 1 {
		t.Errorf("packages count = %d, want 1", count)
	}

	// Event appended.
	var eventType string
	if err := fx.deps.DB.QueryRowContext(context.Background(),
		`SELECT type FROM events ORDER BY id DESC LIMIT 1`).Scan(&eventType); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if eventType != "publish" {
		t.Errorf("event type = %q, want publish", eventType)
	}
}

func TestPublishImmutableIdempotentOnIdenticalBytes(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	src := []byte("stable source")

	manifest := map[string]any{"source": "source"}
	body1, ct := buildPublishBody(t, manifest, publishPart{name: "source", body: src})
	rec := doPublish(t, fx, "prod", "mypkg", "1.0.0", fx.token, body1, ct)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first publish: status %d body %s", rec.Code, rec.Body.String())
	}

	body2, ct2 := buildPublishBody(t, manifest, publishPart{name: "source", body: src})
	rec2 := doPublish(t, fx, "prod", "mypkg", "1.0.0", fx.token, body2, ct2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay: status %d body %s, want 200", rec2.Code, rec2.Body.String())
	}
	var resp PublishResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.AlreadyExisted {
		t.Error("already_existed = false on idempotent replay")
	}
}

func TestPublishImmutableConflictOnDifferentBytes(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	manifest := map[string]any{"source": "source"}

	body1, ct := buildPublishBody(t, manifest, publishPart{name: "source", body: []byte("v1")})
	if rec := doPublish(t, fx, "prod", "mypkg", "1.0.0", fx.token, body1, ct); rec.Code != http.StatusCreated {
		t.Fatalf("first: status %d body %s", rec.Code, rec.Body.String())
	}

	body2, ct2 := buildPublishBody(t, manifest, publishPart{name: "source", body: []byte("v2")})
	rec := doPublish(t, fx, "prod", "mypkg", "1.0.0", fx.token, body2, ct2)
	if rec.Code != http.StatusConflict {
		t.Fatalf("tampered replay: status %d body %s, want 409", rec.Code, rec.Body.String())
	}
	var errBody ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatal(err)
	}
	if errBody.ErrorCode != CodeVersionImmutable {
		t.Errorf("error_code = %q, want %q", errBody.ErrorCode, CodeVersionImmutable)
	}
}

func TestPublishMutableOverwriteReplacesRow(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	manifest := map[string]any{"source": "source"}

	body1, ct := buildPublishBody(t, manifest, publishPart{name: "source", body: []byte("v1")})
	if rec := doPublish(t, fx, "dev", "mypkg", "1.0.0", fx.token, body1, ct); rec.Code != http.StatusCreated {
		t.Fatalf("first: status %d body %s", rec.Code, rec.Body.String())
	}

	body2, ct2 := buildPublishBody(t, manifest, publishPart{name: "source", body: []byte("v2")})
	rec := doPublish(t, fx, "dev", "mypkg", "1.0.0", fx.token, body2, ct2)
	if rec.Code != http.StatusCreated {
		t.Fatalf("overwrite: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp PublishResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Overwritten {
		t.Error("overwritten = false on mutable re-publish")
	}

	var sha string
	if err := fx.deps.DB.QueryRowContext(context.Background(),
		`SELECT source_sha256 FROM packages WHERE channel='dev' AND name='mypkg' AND version='1.0.0'`).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	if sha != sha256Hex([]byte("v2")) {
		t.Errorf("source_sha256 after overwrite = %s, want v2 hash", sha)
	}
}

func TestPublishUnknownCellReturns400(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	manifest := map[string]any{
		"source": "source",
		"binaries": []map[string]any{
			{"cell": "bogus-cell", "part": "bin1"},
		},
	}
	body, ct := buildPublishBody(t, manifest,
		publishPart{name: "source", body: []byte("src")},
		publishPart{name: "bin1", body: []byte("bin")},
	)
	rec := doPublish(t, fx, "dev", "mypkg", "1.0.0", fx.token, body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPublishUnknownChannelReturns404(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	body, ct := buildPublishBody(t, map[string]any{"source": "source"},
		publishPart{name: "source", body: []byte("x")})
	rec := doPublish(t, fx, "nosuchchannel", "mypkg", "1.0.0", fx.token, body, ct)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPublishMissingTokenReturns401(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	body, ct := buildPublishBody(t, map[string]any{"source": "source"},
		publishPart{name: "source", body: []byte("x")})
	rec := doPublish(t, fx, "dev", "mypkg", "1.0.0", "", body, ct)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPublishInsufficientScopeReturns403(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	// Seed a token that can only read, not publish.
	reader, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fx.deps.DB.ExecContext(context.Background(), `
		INSERT INTO tokens(token_sha256, scopes_csv, label) VALUES (?, ?, ?)
	`, auth.HashToken(reader), "read:*", "readonly")
	if err != nil {
		t.Fatal(err)
	}

	body, ct := buildPublishBody(t, map[string]any{"source": "source"},
		publishPart{name: "source", body: []byte("x")})
	rec := doPublish(t, fx, "dev", "mypkg", "1.0.0", reader, body, ct)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPublishMissingManifestReturns400(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// Build a body with only a source part, no manifest.
	body := new(bytes.Buffer)
	mw := multipart.NewWriter(body)
	pw, _ := mw.CreateFormFile("source", "source.bin")
	_, _ = pw.Write([]byte("x"))
	_ = mw.Close()

	url := "/api/v1/packages/dev/mypkg/1.0.0"
	req := httptest.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+fx.token)

	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPublishInvalidManifestJSONReturns400(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	body := new(bytes.Buffer)
	mw := multipart.NewWriter(body)
	mp, _ := mw.CreateFormField("manifest")
	_, _ = io.WriteString(mp, "{ not json")
	sp, _ := mw.CreateFormFile("source", "source.bin")
	_, _ = sp.Write([]byte("x"))
	_ = mw.Close()

	url := "/api/v1/packages/dev/mypkg/1.0.0"
	req := httptest.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+fx.token)

	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPublishInvalidPackageNameReturns400(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	body, ct := buildPublishBody(t, map[string]any{"source": "source"},
		publishPart{name: "source", body: []byte("x")})
	// URL-safe but not a valid R name (starts with digit).
	rec := doPublish(t, fx, "dev", "1badname", "1.0.0", fx.token, body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPublishVersionMismatchInManifestReturns400(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	manifest := map[string]any{
		"source":              "source",
		"description_version": "9.9.9",
	}
	body, ct := buildPublishBody(t, manifest,
		publishPart{name: "source", body: []byte("x")})
	rec := doPublish(t, fx, "dev", "mypkg", "1.0.0", fx.token, body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	fmt.Println(rec.Body.String()) // useful when the test tweaks the message
}
