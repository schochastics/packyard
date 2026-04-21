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

	"github.com/schochastics/pakman/internal/auth"
	"github.com/schochastics/pakman/internal/db"
)

func newTestHandler(t *testing.T) (*Handler, *db.DB) {
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

	h, err := NewHandler(Deps{
		DB:         database,
		SessionKey: []byte("test-key-at-least-32-bytes-long!"),
	})
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

func TestNewHandlerRequiresSessionKey(t *testing.T) {
	_, err := NewHandler(Deps{})
	if err == nil {
		t.Fatalf("expected error for empty SessionKey")
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

	// Cookie value should verify with the same key and yield the token.
	got, err := verifySessionCookie(c.Value, h.deps.SessionKey)
	if err != nil {
		t.Fatalf("verifySessionCookie: %v", err)
	}
	if got != tok {
		t.Fatalf("token round-trip mismatch")
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
	value := signSessionCookie(tok, h.deps.SessionKey)

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
	value := signSessionCookie(tok, h.deps.SessionKey)

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
	value := signSessionCookie(tok, h.deps.SessionKey)

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
	value := signSessionCookie(tok, h.deps.SessionKey)

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
	value := signSessionCookie(tok, h.deps.SessionKey)
	// Flip a character in the signature half.
	i := strings.IndexByte(value, '.')
	tampered := value[:i+1] + "AAAA" + value[i+5:]

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
	value := signSessionCookie(tok, h.deps.SessionKey)

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
	value := signSessionCookie(tok, h.deps.SessionKey)

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
	for _, want := range []string{"foo", "1.0.0", "bar", "0.2.1", "ci-bot", "yanked", "bad build", "mutable"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestChannelDetail404(t *testing.T) {
	h, database := newTestHandler(t)
	tok := seedToken(t, database.DB, "op", "admin", false)
	value := signSessionCookie(tok, h.deps.SessionKey)

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
	value := signSessionCookie(tok, h.deps.SessionKey)

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

func TestSessionCookieRoundTrip(t *testing.T) {
	key := []byte("round-trip-key-here-32-bytes!!!!")
	got, err := verifySessionCookie(signSessionCookie("pkm_abc", key), key)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != "pkm_abc" {
		t.Fatalf("got %q want pkm_abc", got)
	}
}

func TestSessionCookieWrongKeyRejected(t *testing.T) {
	v := signSessionCookie("pkm_abc", []byte("key-one-32-bytes-padded-padded!!"))
	if _, err := verifySessionCookie(v, []byte("key-two-32-bytes-padded-padded!!")); err == nil {
		t.Fatal("expected error for wrong key")
	}
}

func TestSessionCookieMalformedRejected(t *testing.T) {
	key := []byte("test-key")
	cases := []string{"", "nodothere", "a.!!!invalid", "!!!.bbb"}
	for _, c := range cases {
		if _, err := verifySessionCookie(c, key); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}
