// Package server builds the librarian HTTP handler. CONTRACT-01 exposed
// GET /health; CONTRACT-02 adds the dual-auth surface: POST /auth/login
// (password → JWT) and the GET /whoami demo endpoint protected by either a
// valid JWT or a valid API key.
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MauricioPerera/librarian/internal/auth"
)

// Deps carries the runtime dependencies the routes need. JWTSecret must be
// non-empty — NewMux fails (fail-closed) if it is not, so the server never
// starts with a default/hardcoded secret. DB is the shared *sql.DB from
// compat.Store.DB.
type Deps struct {
	DB        *sql.DB
	JWTSecret string
}

// NewMux returns the librarian HTTP handler wired with the auth routes. It
// fails if JWTSecret is empty — there is no default secret, by contract.
func NewMux(deps Deps) (*http.ServeMux, error) {
	if deps.JWTSecret == "" {
		return nil, errors.New("JWT secret must not be empty (set LIBRARIAN_JWT_SECRET)")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)

	h := &handlers{db: deps.DB, jwtSecret: deps.JWTSecret, now: time.Now}
	mux.HandleFunc("POST /auth/login", h.handleLogin)
	mux.HandleFunc("GET /whoami", h.handleWhoami)

	// CONTRACT-03 T3: CRUD over the articles content type. Create/update/
	// publish/delete are gated by permission via the reusable middleware;
	// list/get require only a valid identity (reading is not permission-gated
	// in v1). Go 1.26 ServeMux patterns with {id} wildcards route here.
	mux.Handle("POST /articles", h.requirePermission("content.create")(http.HandlerFunc(h.handleCreateArticle)))
	mux.Handle("GET /articles", h.requireAuth(http.HandlerFunc(h.handleListArticles)))
	mux.Handle("GET /articles/{id}", h.requireAuth(http.HandlerFunc(h.handleGetArticle)))
	mux.Handle("PUT /articles/{id}", h.requirePermission("content.update")(http.HandlerFunc(h.handleUpdateArticle)))
	mux.Handle("POST /articles/{id}/publish", h.requirePermission("content.publish")(http.HandlerFunc(h.handlePublishArticle)))
	mux.Handle("DELETE /articles/{id}", h.requirePermission("content.delete")(http.HandlerFunc(h.handleDeleteArticle)))

	// CONTRACT-11 T2: CRUD over the products content type — the exact same
	// pattern as articles, gated by the SAME generic content.* permissions.
	// products has no publish route (no published_at column); it is simple CRUD.
	mux.Handle("POST /products", h.requirePermission("content.create")(http.HandlerFunc(h.handleCreateProduct)))
	mux.Handle("GET /products", h.requireAuth(http.HandlerFunc(h.handleListProducts)))
	mux.Handle("GET /products/{id}", h.requireAuth(http.HandlerFunc(h.handleGetProduct)))
	mux.Handle("PUT /products/{id}", h.requirePermission("content.update")(http.HandlerFunc(h.handleUpdateProduct)))
	mux.Handle("DELETE /products/{id}", h.requirePermission("content.delete")(http.HandlerFunc(h.handleDeleteProduct)))

	// CONTRACT-12 T2/T3: taxonomy terms (categories/tags). CRUD of terms and the
	// atomic (re)assignment of terms to content are gated by the new terms.manage
	// permission; listing/reading a term requires only a valid identity (reading
	// is not permission-gated, like the rest of the project). The two per-content
	// assignment routes replace the FULL set of a piece of content's terms.
	mux.Handle("POST /terms", h.requirePermission("terms.manage")(http.HandlerFunc(h.handleCreateTerm)))
	mux.Handle("GET /terms", h.requireAuth(http.HandlerFunc(h.handleListTerms)))
	mux.Handle("GET /terms/{id}", h.requireAuth(http.HandlerFunc(h.handleGetTerm)))
	mux.Handle("PUT /terms/{id}", h.requirePermission("terms.manage")(http.HandlerFunc(h.handleUpdateTerm)))
	mux.Handle("DELETE /terms/{id}", h.requirePermission("terms.manage")(http.HandlerFunc(h.handleDeleteTerm)))
	mux.Handle("PUT /articles/{id}/terms", h.requirePermission("terms.manage")(http.HandlerFunc(h.handleSetArticleTerms)))
	mux.Handle("PUT /products/{id}/terms", h.requirePermission("terms.manage")(http.HandlerFunc(h.handleSetProductTerms)))

	// CONTRACT-13 T3: dynamic content types. Creating one CREATES A REAL TABLE
	// in the production database, so it is gated by its own dedicated
	// permission, content_types.manage — not by the generic content.* grants
	// (DEFINITION-CPT-DINAMICOS.md). Reading the definitions requires only a
	// valid identity, like every other read route.
	//
	// CONTRACT-18 adds the PUT: editing the FIELDS of an already-applied type,
	// gated by the SAME content_types.manage permission (no new permission — it
	// is the same class of action as creating one: it rebuilds a real table).
	// There is still no DELETE: dropping a type was never in scope.
	mux.Handle("POST /content-types", h.requirePermission("content_types.manage")(http.HandlerFunc(h.handleCreateContentType)))
	mux.Handle("GET /content-types", h.requireAuth(http.HandlerFunc(h.handleListContentTypes)))
	mux.Handle("GET /content-types/{name}", h.requireAuth(http.HandlerFunc(h.handleGetContentType)))
	mux.Handle("PUT /content-types/{name}", h.requirePermission("content_types.manage")(http.HandlerFunc(h.handleEditContentType)))

	// CONTRACT-14: the GENERIC JSON CRUD over any dynamic content type, driven
	// entirely by its persisted definition — no Go file per type. It lives under
	// a dedicated /content/ prefix so the dynamic namespace can never collide
	// with an existing or future static route. Gating reuses the SAME generic
	// content.* permissions that already gate articles/products;
	// content_types.manage gates DEFINING a type (CONTRACT-13), not loading
	// CONTENT into one — they are different actions and are not mixed.
	mux.Handle("GET /content/{type}", h.requireAuth(http.HandlerFunc(h.handleListContent)))
	mux.Handle("GET /content/{type}/{id}", h.requireAuth(http.HandlerFunc(h.handleGetContent)))
	mux.Handle("POST /content/{type}", h.requirePermission("content.create")(http.HandlerFunc(h.handleCreateContent)))
	mux.Handle("PUT /content/{type}/{id}", h.requirePermission("content.update")(http.HandlerFunc(h.handleUpdateContent)))
	mux.Handle("DELETE /content/{type}/{id}", h.requirePermission("content.delete")(http.HandlerFunc(h.handleDeleteContent)))

	// CONTRACT-06: browser-facing UI (static assets, login/logout, protected
	// home) on the same mux/handlers. JSON routes above are unaffected.
	h.registerUIRoutes(mux)
	return mux, nil
}

