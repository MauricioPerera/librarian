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
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// Deps carries the runtime dependencies the routes need. JWTSecret must be
// non-empty — NewMux fails (fail-closed) if it is not, so the server never
// starts with a default/hardcoded secret.
//
// CONTRACT-21 T2 REPLACED `DB *sql.DB` WITH `Store *compat.Store`, and that is
// the whole point of the change rather than a tidy-up. A *sql.DB cannot say
// which engine it belongs to, so anyone holding one has to SUPPLY the engine —
// which is what NewMux used to do, with a literal `schema.SQLiteTarget` written
// by hand. That line was correct only because librarian had exactly one engine;
// the moment there are two, a hand-written target is a way to run a PostgreSQL
// pool while emitting `?` placeholders and compiling SQLite DDL, and it compiles
// perfectly. Taking the compat.Store — which store.Open builds with the target
// it opened the connection FROM — makes the mismatch unrepresentable instead of
// merely discouraged.
type Deps struct {
	Store     *compat.Store
	JWTSecret string
	// Capabilities is this installation's declared set of optional schema
	// capabilities (CONTRACT-23). Its ZERO VALUE is "everything enabled", which
	// is what every caller written before that contract means and what every
	// deployed installation has, so an un-updated construction of Deps keeps
	// today's behavior exactly.
	Capabilities schema.Capabilities
}

// NewMux returns the librarian HTTP handler wired with the auth routes. It
// fails if JWTSecret is empty — there is no default secret, by contract — and
// if Store is nil, since there is no longer any engine it could assume.
func NewMux(deps Deps) (*http.ServeMux, error) {
	if deps.JWTSecret == "" {
		return nil, errors.New("JWT secret must not be empty (set LIBRARIAN_JWT_SECRET)")
	}
	if deps.Store == nil {
		return nil, errors.New("Deps.Store must be the *compat.Store returned by store.Open (connection AND engine)")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)

	h := &handlers{
		db:        deps.Store.DB,
		store:     deps.Store,
		jwtSecret: deps.JWTSecret,
		now:       time.Now,
		caps:      deps.Capabilities,
	}
	h.registerRoutes(mux)
	return mux, nil
}

// registerRoutes wires every route onto the mux. It is split out of NewMux with
// NO behavior change so a handlers value built over a DIFFERENT engine can be
// given the same routes — which is what the dual-engine batteries need to drive
// the real HTTP surface against PostgreSQL. Since CONTRACT-21 the production
// path does the same thing: NewMux is engine-agnostic and takes whichever store
// the process opened.
func (h *handlers) registerRoutes(mux *http.ServeMux) {
	// CONTRACT-24: the availability probe. It is registered HERE and not next to
	// /health in NewMux because, unlike handleHealth, it needs the handlers value
	// (it pings the pool) — and because registering it here means every mux built
	// from a handlers value, including the dual-engine batteries', gets it.
	mux.HandleFunc(readyPath, h.handleReady)

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
}

// handlers holds the per-request-shared state for the auth routes.
type handlers struct {
	db *sql.DB
	// store is the same connection as db, paired with the engine it is bound
	// to. CONTRACT-19 introduced it for internal/auth; CONTRACT-20 makes it the
	// door for every statement this package runs too, which is why it lost the
	// `auth` in its name. `db` survives only for the two things a *compat.Store
	// does not offer: BeginTx (the transactions terms.go owns) and the
	// *sql.DB that internal/store's own helpers still take.
	store     *compat.Store
	jwtSecret string
	now       func() time.Time
	// caps is this installation's declared capability set (CONTRACT-23). Zero
	// value = everything enabled, so a handlers built without it behaves exactly
	// as it did before that contract. It decides two things and no others: which
	// canonical schema the read routines are compiled from (codeSchema), and
	// whether a request carrying an `embedding` field is accepted or refused.
	caps schema.Capabilities
	// schemaMu serializes the schema-MUTATING requests (CONTRACT-13: creating a
	// dynamic content type). That operation reads which tables are missing and
	// then creates one; two concurrent creations of different types could
	// otherwise interleave their diff and create steps. It does NOT guard
	// uniqueness of a type name — that is the schema-level UNIQUE(name) on
	// content_types, the only guarantee that also holds across processes.
	schemaMu sync.Mutex
	// ready memoizes the CONTRACT-24 availability probe so an unauthenticated,
	// freely pollable endpoint cannot be turned into a load path against the
	// database. See readiness.go.
	ready readiness
}

// handleHealth answers 200 {"status":"ok"}.
//
// CONTRACT-24 deliberately LEFT THIS ALONE. Its contract is "the process is
// alive" and there is external monitoring asserting exactly that; making it
// consult the database would silently redefine what every existing monitor
// measures. Availability is the separate GET /ready (readiness.go).
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// loginRequest is the body of POST /auth/login.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleLogin verifies credentials (auth.VerifyCredentials) and, on success,
// issues a 24h JWT. A CREDENTIAL failure returns 401 with the same generic
// envelope and message for every case — anti-enumeration. An INFRASTRUCTURE
// failure returns 503; see the guard below.
func (h *handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	user, err := auth.VerifyCredentials(r.Context(), h.store, req.Email, req.Password)
	if err != nil {
		// CONTRACT-24. READ THIS BEFORE "FIXING" IT BACK.
		//
		// This guard does NOT weaken the anti-enumeration property, and the
		// reason is that the two things being separated are of different kinds.
		//
		// The collapse to a single 401 exists so an attacker cannot tell "that
		// user does not exist" from "that password is wrong" — auth.VerifyCredentials
		// goes as far as running a bcrypt compare against a fixed dummy hash on
		// the unknown-email branch so even the TIMING of the two is the same. All
		// of that is intact: EVERY case ErrInvalidCredentials covers — unknown
		// email, wrong password, and a suspended or invited account with the right
		// password — still takes this same 401 with the same body and the same
		// message. Nothing below can distinguish them.
		//
		// What the guard separates out is a database that is not answering. That
		// answer is a FUNCTION OF THE INFRASTRUCTURE ALONE: it is identical for
		// every email and every password, including emails that do not exist and
		// passwords that are wrong, so observing it tells an attacker nothing
		// about any account. There is no account-dependent signal to leak,
		// because the branch never got far enough to look at an account.
		//
		// It was measured in production (docs/PENDIENTES.md, hueco 7): with the
		// database stopped, login answered 401, so an outage presented itself as
		// "wrong credentials" and the incident was diagnosed as a login problem.
		// Reporting 401 there is not a security property, it is a wrong answer.
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			writeInfraUnavailable(w)
			return
		}
		// Same generic message for unknown user, wrong password, and inactive
		// account.
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
	id, err := resolveIdentity(r.Context(), h.store, h.jwtSecret, token)
	if err != nil {
		// CONTRACT-24: 401 only when the token was actually REJECTED; 503 when
		// the API-key lookup could not reach the database. See resolveIdentity.
		writeIdentityError(w, err)
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
