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
	adminListTmpl = template.Must(template.ParseFS(templatesFS,
		"templates/layout.html", "templates/articles_list.html", "templates/articles_row.html"))
	adminRowTmpl  = template.Must(template.ParseFS(templatesFS, "templates/articles_row.html"))
	adminNewTmpl  = template.Must(template.ParseFS(templatesFS, "templates/layout.html", "templates/articles_new.html"))
	adminEditTmpl = template.Must(template.ParseFS(templatesFS, "templates/layout.html", "templates/articles_edit.html"))
	forbiddenTmpl = template.Must(template.ParseFS(templatesFS, "templates/layout.html", "templates/error_403.html"))
	notFoundTmpl  = template.Must(template.ParseFS(templatesFS, "templates/layout.html", "templates/error_404.html"))
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
func renderForbidden(w http.ResponseWriter, email string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = forbiddenTmpl.ExecuteTemplate(w, "layout", pageData{
		Title:         "Sin permiso — librarian",
		Authenticated: email != "",
		Email:         email,
	})
}

// renderNotFound writes the simple 404 HTML page for a missing/malformed article
// id on an admin route — never a 500 and never raw JSON.
func renderNotFound(w http.ResponseWriter, email string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = notFoundTmpl.ExecuteTemplate(w, "layout", pageData{
		Title:         "No encontrado — librarian",
		Authenticated: email != "",
		Email:         email,
	})
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
// the create form; Error holds an optional validation banner.
type adminFormPage struct {
	pageData
	Article articleView
	Error   string
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
	id, _ := identityFromContext(r.Context())
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
		pageData: pageData{Title: "Artículos — librarian", Authenticated: true, Email: emailOf(id), Path: r.URL.Path},
		Articles: views,
	})
}

// handleAdminArticleNewForm renders the empty create form.
func (h *handlers) handleAdminArticleNewForm(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFromContext(r.Context())
	renderAdminForm(w, adminNewTmpl, http.StatusOK, adminFormPage{
		pageData: pageData{Title: "Nuevo artículo — librarian", Authenticated: true, Email: emailOf(id), Path: r.URL.Path},
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
	if err := r.ParseForm(); err != nil {
		renderAdminForm(w, adminNewTmpl, http.StatusBadRequest, adminFormPage{
			pageData: pageData{Title: "Nuevo artículo — librarian", Authenticated: true, Email: id.Email, Path: r.URL.Path},
			Error:    "Formulario inválido.",
		})
		return
	}
	title := r.PostFormValue("title")
	body := r.PostFormValue("body")
	if title == "" || body == "" {
		renderAdminForm(w, adminNewTmpl, http.StatusBadRequest, adminFormPage{
			pageData: pageData{Title: "Nuevo artículo — librarian", Authenticated: true, Email: id.Email, Path: r.URL.Path},
			Article:  articleView{Title: title, Body: body},
			Error:    "title and body are required",
		})
		return
	}
	if _, err := h.insertArticleBasic(r.Context(), id.UserID, title, body); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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
		renderNotFound(w, emailOf(id))
		return
	}
	renderAdminForm(w, adminEditTmpl, http.StatusOK, adminFormPage{
		pageData: pageData{Title: "Editar artículo — librarian", Authenticated: true, Email: emailOf(id), Path: r.URL.Path},
		Article:  toArticleView(a),
	})
}

// handleAdminArticleUpdate handles hx-put from the edit form (content.update). A
// missing/malformed id → 404 HTML. On success it sets HX-Redirect so htmx
// navigates to the list without a full reload of the current page.
func (h *handlers) handleAdminArticleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	idn, _ := identityFromContext(r.Context())
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
		renderNotFound(w, emailOf(idn))
		return
	}
	if title == "" || body == "" {
		// Re-render the edit form with the error (htmx swaps it back in).
		renderAdminForm(w, adminEditTmpl, http.StatusBadRequest, adminFormPage{
			pageData: pageData{Title: "Editar artículo — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
			Article:  articleView{ID: id, Title: title, Body: body},
			Error:    "title and body are required",
		})
		return
	}
	if _, err := h.updateArticleTitleBody(r.Context(), id, title, body); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", "/admin/articles")
	w.WriteHeader(http.StatusOK)
}

// handleAdminArticlePublish handles hx-post from a list row (content.publish). A
// missing/malformed id → 404 HTML. On success it returns the single updated <tr>
// fragment so htmx swaps the row in place.
func (h *handlers) handleAdminArticlePublish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	idn, _ := identityFromContext(r.Context())
	_, found, err := h.publishArticleByID(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		renderNotFound(w, emailOf(idn))
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
	idn, _ := identityFromContext(r.Context())
	n, err := h.deleteArticleByID(r.Context(), id)
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
