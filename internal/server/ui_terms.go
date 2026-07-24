package server

// CONTRACT-12 T4 — admin UI for taxonomy terms (categories/tags). These HTML
// routes live under /admin/terms (parallel to /admin/products) and reuse the
// shared term data-access helpers in terms.go — the UI reimplements no SQL. Write
// routes are gated by requireSessionPermission("terms.manage"); read routes only
// require a session. It follows the ui_products.go conventions exactly (per-page
// template sets, form POST → 303 on create, hx-put → HX-Redirect on update,
// hx-delete → empty 200 on delete).

import (
	"context"
	"errors"
	"html/template"
	"net/http"

	"github.com/MauricioPerera/librarian/internal/schema"
)

var (
	adminTermListTmpl = mustParseFS("templates/layout.html", "templates/terms_list.html", "templates/terms_row.html")
	adminTermNewTmpl  = mustParseFS("templates/layout.html", "templates/terms_new.html")
	adminTermEditTmpl = mustParseFS("templates/layout.html", "templates/terms_edit.html")
)

// termView is the row/detail view model for the term templates. ParentName is
// resolved for display in the list ("—" when a top-level term).
type termView struct {
	ID         string
	Taxonomy   string
	Name       string
	Slug       string
	ParentID   string
	ParentName string
}

// taxonomyOption is one <option> in the taxonomy selector.
type taxonomyOption struct {
	Value    string
	Selected bool
}

// parentOption is one <option> in the parent selector: an existing term the new/
// edited term may nest under. Label shows "taxonomy / name" so the admin can tell
// same-named terms in different taxonomies apart.
type parentOption struct {
	Value    string
	Label    string
	Selected bool
}

// adminTermListPage is the view model for the list page.
type adminTermListPage struct {
	pageData
	Terms []termView
}

// adminTermFormPage is the view model for the new/edit forms.
type adminTermFormPage struct {
	pageData
	Term       termView
	Taxonomies []taxonomyOption
	Parents    []parentOption
	Error      string
}

// registerAdminTermRoutes wires the /admin/terms HTML surface.
func (h *handlers) registerAdminTermRoutes(mux *http.ServeMux) {
	mux.Handle("GET /admin/terms", h.requireSession(http.HandlerFunc(h.handleAdminTermsList)))
	mux.Handle("GET /admin/terms/new", h.requireSession(http.HandlerFunc(h.handleAdminTermNewForm)))
	mux.Handle("POST /admin/terms", h.requireSessionPermission("terms.manage")(http.HandlerFunc(h.handleAdminTermCreate)))
	mux.Handle("GET /admin/terms/{id}/edit", h.requireSession(http.HandlerFunc(h.handleAdminTermEditForm)))
	mux.Handle("PUT /admin/terms/{id}", h.requireSessionPermission("terms.manage")(http.HandlerFunc(h.handleAdminTermUpdate)))
	mux.Handle("DELETE /admin/terms/{id}", h.requireSessionPermission("terms.manage")(http.HandlerFunc(h.handleAdminTermDelete)))
}

