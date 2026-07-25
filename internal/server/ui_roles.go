package server

// CONTRACT-16 T2 + T3 — the per-role permission editor, gated by roles.manage,
// with the anti-lockout guard.
//
// It extends the EXISTING read-only /admin/roles view (handleAdminRolesList,
// ui_users.go) instead of introducing a parallel surface. Routes:
//
//	GET  /admin/roles/{name}/edit        → the checkbox form for one role
//	POST /admin/roles/{name}/permissions → replaces that role's whole grant set
//
// Route-shape decision: the role NAME is the path key (roles are a fixed,
// name-unique catalog and the read-only view already identifies rows by name —
// `<tr id="role-{{.Name}}">`), and the write verb lives on a `/permissions`
// sub-resource rather than on the role itself, so the URL says exactly WHAT is
// being replaced: this contract edits GRANTS, never the role catalog. A future
// POST /admin/roles (create a role) would therefore not collide with it.
//
// Form-shape decision: a plain `method="post"` HTML form, NOT hx-post. The
// project uses hx-* for writes whose only outcomes are success or a 404
// (users_detail.html) and a plain POST for every form that must be able to
// re-render itself with an error banner (users_new.html, products_new.html,
// content_types_new.html). This form is in the second family: T3 requires that a
// guard rejection be shown as a FORM ERROR, and htmx does not swap a non-2xx
// response by default, so an hx-post rejection would silently do nothing in the
// browser. Success 303-redirects back to the edit page, which then renders the
// just-persisted checkboxes from the source of truth.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/schema"
)

// permRolesManage is the permission that gates every write in this file. It has
// existed in the catalog since CONTRACT-02 and had NO consumer until now,
// precisely because this capability had never been built. No new permission is
// introduced by this contract.
const permRolesManage = "roles.manage"

// adminRoleEditTmpl is the edit page's template set (layout + page), following
// the one-set-per-page pattern of the rest of the UI.
var adminRoleEditTmpl = mustParseFS("templates/layout.html", "templates/roles_edit.html")

// permissionCheck is one permission checkbox: its catalog name and whether the
// role being edited currently holds it.
type permissionCheck struct {
	Name    string
	Checked bool
}

// adminRoleEditPage is the view model for the per-role permission editor. Error
// is the form banner (guard rejection or an invalid submission); the checkbox
// state is preserved across a re-render so the admin does not lose their edit.
type adminRoleEditPage struct {
	pageData
	RoleName    string
	Permissions []permissionCheck
	Error       string
}

// registerAdminRoleRoutes wires the CONTRACT-16 write surface. The read-only
// listing (GET /admin/roles) keeps its session-only gate and stays registered
// with the rest of the users UI; both routes here require roles.manage, so a
// session without it gets the 403 HTML page — never a JSON envelope.
func (h *handlers) registerAdminRoleRoutes(mux *http.ServeMux) {
	mux.Handle("GET /admin/roles/{name}/edit",
		h.requireSessionPermission(permRolesManage)(http.HandlerFunc(h.handleAdminRoleEditForm)))
	mux.Handle("POST /admin/roles/{name}/permissions",
		h.requireSessionPermission(permRolesManage)(http.HandlerFunc(h.handleAdminRolePermissions)))
}

// handleAdminRoleEditForm renders the checkbox form for one role: EVERY
// permission of the fixed catalog, with the ones currently granted pre-checked.
// A role name outside the catalog → 404 HTML (never a 500).
func (h *handlers) handleAdminRoleEditForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	current, found, err := auth.RolePermissions(r.Context(), h.authStore, name)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		h.renderNotFound(w, r)
		return
	}
	renderRoleEdit(w, http.StatusOK, adminRoleEditPage{
		pageData:    h.page(r, "Editar permisos del rol — librarian"),
		RoleName:    name,
		Permissions: permissionChecks(current),
	})
}

