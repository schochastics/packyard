package ui

import (
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
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
		"fmtTime": fmtTime,
	}).ParseFS(templatesFS, "templates/*.html")
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
