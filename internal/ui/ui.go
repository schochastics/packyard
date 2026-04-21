package ui

import (
	"database/sql"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/schochastics/pakman/internal/auth"
	"github.com/schochastics/pakman/internal/db"
)

// Deps bundles everything the UI handlers need.
type Deps struct {
	DB            *db.DB
	SessionKey    []byte // HMAC key for the session cookie; must be non-empty
	SecureCookies bool   // set Secure flag on Set-Cookie (production)
}

// Handler is the pakman UI handler. Expected mount point is /ui/ on the
// parent mux; the strip + route table below assumes that.
type Handler struct {
	deps      Deps
	templates *template.Template
	mux       *http.ServeMux
}

// NewHandler parses the embedded templates and wires routes on a new
// mux. The returned http.Handler can be mounted under any path prefix.
func NewHandler(deps Deps) (*Handler, error) {
	if len(deps.SessionKey) == 0 {
		return nil, errors.New("ui: SessionKey is required")
	}

	tpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}

	h := &Handler{deps: deps, templates: tpl}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.handleHome)
	mux.HandleFunc("GET /login", h.handleLoginForm)
	mux.HandleFunc("POST /login", h.handleLoginSubmit)
	mux.HandleFunc("POST /logout", h.handleLogout)
	mux.HandleFunc("GET /channels/{name}", h.handleChannelDetail)
	mux.HandleFunc("GET /events", h.handleEvents)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
	h.mux = mux

	return h, nil
}

// ServeHTTP dispatches to the internal routes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// parseTemplates loads every embedded HTML file into a single template
// namespace so child templates can {{template "content" .}} a sibling.
// We parse once at startup; serving pages never allocates a new tree.
func parseTemplates() (*template.Template, error) {
	return template.New("").Funcs(template.FuncMap{
		"fmtTime":    fmtTime,
		"fmtBytes":   fmtBytes,
		"add":        func(a, b int) int { return a + b },
		"pagerQuery": pagerQuery,
	}).ParseFS(templatesFS, "templates/*.html")
}

// pagerQuery rebuilds the events page querystring preserving active
// filters while swapping the page number. Returned as a plain string
// — html/template will attribute-escape `&` as `&amp;` which browsers
// parse back into a valid query.
func pagerQuery(data *eventsPageData, page int) string {
	v := url.Values{}
	v.Set("page", strconv.Itoa(page))
	if data.Filter.Channel != "" {
		v.Set("channel", data.Filter.Channel)
	}
	if data.Filter.Type != "" {
		v.Set("type", data.Filter.Type)
	}
	if data.Filter.Package != "" {
		v.Set("package", data.Filter.Package)
	}
	return v.Encode()
}

// fmtBytes renders a byte count in the closest IEC unit (KiB, MiB…).
// Designed for dashboard readability, not machine precision.
func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return formatInt(n) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	if exp >= len(units) {
		exp = len(units) - 1
	}
	// One decimal place; trim trailing .0 for whole numbers.
	v := float64(n) / float64(div)
	if v == float64(int(v)) {
		return formatInt(int64(v)) + " " + units[exp]
	}
	return trimTrailingZero(formatFloat(v)) + " " + units[exp]
}

func formatInt(n int64) string     { return strconv.FormatInt(n, 10) }
func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }
func trimTrailingZero(s string) string {
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		return s[:len(s)-2]
	}
	return s
}

// fmtTime renders an ISO-8601 timestamp in a more human form for the
// dashboard. Falls back to the raw string if the input doesn't parse.
func fmtTime(raw string) string {
	if raw == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return raw
	}
	return t.Format("2006-01-02 15:04:05")
}

// viewData is the base context every page template receives. Specific
// pages embed this as the anonymous outer struct and add fields.
type viewData struct {
	Title    string
	Identity *auth.Identity
	Flash    *flash
}

type flash struct {
	Kind    string // error, info
	Message string
}

