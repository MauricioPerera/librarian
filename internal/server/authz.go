package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// Identity is the resolved caller surfaced by the authorization layer. It is
// produced once per request by resolveIdentity (the single source of truth for
// "who is calling", reused by /whoami and the permission middleware) and
// carried through the request via context so handlers never re-resolve it.
//
// The Kind is "jwt" (a human user) or "apikey" (a service identity with a
// single role and no human user behind it). The fields populated depend on
// Kind: JWT identities carry UserID/Email/Roles; API-key identities carry
// RoleID/Label. Permissions are NOT resolved here — the permission middleware
// loads them on demand, since /whoami needs only identity (no DB hit) while the
// gated routes need the permission set.
type Identity struct {
	Kind   string   // "jwt" or "apikey"
	UserID string   // jwt: claims.Subject (users.id)
	Email  string   // jwt: claims.Email
	Roles  []string // jwt: claims.Roles (role names)
	RoleID string   // apikey: the single role the key is bound to
	Label  string   // apikey: the key's label (surfaced by /whoami)
}

// identityKey is the context.Value key for the resolved Identity. It is an
// unexported type so no other package can collide with it.
type identityKey struct{}

// identityFromContext returns the Identity the auth layer stashed on the
// request, or ok=false when the request did not go through an auth middleware
// (which should not happen for a gated route, but the guard keeps handlers
// safe).
func identityFromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(*Identity)
	return id, ok
}

// errIdentityRejected is the single generic error for "this bearer token does
// not authenticate": not a valid JWT AND not a live API key. It is one sentinel
// for both, and it carries no detail, so a caller cannot tell which mechanism
// refused it — the same anti-enumeration stance the credential path takes.
var errIdentityRejected = errors.New("identity rejected")

// resolveIdentity resolves a bearer token to an Identity, trying JWT first
// and falling back to an API key — the exact same order and semantics as the
// original inline logic in handleWhoami. It is the single reusable identity
// resolution function: /whoami and the permission middleware both call it, so
// there is no duplicated resolution logic.
//
// CONTRACT-19: the API-key branch calls internal/auth, which now takes the
// *compat.Store (connection + engine) rather than a bare *sql.DB. Only the
// parameter type changes; the resolution order and semantics are untouched.
//
// CONTRACT-24 REPLACED the `ok bool` return with an error, and the distinction
// it carries is the whole point. THE SAME REASONING AS handleLogin APPLIES HERE,
// so read it there before changing this: a REJECTED token yields
// errIdentityRejected → 401, exactly as before and indistinguishable between the
// JWT and the API-key case; a token the API-key lookup could not even CHECK,
// because the database did not answer, yields the underlying error → 503. The
// second outcome does not depend on the token, so it discloses nothing about any
// key or any account — it is a fact about this process's infrastructure, which
// is precisely what the caller and the operator need to be told. With `ok bool`
// the two were the same value, which is why a stopped database made every
// authenticated read answer 401.
func resolveIdentity(ctx context.Context, store *compat.Store, secret, token string) (*Identity, error) {
	// Try JWT first. VerifyJWT is the canonical check (rejects non-HMAC algs,
	// bad signature, expired). It touches no database, so it cannot fail for
	// infrastructure reasons and a valid JWT keeps working while the database is
	// down — the request itself will fail later, on its own terms.
	if claims, err := auth.VerifyJWT(secret, token); err == nil {
		return &Identity{
			Kind:   "jwt",
			UserID: claims.Subject,
			Email:  claims.Email,
			Roles:  claims.Roles,
		}, nil
	}
	// Fall back to API key: exact-match on the SHA-256 hash inside the DB,
	// rejected if revoked. Same path as /whoami.
	key, err := auth.VerifyAPIKey(ctx, store, token)
	switch {
	case err == nil:
		return &Identity{
			Kind:   "apikey",
			RoleID: key.RoleID,
			Label:  key.Label,
		}, nil
	case errors.Is(err, auth.ErrAPIKeyRejected):
		// The token is neither a valid JWT nor a live key. Genuinely 401.
		return nil, errIdentityRejected
	default:
		// The lookup could not run (auth.VerifyAPIKey wraps the query error).
		// The error is propagated so the HTTP layer can answer 503; it is NEVER
		// rendered into a response — see infraUnavailableMessage.
		return nil, err
	}
}

// writeIdentityError maps a resolveIdentity failure onto the wire: the
// unchanged 401 "unauthorized" envelope for a rejected token, 503 with the fixed
// generic message when the database could not be consulted. It is the single
// place that mapping is made, so /whoami and the middleware cannot drift.
func writeIdentityError(w http.ResponseWriter, err error) {
	if errors.Is(err, errIdentityRejected) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeInfraUnavailable(w)
}

// authenticate is the shared first stage of every protected route: it extracts
// the bearer token, resolves the Identity, and writes 401 on any failure. On
// success it returns a new request carrying the Identity in its context and
// the Identity itself, so the caller can decide whether to additionally gate by
// permission (requirePermission) or just proceed (requireAuth). Factoring the
// auth stage here keeps the two middleware wrappers free of duplicated
// resolution/401 logic.
func (h *handlers) authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, *Identity, bool) {
	token, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, nil, false
	}
	id, err := resolveIdentity(r.Context(), h.store, h.jwtSecret, token)
	if err != nil {
		// CONTRACT-24: 401 for a rejected token, 503 when the database could not
		// be consulted. This is the point the production measurement hit — every
		// authenticated route answered 401 with the database stopped.
		writeIdentityError(w, err)
		return nil, nil, false
	}
	return r.WithContext(context.WithValue(r.Context(), identityKey{}, id)), id, true
}

