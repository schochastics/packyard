package ui

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/schochastics/packyard/internal/auth"
	"github.com/schochastics/packyard/internal/config"
	"github.com/schochastics/packyard/internal/db"
)

func newTestHandler(t *testing.T) (*Handler, *db.DB) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	database, err := db.Open(ctx, filepath.Join(dir, "packyard.sqlite"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateEmbedded(ctx, database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	h, err := NewHandler(Deps{DB: database})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, database
}

func seedToken(t *testing.T, d *sql.DB, label, scopes string, revoked bool) string {
	t.Helper()
	plaintext, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	var revAt sql.NullString
	if revoked {
		revAt = sql.NullString{String: "2020-01-01T00:00:00Z", Valid: true}
	}
	_, err = d.ExecContext(context.Background(), `
		INSERT INTO tokens(token_sha256, scopes_csv, label, revoked_at)
		VALUES (?, ?, ?, ?)
	`, auth.HashToken(plaintext), scopes, label, revAt)
	if err != nil {
		t.Fatalf("seed token: %v", err)
	}
	return plaintext
}

// sessionCookie pulls the signed UI cookie out of a response, or fails
// the test if the server didn't set one.
func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie set; got %v", sessionCookieName, rec.Result().Cookies())
	return nil
}

func TestNewHandlerRequiresDB(t *testing.T) {
	if _, err := NewHandler(Deps{}); err == nil {
		t.Fatalf("expected error for nil DB")
	}
}

func TestHomeRedirectsToLoginWhenAnonymous(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/ui/login" {
		t.Fatalf("Location = %q; want /ui/login", got)
	}
}

func TestLoginFormRendersOK(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/login", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="token"`) {
		t.Fatalf("expected token form field; got body:\n%s", body)
	}
	if strings.Contains(body, "flash-error") {
		t.Fatalf("unexpected error flash on clean GET")
	}
}

func TestLoginFormShowsFlashOnInvalidQuery(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/login?invalid=1", nil)
	h.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "flash-error") {
		t.Fatalf("expected error flash for ?invalid=1")
	}
}

func TestLoginSubmitValidTokenSetsCookieAndRedirects(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)

	form := url.Values{"token": {tok}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/ui/" {
		t.Fatalf("Location = %q; want /ui/", got)
	}
	c := sessionCookie(t, rec)
	if !c.HttpOnly {
		t.Errorf("cookie should be HttpOnly")
	}
	if c.Path != "/ui/" {
		t.Errorf("cookie Path = %q; want /ui/", c.Path)
	}

	// The cookie is an opaque session id: it must not carry the token,
	// and it must resolve to a session.
	if strings.Contains(c.Value, tok) || strings.Contains(c.Value, tok[4:12]) {
		t.Fatalf("session cookie contains the bearer token")
	}
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(c)
	if _, ok := h.sessionIdentity(req); !ok {
		t.Fatalf("cookie from login does not resolve to a session")
	}
}

func TestLoginSubmitInvalidTokenRedirectsWithFlash(t *testing.T) {
	h, _ := newTestHandler(t)
	form := url.Values{"token": {"pkm_nope"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/ui/login?invalid=1" {
		t.Fatalf("Location = %q; want /ui/login?invalid=1", got)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Fatalf("should not set session cookie on invalid login")
		}
	}
}

func TestLoginSubmitNonAdminTokenRejected(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "reader", "read:*,publish:dev", false)
	form := url.Values{"token": {tok}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Location"); got != "/ui/login?forbidden=1" {
		t.Fatalf("Location = %q; want /ui/login?forbidden=1", got)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Fatalf("should not set session cookie for a non-admin token")
		}
	}
}

// A cookie minted for a non-admin token (e.g. before the scope check
// existed, or after the token was rescoped) must not open any page.
func TestNonAdminSessionTreatedAsAnonymous(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "reader", "read:prod", false)
	value := sessionFor(t, h, tok)

	for _, path := range []string{"/", "/events", "/storage", "/cells"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login" {
			t.Errorf("%s: status=%d loc=%q, want redirect to login", path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestLoginSubmitEmptyTokenRedirects(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Location"); got != "/ui/login?invalid=1" {
		t.Fatalf("Location = %q; want /ui/login?invalid=1", got)
	}
}

func TestHomeRendersDashboardWhenAuthenticated(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Overview") {
		t.Fatalf("expected dashboard title; body:\n%s", body)
	}
	if !strings.Contains(body, "op") {
		t.Fatalf("expected token label in topbar; body:\n%s", body)
	}
}

