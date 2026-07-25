package server

// CONTRACT-07 — admin UI for the articles content type. These HTML routes live
// under the /admin/articles namespace (distinct from the JSON API's /articles,
// which shares the same ServeMux) and reuse the shared data-access helpers in
// articles.go — the UI reimplements no business logic or SQL, it renders the
// same data the API does. Write routes are gated by requireSessionPermission
// (session cookie + specific permission); read routes only require a session.
//
// Swap/redirect design decisions (documented in the CONTRACT-07 report):
//   - Create: a plain HTML form POST → 303 redirect to the list (full page).
//     A create has no row to swap into, so a redirect is the natural result and
//     needs no htmx.
//   - Edit/update: hx-put from the edit form. On success the server replies with
//     an HX-Redirect header to the list — htmx performs a client-side navigation
//     with no full reload of the current document. Chosen over an in-place
//     fragment swap because after an edit the user's next action is the list.
//   - Publish: hx-post from the list row; the server replies with the SINGLE
//     updated <tr> fragment, which htmx swaps in place (hx-swap="outerHTML" on
//     the row) — no page reload, the row flips draft→published.
//   - Delete: hx-delete from the list row; the server replies 200 with an empty
//     body, and htmx's outerHTML swap removes the row.

import (
	"html/template"
	"net/http"
)

// Admin template sets. Each page is layout + its page template, following the
// CONTRACT-06 pattern (one set per page to avoid a shared "content" collision).
// adminRowTmpl parses the row fragment standalone so the publish handler can
// render a single <tr> for an htmx swap; the list set also includes it so the
// list can range over rows calling the same "article_row" definition.
var (
	adminListTmpl = mustParseFS("templates/layout.html", "templates/articles_list.html", "templates/articles_row.html")
	adminRowTmpl  = mustParseFS("templates/articles_row.html")
	adminNewTmpl  = mustParseFS("templates/layout.html", "templates/articles_new.html")
	adminEditTmpl = mustParseFS("templates/layout.html", "templates/articles_edit.html")
	forbiddenTmpl = mustParseFS("templates/layout.html", "templates/error_403.html")
	notFoundTmpl  = mustParseFS("templates/layout.html", "templates/error_404.html")
)

// renderAdminForm writes a new/edit form page with the given status.
func renderAdminForm(w http.ResponseWriter, tmpl *template.Template, status int, data adminFormPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tmpl.ExecuteTemplate(w, "layout", data)
}

// renderForbidden writes the simple 403 HTML page (valid session, missing the
// required permission). It is NEVER the JSON writeError envelope — a human in a
// browser must not see raw API JSON.
// CONTRACT-15 T1: it is a METHOD taking the request so it renders through
// h.page(r, …) like every other page — the 403 keeps the caller's sidebar,
// dynamic content types included.
func (h *handlers) renderForbidden(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = forbiddenTmpl.ExecuteTemplate(w, "layout", h.page(r, "Sin permiso — librarian"))
}

// renderNotFound writes the simple 404 HTML page for a missing/malformed article
// id on an admin route — never a 500 and never raw JSON. Same CONTRACT-15 T1
// note as renderForbidden: a method, rendered through h.page(r, …).
func (h *handlers) renderNotFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = notFoundTmpl.ExecuteTemplate(w, "layout", h.page(r, "No encontrado — librarian"))
}

// articleView is the row/detail view model for the admin templates. Published is
// derived from the nullable published_at so the template can branch without
// pointer logic; the string fields are shown verbatim.
type articleView struct {
	ID          string
	Title       string
	Body        string
	Published   bool
	PublishedAt string
	CreatedAt   string
}

// toArticleView maps the shared article row to the template view model.
func toArticleView(a article) articleView {
	v := articleView{
		ID:        a.ID,
		Title:     a.Title,
		Body:      a.Body,
		CreatedAt: a.CreatedAt,
	}
	if a.PublishedAt != nil {
		v.Published = true
		v.PublishedAt = *a.PublishedAt
	}
	return v
}

// adminListPage is the view model for the list page.
type adminListPage struct {
	pageData
	Articles []articleView
}

