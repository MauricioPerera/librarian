package server

// CONTRACT-06 — UI foundation. Adds the browser-facing surface on the SAME
// mux/handlers as the JSON API: embedded static assets (htmx + CSS, served by
// the binary, never from a CDN at runtime), a login/logout flow backed by a
// JWT-in-cookie session, and a protected home page. The cookie is an ADDITIONAL
// transport for the same JWT the API already issues — the Authorization: Bearer
// header path is untouched. Browser sessions reuse the exact Identity type and
// identityKey{} from authz.go so a future articles-UI contract can call
// identityFromContext/requirePermission without caring whether the identity
// arrived via header or cookie.

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"path"
	"strings"

	"github.com/MauricioPerera/librarian/internal/auth"
)

// sessionCookieName is the name of the browser-session cookie carrying the JWT.
// Documented design decision (CONTRACT-06): a single, app-prefixed name so it
// is unambiguous in a shared-host cookie jar and easy to target for deletion on
// logout.
const sessionCookieName = "librarian_session"

// sessionMaxAgeSeconds bounds the cookie lifetime to 24h, mirroring the JWT's
// own TTL (auth.IssueJWT signs a 24h token). Even so, requireSession always
// re-validates the JWT on every request, so an expired-but-still-present cookie
// is treated exactly like an absent one.
const sessionMaxAgeSeconds = 24 * 60 * 60

//go:embed assets/htmx.min.js assets/app.css
var assetsFS embed.FS

//go:embed templates/layout.html templates/login.html templates/home.html templates/articles_list.html templates/articles_row.html templates/articles_new.html templates/articles_edit.html templates/error_403.html templates/error_404.html templates/users_list.html templates/users_new.html templates/users_detail.html templates/roles_list.html templates/apikeys_list.html templates/apikeys_row.html templates/apikeys_new.html templates/apikeys_created.html
var templatesFS embed.FS

// assetVersion is a short hash of the embedded static assets' content,
// computed once at process start. It is appended to /static/* URLs as a
// ?v= query param so that a CDN or browser caching them as
// "Cache-Control: immutable" (see handleStatic) is forced to re-fetch
// whenever the embedded content actually changes across a redeploy — the
// URL itself changes, so there is nothing stale to serve. Found necessary
// after a real redeploy: Cloudflare kept serving the pre-CONTRACT-10
// stylesheet for the immutable-cached path long after the origin was
// updated, because nothing about the request ever changed.
var assetVersion = computeAssetVersion()