func TestHomeShowsSeededChannelAndEvent(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	// Seed a channel, a package (so the card shows "1 package"), and a
	// publish event (so the activity table has a row).
	ctx := context.Background()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO channels(name, overwrite_policy, is_default) VALUES ('dev', 'mutable', 1)`,
	); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO packages(channel, name, version, source_sha256, source_size)
		 VALUES ('dev', 'foo', '1.0.0', 'abc', 42)`,
	); err != nil {
		t.Fatalf("seed package: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO events(type, actor, channel, package, version)
		 VALUES ('publish', 'ci', 'dev', 'foo', '1.0.0')`,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"dev", "default", "mutable", "foo", "1.0.0", "publish"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestLoginFormRedirectsWhenAlreadyAuthenticated(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/login", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/ui/" {
		t.Fatalf("Location = %q; want /ui/", got)
	}
}

func TestRevokedTokenCookieTreatedAsAnonymous(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "ex", "admin", true)
	value := sessionFor(t, h, tok)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("revoked-token holder should be redirected to login; got status=%d loc=%q",
			rec.Code, rec.Header().Get("Location"))
	}
}

func TestTamperedCookieRejected(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)
	// Change one character of the session id.
	tampered := "A" + value[1:]
	if value[0] == 'A' {
		tampered = "B" + value[1:]
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tampered})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("tampered cookie should redirect to login; got %d %q",
			rec.Code, rec.Header().Get("Location"))
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d; want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/ui/login" {
		t.Fatalf("Location = %q; want /ui/login", got)
	}
	c := sessionCookie(t, rec)
	if c.MaxAge != -1 {
		t.Errorf("logout cookie MaxAge = %d; want -1", c.MaxAge)
	}
}

func TestChannelDetailRenders(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	ctx := context.Background()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO channels(name, overwrite_policy, is_default) VALUES ('dev', 'mutable', 0)`,
	); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO packages(channel, name, version, source_sha256, source_size, published_by, yanked, yank_reason)
		VALUES ('dev', 'foo', '1.0.0', 'abc', 1024, 'ci-bot', 0, NULL),
		       ('dev', 'bar', '0.2.1', 'def', 2048, 'ci-bot', 1, 'bad build')
	`); err != nil {
		t.Fatalf("seed packages: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/channels/dev", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"foo", "1.0.0", "bar", "0.2.1", "ci-bot", "yanked", "bad build", "mutable",
		"http://example.com/dev"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// With public_url set, snippets use it instead of the request host.
	h.deps.PublicURL = "https://packages.example.org"
	h.deps.Matrix = &config.MatrixConfig{Distro: "rhel9"}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if want := "https://packages.example.org/dev/__linux__/rhel9/latest"; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body missing %q", want)
	}
}

func TestChannelDetail404(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/channels/nope", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rec.Code)
	}
}

func TestChannelDetailRequiresAuth(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/channels/dev", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("want redirect to login; got status=%d loc=%q",
			rec.Code, rec.Header().Get("Location"))
	}
}

func TestFmtBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1024, "1 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1 MiB"},
		{int64(1024) * 1024 * 1024, "1 GiB"},
	}
	for _, c := range cases {
		if got := fmtBytes(c.n); got != c.want {
			t.Errorf("fmtBytes(%d) = %q; want %q", c.n, got, c.want)
		}
	}
}

func TestEventsPageRendersWithFiltersAndPagination(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	ctx := context.Background()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO channels(name, overwrite_policy) VALUES ('dev', 'mutable'), ('prod', 'immutable')`,
	); err != nil {
		t.Fatalf("seed channels: %v", err)
	}
	// Seed > 1 page of events so we can check HasNext.
	for i := 0; i < 60; i++ {
		channel := "dev"
		if i%2 == 0 {
			channel = "prod"
		}
		typ := "publish"
		if i%5 == 0 {
			typ = "yank"
		}
		if _, err := database.ExecContext(ctx,
			`INSERT INTO events(type, actor, channel, package, version) VALUES (?, 'ci', ?, 'foo', ?)`,
			typ, channel, "1.0."+strconv.Itoa(i),
		); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/events", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Next →") {
		t.Errorf("expected Next link on page 1 with 60 events and default 50 pageSize")
	}
	if !strings.Contains(body, `of 60 total`) {
		t.Errorf("expected total count 60 in body")
	}

	// Filter by type=yank (12 yanks out of 60).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/events?type=yank", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "evt-publish") {
		t.Errorf("type=yank filter should not show publish events")
	}
}