// adminFormPage is the view model for the new/edit forms. Article is zero for
// the create form; Error holds an optional validation banner. CONTRACT-12 T4
// adds the term checkboxes: CanManageTerms controls whether the fieldset is
// shown at all (only for terms.manage holders), TermGroups are the taxonomy-
// grouped checkboxes with their checked state.
type adminFormPage struct {
	pageData
	Article        articleView
	Error          string
	CanManageTerms bool
	TermGroups     []termGroup
}

// registerAdminArticleRoutes wires the /admin/articles HTML surface. Read routes
// (list, new-form, edit-form) require only a session; write routes are gated by
// the matching content.* permission via requireSessionPermission, mirroring the
// JSON API's permission mapping exactly.
func (h *handlers) registerAdminArticleRoutes(mux *http.ServeMux) {
	mux.Handle("GET /admin/articles", h.requireSession(http.HandlerFunc(h.handleAdminArticlesList)))
	mux.Handle("GET /admin/articles/new", h.requireSession(http.HandlerFunc(h.handleAdminArticleNewForm)))
	mux.Handle("POST /admin/articles", h.requireSessionPermission("content.create")(http.HandlerFunc(h.handleAdminArticleCreate)))
	mux.Handle("GET /admin/articles/{id}/edit", h.requireSession(http.HandlerFunc(h.handleAdminArticleEditForm)))
	mux.Handle("PUT /admin/articles/{id}", h.requireSessionPermission("content.update")(http.HandlerFunc(h.handleAdminArticleUpdate)))
	mux.Handle("POST /admin/articles/{id}/publish", h.requireSessionPermission("content.publish")(http.HandlerFunc(h.handleAdminArticlePublish)))
	mux.Handle("DELETE /admin/articles/{id}", h.requireSessionPermission("content.delete")(http.HandlerFunc(h.handleAdminArticleDelete)))
}

