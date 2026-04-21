package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestUIMountSmoke drives the full api.NewMux with a UI session key
// set, walking the login flow end-to-end and fetching every page the
// nav links to. A regression here would catch a route the top-level
// mux rejects (StripPrefix miscount, route conflict, auth gate leak).
func TestUIMountSmoke(t *testing.T) {
	deps := newAuthTestDeps(t)
	deps.UISessionKey = []byte("smoke-test-key-32-bytes-padded!!")
	tok := seedTokenRow(t, deps.DB.DB, "smoke", "admin", false)

	srv := httptest.NewServer(NewMux(deps))
	t.Cleanup(srv.Close)

	// Manual cookie jar so we can inspect cookies on each hop.
	jar := newCookieJar()

	// 1. GET /ui/ — unauthenticated, redirects to login.
	res := doReq(t, srv, jar, "GET", "/ui/", nil)
	if res.StatusCode != http.StatusFound {
		t.Fatalf("GET /ui/ status = %d; want 302", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/ui/login" {
		t.Errorf("GET /ui/ Location = %q; want /ui/login", loc)
	}

	// 2. GET /ui/login — login form.
	res = doReq(t, srv, jar, "GET", "/ui/login", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/login status = %d", res.StatusCode)
	}

	// 3. POST /ui/login with valid token — cookie set, redirects to /ui/.
	form := url.Values{"token": {tok}}
	res = doReq(t, srv, jar, "POST", "/ui/login",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		form.Encode(),
	)
	if res.StatusCode != http.StatusFound {
		t.Fatalf("POST /ui/login status = %d", res.StatusCode)
	}
	if len(jar.cookies("pakman_ui")) == 0 {
		t.Fatalf("expected session cookie after login")
	}

	// 4. Every nav destination must return 200.
	pages := []string{"/ui/", "/ui/events", "/ui/cells", "/ui/storage"}
	for _, p := range pages {
		res = doReq(t, srv, jar, "GET", p, nil)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d; want 200", p, res.StatusCode)
		}
	}

	// 5. Static asset served through the handler.
	res = doReq(t, srv, jar, "GET", "/ui/static/style.css", nil)
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /ui/static/style.css status = %d", res.StatusCode)
	}

	// 6. Logout clears the cookie.
	res = doReq(t, srv, jar, "POST", "/ui/logout", nil)
	if res.StatusCode != http.StatusFound {
		t.Errorf("POST /ui/logout status = %d", res.StatusCode)
	}
	// The MaxAge=-1 cookie wipes the jar entry immediately on merge.
	if got := jar.get("pakman_ui"); got != "" {
		t.Errorf("session cookie still present after logout: %q", got)
	}

	// 7. A follow-up GET /ui/ must redirect to login again.
	res = doReq(t, srv, jar, "GET", "/ui/", nil)
	if res.StatusCode != http.StatusFound {
		t.Errorf("post-logout /ui/ status = %d; want 302", res.StatusCode)
	}
}

// TestUIDisabledWhenNoSessionKey verifies /ui/ returns 404 when the
// operator hasn't opted into the UI. Keeps the blast radius small for
// CLI-only deployments that don't want an HTML surface at all.
func TestUIDisabledWhenNoSessionKey(t *testing.T) {
	deps := newAuthTestDeps(t)
	// deliberately leave UISessionKey empty

	srv := httptest.NewServer(NewMux(deps))
	t.Cleanup(srv.Close)

	res := doReq(t, srv, newCookieJar(), "GET", "/ui/", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /ui/ with no UISessionKey status = %d; want 404", res.StatusCode)
	}
}

// ---------- helpers -----------------------------------------------------

// cookieJar is a minimal stand-in for net/http/cookiejar that lets us
// inspect stored cookies by name (the stdlib jar's interface hides them
// behind a URL lookup).
type cookieJar struct{ m map[string]*http.Cookie }

func newCookieJar() *cookieJar { return &cookieJar{m: map[string]*http.Cookie{}} }

func (j *cookieJar) merge(cs []*http.Cookie) {
	for _, c := range cs {
		if c.MaxAge < 0 || c.Value == "" {
			delete(j.m, c.Name)
			continue
		}
		j.m[c.Name] = c
	}
}

func (j *cookieJar) cookies(name string) []*http.Cookie {
	if c, ok := j.m[name]; ok {
		return []*http.Cookie{c}
	}
	return nil
}

func (j *cookieJar) get(name string) string {
	if c, ok := j.m[name]; ok {
		return c.Value
	}
	return ""
}

func (j *cookieJar) applyTo(req *http.Request) {
	for _, c := range j.m {
		req.AddCookie(c)
	}
}

// httpResult carries the pieces of an http.Response the smoke test
// actually needs. Returning a struct instead of *http.Response keeps
// the body-close lifecycle inside doReq where bodyclose can see it.
type httpResult struct {
	StatusCode int
	Header     http.Header
}

func doReq(t *testing.T, srv *httptest.Server, jar *cookieJar, method, path string, extras ...any) httpResult {
	t.Helper()
	var body string
	var headers map[string]string
	for _, e := range extras {
		switch v := e.(type) {
		case map[string]string:
			headers = v
		case string:
			body = v
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	jar.applyTo(req)

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	jar.merge(resp.Cookies())
	return httpResult{StatusCode: resp.StatusCode, Header: resp.Header.Clone()}
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
