package server

// CONTRACT-08 — admin UI for users and the read-only roles/permissions view.
// These HTML routes live under /admin/users (parallel to /admin/articles) and
// /admin/roles, on the SAME mux, and reuse the auth package's data functions
// (auth.ListUsers/GetUser/CreateUser/UpdateUserStatus/SetUserRoles) — the UI
// reimplements no SQL. Write routes (create user, change status, replace roles)
// are gated by requireSessionPermission("users.manage"); the read routes (list,
// new-form, detail, roles view) require only a valid session, mirroring the rest
// of the UI.
//
// Swap/redirect design decisions (documented in the CONTRACT-08 report):
//   - Create user: a plain HTML form POST → 303 redirect to the user list (full
//     page). A create has no row to swap and the natural next view is the list.
//   - Change status / replace roles: hx-post from the detail page; on success the
//     server replies with an HX-Redirect header back to the SAME detail page, so
//     htmx re-navigates and the page shows the just-persisted state (new badge,
//     new checked roles) with no full document reload. Chosen over an in-place
//     fragment swap because both writes change several parts of the detail view
//     (status badge, roles line, selected option) at once, and a single redirect
//     re-renders all of them from the source of truth without hand-maintaining a
//     partial.
//
// KNOWN LIMITATION (documented, intentionally NOT fixed in this contract):
// suspending or changing the roles of a user does NOT affect a session that user
// already holds. requireSession only re-validates the JWT signature+expiry on
// each request; it does not re-read users.status or user_roles. So a suspended
// user with a live session cookie keeps access until that JWT expires (24h TTL,
// see sessionMaxAgeSeconds). VerifyCredentials rejects the NEXT login. Closing
// this gap (per-request status/role revalidation, or a token deny-list) is a
// separate scope decision, not part of CONTRACT-08.

import (
	"context"
	"errors"
	"net/http"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/schema"
)

// Admin user/role template sets. One set per page (layout + page) following the
// CONTRACT-06/07 pattern that avoids a shared "content" definition collision.
var (
	adminUsersListTmpl   = mustParseFS("templates/layout.html", "templates/users_list.html")
	adminUsersNewTmpl    = mustParseFS("templates/layout.html", "templates/users_new.html")
	adminUsersDetailTmpl = mustParseFS("templates/layout.html", "templates/users_detail.html")
	adminRolesTmpl       = mustParseFS("templates/layout.html", "templates/roles_list.html")
)

// userView is the row/detail view model for the user templates.
type userView struct {
	ID     string
	Email  string
	Status string
	Roles  []string
}

// roleCheck is one role checkbox: its catalog name and whether it is currently
// assigned to the user being viewed/created.
type roleCheck struct {
	Name    string
	Checked bool
}

// statusOption is one <option> in the status selector: the status value and
// whether it is the user's current status (pre-selected).
type statusOption struct {
	Value    string
	Selected bool
}

// rolePermsView is one row of the read-only roles/permissions view: a role and
// the permission names actually granted to it (read live from role_permissions).
type rolePermsView struct {
	Name        string
	Permissions []string
}

// adminUsersListPage is the view model for the user list.
type adminUsersListPage struct {
	pageData
	Users []userView
}

// adminUserNewPage is the view model for the create-user form. FormEmail and
// Roles are preserved across a re-render after a validation error; Error is the
// banner. Named FormEmail (not Email) because pageData already promotes an
// Email field for the nav's logged-in-user display — an outer field with the
// same name would shadow it in the template, showing the entered form value
// (empty on first render) instead of the session's email in the nav.
type adminUserNewPage struct {
	pageData
	FormEmail string
	Roles     []roleCheck
	Error     string
}

// adminUserDetailPage is the view model for the combined detail + edit page.
type adminUserDetailPage struct {
	pageData
	User     userView
	Statuses []statusOption
	Roles    []roleCheck
}

// adminRolesPage is the view model for the read-only roles/permissions view.
type adminRolesPage struct {
	pageData
	Roles []rolePermsView
}

