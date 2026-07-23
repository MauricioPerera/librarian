package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/MauricioPerera/librarian/internal/auth"
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

// resolveIdentity resolves a bearer token to an Identity, trying JWT first
// and falling back to an API key — the exact same order and semantics as the
// original inline logic in handleWhoami. It is the single reusable identity
// resolution function: /whoami and the permission middleware both call it, so
// there is no duplicated resolution logic. Returns ok=false when neither
// mechanism authenticates (the caller writes 401).
func resolveIdentity(ctx context.Context, db *sql.DB, secret, token string) (*Identity, bool) {
	// Try JWT first. VerifyJWT is the canonical check (rejects non-HMAC algs,
	// bad signature, expired).
	if claims, err := auth.VerifyJWT(secret, token); err == nil {
		return &Identity{
			Kind:   "jwt",
			UserID: claims.Subject,
			Email:  claims.Email,
			Roles:  claims.Roles,
		}, true
	}
	// Fall back to API key: exact-match on the SHA-256 hash inside the DB,
	// rejected if revoked. Same path as /whoami.
	if key, err := auth.VerifyAPIKey(ctx, db, token); err == nil {
		return &Identity{
			Kind:   "apikey",
			RoleID: key.RoleID,
			Label:  key.Label,
		}, true
	}
	return nil, false
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
	id, ok := resolveIdentity(r.Context(), h.db, h.jwtSecret, token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
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
		return permissionsForRoles(ctx, h.db, id.Roles)
	}
	return permissionsForRoleID(ctx, h.db, id.RoleID)
}

// permissionsForRoles returns the distinct permission names granted to any of
// the given role names, via role_permissions. An empty role list yields no
// permissions (a user with no roles has no grants).
func permissionsForRoles(ctx context.Context, db *sql.DB, roleNames []string) ([]string, error) {
	if len(roleNames) == 0 {
		return nil, nil
	}
	// Role names come from the verified JWT claims, not user input, but they
	// are still bound as parameters (never interpolated). The IN list is built
	// with the standard placeholder-per-arg pattern.
	q := `SELECT DISTINCT p.name
	        FROM role_permissions rp
	        JOIN roles r       ON r.id = rp.role_id
	        JOIN permissions p  ON p.id = rp.permission_id
	       WHERE r.name IN (` + placeholders(len(roleNames)) + `)`
	args := make([]any, len(roleNames))
	for i, n := range roleNames {
		args[i] = n
	}
	rows, err := db.QueryContext(ctx, q, args...)
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
	return names, nil
}

// permissionsForRoleID returns the permission names granted to the single role
// an API key is bound to.
func permissionsForRoleID(ctx context.Context, db *sql.DB, roleID string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT p.name
		   FROM role_permissions rp
		   JOIN permissions p ON p.id = rp.permission_id
		  WHERE rp.role_id = ?`,
		roleID,
	)
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
	return names, nil
}

// placeholders returns a "?, ?, ?" list of n placeholders for an IN clause.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(n*3 - 2)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('?')
	}
	return b.String()
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
