package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func doAttach(t *testing.T, fx *publishFixture, channel, name, version, cell, token string, bin []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	pw, err := mw.CreateFormFile("binary", name+"_"+version+".tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(bin); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/packages/"+channel+"/"+name+"/"+version+"/binaries/"+cell, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	return rec
}

func TestAttachBinaryStatuses(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "prod", "alpha", "1.0.0", []byte("src"))
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("src"))

	cases := []struct {
		name    string
		channel string
		pkg     string
		cell    string
		body    string
		status  int
		code    string
	}{
		{"new cell on immutable channel", "prod", "alpha", "r-4.5", "bin-a", http.StatusCreated, ""},
		{"same bytes again is idempotent", "prod", "alpha", "r-4.5", "bin-a", http.StatusOK, ""},
		{"different bytes on immutable", "prod", "alpha", "r-4.5", "bin-b", http.StatusConflict, CodeVersionImmutable},
		{"new cell on mutable channel", "dev", "alpha", "r-4.5", "bin-a", http.StatusCreated, ""},
		{"overwrite on mutable channel", "dev", "alpha", "r-4.5", "bin-b", http.StatusOK, ""},
		{"source row missing", "prod", "nosuch", "r-4.5", "bin", http.StatusNotFound, CodeNotFound},
		{"cell not in matrix", "prod", "alpha", "r-9.9", "bin", http.StatusBadRequest, CodeBadRequest},
		{"unknown channel", "nope", "alpha", "r-4.5", "bin", http.StatusNotFound, CodeNotFound},
	}
	for _, c := range cases {
		rec := doAttach(t, fx, c.channel, c.pkg, "1.0.0", c.cell, fx.token, []byte(c.body))
		if rec.Code != c.status {
			t.Errorf("%s: status = %d, want %d; body %s", c.name, rec.Code, c.status, rec.Body.String())
			continue
		}
		if c.code != "" {
			var e struct {
				ErrorCode string `json:"error_code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			if e.ErrorCode != c.code {
				t.Errorf("%s: error_code = %q, want %q", c.name, e.ErrorCode, c.code)
			}
		}
	}

	// The attached binary is served to R 4.5 clients.
	rec := getLinux(t, fx, "/prod/__linux__/jammy/latest/src/contrib/alpha_1.0.0.tar.gz", fx.token, rUA("4.5.0"))
	if rec.Body.String() != "bin-a" {
		t.Errorf("R 4.5 download = %q, want bin-a", rec.Body.String())
	}

	var n int
	if err := fx.deps.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE type = 'binary_attach'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 { // prod create, dev create, dev overwrite
		t.Errorf("binary_attach events = %d, want 3", n)
	}
}

func TestAttachBinaryRequiresPublishScope(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "prod", "alpha", "1.0.0", []byte("src"))
	tok := seedScopedToken(t, fx, "dev-ci", "publish:dev")
	if rec := doAttach(t, fx, "prod", "alpha", "1.0.0", "r-4.5", tok, []byte("b")); rec.Code != http.StatusForbidden {
		t.Errorf("wrong channel scope: status = %d, want 403", rec.Code)
	}
	if rec := doAttach(t, fx, "prod", "alpha", "1.0.0", "r-4.5", "", []byte("b")); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon: status = %d, want 401", rec.Code)
	}
}

func TestAttachBinaryRejectsWrongPart(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "prod", "alpha", "1.0.0", []byte("src"))
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("source", "x")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/packages/prod/alpha/1.0.0/binaries/r-4.5", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+fx.token)
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestPublishReportsMissingCells(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	reqBody, ct := buildPublishBody(t, map[string]any{
		"source":   "source",
		"binaries": []map[string]any{{"cell": "r-4.4", "part": "bin"}},
	}, publishPart{name: "source", body: []byte("s")}, publishPart{name: "bin", body: []byte("b")})
	rec := doPublish(t, fx, "prod", "alpha", "1.0.0", fx.token, reqBody, ct)
	var resp PublishResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.MissingCells) != 1 || resp.MissingCells[0] != "r-4.5" {
		t.Errorf("missing_cells = %v, want [r-4.5]", resp.MissingCells)
	}

	// After attaching r-4.5, an idempotent republish reports none.
	doAttach(t, fx, "prod", "alpha", "1.0.0", "r-4.5", fx.token, []byte("b45"))
	reqBody, ct = buildPublishBody(t, map[string]any{"source": "source"}, publishPart{name: "source", body: []byte("s")})
	rec = doPublish(t, fx, "prod", "alpha", "1.0.0", fx.token, reqBody, ct)
	resp = PublishResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.AlreadyExisted || len(resp.MissingCells) != 0 || resp.MissingCells == nil {
		t.Errorf("replay: already_existed=%v missing_cells=%#v", resp.AlreadyExisted, resp.MissingCells)
	}
}

func TestListMissingBinaries(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishWithBinary(t, fx, "prod", "alpha", "1.0.0", "r-4.4")
	publishSource(t, fx, "prod", "beta", "0.9", []byte("old"))
	publishSource(t, fx, "prod", "beta", "1.0", []byte("new"))

	get := func(path, token string) (*httptest.ResponseRecorder, ListMissingBinariesResponse) {
		rec := doGet(t, fx, path, token)
		var resp ListMissingBinariesResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec, resp
	}

	rec, resp := get("/api/v1/channels/prod/missing-binaries", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	// Only current versions: beta 0.9 is archived and never listed.
	want := []MissingBinary{
		{Name: "beta", Version: "1.0", Cell: "r-4.4"},
		{Name: "alpha", Version: "1.0.0", Cell: "r-4.5"},
		{Name: "beta", Version: "1.0", Cell: "r-4.5"},
	}
	if len(resp.Missing) != len(want) {
		t.Fatalf("missing = %+v, want %+v", resp.Missing, want)
	}
	for i := range want {
		if resp.Missing[i] != want[i] {
			t.Errorf("missing[%d] = %+v, want %+v", i, resp.Missing[i], want[i])
		}
	}

	_, resp = get("/api/v1/channels/prod/missing-binaries?cell=r-4.4", fx.token)
	if len(resp.Missing) != 1 || resp.Missing[0].Name != "beta" {
		t.Errorf("cell filter: %+v", resp.Missing)
	}
	if rec, _ := get("/api/v1/channels/prod/missing-binaries?cell=r-9.9", fx.token); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown cell: status = %d, want 400", rec.Code)
	}

	// A publish token for the channel is enough; other channels' are not.
	if rec, _ := get("/api/v1/channels/prod/missing-binaries", seedScopedToken(t, fx, "ci", "publish:prod")); rec.Code != http.StatusOK {
		t.Errorf("publish:prod token: status = %d", rec.Code)
	}
	if rec, _ := get("/api/v1/channels/prod/missing-binaries", seedScopedToken(t, fx, "ci-dev", "publish:dev")); rec.Code != http.StatusForbidden {
		t.Errorf("publish:dev token: status = %d, want 403", rec.Code)
	}
	if rec, _ := get("/api/v1/channels/nope/missing-binaries", fx.token); rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel: status = %d, want 404", rec.Code)
	}
}