// registerAdminUserRoutes wires the /admin/users HTML surface plus the read-only
// /admin/roles view. Read routes require only a session; the three write routes
// are gated by users.manage via requireSessionPermission.
func (h *handlers) registerAdminUserRoutes(mux *http.ServeMux) {
	mux.Handle("GET /admin/users", h.requireSession(http.HandlerFunc(h.handleAdminUsersList)))
	mux.Handle("GET /admin/users/new", h.requireSession(http.HandlerFunc(h.handleAdminUserNewForm)))
	mux.Handle("POST /admin/users", h.requireSessionPermission("users.manage")(http.HandlerFunc(h.handleAdminUserCreate)))
	mux.Handle("GET /admin/users/{id}", h.requireSession(http.HandlerFunc(h.handleAdminUserDetail)))
	mux.Handle("POST /admin/users/{id}/status", h.requireSessionPermission("users.manage")(http.HandlerFunc(h.handleAdminUserStatus)))
	mux.Handle("POST /admin/users/{id}/roles", h.requireSessionPermission("users.manage")(http.HandlerFunc(h.handleAdminUserRoles)))
	mux.Handle("GET /admin/roles", h.requireSession(http.HandlerFunc(h.handleAdminRolesList)))
}

// handleAdminUsersList renders the user list (email, status, roles).
func (h *handlers) handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	users, err := auth.ListUsers(r.Context(), h.db)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	views := make([]userView, 0, len(users))
	for _, u := range users {
		views = append(views, userView{ID: u.ID, Email: u.Email, Status: u.Status, Roles: u.Roles})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminUsersListTmpl.ExecuteTemplate(w, "layout", adminUsersListPage{
		pageData: pageData{Title: "Usuarios — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
		Users:    views,
	})
}

// handleAdminUserNewForm renders the empty create-user form with all catalog
// roles unchecked.
func (h *handlers) handleAdminUserNewForm(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	renderUserNew(w, http.StatusOK, adminUserNewPage{
		pageData: pageData{Title: "Nuevo usuario — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
		Roles:    roleChecks(nil),
	})
}

// handleAdminUserCreate handles the create-user form POST (users.manage). It
// requires a non-empty email + password, reuses auth.CreateUser (which hashes
// the password and creates the user as active), and 303-redirects to the list.
// An unknown role in the crafted POST or a duplicate email re-renders the form
// with a 400 and the error, preserving the entered email and role selection.
func (h *handlers) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		renderUserNew(w, http.StatusBadRequest, adminUserNewPage{
			pageData: pageData{Title: "Nuevo usuario — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
			Roles:    roleChecks(nil),
			Error:    "Formulario inválido.",
		})
		return
	}
	email := r.PostFormValue("email")
	password := r.PostFormValue("password")
	roles := r.PostForm["roles"]
	if email == "" || password == "" {
		renderUserNew(w, http.StatusBadRequest, adminUserNewPage{
			pageData:  pageData{Title: "Nuevo usuario — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
			FormEmail: email,
			Roles:     roleChecks(roles),
			Error:     "email and password are required",
		})
		return
	}
	if _, err := auth.CreateUser(r.Context(), h.db, email, password, roles); err != nil {
		// Unknown role (crafted request) or duplicate email — a client error, not
		// a 500. Re-render with the reason and the user's input preserved.
		renderUserNew(w, http.StatusBadRequest, adminUserNewPage{
			pageData:  pageData{Title: "Nuevo usuario — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
			FormEmail: email,
			Roles:     roleChecks(roles),
			Error:     userCreateErrorMessage(err),
		})
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// handleAdminUserDetail renders the combined detail + edit page. A missing or
// malformed id → 404 HTML (never 500), the same pattern as the articles UI.
func (h *handlers) handleAdminUserDetail(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	u, found, err := auth.GetUser(r.Context(), h.db, r.PathValue("id"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		renderNotFound(w, emailOf(idn))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminUsersDetailTmpl.ExecuteTemplate(w, "layout", adminUserDetailPage{
		pageData: pageData{Title: "Usuario — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
		User:     userView{ID: u.ID, Email: u.Email, Status: u.Status, Roles: u.Roles},
		Statuses: statusOptions(u.Status),
		Roles:    roleChecks(u.Roles),
	})
}

// handleAdminUserStatus handles hx-post from the status form (users.manage). A
// missing id → 404 HTML; an invalid status value (crafted request, the select
// only offers valid ones) → 400. On success it sets HX-Redirect back to the
// detail page so htmx re-navigates and the page shows the persisted status.
func (h *handlers) handleAdminUserStatus(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	uid := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	err := auth.UpdateUserStatus(r.Context(), h.db, uid, r.PostFormValue("status"))
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		renderNotFound(w, emailOf(idn))
		return
	case errors.Is(err, auth.ErrInvalidStatus):
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", "/admin/users/"+uid)
	w.WriteHeader(http.StatusOK)
}

// handleAdminUserRoles handles hx-post from the roles form (users.manage). It
// replaces the user's entire role set with the checked boxes (empty = remove
// all). A missing id → 404 HTML; an unknown role in a crafted POST → 400 (the
// assignment is rejected and nothing changes). On success it sets HX-Redirect
// back to the detail page.
func (h *handlers) handleAdminUserRoles(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	uid := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	err := auth.SetUserRoles(r.Context(), h.db, uid, r.PostForm["roles"])
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		renderNotFound(w, emailOf(idn))
		return
	case errors.Is(err, auth.ErrUnknownRole):
		http.Error(w, "unknown role", http.StatusBadRequest)
		return
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", "/admin/users/"+uid)
	w.WriteHeader(http.StatusOK)
}

// handleAdminRolesList renders the read-only roles/permissions view. It reads the
// live role_permissions table (via rolesWithPermissions), NOT a hardcoded map, so
// a grant made elsewhere shows up here. Requires only a session — no write.
func (h *handlers) handleAdminRolesList(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	roles, err := h.rolesWithPermissions(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminRolesTmpl.ExecuteTemplate(w, "layout", adminRolesPage{
		pageData: pageData{Title: "Roles y permisos — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
		Roles:    roles,
	})
}

// renderUserNew writes the create-user form with the given status/data.
func renderUserNew(w http.ResponseWriter, status int, data adminUserNewPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = adminUsersNewTmpl.ExecuteTemplate(w, "layout", data)
}

// roleChecks builds the checkbox view models for the fixed role catalog
// (schema.Roles), marking each role checked when it is in assigned.
func roleChecks(assigned []string) []roleCheck {
	out := make([]roleCheck, 0, len(schema.Roles))
	for _, name := range schema.Roles {
		out = append(out, roleCheck{Name: name, Checked: containsString(assigned, name)})
	}
	return out
}

// statusOptions builds the status selector options from the fixed status set
// (auth.UserStatuses), pre-selecting current.
func statusOptions(current string) []statusOption {
	out := make([]statusOption, 0, len(auth.UserStatuses))
	for _, s := range auth.UserStatuses {
		out = append(out, statusOption{Value: s, Selected: s == current})
	}
	return out
}

// userCreateErrorMessage maps a CreateUser failure to a user-facing message: a
// clear one for an unknown role, otherwise a generic "could not create" (a
// duplicate email or any other DB error) — the raw SQL error is never shown.
func userCreateErrorMessage(err error) string {
	if errors.Is(err, auth.ErrUnknownRole) {
		return "Rol desconocido."
	}
	return "No se pudo crear el usuario (¿email ya registrado?)."
}

// rolesWithPermissions returns every catalog role with the permission names
// actually granted to it, read from role_permissions via the shared
// permissionsForRoleID helper (authz.go). It reflects the live table, so the
// read-only view is never a hardcoded catalog.
func (h *handlers) rolesWithPermissions(ctx context.Context) ([]rolePermsView, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT id, name FROM roles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type roleRow struct{ id, name string }
	var roleRows []roleRow
	for rows.Next() {
		var rr roleRow
		if err := rows.Scan(&rr.id, &rr.name); err != nil {
			return nil, err
		}
		roleRows = append(roleRows, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]rolePermsView, 0, len(roleRows))
	for _, rr := range roleRows {
		perms, err := permissionsForRoleID(ctx, h.db, rr.id)
		if err != nil {
			return nil, err
		}
		out = append(out, rolePermsView{Name: rr.name, Permissions: perms})
	}
	return out, nil
}