// requireAuth wraps a handler that needs ANY authenticated identity (no
// specific permission). Used by the read routes (GET /articles,
// GET /articles/{id}) — reading is not permission-gated in v1, only
// authenticated. It writes 401 when the caller is not authenticated.
func (h *handlers) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2, _, ok := h.authenticate(w, r)
		if !ok {
			return
		}
		next.ServeHTTP(w, r2)
	})
}

// requirePermission returns a middleware that wraps a handler requiring a
// specific permission. It authenticates (401 on failure), loads the caller's
// permission set from role_permissions, and writes 403 when the required
// permission is absent. The Identity is stashed on the request context for the
// wrapped handler (e.g. POST /articles needs the author id). This is the
// reusable http.Handler-wrapper the contract asks for — callers pass the
// handler once; they do not call a permission check by hand inside each
// handler.
func (h *handlers) requirePermission(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r2, id, ok := h.authenticate(w, r)
			if !ok {
				return
			}
			perms, err := h.permissionsFor(r2.Context(), id)
			if err != nil {
				// A DB failure resolving permissions is an internal error, not a
				// 401/403 — the caller IS authenticated, we just could not load
				// their grants.
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			if !containsString(perms, permission) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r2)
		})
	}
}

// permissionsFor returns the permission-name set for the resolved Identity:
// for a JWT identity, the union of permissions across all its roles; for an
// API-key identity, the permissions of its single bound role. Both are read
// from role_permissions joined to permissions. The set is resolved per request
// (not cached) — v1 has no per-request caching, and the catalog is small.
func (h *handlers) permissionsFor(ctx context.Context, id *Identity) ([]string, error) {
	if id.Kind == "jwt" {
		return h.permissionsForRoles(ctx, id.Roles)
	}
	return h.permissionsForRoleID(ctx, id.RoleID)
}

// permissionsForRoles returns the distinct permission names granted to any of
// the given role names. An empty role list yields no permissions (a user with no
// roles has no grants) — preserved exactly.
//
// CONTRACT-20: the two-JOIN query moved into the canonical view
// schema.ViewRoleNamePermissionNames, so the join is compiled to both engines by
// compat instead of being written here. What is left is the one thing a routine
// cannot express: an IN predicate whose arity comes from the caller (a routine's
// WHERE grammar has no such form, and a routine's action list is static). That
// residue is composed with compat.Placeholder, which is dual-engine — the
// statement below is standard SQL over a declared view, with holes.
//
// Two things changed that are NOT visible through HTTP and are deliberate:
// the DISTINCT is still the engine's, but the result is now explicitly ORDERED,
// where before it came back in whatever sequence each engine chose. Callers only
// ever ask "does this set contain X", so the order was never observable — but an
// unordered read is exactly the kind of latent divergence this contract exists
// to remove, and the dual-engine battery compares it.
func (h *handlers) permissionsForRoles(ctx context.Context, roleNames []string) ([]string, error) {
	if len(roleNames) == 0 {
		return nil, nil
	}
	// Role names come from the verified JWT claims, not from user input, but
	// they are still bound as parameters — never interpolated.
	statement := `SELECT DISTINCT ` + quote("permission_name") +
		` FROM ` + quote(schema.ViewRoleNamePermissionNames) +
		` WHERE ` + quote("role_name") + ` IN (` + bindList(h.engine(), 1, len(roleNames)) + `)` +
		` ORDER BY ` + quote("permission_name")
	args := make([]any, len(roleNames))
	for i, n := range roleNames {
		args[i] = n
	}
	rows, err := h.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query role permissions: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan permission: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate permissions: %w", err)
	}
	// The final order is imposed HERE, not by the engine: permission names carry
	// `.` and `_`, and PostgreSQL's collation ranks those differently from
	// SQLite's byte order. See the COLLATION note in dual.go.
	dual.SortStrings(names)
	return names, nil
}

// permissionsForRoleID returns the permission names granted to the single role
// an API key is bound to. This one IS expressible as a routine (a single bound
// key), so it is one: schema.RoutinePermissionsByRoleID over CONTRACT-19's
// existing role_permission_names view — the JOIN this function used to write by
// hand was already declared there and is simply reused.
func (h *handlers) permissionsForRoleID(ctx context.Context, roleID string) ([]string, error) {
	rows, err := h.queryRoutine(ctx, schema.RoutinePermissionsByRoleID,
		map[string]compat.Value{"role_id": dual.UUIDValue(roleID)})
	if err != nil {
		return nil, fmt.Errorf("query role permissions: %w", err)
	}
	var names []string
	for _, row := range rows {
		names = append(names, dual.RowText(row, "permission_name"))
	}
	dual.SortStrings(names)
	return names, nil
}

// containsString reports whether s contains v.
func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