// handlers holds the per-request-shared state for the auth routes.
type handlers struct {
	db        *sql.DB
	jwtSecret string
	now       func() time.Time
	// schemaMu serializes the schema-MUTATING requests (CONTRACT-13: creating a
	// dynamic content type). That operation reads which tables are missing and
	// then creates one; two concurrent creations of different types could
	// otherwise interleave their diff and create steps. It does NOT guard
	// uniqueness of a type name — that is the schema-level UNIQUE(name) on
	// content_types, the only guarantee that also holds across processes.
	schemaMu sync.Mutex
}

// handleHealth answers 200 {"status":"ok"}.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// loginRequest is the body of POST /auth/login.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleLogin verifies credentials (auth.VerifyCredentials) and, on success,
// issues a 24h JWT. On any credential failure it returns 401 with the same
// generic envelope and message that VerifyCredentials uses — anti-enumeration.
func (h *handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	user, err := auth.VerifyCredentials(r.Context(), h.db, req.Email, req.Password)
	if err != nil {
		// Same generic message for unknown user and wrong password.
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, err := auth.IssueJWT(h.jwtSecret, user, h.now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// handleWhoami resolves the caller's identity from either a valid JWT or a
// valid API key supplied as Authorization: Bearer <token>. Resolution is shared
// with the permission middleware via resolveIdentity (CONTRACT-03 T2) — this
// handler no longer duplicates the JWT-then-API-key resolution inline. The
// response shape is unchanged (public contract from CONTRACT-02): it depends
// on which mechanism authenticated the request.
func (h *handlers) handleWhoami(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := resolveIdentity(r.Context(), h.db, h.jwtSecret, token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch id.Kind {
	case "jwt":
		writeJSON(w, http.StatusOK, map[string]any{
			"auth":    "jwt",
			"user_id": id.UserID,
			"email":   id.Email,
			"roles":   id.Roles,
		})
	case "apikey":
		writeJSON(w, http.StatusOK, map[string]any{
			"auth":    "apikey",
			"label":   id.Label,
			"role_id": id.RoleID,
		})
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. It returns ok=false when the header is absent or malformed; it never
// distinguishes "absent" from "malformed" in the response (both route to 401).
func bearerToken(r *http.Request) (string, bool) {
	v := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(v[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// writeJSON encodes v as JSON with the given status. It uses json.Marshal (not
// an Encoder, which would append a trailing newline) so response bodies are
// exact and deterministic — e.g. /health stays the byte-exact {"status":"ok"}.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write(body)
}

// writeError emits the standard error envelope: {"error": <msg>}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