// renderPage runs the layout template with the named page file's
// "content" definition. Page files live in templates/ and each redefines
// "content". Parsing happens at startup once; callers just name the file.
func (h *Handler) renderPage(w http.ResponseWriter, r *http.Request, pageFile string, data any) {
	// Clone so concurrent requests don't race the tree we layer
	// page-specific content onto.
	tpl, err := h.templates.Clone()
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	if _, err := tpl.ParseFS(templatesFS, "templates/"+pageFile); err != nil {
		h.renderError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.Lookup("layout").Execute(w, data); err != nil {
		// Header already written — log and move on.
		slog.Default().ErrorContext(r.Context(), "ui render", "err", err, "page", pageFile)
	}
}

// renderError shows a very plain error page. Used for template
// failures; normal flow errors (auth, bad input) flash on the next
// page instead.
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Default().ErrorContext(r.Context(), "ui error", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// ---------- handlers ---------------------------------------------------

func (h *Handler) handleHome(w http.ResponseWriter, r *http.Request) {
	id, ok := h.sessionIdentity(r)
	if !ok {
		redirectLogin(w, r)
		return
	}
	data, err := loadDashboardData(r.Context(), h.deps.DB.DB, 20)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	h.renderPage(w, r, "dashboard.html", struct {
		viewData
		Data *dashboardData
	}{
		viewData: viewData{Title: "Overview", Identity: &id},
		Data:     data,
	})
}

func (h *Handler) handleChannelDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := h.sessionIdentity(r)
	if !ok {
		redirectLogin(w, r)
		return
	}
	name := r.PathValue("name")
	data, err := loadChannelDetail(r.Context(), h.deps.DB.DB, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		h.renderError(w, r, err)
		return
	}
	h.renderPage(w, r, "channel_detail.html", struct {
		viewData
		Data *channelDetailData
	}{
		viewData: viewData{Title: "Channel " + name, Identity: &id},
		Data:     data,
	})
}

func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := h.sessionIdentity(r)
	if !ok {
		redirectLogin(w, r)
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	filter := eventFilter{
		Channel: q.Get("channel"),
		Type:    q.Get("type"),
		Package: q.Get("package"),
	}

	data, err := loadEventsPage(r.Context(), h.deps.DB.DB, page, pageSize, filter)
	if err != nil {
		h.renderError(w, r, err)
		return
	}
	h.renderPage(w, r, "events.html", struct {
		viewData
		Data *eventsPageData
	}{
		viewData: viewData{Title: "Events", Identity: &id},
		Data:     data,
	})
}

func (h *Handler) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	// Already logged in? Skip the form.
	if _, ok := h.sessionIdentity(r); ok {
		http.Redirect(w, r, "/ui/", http.StatusFound)
		return
	}
	var fl *flash
	if r.URL.Query().Get("invalid") == "1" {
		fl = &flash{Kind: "error", Message: "Token not recognized, revoked, or expired."}
	}
	h.renderPage(w, r, "login.html", viewData{
		Title: "Sign in",
		Flash: fl,
	})
}

func (h *Handler) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/login?invalid=1", http.StatusFound)
		return
	}
	tok := r.FormValue("token")
	if tok == "" {
		http.Redirect(w, r, "/ui/login?invalid=1", http.StatusFound)
		return
	}

	// Validate against the DB exactly like bearer auth would.
	if _, err := auth.Lookup(r.Context(), h.deps.DB.DB, tok); err != nil {
		http.Redirect(w, r, "/ui/login?invalid=1", http.StatusFound)
		return
	}

	value := signSessionCookie(tok, h.deps.SessionKey)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/ui/",
		HttpOnly: true,
		Secure:   h.deps.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(defaultSessionTTL),
	})
	http.Redirect(w, r, "/ui/", http.StatusFound)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/ui/",
		HttpOnly: true,
		Secure:   h.deps.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/ui/login", http.StatusFound)
}

// sessionIdentity reads the cookie, verifies the signature, and resolves
// the token to a DB row. Returns (id, true) on success; (zero, false)
// for missing, tampered, or revoked sessions.
func (h *Handler) sessionIdentity(r *http.Request) (auth.Identity, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return auth.Identity{}, false
	}
	tok, err := verifySessionCookie(c.Value, h.deps.SessionKey)
	if err != nil {
		return auth.Identity{}, false
	}
	id, err := auth.Lookup(r.Context(), h.deps.DB.DB, tok)
	if err != nil {
		return auth.Identity{}, false
	}
	return id, true
}

func redirectLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/login", http.StatusFound)
}
