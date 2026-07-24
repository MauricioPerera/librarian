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
