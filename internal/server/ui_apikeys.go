package server

// CONTRACT-09 — admin UI for API keys: create (show the secret once), list, and
// revoke. These HTML routes live under /admin/api-keys (parallel to
// /admin/users) on the SAME mux and reuse the auth package's data functions
// (auth.MintAPIKey/ListAPIKeys/RevokeAPIKeyByID/GetAPIKey) — the UI reimplements
// no SQL and no crypto. The two write routes (create, revoke) are gated by the
// EXISTING users.manage permission via requireSessionPermission — API keys are
// an access-control resource like users, and the fixed permission catalog is not
// expanded for this contract (see CONTRACT-09 RECON). The read routes (list,
// new-form) require only a valid session.
//
// Design decisions (documented in the CONTRACT-09 report):
//   - Create: a plain HTML form POST → renders a success page DIRECTLY (no
//     redirect) showing the freshly minted secret ONCE with a clear warning. A
//     redirect would drop the secret, which is recoverable only at this instant;
//     rendering it inline is the single point in the whole contract where the
//     plaintext exists in any HTTP response.
//   - Revoke: hx-post to /admin/api-keys/{id}/revoke (NOT hx-delete). Revocation
//     is a state transition that KEEPS the row as a historical record — the exact
//     analog of the articles "publish" action (hx-post → single updated <tr>
//     swapped in place), not the articles "delete" action (hx-delete → row
//     removed). The server replies with the updated row fragment; htmx swaps it
//     via hx-swap="outerHTML", flipping the row from Activa to Revocada in place.
//   - Revoked row: the revoke button is replaced by the revoked-on text; there is
//     nothing left to revoke, so no button is shown. The row never disappears.

import (
	"context"
	"database/sql"
	"errors"
	"html/template"
	"net/http"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/schema"
)

// Admin API-key template sets. One set per page (layout + page), following the
// CONTRACT-06/07/08 pattern. The list set also parses the row fragment so the
// list can range over rows calling "apikey_row"; adminAPIKeysRowTmpl parses the
// fragment standalone so the revoke handler can render a single <tr> for an htmx
// swap.
var (
	adminAPIKeysListTmpl = template.Must(template.ParseFS(templatesFS,
		"templates/layout.html", "templates/apikeys_list.html", "templates/apikeys_row.html"))
	adminAPIKeysRowTmpl     = template.Must(template.ParseFS(templatesFS, "templates/apikeys_row.html"))
	adminAPIKeysNewTmpl     = template.Must(template.ParseFS(templatesFS, "templates/layout.html", "templates/apikeys_new.html"))
	adminAPIKeysCreatedTmpl = template.Must(template.ParseFS(templatesFS, "templates/layout.html", "templates/apikeys_created.html"))
)

// apiKeyView is the row/list view model. It carries NO secret and NO key_hash —
// the list and row templates render only these fields, so neither can appear in
// the listing HTML.
type apiKeyView struct {
	ID        string
	Label     string
	RoleName  string
	CreatedAt string
	Revoked   bool
	RevokedAt string
}

// toAPIKeyView maps the auth record to the template view model.
func toAPIKeyView(r auth.APIKeyRecord) apiKeyView {
	return apiKeyView{
		ID:        r.ID,
		Label:     r.Label,
		RoleName:  r.RoleName,
		CreatedAt: r.CreatedAt,
		Revoked:   r.Revoked,
		RevokedAt: r.RevokedAt,
	}
}

// adminAPIKeysListPage is the view model for the API-key list.
type adminAPIKeysListPage struct {
	pageData
	Keys []apiKeyView
}

// adminAPIKeyNewPage is the view model for the create form. FormLabel and
// SelectedRole are preserved across a re-render after a validation error; Error
// is the banner. Roles is the fixed catalog (schema.Roles) offered in the
// selector.
type adminAPIKeyNewPage struct {
	pageData
	FormLabel    string
	Roles        []string
	SelectedRole string
	Error        string
}

// adminAPIKeyCreatedPage is the view model for the ONE-TIME success page. Secret
// is the plaintext secret, shown here and NOWHERE else.
type adminAPIKeyCreatedPage struct {
	pageData
	Label    string
	RoleName string
	Secret   string
}