func TestCellsPageShowsMatrixAndCoverage(t *testing.T) {
	h, database := newTestHandler(t)
	h.deps.Matrix = &config.MatrixConfig{
		Distro: "jammy", Arch: "amd64", DefaultRMinor: "4.4",
		Cells: []config.Cell{
			{Name: "r-4.4", RMinor: "4.4"},
			{Name: "r-4.5", RMinor: "4.5"},
		},
	}
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	ctx := context.Background()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO channels(name, overwrite_policy) VALUES ('dev', 'mutable')`,
	); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	res, err := database.ExecContext(ctx,
		`INSERT INTO packages(channel, name, version, source_sha256, source_size)
		 VALUES ('dev', 'foo', '1.0.0', 'abc', 1024)`,
	)
	if err != nil {
		t.Fatalf("seed package: %v", err)
	}
	pkgID, _ := res.LastInsertId()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO binaries(package_id, cell, binary_sha256, size)
		 VALUES (?, 'r-4.4', 'aaa', 2048)`, pkgID,
	); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/cells", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"r-4.4", "r-4.5", "1 / 1", "<code>jammy</code> (amd64)", "get R 4.4"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestCellsPageNoMatrix(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/cells", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Matrix not loaded") {
		t.Errorf("expected placeholder when Matrix is nil")
	}
}

func TestStoragePageRenders(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	ctx := context.Background()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO channels(name, overwrite_policy) VALUES ('dev', 'mutable')`,
	); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	res, err := database.ExecContext(ctx,
		`INSERT INTO packages(channel, name, version, source_sha256, source_size)
		 VALUES ('dev', 'foo', '1.0.0', 'abc', 4096)`,
	)
	if err != nil {
		t.Fatalf("seed package: %v", err)
	}
	pkgID, _ := res.LastInsertId()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO binaries(package_id, cell, binary_sha256, size)
		 VALUES (?, 'linux', 'aaa', 2048)`, pkgID,
	); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/storage", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Storage", "6 KiB", "dev", "foo", "1.0.0"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestStaticAssetsServed(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/static/style.css", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), ".topbar") {
		t.Fatalf("expected CSS body; got:\n%s", rec.Body.String())
	}
}

// sessionFor creates a session for the token directly, like a login
// would, including for revoked or non-admin tokens.
func sessionFor(t *testing.T, h *Handler, tok string) string {
	t.Helper()
	var id int64
	if err := h.deps.DB.QueryRowContext(context.Background(),
		`SELECT id FROM tokens WHERE token_sha256 = ?`, auth.HashToken(tok)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	value, _, err := h.createSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func withCookie(req *http.Request, value string) *http.Request {
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})
	return req
}

func TestLogoutRevokesSessionServerSide(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)

	h.ServeHTTP(httptest.NewRecorder(), withCookie(httptest.NewRequest("POST", "/logout", nil), value))

	// Replaying the old cookie (e.g. a copy taken before logout) fails.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withCookie(httptest.NewRequest("GET", "/", nil), value))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/login" {
		t.Fatalf("session survived logout: status=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := sessionFor(t, h, tok)
	if _, err := database.ExecContext(context.Background(),
		`UPDATE ui_sessions SET expires_at = ?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withCookie(httptest.NewRequest("GET", "/", nil), value))
	if rec.Code != http.StatusFound {
		t.Fatalf("expired session accepted: status=%d", rec.Code)
	}
}

func TestUnknownSessionRejected(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, v := range []string{"", "garbage", strings.Repeat("A", 43)} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withCookie(httptest.NewRequest("GET", "/", nil), v))
		if rec.Code != http.StatusFound {
			t.Errorf("cookie %q: status=%d, want redirect", v, rec.Code)
		}
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	form := url.Values{"token": {tok}}

	cases := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"foreign origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"same origin", map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}, http.StatusFound},
		{"no browser headers", nil, http.StatusFound},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Header().Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("missing anti-framing headers: %v", rec.Header())
	}
}