// handleAdminTermsList renders the term list (name, taxonomy, slug, parent).
func (h *handlers) handleAdminTermsList(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFromContext(r.Context())
	terms, err := h.listTerms(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	names := termNames(terms)
	views := make([]termView, 0, len(terms))
	for _, t := range terms {
		views = append(views, toTermView(t, names))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminTermListTmpl.ExecuteTemplate(w, "layout", adminTermListPage{
		pageData: pageData{Title: "Categorías y tags — librarian", Authenticated: true, Email: emailOf(id), Path: r.URL.Path},
		Terms:    views,
	})
}

// handleAdminTermNewForm renders the empty create form.
func (h *handlers) handleAdminTermNewForm(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFromContext(r.Context())
	parents, err := h.parentOptions(r.Context(), "", "")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	renderTermForm(w, adminTermNewTmpl, http.StatusOK, adminTermFormPage{
		pageData:   pageData{Title: "Nueva categoría/tag — librarian", Authenticated: true, Email: emailOf(id), Path: r.URL.Path},
		Taxonomies: taxonomyOptions(""),
		Parents:    parents,
	})
}

// handleAdminTermCreate handles the create form POST (terms.manage). A validation
// failure or duplicate slug re-renders the form with the error (no row created).
func (h *handlers) handleAdminTermCreate(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFromContext(r.Context())
	req, msg, ok := parseTermForm(r)
	if !ok {
		h.rerenderTermForm(w, r, adminTermNewTmpl, "Nueva categoría/tag — librarian", termView{Taxonomy: req.Taxonomy, Name: req.Name, Slug: req.Slug, ParentID: ptrString(req.ParentID)}, msg, emailOf(id))
		return
	}
	if _, err := h.insertTerm(r.Context(), req); err != nil {
		status, emsg := termWriteError(err)
		if status == http.StatusInternalServerError {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h.rerenderTermForm(w, r, adminTermNewTmpl, "Nueva categoría/tag — librarian", termView{Taxonomy: req.Taxonomy, Name: req.Name, Slug: req.Slug, ParentID: ptrString(req.ParentID)}, emsg, emailOf(id))
		return
	}
	http.Redirect(w, r, "/admin/terms", http.StatusSeeOther)
}

// handleAdminTermEditForm renders the edit form preloaded with the term. A
// missing/malformed id → 404 HTML.
func (h *handlers) handleAdminTermEditForm(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFromContext(r.Context())
	t, found, err := h.fetchTerm(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		renderNotFound(w, emailOf(id))
		return
	}
	// A term cannot be its own parent, so it is excluded from the parent options.
	parents, err := h.parentOptions(r.Context(), t.ID, ptrString(t.ParentID))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	renderTermForm(w, adminTermEditTmpl, http.StatusOK, adminTermFormPage{
		pageData:   pageData{Title: "Editar categoría/tag — librarian", Authenticated: true, Email: emailOf(id), Path: r.URL.Path},
		Term:       termView{ID: t.ID, Taxonomy: t.Taxonomy, Name: t.Name, Slug: t.Slug, ParentID: ptrString(t.ParentID)},
		Taxonomies: taxonomyOptions(t.Taxonomy),
		Parents:    parents,
	})
}

// handleAdminTermUpdate handles hx-put from the edit form (terms.manage). A
// missing id → 404; validation/duplicate-slug → re-render (400). On success it
// sets HX-Redirect to the list.
func (h *handlers) handleAdminTermUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	idn, _ := identityFromContext(r.Context())
	req, msg, ok := parseTermForm(r)
	if !ok {
		h.rerenderTermForm(w, r, adminTermEditTmpl, "Editar categoría/tag — librarian", termView{ID: id, Taxonomy: req.Taxonomy, Name: req.Name, Slug: req.Slug, ParentID: ptrString(req.ParentID)}, msg, emailOf(idn))
		return
	}
	if _, err := h.updateTerm(r.Context(), id, req); err != nil {
		if errors.Is(err, errTermNotFound) {
			renderNotFound(w, emailOf(idn))
			return
		}
		status, emsg := termWriteError(err)
		if status == http.StatusInternalServerError {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h.rerenderTermForm(w, r, adminTermEditTmpl, "Editar categoría/tag — librarian", termView{ID: id, Taxonomy: req.Taxonomy, Name: req.Name, Slug: req.Slug, ParentID: ptrString(req.ParentID)}, emsg, emailOf(idn))
		return
	}
	w.Header().Set("HX-Redirect", "/admin/terms")
	w.WriteHeader(http.StatusOK)
}

// handleAdminTermDelete handles hx-delete from a list row (terms.manage). A
// missing id → 404. On success it returns an empty 200 body so htmx removes the
// row.
func (h *handlers) handleAdminTermDelete(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	n, err := h.deleteTermByID(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		renderNotFound(w, emailOf(idn))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// --- helpers -----------------------------------------------------------------

// parseTermForm reads + validates the term form fields. It returns a termBody on
// success (parent_id nil when the "(sin padre)" option was chosen). A missing
// required field → ok=false with a human message.
func parseTermForm(r *http.Request) (termBody, string, bool) {
	if err := r.ParseForm(); err != nil {
		return termBody{}, "Formulario inválido.", false
	}
	req := termBody{
		Taxonomy: r.PostFormValue("taxonomy"),
		Name:     r.PostFormValue("name"),
		Slug:     r.PostFormValue("slug"),
	}
	if p := r.PostFormValue("parent_id"); p != "" {
		req.ParentID = &p
	}
	if req.Taxonomy == "" || req.Name == "" || req.Slug == "" {
		return req, "taxonomy, name and slug are required", false
	}
	return req, "", true
}

// rerenderTermForm re-renders a term form (new or edit) after a validation or
// write error, rebuilding the taxonomy + parent option lists so the page is
// consistent.
func (h *handlers) rerenderTermForm(w http.ResponseWriter, r *http.Request, tmpl *template.Template, title string, tv termView, errMsg, email string) {
	parents, err := h.parentOptions(r.Context(), tv.ID, tv.ParentID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	status := http.StatusBadRequest
	renderTermForm(w, tmpl, status, adminTermFormPage{
		pageData:   pageData{Title: title, Authenticated: true, Email: email, Path: r.URL.Path},
		Term:       tv,
		Taxonomies: taxonomyOptions(tv.Taxonomy),
		Parents:    parents,
		Error:      errMsg,
	})
}

// renderTermForm writes a new/edit form page with the given status.
func renderTermForm(w http.ResponseWriter, tmpl *template.Template, status int, data adminTermFormPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tmpl.ExecuteTemplate(w, "layout", data)
}

// termWriteError maps a term data-access error to an HTTP status + human message.
func termWriteError(err error) (int, string) {
	switch {
	case errors.Is(err, errDuplicateSlug):
		return http.StatusBadRequest, "Ya existe un término con ese slug en esta taxonomía."
	case errors.Is(err, errUnknownTaxonomy):
		return http.StatusBadRequest, "Taxonomía desconocida."
	case errors.Is(err, errUnknownTerm):
		return http.StatusBadRequest, "El término padre no existe."
	case errors.Is(err, errParentIsSelf):
		return http.StatusBadRequest, "Un término no puede ser su propio padre."
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// taxonomyOptions builds the taxonomy selector from the fixed catalog
// (schema.Taxonomies), pre-selecting current.
func taxonomyOptions(current string) []taxonomyOption {
	out := make([]taxonomyOption, 0, len(schema.Taxonomies))
	for _, name := range schema.Taxonomies {
		out = append(out, taxonomyOption{Value: name, Selected: name == current})
	}
	return out
}

// parentOptions builds the parent selector: every existing term EXCEPT excludeID
// (a term cannot be its own parent), labelled "taxonomy / name", pre-selecting
// currentParent. Reuses the shared listTerms helper — no extra SQL.
func (h *handlers) parentOptions(ctx context.Context, excludeID, currentParent string) ([]parentOption, error) {
	terms, err := h.listTerms(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]parentOption, 0, len(terms))
	for _, t := range terms {
		if t.ID == excludeID {
			continue
		}
		out = append(out, parentOption{
			Value:    t.ID,
			Label:    t.Taxonomy + " / " + t.Name,
			Selected: t.ID == currentParent,
		})
	}
	return out, nil
}

// termNames maps every term id to its name, for resolving a parent id to a name
// in the list view without a second query per row.
func termNames(terms []term) map[string]string {
	m := make(map[string]string, len(terms))
	for _, t := range terms {
		m[t.ID] = t.Name
	}
	return m
}

// toTermView maps a term to its list view model, resolving the parent name.
func toTermView(t term, names map[string]string) termView {
	v := termView{ID: t.ID, Taxonomy: t.Taxonomy, Name: t.Name, Slug: t.Slug}
	if t.ParentID != nil {
		v.ParentID = *t.ParentID
		v.ParentName = names[*t.ParentID]
	}
	return v
}

// ptrString dereferences a *string to its value or "".
func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// --- shared content-form term assignment (CONTRACT-12 T4) --------------------
//
// The article/product create/edit forms show term checkboxes grouped by
// taxonomy (categories separated from tags). Design decision (documented in the
// report): the checkboxes and their processing are gated by terms.manage — the
// SAME single permission that gates the JSON assignment routes — layered ON TOP
// of the content permission the form already requires (content.create/update).
// So a user with content rights but WITHOUT terms.manage edits content normally
// and simply never sees the term fieldset; their submit never touches term
// assignments (so a content-only editor cannot accidentally wipe an article's
// terms). This keeps ONE authorization rule for "assign a term" everywhere.

// termCheck is one term checkbox in a content form.
type termCheck struct {
	ID      string
	Name    string
	Checked bool
}

// termGroup is a taxonomy heading with its term checkboxes (categories vs tags).
type termGroup struct {
	Taxonomy string
	Terms    []termCheck
}

// sessionCanManageTerms reports whether the session identity holds terms.manage,
// used to decide whether to render/process the content-form term checkboxes.
func (h *handlers) sessionCanManageTerms(ctx context.Context, id *Identity) bool {
	if id == nil {
		return false
	}
	perms, err := h.permissionsFor(ctx, id)
	if err != nil {
		return false
	}
	return containsString(perms, "terms.manage")
}

// termGroupsFor builds the taxonomy-grouped term checkboxes for a content form,
// marking checked the terms currently assigned to contentID (none when contentID
// is empty, i.e. a create form). Groups preserve listTerms' (taxonomy, name)
// order so categories and tags render as stable, separated blocks.
func (h *handlers) termGroupsFor(ctx context.Context, junction, idCol, contentID string) ([]termGroup, error) {
	terms, err := h.listTerms(ctx)
	if err != nil {
		return nil, err
	}
	assigned := make(map[string]bool)
	if contentID != "" {
		rows, err := h.assignedTermsFor(ctx, junction, idCol, contentID)
		if err != nil {
			return nil, err
		}
		for _, a := range rows {
			assigned[a.ID] = true
		}
	}
	var groups []termGroup
	idx := make(map[string]int)
	for _, t := range terms {
		gi, ok := idx[t.Taxonomy]
		if !ok {
			groups = append(groups, termGroup{Taxonomy: t.Taxonomy})
			gi = len(groups) - 1
			idx[t.Taxonomy] = gi
		}
		groups[gi].Terms = append(groups[gi].Terms, termCheck{ID: t.ID, Name: t.Name, Checked: assigned[t.ID]})
	}
	return groups, nil
}