// handleAdminRolePermissions replaces a role's whole permission set with the
// checked boxes (none checked = revoke everything from that role).
//
// Order of checks, each one deliberate:
//  1. role must exist                     → 404 HTML
//  2. every submitted name must be in the → 400, re-rendered form, NOTHING mutated
//     fixed catalog (crafted request)
//  3. the anti-lockout guard (T2)         → 409, re-rendered form with the reason
//  4. the atomic replacement              → 303 back to the edit page
//
// (2) runs BEFORE (3) on purpose: a crafted body carrying a nonexistent
// permission is a malformed request and must answer 400 regardless of what the
// guard would have said about the rest of the set.
func (h *handlers) handleAdminRolePermissions(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		h.renderRoleEditError(w, r, name, nil, http.StatusBadRequest, "Formulario inválido.")
		return
	}
	selected := r.PostForm["permissions"]

	// (1) Unknown role → 404, before anything else.
	_, found, err := auth.RolePermissions(r.Context(), h.authStore, name)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		h.renderNotFound(w, r)
		return
	}

	// (2) Every submitted name must be in the code-fixed catalog. The checkboxes
	// can only offer catalog names, so this only ever fires on a crafted request.
	// auth.SetRolePermissions enforces the same invariant against the seeded
	// table (defence in depth); this check exists so the answer is a rendered 400
	// form rather than a bare error, and so it is decided before the guard.
	if bad, ok := firstUnknownPermission(selected); !ok {
		h.renderRoleEditError(w, r, name, selected, http.StatusBadRequest,
			"Permiso desconocido: "+bad+". No se cambió nada.")
		return
	}

	// (3) The anti-lockout guard.
	id, _ := identityFromContext(r.Context())
	keeps, err := h.actorKeepsRolesManage(r.Context(), id, name, selected)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !keeps {
		h.renderRoleEditError(w, r, name, selected, http.StatusConflict, lockoutMessage(name))
		return
	}

	// (4) Atomic replacement.
	switch err := auth.SetRolePermissions(r.Context(), h.authStore, name, selected); {
	case errors.Is(err, auth.ErrRoleNotFound):
		h.renderNotFound(w, r)
		return
	case errors.Is(err, auth.ErrUnknownPermission):
		h.renderRoleEditError(w, r, name, selected, http.StatusBadRequest,
			"Permiso desconocido. No se cambió nada.")
		return
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/roles/"+name+"/edit", http.StatusSeeOther)
}

// actorKeepsRolesManage is THE anti-lockout guard (T2). It answers: after
// applying newPerms to editedRole, would the identity performing the change
// STILL hold roles.manage?
//
// The whole point is that it evaluates the RESULTING state, not the current one.
// The computation:
//
//   - Resolve the actor's own role names (JWT: the verified token claims — the
//     same source permissionsFor uses; API key: the single role the key is bound
//     to, resolved from its role_id).
//   - If editedRole is NOT one of them, the change cannot alter the actor's
//     effective permissions at all → allow. This is what makes "revoke
//     roles.manage from a role I do not have" legal.
//   - If editedRole IS one of them and newPerms still contains roles.manage, the
//     actor keeps it through that very role → allow. This is what makes "edit my
//     own role, dropping content.create but keeping roles.manage" legal.
//   - Otherwise the actor would lose it THROUGH editedRole, so the answer depends
//     entirely on the actor's OTHER roles: read their live grants and check
//     whether any of them still yields roles.manage. Two roles that both grant it
//     → removing it from one is allowed; the SECOND such operation is then
//     rejected, because by then no other role supplies it.
//
// Deliberately identity-kind-agnostic: an API key bound to the edited role can
// lock ITSELF out exactly like a human, and is stopped by the same code path.
// (In this contract the editor surface is session-only, so an API key cannot
// reach it — see the report — but the guard is where the rule lives, not the
// route, so a future JSON route inherits it instead of reimplementing it.)
//
// A nil identity (which cannot happen behind requireSessionPermission) is
// treated as "does not keep it" — fail closed.
func (h *handlers) actorKeepsRolesManage(ctx context.Context, id *Identity, editedRole string, newPerms []string) (bool, error) {
	if id == nil {
		return false, nil
	}
	actorRoles, err := h.actorRoleNames(ctx, id)
	if err != nil {
		return false, err
	}
	if !containsString(actorRoles, editedRole) {
		// The edited role is not one of the actor's — their effective set is
		// untouched by this change.
		return true, nil
	}
	if containsString(newPerms, permRolesManage) {
		// The actor keeps it through the very role being edited.
		return true, nil
	}
	others := make([]string, 0, len(actorRoles))
	for _, rn := range actorRoles {
		if rn != editedRole {
			others = append(others, rn)
		}
	}
	if len(others) == 0 {
		return false, nil
	}
	perms, err := permissionsForRoles(ctx, h.db, others)
	if err != nil {
		return false, err
	}
	return containsString(perms, permRolesManage), nil
}

