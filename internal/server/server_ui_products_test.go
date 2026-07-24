package server_test

// CONTRACT-11 T3 acceptance tests: the admin products UI (/admin/products). Uses
// the TLS server + cookie jar (openUITLS) for anything depending on the Secure
// session cookie round-trip — same reason as the articles UI tests. Reuses
// loginUI, getBody, uiDo, noRedirectJarClient, noRedirectClient, grant, and
// nonexistentID from the sibling test files (same package). Products has no
// publish control.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// uiCreateProduct creates a product through the real POST form and returns its
// id, resolved by scraping the list HTML (proving the row is really in the UI).
func uiCreateProduct(t *testing.T, client *http.Client, srv *httptest.Server, title, body, price, sku string) string {
	t.Helper()
	resp, err := client.PostForm(srv.URL+"/admin/products",
		url.Values{"title": {title}, "body": {body}, "price": {price}, "sku": {sku}})
	if err != nil {
		t.Fatalf("uiCreateProduct POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("uiCreateProduct final status = %d, want 200", resp.StatusCode)
	}
	_, list := getBody(t, client, srv.URL+"/admin/products")
	rows := strings.Split(list, `<tr id="product-`)
	for _, chunk := range rows[1:] {
		id := chunk[:strings.IndexByte(chunk, '"')]
		if strings.Contains(chunk, ">"+title+"<") {
			return id
		}
	}
	t.Fatalf("created product %q not found in list HTML", title)
	return ""
}

// --- T3: permission gating (mirrors the articles UI gate) -------------------

// TestAdminProductsNoSessionRedirectsToLogin: read and write routes with no
// session → 302 /login, never JSON.
func TestAdminProductsNoSessionRedirectsToLogin(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := noRedirectClient(srv)

	for _, u := range []string{"/admin/products", "/admin/products/new"} {
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

	resp, err := client.PostForm(srv.URL+"/admin/products",
		url.Values{"title": {"x"}, "body": {"y"}, "price": {"1"}, "sku": {"S"}})
	if err != nil {
		t.Fatalf("POST create no-session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("POST create no-session status = %d, want 302", resp.StatusCode)
	}
}

// TestAdminProductsSessionWithoutPermissionIs403: valid session but no
// content.create → 403 HTML, not JSON, not 500.
func TestAdminProductsSessionWithoutPermissionIs403(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := loginUI(t, db, srv, "noperm@example.com", "pw", "author") // no grants

	resp, err := client.PostForm(srv.URL+"/admin/products",
		url.Values{"title": {"x"}, "body": {"y"}, "price": {"1"}, "sku": {"S"}})
	if err != nil {
		t.Fatalf("POST create: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if strings.Contains(string(body), `"error"`) {
		t.Errorf("403 body looks like JSON envelope: %.80q", body)
	}
	if !strings.Contains(string(body), "Sin permiso") {
		t.Errorf("403 body missing HTML marker: %.120q", body)
	}
}

// --- T3: list + create ------------------------------------------------------

// TestAdminProductCreateAppearsInList covers create via the real form, then the
// product shows in the list with its title, price and sku; the author is the
// session user.
func TestAdminProductCreateAppearsInList(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	status, body := getBody(t, client, srv.URL+"/admin/products")
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}
	if !strings.Contains(body, "No hay productos") {
		t.Errorf("empty list missing empty-state marker")
	}

	if _, err := client.PostForm(srv.URL+"/admin/products",
		url.Values{"title": {"Mi Producto"}, "body": {"Desc"}, "price": {"19.99"}, "sku": {"MP-1"}}); err != nil {
		t.Fatalf("POST create: %v", err)
	}

	status, body = getBody(t, client, srv.URL+"/admin/products")
	if status != http.StatusOK {
		t.Fatalf("list-after status = %d, want 200", status)
	}
	for _, want := range []string{"Mi Producto", "19.99", "MP-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("list missing %q: %.300q", want, body)
		}
	}

	var author string
	if err := db.QueryRow(`SELECT u.email FROM products p JOIN users u ON u.id = p.author_id WHERE p.title = ?`,
		"Mi Producto").Scan(&author); err != nil {
		t.Fatalf("author lookup: %v", err)
	}
	if author != "ed@example.com" {
		t.Errorf("author = %q, want ed@example.com", author)
	}
}

// TestAdminProductCreateValidationReRendersForm covers the validation branch: a
// non-numeric price re-renders the form (400) with the clear message; no row.
func TestAdminProductCreateValidationReRendersForm(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	resp, err := client.PostForm(srv.URL+"/admin/products",
		url.Values{"title": {"Bad"}, "body": {"b"}, "price": {"abc"}, "sku": {"BAD-1"}})
	if err != nil {
		t.Fatalf("POST create: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "price must be a valid decimal number") {
		t.Errorf("missing validation message: %.200q", body)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM products`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("row created despite validation failure: %d", n)
	}
}

// TestAdminProductDuplicateSKUReRendersForm: creating a second product with an
// existing sku re-renders the form (400) with a clear message; no second row.
func TestAdminProductDuplicateSKUReRendersForm(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	uiCreateProduct(t, client, srv, "First", "b", "1.00", "DUP")

	resp, err := client.PostForm(srv.URL+"/admin/products",
		url.Values{"title": {"Second"}, "body": {"b"}, "price": {"2.00"}, "sku": {"DUP"}})
	if err != nil {
		t.Fatalf("POST create dup: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dup-sku status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "SKU") {
		t.Errorf("dup-sku form missing clear message: %.200q", body)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM products WHERE sku = ?`, "DUP").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("dup sku produced %d rows, want 1", n)
	}
}

// --- T3: edit / delete + 404 ------------------------------------------------

// TestAdminProductEditForm covers the preloaded edit form and its 404.
func TestAdminProductEditForm(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	id := uiCreateProduct(t, client, srv, "Editable", "Desc", "7.50", "ED-1")

	status, body := getBody(t, client, srv.URL+"/admin/products/"+id+"/edit")
	if status != http.StatusOK {
		t.Fatalf("edit-form status = %d, want 200", status)
	}
	for _, want := range []string{"Editable", "7.50", "ED-1", `hx-put="/admin/products/` + id + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form missing %q", want)
		}
	}

	status, body = getBody(t, client, srv.URL+"/admin/products/"+nonexistentID+"/edit")
	if status != http.StatusNotFound {
		t.Fatalf("edit-form missing status = %d, want 404", status)
	}
	if strings.Contains(body, `"error"`) {
		t.Errorf("404 edit body looks like JSON")
	}
}

// TestAdminProductUpdate covers hx-put: HX-Redirect on success, persisted change,
// and 404 for a missing id.
func TestAdminProductUpdate(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	id := uiCreateProduct(t, client, srv, "Old", "OldBody", "1.00", "UP-1")

	resp := uiDo(t, client, http.MethodPut, srv.URL+"/admin/products/"+id,
		url.Values{"title": {"New"}, "body": {"NewBody"}, "price": {"3.33"}, "sku": {"UP-2"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200", resp.StatusCode)
	}
	if loc := resp.Header.Get("HX-Redirect"); loc != "/admin/products" {
		t.Errorf("HX-Redirect = %q, want /admin/products", loc)
	}
	resp.Body.Close()
	var title, body, price, sku string
	if err := db.QueryRow(`SELECT title, body, price, sku FROM products WHERE id = ?`, id).Scan(&title, &body, &price, &sku); err != nil {
		t.Fatalf("select: %v", err)
	}
	if title != "New" || body != "NewBody" || price != "3.33" || sku != "UP-2" {
		t.Errorf("persisted = (%q,%q,%q,%q), want (New,NewBody,3.33,UP-2)", title, body, price, sku)
	}

	resp = uiDo(t, client, http.MethodPut, srv.URL+"/admin/products/"+nonexistentID,
		url.Values{"title": {"x"}, "body": {"y"}, "price": {"1"}, "sku": {"z"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update missing status = %d, want 404", resp.StatusCode)
	}
}

// TestAdminProductDelete covers hx-delete: 200 empty body + row gone. Missing id
// → 404.
func TestAdminProductDelete(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.delete")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	id := uiCreateProduct(t, client, srv, "ToDelete", "Body", "1.00", "DEL-1")

	resp := uiDo(t, client, http.MethodDelete, srv.URL+"/admin/products/"+id, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp.StatusCode)
	}
	if productExistsDB(t, db, id) {
		t.Errorf("row still present after delete")
	}

	resp = uiDo(t, client, http.MethodDelete, srv.URL+"/admin/products/"+nonexistentID, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing status = %d, want 404", resp.StatusCode)
	}
}

// TestAdminProductMalformedIDIsNotFound: a non-UUID id on any write route → 404
// HTML, never 500/panic.
func TestAdminProductMalformedIDIsNotFound(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.update", "content.delete")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	cases := []struct{ method, path string }{
		{http.MethodPut, srv.URL + "/admin/products/not-a-uuid"},
		{http.MethodDelete, srv.URL + "/admin/products/abc"},
	}
	for _, c := range cases {
		resp := uiDo(t, client, c.method, c.path,
			url.Values{"title": {"x"}, "body": {"y"}, "price": {"1"}, "sku": {"z"}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404 (not 500)", c.method, c.path, resp.StatusCode)
		}
	}
}

// --- T3: navigation + round-trip + red-team ---------------------------------

// TestAdminNavShowsProductos confirms "Productos" is in the rendered navigation
// of an authenticated page (home), with its submenu links.
func TestAdminNavShowsProductos(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := loginUI(t, db, srv, "nav@example.com", "pw", "author")

	_, body := getBody(t, client, srv.URL+"/admin/products")
	for _, want := range []string{"Productos", "/admin/products", "Todos los productos", "/admin/products/new"} {
		if !strings.Contains(body, want) {
			t.Errorf("navigation missing %q", want)
		}
	}
}

// TestAdminProductRoundTrip is the full end-to-end UI flow: login → create →
// appears → edit → delete → gone.
func TestAdminProductRoundTrip(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.update", "content.delete")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	id := uiCreateProduct(t, client, srv, "RoundTrip", "Body", "10.00", "RT-1")
	if _, body := getBody(t, client, srv.URL+"/admin/products"); !strings.Contains(body, "RoundTrip") {
		t.Fatalf("created product not in list")
	}

	uiDo(t, client, http.MethodPut, srv.URL+"/admin/products/"+id,
		url.Values{"title": {"RoundTrip 2"}, "body": {"Body 2"}, "price": {"12.00"}, "sku": {"RT-1"}}).Body.Close()
	if _, body := getBody(t, client, srv.URL+"/admin/products"); !strings.Contains(body, "RoundTrip 2") {
		t.Fatalf("edited title not in list")
	}

	uiDo(t, client, http.MethodDelete, srv.URL+"/admin/products/"+id, nil).Body.Close()
	if _, body := getBody(t, client, srv.URL+"/admin/products"); strings.Contains(body, "RoundTrip") {
		t.Fatalf("product still in list after delete")
	}
}

// TestAdminProductDeleteWithoutPermissionServerSide is the red-team: a valid
// session WITHOUT content.delete sending hx-delete directly → 403 HTML, row
// survives. The gate is server-side.
func TestAdminProductDeleteWithoutPermissionServerSide(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editor := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	id := uiCreateProduct(t, editor, srv, "Protected", "Body", "1.00", "PROT-1")

	attacker := loginUI(t, db, srv, "attacker@example.com", "pw", "author")
	resp := uiDo(t, attacker, http.MethodDelete, srv.URL+"/admin/products/"+id, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("direct hx-delete without perm status = %d, want 403", resp.StatusCode)
	}
	if strings.Contains(string(body), `"error"`) {
		t.Errorf("403 body looks like JSON envelope")
	}
	if !productExistsDB(t, db, id) {
		t.Errorf("row deleted despite missing permission — gate is not server-side")
	}
}
