package server

// CONTRACT-11 T3 — admin UI for the products content type. These HTML routes
// live under /admin/products (parallel to /admin/articles) and reuse the shared
// data-access helpers in products.go — the UI reimplements no business logic or
// SQL. Write routes are gated by requireSessionPermission with the SAME generic
// content.* permissions the JSON API uses; read routes only require a session.
// It reuses the shared renderForbidden/renderNotFound helpers and the same
// per-page template-set pattern as ui_articles.go.
//
// Swap/redirect design decisions (same rationale as CONTRACT-07's articles UI):
//   - Create: a plain HTML form POST → 303 redirect to the list. A validation
//     failure (missing field, bad price, duplicate sku) re-renders the form with
//     the error banner and no redirect.
//   - Edit/update: hx-put from the edit form. On success the server replies with
//     an HX-Redirect header to the list. A duplicate-sku or invalid-price on
//     update re-renders the edit form (400) with the error, exactly like create.
//   - Delete: hx-delete from the list row; the server replies 200 with an empty
//     body and htmx's outerHTML swap removes the row.
//   - There is NO publish control (products has no publish workflow).

import (
	"errors"
	"html/template"
	"net/http"
)

// Product admin template sets. One set per page (layout + page), matching the
// ui_articles.go convention to avoid a shared "content" definition collision.
var (
	adminProductListTmpl = mustParseFS("templates/layout.html", "templates/products_list.html", "templates/products_row.html")
	adminProductNewTmpl  = mustParseFS("templates/layout.html", "templates/products_new.html")
	adminProductEditTmpl = mustParseFS("templates/layout.html", "templates/products_edit.html")
)

// productView is the row/detail view model for the product admin templates.
type productView struct {
	ID        string
	Title     string
	Body      string
	Price     string
	SKU       string
	CreatedAt string
}

// toProductView maps the shared product row to the template view model.
func toProductView(p product) productView {
	return productView{
		ID:        p.ID,
		Title:     p.Title,
		Body:      p.Body,
		Price:     p.Price,
		SKU:       p.SKU,
		CreatedAt: p.CreatedAt,
	}
}

// adminProductListPage is the view model for the list page.
type adminProductListPage struct {
	pageData
	Products []productView
}

// adminProductFormPage is the view model for the new/edit forms. Product is zero
// for the create form; Error holds an optional validation banner. CONTRACT-12 T4
// adds the term checkboxes (CanManageTerms + TermGroups), same as the articles UI.
type adminProductFormPage struct {
	pageData
	Product        productView
	Error          string
	CanManageTerms bool
	TermGroups     []termGroup
}

// registerAdminProductRoutes wires the /admin/products HTML surface. Read routes
// require only a session; write routes are gated by the matching content.*
// permission via requireSessionPermission, mirroring the JSON API exactly.
func (h *handlers) registerAdminProductRoutes(mux *http.ServeMux) {
	mux.Handle("GET /admin/products", h.requireSession(http.HandlerFunc(h.handleAdminProductsList)))
	mux.Handle("GET /admin/products/new", h.requireSession(http.HandlerFunc(h.handleAdminProductNewForm)))
	mux.Handle("POST /admin/products", h.requireSessionPermission("content.create")(http.HandlerFunc(h.handleAdminProductCreate)))
	mux.Handle("GET /admin/products/{id}/edit", h.requireSession(http.HandlerFunc(h.handleAdminProductEditForm)))
	mux.Handle("PUT /admin/products/{id}", h.requireSessionPermission("content.update")(http.HandlerFunc(h.handleAdminProductUpdate)))
	mux.Handle("DELETE /admin/products/{id}", h.requireSessionPermission("content.delete")(http.HandlerFunc(h.handleAdminProductDelete)))
}