// actorRoleNames returns the role NAMES the identity holds, normalizing the two
// identity kinds to the single representation the guard reasons about. A JWT
// carries its role names directly (the same claims permissionsFor trusts); an
// API key carries a role id, resolved here against the catalog. An api-key row
// whose role vanished yields no roles, which makes the guard fail closed.
func (h *handlers) actorRoleNames(ctx context.Context, id *Identity) ([]string, error) {
	if id.Kind == "jwt" {
		return id.Roles, nil
	}
	var name string
	err := h.db.QueryRowContext(ctx, `SELECT name FROM roles WHERE id = ?`, id.RoleID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []string{name}, nil
}

// lockoutMessage is the guard's user-facing explanation: why it was refused and
// the concrete way out (grant first, revoke second).
func lockoutMessage(role string) string {
	return "No se guardó: este cambio te dejaría sin el permiso " + permRolesManage +
		", y quedarías sin poder administrar permisos. Si estás reorganizando roles, " +
		"otorgá primero " + permRolesManage + " al otro rol y recién después quitáselo a " + role + "."
}

// renderRoleEditError re-renders the edit form with a banner, preserving the
// submitted checkbox selection so the admin does not lose their work. It never
// writes a 500 or a JSON envelope — a rejected edit is a form error.
func (h *handlers) renderRoleEditError(w http.ResponseWriter, r *http.Request, name string, selected []string, status int, msg string) {
	renderRoleEdit(w, status, adminRoleEditPage{
		pageData:    h.page(r, "Editar permisos del rol — librarian"),
		RoleName:    name,
		Permissions: permissionChecks(selected),
		Error:       msg,
	})
}

// renderRoleEdit writes the edit page with the given status/data.
func renderRoleEdit(w http.ResponseWriter, status int, data adminRoleEditPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = adminRoleEditTmpl.ExecuteTemplate(w, "layout", data)
}

// permissionChecks builds the checkbox view models from the code-fixed permission
// catalog (schema.Permissions), marking each one checked when it is in granted —
// the exact mirror of roleChecks (ui_users.go) for roles.
func permissionChecks(granted []string) []permissionCheck {
	out := make([]permissionCheck, 0, len(schema.Permissions))
	for _, name := range schema.Permissions {
		out = append(out, permissionCheck{Name: name, Checked: containsString(granted, name)})
	}
	return out
}

// firstUnknownPermission reports the first submitted name absent from the fixed
// catalog. ok is true when every name is a catalog permission.
func firstUnknownPermission(names []string) (string, bool) {
	for _, n := range names {
		if !containsString(schema.Permissions, n) {
			return n, false
		}
	}
	return "", true
}

// sessionCanManageRoles reports whether the current session holds roles.manage,
// so the read-only listing can show or hide the "Editar" link. It is UI polish
// only — the authoritative gate is requireSessionPermission on the routes above,
// which a crafted POST hits regardless of what the listing rendered.
func (h *handlers) sessionCanManageRoles(ctx context.Context, id *Identity) bool {
	if id == nil {
		return false
	}
	perms, err := h.permissionsFor(ctx, id)
	if err != nil {
		return false
	}
	return containsString(perms, permRolesManage)
}