// handleAdminArticlesList renders the article list (title, status, created).
func (h *handlers) handleAdminArticlesList(w http.ResponseWriter, r *http.Request) {
	rows, err := h.listArticles(r.Context(), 100, 0)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	views := make([]articleView, 0, len(rows))
	for _, a := range rows {
		views = append(views, toArticleView(a))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminListTmpl.ExecuteTemplate(w, "layout", adminListPage{
		pageData: h.page(r, "Artículos — librarian"),
		Articles: views,
	})
}

// handleAdminArticleNewForm renders the empty create form.
func (h *handlers) handleAdminArticleNewForm(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFromContext(r.Context())
	canManage := h.sessionCanManageTerms(r.Context(), id)
	var groups []termGroup
	if canManage {
		var err error
		if groups, err = h.termGroupsFor(r.Context(), "article_terms", "article_id", ""); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	renderAdminForm(w, adminNewTmpl, http.StatusOK, adminFormPage{
		pageData:       h.page(r, "Nuevo artículo — librarian"),
		CanManageTerms: canManage,
		TermGroups:     groups,
	})
}

// handleAdminArticleCreate handles the create form POST (content.create). It
// validates title/body with the same message as the JSON API, inserts with the
// session user as author, and 303-redirects to the list. On validation failure
// it re-renders the form with the error (no redirect).
func (h *handlers) handleAdminArticleCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	canManage := h.sessionCanManageTerms(r.Context(), id)
	if err := r.ParseForm(); err != nil {
		renderAdminForm(w, adminNewTmpl, http.StatusBadRequest, adminFormPage{
			pageData:       h.page(r, "Nuevo artículo — librarian"),
			Error:          "Formulario inválido.",
			CanManageTerms: canManage,
		})
		return
	}
	title := r.PostFormValue("title")
	body := r.PostFormValue("body")
	if title == "" || body == "" {
		groups, _ := h.termGroupsFor(r.Context(), "article_terms", "article_id", "")
		renderAdminForm(w, adminNewTmpl, http.StatusBadRequest, adminFormPage{
			pageData:       h.page(r, "Nuevo artículo — librarian"),
			Article:        articleView{Title: title, Body: body},
			Error:          "title and body are required",
			CanManageTerms: canManage,
			TermGroups:     groups,
		})
		return
	}
	newID, err := h.insertArticleBasic(r.Context(), id.UserID, title, body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Term assignment is processed only for terms.manage holders (see ui_terms.go).
	if canManage {
		if err := h.setContentTerms(r.Context(), "articles", "article_terms", "article_id", newID, r.PostForm["terms"]); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/admin/articles", http.StatusSeeOther)
}

// handleAdminArticleEditForm renders the edit form preloaded with the article.
// A missing/malformed id → 404 HTML (never 500).
func (h *handlers) handleAdminArticleEditForm(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFromContext(r.Context())
	a, found, err := h.fetchArticle(r, r.PathValue("id"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		h.renderNotFound(w, r)
		return
	}
	canManage := h.sessionCanManageTerms(r.Context(), id)
	var groups []termGroup
	if canManage {
		if groups, err = h.termGroupsFor(r.Context(), "article_terms", "article_id", a.ID); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	renderAdminForm(w, adminEditTmpl, http.StatusOK, adminFormPage{
		pageData:       h.page(r, "Editar artículo — librarian"),
		Article:        toArticleView(a),
		CanManageTerms: canManage,
		TermGroups:     groups,
	})
}

// handleAdminArticleUpdate handles hx-put from the edit form (content.update). A
// missing/malformed id → 404 HTML. On success it sets HX-Redirect so htmx
// navigates to the list without a full reload of the current page.
func (h *handlers) handleAdminArticleUpdate(w http.ResponseWriter, r *http.Request) {
	idn, _ := identityFromContext(r.Context())
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	title := r.PostFormValue("title")
	body := r.PostFormValue("body")
	present, err := h.articleExists(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !present {
		h.renderNotFound(w, r)
		return
	}
	canManage := h.sessionCanManageTerms(r.Context(), idn)
	if title == "" || body == "" {
		// Re-render the edit form with the error (htmx swaps it back in).
		var groups []termGroup
		if canManage {
			groups, _ = h.termGroupsFor(r.Context(), "article_terms", "article_id", id)
		}
		renderAdminForm(w, adminEditTmpl, http.StatusBadRequest, adminFormPage{
			pageData:       h.page(r, "Editar artículo — librarian"),
			Article:        articleView{ID: id, Title: title, Body: body},
			Error:          "title and body are required",
			CanManageTerms: canManage,
			TermGroups:     groups,
		})
		return
	}
	if _, err := h.updateArticleTitleBody(r.Context(), id, title, body); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Term assignment processed only for terms.manage holders (see ui_terms.go):
	// a content-only editor never submits term data, so their edit cannot wipe
	// the article's existing terms.
	if canManage {
		if err := h.setContentTerms(r.Context(), "articles", "article_terms", "article_id", id, r.PostForm["terms"]); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("HX-Redirect", "/admin/articles")
	w.WriteHeader(http.StatusOK)
}

// handleAdminArticlePublish handles hx-post from a list row (content.publish). A
// missing/malformed id → 404 HTML. On success it returns the single updated <tr>
// fragment so htmx swaps the row in place.
func (h *handlers) handleAdminArticlePublish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, found, err := h.publishArticleByID(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		h.renderNotFound(w, r)
		return
	}
	// Re-fetch so the row reflects the persisted published_at.
	a, ok, err := h.fetchArticle(r, id)
	if err != nil || !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminRowTmpl.ExecuteTemplate(w, "article_row", toArticleView(a))
}

// handleAdminArticleDelete handles hx-delete from a list row (content.delete). A
// missing/malformed id → 404 HTML. On success it returns an empty 200 body so
// htmx's outerHTML swap removes the row.
func (h *handlers) handleAdminArticleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := h.deleteArticleByID(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		h.renderNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// Empty body → htmx outerHTML swap removes the row.
}

// emailOf returns the identity email, or "" when the identity is absent (the
// read routes run behind requireSession, so it is always present in practice).
func emailOf(id *Identity) string {
	if id == nil {
		return ""
	}
	return id.Email
}
