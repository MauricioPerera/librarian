package server_test

// CONTRACT-12 T4 acceptance tests: the admin terms UI (/admin/terms) and the
// term-assignment checkboxes in the article/product content forms. Uses the TLS
// server + cookie jar (openUITLS/loginUI) like the other UI tests. Reuses grant,
// getBody, noRedirectClient, nonexistentID from sibling files (same package).

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// uiCreateTerm creates a term through the real POST form and returns its id,
// resolved by scraping the list HTML.
func uiCreateTerm(t *testing.T, client *http.Client, srv *httptest.Server, taxonomy, name, slug string) string {
	t.Helper()
	resp, err := client.PostForm(srv.URL+"/admin/terms",
		url.Values{"taxonomy": {taxonomy}, "name": {name}, "slug": {slug}, "parent_id": {""}})
	if err != nil {
		t.Fatalf("uiCreateTerm POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("uiCreateTerm final status = %d, want 200", resp.StatusCode)
	}
	_, list := getBody(t, client, srv.URL+"/admin/terms")
	rows := strings.Split(list, `<tr id="term-`)
	for _, chunk := range rows[1:] {
		id := chunk[:strings.IndexByte(chunk, '"')]
		if strings.Contains(chunk, ">"+name+"<") {
			return id
		}
	}
	t.Fatalf("created term %q not found in list HTML", name)
	return ""
}

// --- T4: gating -------------------------------------------------------------

// TestAdminTermsNoSessionRedirectsToLogin: read and write with no session → 302.
func TestAdminTermsNoSessionRedirectsToLogin(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := noRedirectClient(srv)

	for _, u := range []string{"/admin/terms", "/admin/terms/new"} {
		resp, err := client.Get(srv.URL + u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("GET %s status = %d, want 302", u, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Errorf("GET %s Location = %q, want /login", u, loc)
		}
	}
	resp, err := client.PostForm(srv.URL+"/admin/terms",
		url.Values{"taxonomy": {"category"}, "name": {"x"}, "slug": {"x"}})
	if err != nil {
		t.Fatalf("POST create no-session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("POST create no-session status = %d, want 302", resp.StatusCode)
	}
}

// TestAdminTermsSessionWithoutPermissionIs403: valid session but no terms.manage
// → 403 HTML on a write, not JSON, not 500.
func TestAdminTermsSessionWithoutPermissionIs403(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := loginUI(t, db, srv, "noperm@example.com", "pw", "author") // no grants

	resp, err := client.PostForm(srv.URL+"/admin/terms",
		url.Values{"taxonomy": {"category"}, "name": {"x"}, "slug": {"x"}})
	if err != nil {
		t.Fatalf("POST create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// --- T4: CRUD via the UI + nav ----------------------------------------------

// TestAdminTermCRUDAndNav covers the full UI CRUD round-trip plus the nav entry:
// the list shows the empty-state, "Categorías y tags" is in the sidebar, a term
// created via the form appears in the list, an edit persists, and a delete
// removes it.
func TestAdminTermCRUDAndNav(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "terms.manage")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	status, body := getBody(t, client, srv.URL+"/admin/terms")
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}
	if !strings.Contains(body, "No hay categorías") {
		t.Errorf("empty list missing empty-state marker")
	}
	// Nav entry present (CONTRACT-10 sidebar).
	if !strings.Contains(body, "Categorías y tags") {
		t.Errorf("sidebar nav missing 'Categorías y tags'")
	}

	id := uiCreateTerm(t, client, srv, "category", "Electrónica", "electronica")

	_, list := getBody(t, client, srv.URL+"/admin/terms")
	if !strings.Contains(list, "Electrónica") || !strings.Contains(list, "electronica") {
		t.Errorf("created term not in list HTML")
	}

	// Edit → new name persisted.
	resp, err := noRedirectJarClient(srv, client).Do(mustPutForm(t, srv.URL+"/admin/terms/"+id,
		url.Values{"taxonomy": {"category"}, "name": {"Gadgets"}, "slug": {"gadgets"}, "parent_id": {""}}))
	if err != nil {
		t.Fatalf("edit PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit status = %d, want 200", resp.StatusCode)
	}
	var name string
	db.QueryRow(`SELECT name FROM terms WHERE id = ?`, id).Scan(&name)
	if name != "Gadgets" {
		t.Fatalf("term name after edit = %q, want Gadgets", name)
	}

	// Delete → row gone.
	req := mustDelete(t, srv.URL+"/admin/terms/"+id)
	resp, err = noRedirectJarClient(srv, client).Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp.StatusCode)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM terms WHERE id = ?`, id).Scan(&n)
	if n != 0 {
		t.Fatalf("term still present after delete: %d", n)
	}
}

// --- T4: checkboxes in the content forms ------------------------------------

// TestArticleFormTermCheckboxes covers the article form checkboxes: a terms.manage
// holder sees the term fieldset and grouped checkboxes, and a submit assigns the
// checked term (verified in the DB). A content-only editor (no terms.manage) does
// NOT see the fieldset.
func TestArticleFormTermCheckboxes(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "terms.manage")
	grant(t, db, "author", "content.create") // content only, NO terms.manage
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	cat := uiCreateTerm(t, client, srv, "category", "Electrónica", "electronica")
	uiCreateTerm(t, client, srv, "tag", "Oferta", "oferta")

	// The new-article form shows the term fieldset with both taxonomy groups.
	_, form := getBody(t, client, srv.URL+"/admin/articles/new")
	if !strings.Contains(form, "terms-fieldset") {
		t.Fatalf("article form missing term fieldset")
	}
	if !strings.Contains(form, "Electrónica") || !strings.Contains(form, "Oferta") {
		t.Fatalf("article form missing term checkboxes")
	}
	if !strings.Contains(form, `value="`+cat+`"`) {
		t.Fatalf("article form missing checkbox for category term id")
	}

	// Submit the create form WITH a checked term → the assignment persists.
	resp, err := client.PostForm(srv.URL+"/admin/articles",
		url.Values{"title": {"Con Terminos"}, "body": {"Body"}, "terms": {cat}})
	if err != nil {
		t.Fatalf("create article POST: %v", err)
	}
	resp.Body.Close()
	var artID string
	if err := db.QueryRow(`SELECT id FROM articles WHERE title = ?`, "Con Terminos").Scan(&artID); err != nil {
		t.Fatalf("article not created: %v", err)
	}
	var assigned int
	db.QueryRow(`SELECT COUNT(*) FROM article_terms WHERE article_id = ? AND term_id = ?`, artID, cat).Scan(&assigned)
	if assigned != 1 {
		t.Fatalf("term not assigned via form: article_terms count = %d, want 1", assigned)
	}

	// A content-only editor (no terms.manage) does NOT see the fieldset.
	authorClient := loginUI(t, db, srv, "au@example.com", "pw", "author")
	_, aform := getBody(t, authorClient, srv.URL+"/admin/articles/new")
	if strings.Contains(aform, "terms-fieldset") {
		t.Errorf("content-only editor should NOT see the term fieldset")
	}
}

// TestProductFormTermCheckboxes mirrors the article checkbox test for products.
func TestProductFormTermCheckboxes(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "terms.manage")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	cat := uiCreateTerm(t, client, srv, "category", "Gadgets", "gadgets")

	_, form := getBody(t, client, srv.URL+"/admin/products/new")
	if !strings.Contains(form, "terms-fieldset") || !strings.Contains(form, "Gadgets") {
		t.Fatalf("product form missing term fieldset/checkbox")
	}

	resp, err := client.PostForm(srv.URL+"/admin/products",
		url.Values{"title": {"Prod T"}, "body": {"d"}, "price": {"9.99"}, "sku": {"PT-1"}, "terms": {cat}})
	if err != nil {
		t.Fatalf("create product POST: %v", err)
	}
	resp.Body.Close()
	var prodID string
	if err := db.QueryRow(`SELECT id FROM products WHERE title = ?`, "Prod T").Scan(&prodID); err != nil {
		t.Fatalf("product not created: %v", err)
	}
	var assigned int
	db.QueryRow(`SELECT COUNT(*) FROM product_terms WHERE product_id = ? AND term_id = ?`, prodID, cat).Scan(&assigned)
	if assigned != 1 {
		t.Fatalf("term not assigned via product form: count = %d, want 1", assigned)
	}
}

// mustPutForm builds a PUT request with a form-encoded body (htmx hx-put sends
// application/x-www-form-urlencoded).
func mustPutForm(t *testing.T, u string, form url.Values) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, u, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// mustDelete builds a DELETE request.
func mustDelete(t *testing.T, u string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, u, nil)
	if err != nil {
		t.Fatalf("new DELETE request: %v", err)
	}
	return req
}
