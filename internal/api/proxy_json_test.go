package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestProxyChannelAppearsInJSON covers the api-surface side of the
// proxy feature: a configured proxy channel must surface its kind +
// upstream_source_url in GET /api/v1/channels. The end-to-end fixture
// is reused from proxy_test.go.
func TestProxyChannelAppearsInJSON(t *testing.T) {
	t.Parallel()
	f := newProxyFixture(t)
	token := seedTokenRow(t, f.deps.DB.DB, "ops", "admin", false)

	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/api/v1/channels", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"kind":"proxy"`) {
		t.Errorf("response missing kind field: %s", body)
	}
	if !strings.Contains(string(body), `"upstream_source_url":"`+f.upSrv.URL+`"`) {
		t.Errorf("response missing upstream_source_url: %s", body)
	}
}