// handleAdminProductsList renders the product list (title, price, sku, created).
func (h *handlers) handleAdminProductsList(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFromContext(r.Context())
	rows, err := h.listProducts(r.Context(), 100, 0)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	views := make([]productView, 0, len(rows))
	for _, p := range rows {
		views = append(views, toProductView(p))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminProductListTmpl.ExecuteTemplate(w, "layout", adminProductListPage{
		pageData: pageData{Title: "Productos — librarian", Authenticated: true, Email: emailOf(id), Path: r.URL.Path},
		Products: views,
	})
}

// handleAdminProductNewForm renders the empty create form.
func (h *handlers) handleAdminProductNewForm(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFromContext(r.Context())
	canManage := h.sessionCanManageTerms(r.Context(), id)
	var groups []termGroup
	if canManage {
		var err error
		if groups, err = h.termGroupsFor(r.Context(), "product_terms", "product_id", ""); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	renderProductForm(w, adminProductNewTmpl, http.StatusOK, adminProductFormPage{
		pageData:       pageData{Title: "Nuevo producto — librarian", Authenticated: true, Email: emailOf(id), Path: r.URL.Path},
		CanManageTerms: canManage,
		TermGroups:     groups,
	})
}

// handleAdminProductCreate handles the create form POST (content.create). It
// validates title/body/price/sku, inserts with the session user as author, and
// 303-redirects to the list. A validation failure or duplicate sku re-renders the
// form with the error (no redirect, no row created).
func (h *handlers) handleAdminProductCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	canManage := h.sessionCanManageTerms(r.Context(), id)
	title, body, price, sku, msg, ok := parseProductForm(r)
	if !ok {
		var groups []termGroup
		if canManage {
			groups, _ = h.termGroupsFor(r.Context(), "product_terms", "product_id", "")
		}
		renderProductForm(w, adminProductNewTmpl, http.StatusBadRequest, adminProductFormPage{
			pageData:       pageData{Title: "Nuevo producto — librarian", Authenticated: true, Email: id.Email, Path: r.URL.Path},
			Product:        productView{Title: title, Body: body, Price: rawPrice(r), SKU: sku},
			Error:          msg,
			CanManageTerms: canManage,
			TermGroups:     groups,
		})
		return
	}
	newID, err := h.insertProduct(r.Context(), id.UserID, title, body, price, sku)
	if err != nil {
		status, emsg := productWriteError(err)
		if status == http.StatusInternalServerError {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		var groups []termGroup
		if canManage {
			groups, _ = h.termGroupsFor(r.Context(), "product_terms", "product_id", "")
		}
		renderProductForm(w, adminProductNewTmpl, status, adminProductFormPage{
			pageData:       pageData{Title: "Nuevo producto — librarian", Authenticated: true, Email: id.Email, Path: r.URL.Path},
			Product:        productView{Title: title, Body: body, Price: price, SKU: sku},
			Error:          emsg,
			CanManageTerms: canManage,
			TermGroups:     groups,
		})
		return
	}
	// Term assignment processed only for terms.manage holders (see ui_terms.go).
	if canManage {
		if err := h.setContentTerms(r.Context(), "products", "product_terms", "product_id", newID, r.PostForm["terms"]); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/admin/products", http.StatusSeeOther)
}

// handleAdminProductEditForm renders the edit form preloaded with the product. A
// missing/malformed id → 404 HTML (never 500).
func (h *handlers) handleAdminProductEditForm(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFromContext(r.Context())
	p, found, err := h.fetchProduct(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		renderNotFound(w, emailOf(id))
		return
	}
	canManage := h.sessionCanManageTerms(r.Context(), id)
	var groups []termGroup
	if canManage {
		if groups, err = h.termGroupsFor(r.Context(), "product_terms", "product_id", p.ID); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	renderProductForm(w, adminProductEditTmpl, http.StatusOK, adminProductFormPage{
		pageData:       pageData{Title: "Editar producto — librarian", Authenticated: true, Email: emailOf(id), Path: r.URL.Path},
		Product:        toProductView(p),
		CanManageTerms: canManage,
		TermGroups:     groups,
	})
}

// handleAdminProductUpdate handles hx-put from the edit form (content.update). A
// missing/malformed id → 404 HTML. A validation failure or duplicate sku
// re-renders the edit form (400) with the error. On success it sets HX-Redirect
// so htmx navigates to the list without a full page reload.
func (h *handlers) handleAdminProductUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	idn, _ := identityFromContext(r.Context())
	canManage := h.sessionCanManageTerms(r.Context(), idn)
	title, body, price, sku, msg, ok := parseProductForm(r)
	present, err := h.productExists(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !present {
		renderNotFound(w, emailOf(idn))
		return
	}
	if !ok {
		var groups []termGroup
		if canManage {
			groups, _ = h.termGroupsFor(r.Context(), "product_terms", "product_id", id)
		}
		renderProductForm(w, adminProductEditTmpl, http.StatusBadRequest, adminProductFormPage{
			pageData:       pageData{Title: "Editar producto — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
			Product:        productView{ID: id, Title: title, Body: body, Price: rawPrice(r), SKU: sku},
			Error:          msg,
			CanManageTerms: canManage,
			TermGroups:     groups,
		})
		return
	}
	if _, err := h.updateProductFields(r.Context(), id, title, body, price, sku); err != nil {
		status, emsg := productWriteError(err)
		if status == http.StatusInternalServerError {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		var groups []termGroup
		if canManage {
			groups, _ = h.termGroupsFor(r.Context(), "product_terms", "product_id", id)
		}
		renderProductForm(w, adminProductEditTmpl, status, adminProductFormPage{
			pageData:       pageData{Title: "Editar producto — librarian", Authenticated: true, Email: emailOf(idn), Path: r.URL.Path},
			Product:        productView{ID: id, Title: title, Body: body, Price: price, SKU: sku},
			Error:          emsg,
			CanManageTerms: canManage,
			TermGroups:     groups,
		})
		return
	}
	// Term assignment processed only for terms.manage holders (see ui_terms.go).
	if canManage {
		if err := h.setContentTerms(r.Context(), "products", "product_terms", "product_id", id, r.PostForm["terms"]); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("HX-Redirect", "/admin/products")
	w.WriteHeader(http.StatusOK)
}

// handleAdminProductDelete handles hx-delete from a list row (content.delete). A
// missing/malformed id → 404 HTML. On success it returns an empty 200 body so
// htmx's outerHTML swap removes the row.
func (h *handlers) handleAdminProductDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	idn, _ := identityFromContext(r.Context())
	n, err := h.deleteProductByID(r.Context(), id)
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

// parseProductForm reads + validates the form fields shared by create/update. On
// success it returns the validated fields (price canonicalized) with ok=true. On
// any validation failure it returns ok=false and a human message for the banner;
// the caller re-renders the form. It mirrors the JSON API's validation exactly.
func parseProductForm(r *http.Request) (title, body, price, sku, msg string, ok bool) {
	if err := r.ParseForm(); err != nil {
		return "", "", "", "", "Formulario inválido.", false
	}
	title = r.PostFormValue("title")
	body = r.PostFormValue("body")
	rawP := r.PostFormValue("price")
	sku = r.PostFormValue("sku")
	if title == "" || body == "" || sku == "" || rawP == "" {
		return title, body, "", sku, "title, body, price and sku are required", false
	}
	canonical, valid := validateDecimalText(rawP)
	if !valid {
		return title, body, "", sku, "price must be a valid decimal number", false
	}
	return title, body, canonical, sku, "", true
}

// rawPrice returns the price as the user typed it in the form, so a re-rendered
// form after a validation error preserves their input rather than blanking it.
func rawPrice(r *http.Request) string {
	return r.PostFormValue("price")
}

// productWriteError maps a data-access error from insert/update to an HTTP status
// and a human message: a duplicate sku is a 400 (clear message), anything else is
// a 500 (the caller renders a plain error, never the raw SQL text).
func productWriteError(err error) (int, string) {
	if errors.Is(err, errDuplicateSKU) {
		return http.StatusBadRequest, "Ya existe un producto con ese SKU."
	}
	return http.StatusInternalServerError, "internal error"
}

// renderProductForm writes a new/edit form page with the given status (the
// product analogue of renderAdminForm in ui_articles.go).
func renderProductForm(w http.ResponseWriter, tmpl *template.Template, status int, data adminProductFormPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tmpl.ExecuteTemplate(w, "layout", data)
}