func computeAssetVersion() string {
	h := sha256.New()
	for _, name := range []string{"assets/app.css", "assets/htmx.min.js"} {
		data, err := assetsFS.ReadFile(name)
		if err != nil {
			continue
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// templateFuncs is attached to every template set below (via baseTmpl) so
// layout.html can call {{assetVersion}} to build cache-busted asset URLs.
var templateFuncs = template.FuncMap{
	"assetVersion": func() string { return assetVersion },
}

// baseTmpl carries templateFuncs; every page's template set is parsed from a
// clone of it so all of them can call {{assetVersion}} inside layout.html
// without repeating the Funcs() wiring at each parse site.
var baseTmpl = template.New("base").Funcs(templateFuncs)

func mustParseFS(patterns ...string) *template.Template {
	t := template.Must(baseTmpl.Clone())
	return template.Must(t.ParseFS(templatesFS, patterns...))
}

// Each page is its own template set (layout + one page). Parsing pages into
// separate sets avoids a collision on the shared "content" definition that a
// single combined set would produce.
var (
	loginTmpl = mustParseFS("templates/layout.html", "templates/login.html")
	homeTmpl  = mustParseFS("templates/layout.html", "templates/home.html")
)

// genericLoginError is the SINGLE message shown for every credential failure —
// unknown email and wrong password alike — so the UI never reintroduces the
// user-enumeration leak that auth.VerifyCredentials already closes.
const genericLoginError = "Email o contraseña incorrectos."

// pageData is the layout's view model: title and, when authenticated, the email
// used to render the topbar's logout control. Path is the current request path
// (CONTRACT-10) from which the shared layout infers the active sidebar
// section/sub-item via the Nav() method — presentation only, no route or
// authorization data.
type pageData struct {
	Title         string
	Authenticated bool
	Email         string
	Path          string
}

// loginPage adds the optional error banner shown after a failed submit.
type loginPage struct {
	pageData
	Error string
}

// registerUIRoutes wires the browser-facing routes onto the same mux. The JSON
// routes are registered separately and are unaffected. GET / is the catch-all
// for the HTML site behind requireSession; more specific patterns (/health,
// /whoami, /articles, /static) still win by ServeMux precedence.
func (h *handlers) registerUIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /static/{path...}", h.handleStatic)
	mux.HandleFunc("GET /login", h.handleLoginForm)
	mux.HandleFunc("POST /login", h.handleLoginSubmit)
	mux.HandleFunc("POST /logout", h.handleLogout)
	h.registerAdminArticleRoutes(mux)
	h.registerAdminUserRoutes(mux)
	h.registerAdminAPIKeyRoutes(mux)
	mux.Handle("GET /", h.requireSession(http.HandlerFunc(h.handleHome)))
}

// handleStatic serves an embedded asset with an explicit, deterministic
// Content-Type. It sets the type from the extension itself rather than trusting
// mime.TypeByExtension, whose result for ".js"/".css" can vary by OS (the
// Windows registry can override it) — the test asserts an exact value, so the
// server must not depend on host state.
func (h *handlers) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := path.Clean(r.PathValue("path"))
	if name == "." || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	// Embedded assets are immutable for the life of the binary; allow caching.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

// contentTypeFor maps a known asset extension to a fixed Content-Type. Unknown
// extensions fall back to a safe, non-executable default.
func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// handleLoginForm renders the empty login form (GET /login).
func (h *handlers) handleLoginForm(w http.ResponseWriter, _ *http.Request) {
	renderLogin(w, http.StatusOK, "")
}

// handleLoginSubmit processes a real form POST (r.ParseForm, NOT JSON). It calls
// auth.VerifyCredentials + auth.IssueJWT directly — the same in-process pattern
// as the JSON handleLogin, with no HTTP loopback. On success it sets the session
// cookie (HttpOnly, Secure, SameSite=Strict, Path=/) and 303-redirects to /. On
// any failure it re-renders the form with the single generic error message.
func (h *handlers) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		renderLogin(w, http.StatusBadRequest, genericLoginError)
		return
	}
	email := r.PostFormValue("email")
	password := r.PostFormValue("password")

	user, err := auth.VerifyCredentials(r.Context(), h.db, email, password)
	if err != nil {
		// Same message for unknown-user and wrong-password — anti-enumeration.
		renderLogin(w, http.StatusUnauthorized, genericLoginError)
		return
	}
	token, err := auth.IssueJWT(h.jwtSecret, user, h.now())
	if err != nil {
		renderLogin(w, http.StatusInternalServerError, "No se pudo iniciar sesión. Intentá de nuevo.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   sessionMaxAgeSeconds,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout clears the session cookie (MaxAge -1) with the same attributes it
// was set with, then 303-redirects to /login.
func (h *handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleHome renders the protected home page. It runs behind requireSession, so
// the Identity is guaranteed to be in context; the guard is defensive only. Any
// path other than exactly "/" (which GET / also matches as a catch-all) is a
// 404 rather than a spurious home render.
func (h *handlers) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	id, ok := identityFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	renderHome(w, id.Email)
}

// requireSession is the browser-session middleware for HTML routes. It reads the
// session cookie, validates the JWT with auth.VerifyJWT, and on any failure —
// cookie absent, malformed, wrong signature, or expired — 302-redirects to
// /login (never a 401 JSON: the caller is a human in a browser). On success it
// builds the SAME Identity type and stores it under the SAME identityKey{} that
// authz.go uses, so downstream HTML handlers share the API's authorization
// plumbing rather than a parallel one.
func (h *handlers) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.sessionIdentity(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		r2 := r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
		next.ServeHTTP(w, r2)
	})
}

// sessionIdentity resolves the browser-session Identity from the session cookie:
// it reads the cookie, validates the JWT, and builds the SAME Identity type /
// identityKey{} the API uses. ok is false when the cookie is absent, malformed,
// wrong-signed, or expired — the caller decides what a failure means (redirect
// to /login for a read route, or the permission middleware below). It is the
// single cookie→Identity resolver shared by requireSession and
// requireSessionPermission, so neither duplicates the cookie/JWT plumbing.
func (h *handlers) sessionIdentity(r *http.Request) (*Identity, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, false
	}
	claims, err := auth.VerifyJWT(h.jwtSecret, c.Value)
	if err != nil {
		return nil, false
	}
	return &Identity{
		Kind:   "jwt",
		UserID: claims.Subject,
		Email:  claims.Email,
		Roles:  claims.Roles,
	}, true
}

// requireSessionPermission is the CONTRACT-07 T1 middleware: it gates an HTML
// write route on BOTH a valid browser session AND a specific permission. This
// is the gap CONTRACT-07 closes — requirePermission (authz.go) reads only the
// Authorization header (JSON API), and requireSession (above) checks only the
// session, not any permission. This one combines the cookie-resolved Identity
// with permissionsFor, and on failure it NEVER writes the JSON writeError
// envelope (that is the API's contract): no session → 302 redirect to /login
// (a human, not a 401); valid session but missing the permission → a simple 403
// HTML page. It reuses sessionIdentity + permissionsFor, so it shares the exact
// authorization plumbing rather than a parallel one.
func (h *handlers) requireSessionPermission(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := h.sessionIdentity(r)
			if !ok {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			perms, err := h.permissionsFor(r.Context(), id)
			if err != nil {
				// The caller IS authenticated; we just could not load grants.
				// A plain-text 500 (not the JSON API envelope) for a human.
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !containsString(perms, permission) {
				renderForbidden(w, id.Email)
				return
			}
			r2 := r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
			next.ServeHTTP(w, r2)
		})
	}
}

// renderLogin writes the login page with the given status and optional error.
func renderLogin(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = loginTmpl.ExecuteTemplate(w, "layout", loginPage{
		pageData: pageData{Title: "Iniciar sesión — librarian"},
		Error:    errMsg,
	})
}

// renderHome writes the protected home page for the given authenticated email.
func renderHome(w http.ResponseWriter, email string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = homeTmpl.ExecuteTemplate(w, "layout", pageData{
		Title:         "Inicio — librarian",
		Authenticated: true,
		Email:         email,
		Path:          "/",
	})
}
