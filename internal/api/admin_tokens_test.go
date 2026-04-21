package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/schochastics/pakman/internal/auth"
)

func doAdmin(t *testing.T, fx *publishFixture, method, url, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *bytes.Buffer
	if body != "" {
		r = bytes.NewBufferString(body)
	} else {
		r = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, url, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	fx.mux.ServeHTTP(rec, req)
	return rec
}

func TestCreateTokenReturnsPlaintextOnce(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	body := `{"label":"ci-dev","scopes":["publish:dev","read:*"]}`

	rec := doAdmin(t, fx, http.MethodPost, "/api/v1/admin/tokens", fx.token, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp CreateTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Token, "pkm_") {
		t.Errorf("token missing pkm_ prefix: %q", resp.Token)
	}
	if resp.Label != "ci-dev" {
		t.Errorf("label = %q", resp.Label)
	}

	// The returned token must actually authenticate requests.
	rec = doAdmin(t, fx, http.MethodGet, "/api/v1/admin/tokens", resp.Token, "")
	// But this token has NO admin scope → 403.
	if rec.Code != http.StatusForbidden {
		t.Errorf("newly created publish-only token unexpectedly allowed to list: status %d", rec.Code)
	}

	// Verify list (via the original admin token) omits plaintext.
	listRec := doAdmin(t, fx, http.MethodGet, "/api/v1/admin/tokens", fx.token, "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: %d", listRec.Code)
	}
	if strings.Contains(listRec.Body.String(), resp.Token) {
		t.Error("plaintext token leaked in list response")
	}
	if strings.Contains(listRec.Body.String(), "\"token\"") {
		t.Error(`list response should not carry a "token" field at all`)
	}
}

func TestCreateTokenRejectsBadInputs(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"no label", `{"scopes":["admin"]}`, http.StatusBadRequest},
		{"empty label", `{"label":"","scopes":["admin"]}`, http.StatusBadRequest},
		{"no scopes", `{"label":"x"}`, http.StatusBadRequest},
		{"empty scopes", `{"label":"x","scopes":[]}`, http.StatusBadRequest},
		{"malformed scope", `{"label":"x","scopes":["not a scope"]}`, http.StatusBadRequest},
		{"unknown field", `{"label":"x","scopes":["admin"],"extra":1}`, http.StatusBadRequest},
		{"invalid json", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := doAdmin(t, fx, http.MethodPost, "/api/v1/admin/tokens", fx.token, tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestCreateTokenRequiresAdmin(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// Seed a token with publish:* but no admin.
	plaintext, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fx.deps.DB.ExecContext(context.Background(), `
		INSERT INTO tokens(token_sha256, scopes_csv, label) VALUES (?, ?, ?)
	`, auth.HashToken(plaintext), "publish:*", "not-admin")
	if err != nil {
		t.Fatal(err)
	}

	rec := doAdmin(t, fx, http.MethodPost, "/api/v1/admin/tokens", plaintext,
		`{"label":"malicious","scopes":["admin"]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin got %d, want 403", rec.Code)
	}

	// Anonymous → 401.
	rec = doAdmin(t, fx, http.MethodPost, "/api/v1/admin/tokens", "",
		`{"label":"x","scopes":["admin"]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon got %d, want 401", rec.Code)
	}
}

func TestListTokensShape(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// Create one via the API (adds to whatever fixture already seeded).
	rec := doAdmin(t, fx, http.MethodPost, "/api/v1/admin/tokens", fx.token,
		`{"label":"ci","scopes":["publish:dev","read:*"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	var created CreateTokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	list := doAdmin(t, fx, http.MethodGet, "/api/v1/admin/tokens", fx.token, "")
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d", list.Code)
	}
	var resp ListTokensResponse
	if err := json.Unmarshal(list.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	var got *TokenSummary
	for i := range resp.Tokens {
		if resp.Tokens[i].ID == created.ID {
			got = &resp.Tokens[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("created token id %d not in list: %+v", created.ID, resp.Tokens)
	}
	if got.Label != "ci" {
		t.Errorf("Label = %q", got.Label)
	}
	if len(got.Scopes) != 2 {
		t.Errorf("Scopes = %v, want 2 entries", got.Scopes)
	}
	if got.RevokedAt != nil {
		t.Errorf("fresh token has RevokedAt = %v, want nil", *got.RevokedAt)
	}
}

func TestRevokeTokenStampsRevokedAt(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// Create via API.
	rec := doAdmin(t, fx, http.MethodPost, "/api/v1/admin/tokens", fx.token,
		`{"label":"to-revoke","scopes":["publish:dev"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	var created CreateTokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Token works for publish:dev before revoke.
	body, ct := buildPublishBody(t, map[string]any{"source": "source"},
		publishPart{name: "source", body: []byte("x")})
	pubRec := doPublish(t, fx, "dev", "mypkg", "1.0.0", created.Token, body, ct)
	if pubRec.Code != http.StatusCreated {
		t.Fatalf("pre-revoke publish: %d %s", pubRec.Code, pubRec.Body.String())
	}

	// Revoke.
	delRec := doAdmin(t, fx, http.MethodDelete,
		fmt.Sprintf("/api/v1/admin/tokens/%d", created.ID), fx.token, "")
	if delRec.Code != http.StatusOK {
		t.Fatalf("revoke: %d body %s", delRec.Code, delRec.Body.String())
	}

	// List shows revoked_at populated.
	listRec := doAdmin(t, fx, http.MethodGet, "/api/v1/admin/tokens", fx.token, "")
	var list ListTokensResponse
	_ = json.Unmarshal(listRec.Body.Bytes(), &list)
	found := false
	for _, tk := range list.Tokens {
		if tk.ID == created.ID {
			found = true
			if tk.RevokedAt == nil || *tk.RevokedAt == "" {
				t.Error("RevokedAt not populated after revoke")
			}
		}
	}
	if !found {
		t.Error("revoked token missing from list")
	}

	// Revoked token rejected on next use.
	body2, ct2 := buildPublishBody(t, map[string]any{"source": "source"},
		publishPart{name: "source", body: []byte("x2")})
	pubRec2 := doPublish(t, fx, "dev", "mypkg", "1.0.1", created.Token, body2, ct2)
	if pubRec2.Code != http.StatusUnauthorized {
		t.Errorf("post-revoke: status %d, want 401", pubRec2.Code)
	}
}

func TestRevokeUnknownReturns404(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	rec := doAdmin(t, fx, http.MethodDelete, "/api/v1/admin/tokens/9999", fx.token, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRevokeBadIDReturns400(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	rec := doAdmin(t, fx, http.MethodDelete, "/api/v1/admin/tokens/0", fx.token, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	rec = doAdmin(t, fx, http.MethodDelete, "/api/v1/admin/tokens/-1", fx.token, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative id: status = %d, want 400", rec.Code)
	}
}

func TestRevokeIdempotent(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	rec := doAdmin(t, fx, http.MethodPost, "/api/v1/admin/tokens", fx.token,
		`{"label":"tmp","scopes":["publish:dev"]}`)
	var created CreateTokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	first := doAdmin(t, fx, http.MethodDelete,
		fmt.Sprintf("/api/v1/admin/tokens/%d", created.ID), fx.token, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first revoke: %d", first.Code)
	}
	second := doAdmin(t, fx, http.MethodDelete,
		fmt.Sprintf("/api/v1/admin/tokens/%d", created.ID), fx.token, "")
	if second.Code != http.StatusOK {
		t.Errorf("second revoke: %d, want 200 (idempotent)", second.Code)
	}
}
