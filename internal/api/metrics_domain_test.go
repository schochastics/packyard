package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestPublishEmitsCounter(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a")) // created
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("b")) // overwrote
	publishSource(t, fx, "prod", "beta", "1.0.0", []byte("c")) // created
	publishSource(t, fx, "prod", "beta", "1.0.0", []byte("c")) // already_existed

	rec := doGet(t, fx, "/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: %d", rec.Code)
	}
	body := rec.Body.String()

	cases := []string{
		`pakman_publish_total{channel="dev",result="created"}`,
		`pakman_publish_total{channel="dev",result="overwrote"}`,
		`pakman_publish_total{channel="prod",result="created"}`,
		`pakman_publish_total{channel="prod",result="already_existed"}`,
	}
	for _, want := range cases {
		if !strings.Contains(body, want) {
			t.Errorf("metric line missing: %s", want)
		}
	}
}

func TestYankEmitsCounter(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))
	rec := doYank(t, fx, "dev", "alpha", "1.0.0", fx.token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("yank: %d", rec.Code)
	}

	body := doGet(t, fx, "/metrics", "").Body.String()
	if !strings.Contains(body, `pakman_yank_total{channel="dev"}`) {
		t.Errorf("yank counter missing: %s", truncate(body, 600))
	}
}

func TestDeleteEmitsCounter(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("a"))
	rec := doDelete(t, fx, "dev", "alpha", "1.0.0", fx.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}

	body := doGet(t, fx, "/metrics", "").Body.String()
	if !strings.Contains(body, `pakman_delete_total{channel="dev"}`) {
		t.Errorf("delete counter missing: %s", truncate(body, 600))
	}
}

func TestCASBytesReflectsPublishedContent(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)
	publishSource(t, fx, "dev", "alpha", "1.0.0", []byte("twelve bytes"))
	// 12 source bytes + 0 binary bytes = 12.

	body := doGet(t, fx, "/metrics", "").Body.String()
	if !strings.Contains(body, "pakman_cas_bytes 12") {
		t.Errorf("cas_bytes gauge not 12: %s", truncate(body, 600))
	}

	// Deleting reclaims logically.
	_ = doDelete(t, fx, "dev", "alpha", "1.0.0", fx.token)
	body = doGet(t, fx, "/metrics", "").Body.String()
	if !strings.Contains(body, "pakman_cas_bytes 0") {
		t.Errorf("cas_bytes did not return to 0 after delete: %s", truncate(body, 600))
	}
}

func TestTokenAdminCountersFire(t *testing.T) {
	t.Parallel()

	fx := newPublishFixture(t)

	// Create a token via the admin endpoint.
	rec := doAdmin(t, fx, http.MethodPost, "/api/v1/admin/tokens", fx.token,
		`{"label":"m","scopes":["publish:dev"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}

	body := doGet(t, fx, "/metrics", "").Body.String()
	if !strings.Contains(body, "pakman_token_create_total 1") {
		t.Errorf("token_create_total != 1: %s", truncate(body, 600))
	}
	if !strings.Contains(body, "pakman_token_revoke_total 0") {
		t.Errorf("token_revoke_total != 0: %s", truncate(body, 600))
	}
}
