package server_test

// CONTRACT-11 T2 acceptance tests: the products JSON CRUD surface. Reuses the
// shared helpers from server_auth_test.go / server_articles_test.go (openAuthMux,
// doJSON, grant, jwtFor, apiKeyFor, authHeader, roleID, nonexistentID) — same
// package. Products is gated by the SAME generic content.* permissions as
// articles; it has no publish route.

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
)

// createProduct POSTs a product as the given token and returns the new id.
func createProduct(t *testing.T, srv *httptest.Server, token, title, body, price, sku string) string {
	t.Helper()
	status, resp := doJSON(t, srv, http.MethodPost, "/products",
		map[string]any{"title": title, "body": body, "price": price, "sku": sku}, authHeader(token))
	if status != http.StatusCreated {
		t.Fatalf("create product: status=%d body=%v, want 201", status, resp)
	}
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("create product: no id in response %v", resp)
	}
	return id
}

// productExistsDB reports whether a product row with the given id is present.
func productExistsDB(t *testing.T, db *sql.DB, id string) bool {
	t.Helper()
	var x int
	err := db.QueryRow(`SELECT 1 FROM products WHERE id = ?`, id).Scan(&x)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("product exists check: %v", err)
	}
	return true
}

// TestCreateProduct covers create: with content.create → 201 + real row (price
// and sku persisted); without → 403; unauthenticated → 401.
func TestCreateProduct(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")
	authorToken := jwtFor(t, db, "au@example.com", "pw", "author") // no grants

	status, body := doJSON(t, srv, http.MethodPost, "/products",
		map[string]any{"title": "Widget", "body": "A widget", "price": "9.99", "sku": "SKU-1"}, authHeader(editorToken))
	if status != http.StatusCreated {
		t.Fatalf("with-perm status=%d body=%v, want 201", status, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("no id returned: %v", body)
	}
	var title, price, sku string
	if err := db.QueryRow(`SELECT title, price, sku FROM products WHERE id = ?`, id).Scan(&title, &price, &sku); err != nil {
		t.Fatalf("row not in DB: %v", err)
	}
	if title != "Widget" || price != "9.99" || sku != "SKU-1" {
		t.Fatalf("persisted = (%q,%q,%q), want (Widget,9.99,SKU-1)", title, price, sku)
	}

	// Without perm → 403.
	status, _ = doJSON(t, srv, http.MethodPost, "/products",
		map[string]any{"title": "x", "body": "y", "price": "1", "sku": "SKU-2"}, authHeader(authorToken))
	if status != http.StatusForbidden {
		t.Fatalf("without-perm status=%d, want 403", status)
	}

	// Unauthenticated → 401.
	status, _ = doJSON(t, srv, http.MethodPost, "/products",
		map[string]any{"title": "x", "body": "y", "price": "1", "sku": "SKU-3"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d, want 401", status)
	}
}

// TestCreateProductAcceptsJSONNumberPrice confirms a price sent as a JSON number
// (not a string) is accepted and stored as canonical text.
func TestCreateProductAcceptsJSONNumberPrice(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	status, body := doJSON(t, srv, http.MethodPost, "/products",
		map[string]any{"title": "Num", "body": "b", "price": 12.5, "sku": "NUM-1"}, authHeader(editorToken))
	if status != http.StatusCreated {
		t.Fatalf("json-number price status=%d body=%v, want 201", status, body)
	}
	id, _ := body["id"].(string)
	var price string
	if err := db.QueryRow(`SELECT price FROM products WHERE id = ?`, id).Scan(&price); err != nil {
		t.Fatalf("select price: %v", err)
	}
	if price != "12.5" {
		t.Fatalf("stored price = %q, want 12.5", price)
	}
}

// TestCreateProductAPIKeyRejected confirms a service identity (API key) cannot
// create a product (no human author) → 403 with a clear message.
func TestCreateProductAPIKeyRejected(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	key := apiKeyFor(t, db, "svc", "editor")

	status, body := doJSON(t, srv, http.MethodPost, "/products",
		map[string]any{"title": "x", "body": "y", "price": "1", "sku": "K-1"}, authHeader(key))
	if status != http.StatusForbidden {
		t.Fatalf("apikey-create status=%d, want 403", status)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Fatalf("expected clear error message, got %v", body)
	}
}

// TestProductMissingFields covers the 400 validation: missing title/body/sku
// gives a clear 400, no row created.
func TestProductMissingFields(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	for _, missing := range []map[string]any{
		{"body": "y", "price": "1", "sku": "M-1"},  // no title
		{"title": "x", "price": "1", "sku": "M-2"}, // no body
		{"title": "x", "body": "y", "price": "1"},  // no sku
	} {
		status, body := doJSON(t, srv, http.MethodPost, "/products", missing, authHeader(editorToken))
		if status != http.StatusBadRequest {
			t.Fatalf("missing-field status=%d body=%v, want 400", status, body)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM products`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows created despite validation failure: %d", n)
	}
}

// TestProductPriceNotNumericIs400 is the red-team check: a non-numeric price
// string → 400 (clear), NEVER 500 and NEVER a stored row.
func TestProductPriceNotNumericIs400(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	for _, bad := range []any{"abc", "1.2.3", "$5", "", "12e3", "1,000", true, map[string]any{"x": 1}} {
		status, body := doJSON(t, srv, http.MethodPost, "/products",
			map[string]any{"title": "Bad", "body": "b", "price": bad, "sku": "BAD-1"}, authHeader(editorToken))
		if status != http.StatusBadRequest {
			t.Fatalf("bad-price %v status=%d body=%v, want 400 (never 500)", bad, status, body)
		}
		if msg, _ := body["error"].(string); msg == "" {
			t.Fatalf("bad-price %v: expected clear error message, got %v", bad, body)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM products`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("row stored despite bad price: %d", n)
	}
}

// TestProductDuplicateSKUIs400 is the red-team check: a duplicate sku is rejected
// with a clear 400 (from the schema-level UNIQUE constraint, translated), NEVER a
// 500, and the second row is not stored.
func TestProductDuplicateSKUIs400(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.update")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	// First insert succeeds.
	createProduct(t, srv, editorToken, "First", "b", "1.00", "DUP-SKU")

	// Second insert with the SAME sku → 400 (clear message), not 500.
	status, body := doJSON(t, srv, http.MethodPost, "/products",
		map[string]any{"title": "Second", "body": "b", "price": "2.00", "sku": "DUP-SKU"}, authHeader(editorToken))
	if status != http.StatusBadRequest {
		t.Fatalf("dup-sku status=%d body=%v, want 400 (never 500)", status, body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Fatalf("dup-sku: expected clear error message, got %v", body)
	}

	// Only one row with that sku exists.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM products WHERE sku = ?`, "DUP-SKU").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("dup-sku produced %d rows, want 1", n)
	}

	// Updating a DIFFERENT product to collide with an existing sku → 400 too.
	other := createProduct(t, srv, editorToken, "Other", "b", "3.00", "OTHER-SKU")
	status, body = doJSON(t, srv, http.MethodPut, "/products/"+other,
		map[string]any{"title": "Other", "body": "b", "price": "3.00", "sku": "DUP-SKU"}, authHeader(editorToken))
	if status != http.StatusBadRequest {
		t.Fatalf("update-to-dup-sku status=%d body=%v, want 400", status, body)
	}
}

// TestListAndGetProducts covers reads: any authenticated identity (JWT no perms,
// AND an API key) → 200; unauthenticated → 401.
func TestListAndGetProducts(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")
	authorToken := jwtFor(t, db, "au@example.com", "pw", "author") // no perms
	key := apiKeyFor(t, db, "svc", "editor")

	id := createProduct(t, srv, editorToken, "P1", "B1", "5.00", "L-1")

	if status, _ := doJSON(t, srv, http.MethodGet, "/products", nil, authHeader(authorToken)); status != http.StatusOK {
		t.Fatalf("list jwt-noperm status=%d, want 200", status)
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/products", nil, authHeader(key)); status != http.StatusOK {
		t.Fatalf("list apikey status=%d, want 200", status)
	}
	status, body := doJSON(t, srv, http.MethodGet, "/products/"+id, nil, authHeader(authorToken))
	if status != http.StatusOK {
		t.Fatalf("get jwt status=%d, want 200", status)
	}
	if body["price"] != "5.00" || body["sku"] != "L-1" {
		t.Fatalf("get product price/sku = (%v,%v), want (5.00,L-1)", body["price"], body["sku"])
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/products", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("list unauth status=%d, want 401", status)
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/products/"+id, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("get unauth status=%d, want 401", status)
	}
}

// TestUpdateProduct covers update: with content.update → 200 + changes persisted;
// without → 403 and the row unchanged.
func TestUpdateProduct(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.update")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")
	authorToken := jwtFor(t, db, "au@example.com", "pw", "author") // no update

	id := createProduct(t, srv, editorToken, "Old", "OldBody", "1.00", "U-1")

	status, body := doJSON(t, srv, http.MethodPut, "/products/"+id,
		map[string]any{"title": "New", "body": "NewBody", "price": "2.50", "sku": "U-1b"}, authHeader(editorToken))
	if status != http.StatusOK {
		t.Fatalf("with-perm status=%d body=%v, want 200", status, body)
	}
	var title, prBody, price, sku string
	if err := db.QueryRow(`SELECT title, body, price, sku FROM products WHERE id = ?`, id).Scan(&title, &prBody, &price, &sku); err != nil {
		t.Fatalf("select: %v", err)
	}
	if title != "New" || prBody != "NewBody" || price != "2.50" || sku != "U-1b" {
		t.Fatalf("persisted = (%q,%q,%q,%q), want (New,NewBody,2.50,U-1b)", title, prBody, price, sku)
	}

	status, _ = doJSON(t, srv, http.MethodPut, "/products/"+id,
		map[string]any{"title": "Hack", "body": "Hack", "price": "9", "sku": "Hack"}, authHeader(authorToken))
	if status != http.StatusForbidden {
		t.Fatalf("without-perm status=%d, want 403", status)
	}
	if err := db.QueryRow(`SELECT title FROM products WHERE id = ?`, id).Scan(&title); err != nil {
		t.Fatalf("select: %v", err)
	}
	if title != "New" {
		t.Fatalf("row changed after 403: title=%q, want New", title)
	}
}

// TestDeleteProduct covers delete: with content.delete → 204 + row gone; without
// → 403 and the row still present.
func TestDeleteProduct(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.delete")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")
	authorToken := jwtFor(t, db, "au@example.com", "pw", "author") // no delete

	id := createProduct(t, srv, editorToken, "Bye", "Body", "1.00", "D-1")

	if status, _ := doJSON(t, srv, http.MethodDelete, "/products/"+id, nil, authHeader(authorToken)); status != http.StatusForbidden {
		t.Fatalf("without-perm status=%d, want 403", status)
	}
	if !productExistsDB(t, db, id) {
		t.Fatal("row disappeared after 403")
	}

	status, _ := doJSON(t, srv, http.MethodDelete, "/products/"+id, nil, authHeader(editorToken))
	if status != http.StatusNoContent && status != http.StatusOK {
		t.Fatalf("with-perm status=%d, want 204/200", status)
	}
	if productExistsDB(t, db, id) {
		t.Fatal("row still present after delete")
	}
}

// TestProductRoundTrip is the full JSON round-trip: create → appears in list →
// edit → delete → gone.
func TestProductRoundTrip(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.update", "content.delete")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	id := createProduct(t, srv, editorToken, "RT", "Body", "10.00", "RT-1")

	// Appears in list.
	_, list := doJSON(t, srv, http.MethodGet, "/products", nil, authHeader(editorToken))
	items, _ := list["products"].([]any)
	if len(items) != 1 {
		t.Fatalf("list has %d products, want 1", len(items))
	}

	// Edit.
	if status, _ := doJSON(t, srv, http.MethodPut, "/products/"+id,
		map[string]any{"title": "RT2", "body": "Body2", "price": "11.00", "sku": "RT-1"}, authHeader(editorToken)); status != http.StatusOK {
		t.Fatalf("edit status=%d, want 200", status)
	}

	// Delete.
	if status, _ := doJSON(t, srv, http.MethodDelete, "/products/"+id, nil, authHeader(editorToken)); status != http.StatusNoContent {
		t.Fatalf("delete status=%d, want 204", status)
	}
	if productExistsDB(t, db, id) {
		t.Fatal("row still present after delete")
	}
}

// TestProductNotFoundAndMalformedID confirms every gated route returns 404 (never
// 500/panic) for a nonexistent AND a malformed id.
func TestProductNotFoundAndMalformedID(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.update", "content.delete")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")
	h := authHeader(editorToken)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/products/" + nonexistentID},
		{http.MethodPut, "/products/" + nonexistentID},
		{http.MethodDelete, "/products/" + nonexistentID},
		{http.MethodGet, "/products/not-a-uuid"},
		{http.MethodPut, "/products/x'OR'1'='1"},
		{http.MethodDelete, "/products/abc"},
	}
	for _, c := range cases {
		status, body := doJSON(t, srv, c.method, c.path,
			map[string]any{"title": "x", "body": "y", "price": "1", "sku": "NF-1"}, h)
		if status != http.StatusNotFound {
			t.Fatalf("%s %s status=%d body=%v, want 404 (not 500)", c.method, c.path, status, body)
		}
	}
	// Sanity: the products table survived the injection-style ids.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM products`).Scan(&n); err != nil {
		t.Fatalf("products table missing after malformed-id attempts: %v", err)
	}
}