// registerAdminAPIKeyRoutes wires the /admin/api-keys HTML surface. Read routes
// require only a session; the two write routes (create, revoke) are gated by
// users.manage via requireSessionPermission.
func (h *handlers) registerAdminAPIKeyRoutes(mux *http.ServeMux) {
	mux.Handle("GET /admin/api-keys", h.requireSession(http.HandlerFunc(h.handleAdminAPIKeysList)))
	mux.Handle("GET /admin/api-keys/new", h.requireSession(http.HandlerFunc(h.handleAdminAPIKeyNewForm)))
	mux.Handle("POST /admin/api-keys", h.requireSessionPermission("users.manage")(http.HandlerFunc(h.handleAdminAPIKeyCreate)))
	mux.Handle("POST /admin/api-keys/{id}/revoke", h.requireSessionPermission("users.manage")(http.HandlerFunc(h.handleAdminAPIKeyRevoke)))
}

// handleAdminAPIKeysList renders the API-key list (label, role name, created,
// status). It never shows the secret or the hash.
func (h *handlers) handleAdminAPIKeysList(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	keys, err := auth.ListAPIKeys(r.Context(), h.db)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	views := make([]apiKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, toAPIKeyView(k))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminAPIKeysListTmpl.ExecuteTemplate(w, "layout", adminAPIKeysListPage{
		pageData: pageData{Title: "API keys — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
		Keys:     views,
	})
}

// handleAdminAPIKeyNewForm renders the empty create form with the fixed role
// catalog in the selector.
func (h *handlers) handleAdminAPIKeyNewForm(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	renderAPIKeyNew(w, http.StatusOK, adminAPIKeyNewPage{
		pageData: pageData{Title: "Nueva API key — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
		Roles:    schema.Roles,
	})
}

// handleAdminAPIKeyCreate handles the create form POST (users.manage). It
// requires a non-empty label + a role from the fixed catalog, resolves the role
// name to its id, reuses auth.MintAPIKey (never reimplementing the crypto), and
// renders the success page with the plaintext secret shown ONCE. An empty field
// or an unknown role (crafted POST) re-renders the form with a 400 and the
// error, preserving the entered label and role selection.
func (h *handlers) handleAdminAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	title := pageData{Title: "Nueva API key — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path}
	if err := r.ParseForm(); err != nil {
		renderAPIKeyNew(w, http.StatusBadRequest, adminAPIKeyNewPage{
			pageData: title, Roles: schema.Roles, Error: "Formulario inválido.",
		})
		return
	}
	label := r.PostFormValue("label")
	role := r.PostFormValue("role")
	if label == "" || role == "" {
		renderAPIKeyNew(w, http.StatusBadRequest, adminAPIKeyNewPage{
			pageData: title, Roles: schema.Roles, FormLabel: label, SelectedRole: role,
			Error: "El label y el rol son obligatorios.",
		})
		return
	}
	roleID, ok, err := roleIDForName(r.Context(), h.db, role)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		renderAPIKeyNew(w, http.StatusBadRequest, adminAPIKeyNewPage{
			pageData: title, Roles: schema.Roles, FormLabel: label,
			Error: "Rol desconocido.",
		})
		return
	}
	secret, err := auth.MintAPIKey(r.Context(), h.db, label, roleID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Render the secret directly — the ONE and ONLY response in this contract
	// that contains the plaintext. No redirect: a redirect would lose it.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminAPIKeysCreatedTmpl.ExecuteTemplate(w, "layout", adminAPIKeyCreatedPage{
		pageData: pageData{Title: "API key creada — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
		Label:    label,
		RoleName: role,
		Secret:   secret,
	})
}

// handleAdminAPIKeyRevoke handles hx-post from a list row (users.manage). It
// revokes by id (idempotent: an already-revoked or repeated call is a no-op),
// then re-fetches the row and returns the single updated <tr> fragment so htmx
// swaps it in place — the row flips Activa→Revocada and stays in the list. A
// genuinely unknown id (no such row) → 404 HTML, never a 500.
func (h *handlers) handleAdminAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	id := r.PathValue("id")
	if err := auth.RevokeAPIKeyByID(r.Context(), h.db, id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rec, found, err := auth.GetAPIKey(r.Context(), h.db, id)
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
	_ = adminAPIKeysRowTmpl.ExecuteTemplate(w, "apikey_row", toAPIKeyView(rec))
}

// renderAPIKeyNew writes the create form with the given status/data.
func renderAPIKeyNew(w http.ResponseWriter, status int, data adminAPIKeyNewPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = adminAPIKeysNewTmpl.ExecuteTemplate(w, "layout", data)
}

// roleIDForName resolves a role name to its catalog id. ok is false when the
// name is not in the roles table (a crafted request) — never a raw SQL error —
// so the handler maps it to a 400 rather than a 500.
func roleIDForName(ctx context.Context, db *sql.DB, name string) (string, bool, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM roles WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}
