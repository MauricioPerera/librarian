package server_test

// CONTRACT-12 T2/T3/T5 acceptance tests: the taxonomy-terms JSON API and the
// assignment of terms to content. Reuses the shared helpers (openAuthMux, doJSON,
// grant, jwtFor, apiKeyFor, authHeader, createArticle, createProduct,
// nonexistentID) from the sibling test files — same package.
//
// terms.manage is the single permission gating CRUD of terms AND (re)assignment.

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
)

// createTerm POSTs a term as the given token and returns its id.
func createTerm(t *testing.T, srv *httptest.Server, token, taxonomy, name, slug string, parentID *string) string {
	t.Helper()
	body := map[string]any{"taxonomy": taxonomy, "name": name, "slug": slug}
	if parentID != nil {
		body["parent_id"] = *parentID
	}
	status, resp := doJSON(t, srv, http.MethodPost, "/terms", body, authHeader(token))
	if status != http.StatusCreated {
		t.Fatalf("create term %q: status=%d body=%v, want 201", name, status, resp)
	}
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("create term %q: no id in response %v", name, resp)
	}
	return id
}

// --- T1 seed: terms.manage + taxonomies present ------------------------------

// TestTermsManageAndTaxonomiesSeeded confirms the new permission and the taxonomy
// catalog are seeded by openAuthMux (via store.SeedCatalogs).
func TestTermsManageAndTaxonomiesSeeded(t *testing.T) {
	db, _, cleanup := openAuthMux(t)
	defer cleanup()

	var pid string
	if err := db.QueryRow(`SELECT id FROM permissions WHERE name = 'terms.manage'`).Scan(&pid); err != nil {
		t.Fatalf("terms.manage not seeded: %v", err)
	}
	for _, name := range []string{"category", "tag"} {
		var xid string
		if err := db.QueryRow(`SELECT id FROM taxonomies WHERE name = ?`, name).Scan(&xid); err != nil {
			t.Fatalf("taxonomy %q not seeded: %v", name, err)
		}
	}
}

// --- T2: CRUD + gating -------------------------------------------------------

// TestCreateTermGating covers create: with terms.manage → 201 + real row; without
// → 403; unauthenticated → 401.
func TestCreateTermGating(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "terms.manage")
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")
	author := jwtFor(t, db, "au@example.com", "pw", "author") // no grants

	status, body := doJSON(t, srv, http.MethodPost, "/terms",
		map[string]any{"taxonomy": "category", "name": "Electrónica", "slug": "electronica"}, authHeader(editor))
	if status != http.StatusCreated {
		t.Fatalf("with-perm status=%d body=%v, want 201", status, body)
	}
	id, _ := body["id"].(string)
	var name, slug string
	if err := db.QueryRow(`SELECT name, slug FROM terms WHERE id = ?`, id).Scan(&name, &slug); err != nil {
		t.Fatalf("row not in DB: %v", err)
	}
	if name != "Electrónica" || slug != "electronica" {
		t.Fatalf("persisted (%q,%q), want (Electrónica,electronica)", name, slug)
	}

	status, _ = doJSON(t, srv, http.MethodPost, "/terms",
		map[string]any{"taxonomy": "category", "name": "x", "slug": "x"}, authHeader(author))
	if status != http.StatusForbidden {
		t.Fatalf("without-perm status=%d, want 403", status)
	}
	status, _ = doJSON(t, srv, http.MethodPost, "/terms",
		map[string]any{"taxonomy": "category", "name": "x", "slug": "x"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d, want 401", status)
	}
}

// TestCreateTermUnknownTaxonomyIs400 confirms an unknown taxonomy name → 400.
func TestCreateTermUnknownTaxonomyIs400(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "terms.manage")
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")

	status, body := doJSON(t, srv, http.MethodPost, "/terms",
		map[string]any{"taxonomy": "genre", "name": "x", "slug": "x"}, authHeader(editor))
	if status != http.StatusBadRequest {
		t.Fatalf("unknown-taxonomy status=%d body=%v, want 400", status, body)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM terms`).Scan(&n)
	if n != 0 {
		t.Fatalf("term created despite unknown taxonomy: %d rows", n)
	}
}

// TestTermDuplicateSlugSameTaxonomyIs400 is the core T2 red-team: a duplicate slug
// WITHIN the same taxonomy → clear 400 (never 500), no second row; the SAME slug
// under a DIFFERENT taxonomy is allowed.
func TestTermDuplicateSlugSameTaxonomyIs400(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "terms.manage")
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")

	createTerm(t, srv, editor, "category", "News", "news", nil)

	// Same slug, same taxonomy → 400.
	status, body := doJSON(t, srv, http.MethodPost, "/terms",
		map[string]any{"taxonomy": "category", "name": "News 2", "slug": "news"}, authHeader(editor))
	if status != http.StatusBadRequest {
		t.Fatalf("dup-slug-same-taxonomy status=%d body=%v, want 400 (never 500)", status, body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Fatalf("dup-slug: expected clear error message, got %v", body)
	}

	// Same slug, DIFFERENT taxonomy → allowed (the composite unique's whole point).
	status, body = doJSON(t, srv, http.MethodPost, "/terms",
		map[string]any{"taxonomy": "tag", "name": "News", "slug": "news"}, authHeader(editor))
	if status != http.StatusCreated {
		t.Fatalf("same-slug-different-taxonomy status=%d body=%v, want 201 (allowed)", status, body)
	}

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM terms WHERE slug = 'news'`).Scan(&n)
	if n != 2 {
		t.Fatalf("expected exactly 2 rows with slug 'news' (one per taxonomy), got %d", n)
	}
}

// TestListAndGetTerms covers reads: any authenticated identity (JWT no perms AND
// an API key) → 200; unauthenticated → 401.
func TestListAndGetTerms(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "terms.manage")
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")
	author := jwtFor(t, db, "au@example.com", "pw", "author") // no perms
	key := apiKeyFor(t, db, "svc", "author")

	id := createTerm(t, srv, editor, "category", "Books", "books", nil)

	if status, _ := doJSON(t, srv, http.MethodGet, "/terms", nil, authHeader(author)); status != http.StatusOK {
		t.Fatalf("list jwt-noperm status=%d, want 200", status)
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/terms", nil, authHeader(key)); status != http.StatusOK {
		t.Fatalf("list apikey status=%d, want 200", status)
	}
	status, body := doJSON(t, srv, http.MethodGet, "/terms/"+id, nil, authHeader(author))
	if status != http.StatusOK {
		t.Fatalf("get status=%d, want 200", status)
	}
	if body["taxonomy"] != "category" || body["slug"] != "books" {
		t.Fatalf("get term = %v, want taxonomy=category slug=books", body)
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/terms", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("list unauth status=%d, want 401", status)
	}
}

// TestUpdateAndDeleteTerm covers update + delete gating and 404s.
func TestUpdateAndDeleteTerm(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "terms.manage")
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")
	author := jwtFor(t, db, "au@example.com", "pw", "author")

	id := createTerm(t, srv, editor, "category", "Old", "old-slug", nil)

	// Update with perm → 200 + persisted.
	status, body := doJSON(t, srv, http.MethodPut, "/terms/"+id,
		map[string]any{"taxonomy": "category", "name": "New", "slug": "new-slug"}, authHeader(editor))
	if status != http.StatusOK {
		t.Fatalf("update status=%d body=%v, want 200", status, body)
	}
	var name, slug string
	db.QueryRow(`SELECT name, slug FROM terms WHERE id = ?`, id).Scan(&name, &slug)
	if name != "New" || slug != "new-slug" {
		t.Fatalf("persisted (%q,%q), want (New,new-slug)", name, slug)
	}

	// Update without perm → 403, unchanged.
	if status, _ := doJSON(t, srv, http.MethodPut, "/terms/"+id,
		map[string]any{"taxonomy": "category", "name": "Hack", "slug": "hack"}, authHeader(author)); status != http.StatusForbidden {
		t.Fatalf("update without-perm status=%d, want 403", status)
	}

	// 404 on missing/malformed ids for update + delete.
	for _, badID := range []string{nonexistentID, "not-a-uuid"} {
		if status, _ := doJSON(t, srv, http.MethodPut, "/terms/"+badID,
			map[string]any{"taxonomy": "category", "name": "x", "slug": "z"}, authHeader(editor)); status != http.StatusNotFound {
			t.Fatalf("update %s status=%d, want 404", badID, status)
		}
		if status, _ := doJSON(t, srv, http.MethodDelete, "/terms/"+badID, nil, authHeader(editor)); status != http.StatusNotFound {
			t.Fatalf("delete %s status=%d, want 404", badID, status)
		}
	}

	// Delete without perm → 403; with perm → 204 + gone.
	if status, _ := doJSON(t, srv, http.MethodDelete, "/terms/"+id, nil, authHeader(author)); status != http.StatusForbidden {
		t.Fatalf("delete without-perm status=%d, want 403", status)
	}
	if status, _ := doJSON(t, srv, http.MethodDelete, "/terms/"+id, nil, authHeader(editor)); status != http.StatusNoContent {
		t.Fatalf("delete with-perm status=%d, want 204", status)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM terms WHERE id = ?`, id).Scan(&n)
	if n != 0 {
		t.Fatalf("term still present after delete: %d", n)
	}
}

// --- T5: hierarchy -----------------------------------------------------------

// TestTermHierarchyAndParentDeleteSetsNull covers the hierarchy criteria: a term
// created with a real parent_id is confirmed in the response; deleting the parent
// leaves the child existing with parent_id NULL (SET NULL, confirmed with a real
// query — not assumed). A self-parent is rejected 400.
func TestTermHierarchyAndParentDeleteSetsNull(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "terms.manage")
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")

	parent := createTerm(t, srv, editor, "category", "Hardware", "hardware", nil)

	// Child with a real parent_id → confirmed in the create response.
	status, body := doJSON(t, srv, http.MethodPost, "/terms",
		map[string]any{"taxonomy": "category", "name": "GPUs", "slug": "gpus", "parent_id": parent}, authHeader(editor))
	if status != http.StatusCreated {
		t.Fatalf("child create status=%d body=%v, want 201", status, body)
	}
	child, _ := body["id"].(string)
	if body["parent_id"] != parent {
		t.Fatalf("child parent_id in response = %v, want %q", body["parent_id"], parent)
	}

	// Self-parent → 400.
	if status, _ := doJSON(t, srv, http.MethodPut, "/terms/"+child,
		map[string]any{"taxonomy": "category", "name": "GPUs", "slug": "gpus", "parent_id": child}, authHeader(editor)); status != http.StatusBadRequest {
		t.Fatalf("self-parent status=%d, want 400", status)
	}

	// Unknown parent → 400.
	if status, _ := doJSON(t, srv, http.MethodPost, "/terms",
		map[string]any{"taxonomy": "category", "name": "Orphan", "slug": "orphan", "parent_id": nonexistentID}, authHeader(editor)); status != http.StatusBadRequest {
		t.Fatalf("unknown-parent status=%d, want 400", status)
	}

	// Delete the parent → child survives with parent_id NULL (SET NULL).
	if status, _ := doJSON(t, srv, http.MethodDelete, "/terms/"+parent, nil, authHeader(editor)); status != http.StatusNoContent {
		t.Fatalf("delete parent status=%d, want 204", status)
	}
	var (
		exists int
		parVal sql.NullString
	)
	if err := db.QueryRow(`SELECT 1, parent_id FROM terms WHERE id = ?`, child).Scan(&exists, &parVal); err != nil {
		t.Fatalf("child gone after parent delete (should survive): %v", err)
	}
	if parVal.Valid {
		t.Fatalf("child parent_id = %q after parent delete, want NULL (SET NULL)", parVal.String)
	}
}

// --- T3: assignment ----------------------------------------------------------

// TestAssignTermsRoundTripArticle covers the full T3/T5 round-trip on an article:
// create term → assign → GET includes it → reassign to empty → no longer appears.
func TestAssignTermsRoundTripArticle(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "terms.manage")
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")

	art := createArticle(t, srv, editor, "Post", "Body")
	cat := createTerm(t, srv, editor, "category", "Electrónica", "electronica", nil)
	tag := createTerm(t, srv, editor, "tag", "Oferta", "oferta", nil)

	// Assign two terms.
	status, body := doJSON(t, srv, http.MethodPut, "/articles/"+art+"/terms",
		map[string]any{"term_ids": []string{cat, tag}}, authHeader(editor))
	if status != http.StatusOK {
		t.Fatalf("assign status=%d body=%v, want 200", status, body)
	}

	// GET the article → includes both terms.
	_, art1 := doJSON(t, srv, http.MethodGet, "/articles/"+art, nil, authHeader(editor))
	terms, _ := art1["terms"].([]any)
	if len(terms) != 2 {
		t.Fatalf("GET article terms len=%d, want 2 (body=%v)", len(terms), art1)
	}

	// Reassign to just one → the other drops.
	if status, _ := doJSON(t, srv, http.MethodPut, "/articles/"+art+"/terms",
		map[string]any{"term_ids": []string{cat}}, authHeader(editor)); status != http.StatusOK {
		t.Fatalf("reassign status=%d, want 200", status)
	}
	_, art2 := doJSON(t, srv, http.MethodGet, "/articles/"+art, nil, authHeader(editor))
	terms2, _ := art2["terms"].([]any)
	if len(terms2) != 1 {
		t.Fatalf("after reassign terms len=%d, want 1", len(terms2))
	}

	// Reassign to empty → terms omitted entirely.
	if status, _ := doJSON(t, srv, http.MethodPut, "/articles/"+art+"/terms",
		map[string]any{"term_ids": []string{}}, authHeader(editor)); status != http.StatusOK {
		t.Fatalf("clear status=%d, want 200", status)
	}
	_, art3 := doJSON(t, srv, http.MethodGet, "/articles/"+art, nil, authHeader(editor))
	if terms3, ok := art3["terms"].([]any); ok && len(terms3) != 0 {
		t.Fatalf("after clear terms len=%d, want 0/absent", len(terms3))
	}
}

// TestAssignTermsProductAndGating covers assignment on a product plus the gating:
// terms.manage required; unknown term id → 400 with nothing changed.
func TestAssignTermsProductAndGating(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "terms.manage")
	grant(t, db, "author", "content.create") // author can create content, NOT manage terms
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")
	author := jwtFor(t, db, "au@example.com", "pw", "author")

	prod := createProduct(t, srv, editor, "Widget", "desc", "9.99", "SKU-T1")
	cat := createTerm(t, srv, editor, "category", "Gadgets", "gadgets", nil)

	// author has content.create but not terms.manage → assignment 403.
	if status, _ := doJSON(t, srv, http.MethodPut, "/products/"+prod+"/terms",
		map[string]any{"term_ids": []string{cat}}, authHeader(author)); status != http.StatusForbidden {
		t.Fatalf("assign without terms.manage status=%d, want 403", status)
	}

	// Unknown term id → 400, nothing assigned.
	status, body := doJSON(t, srv, http.MethodPut, "/products/"+prod+"/terms",
		map[string]any{"term_ids": []string{cat, nonexistentID}}, authHeader(editor))
	if status != http.StatusBadRequest {
		t.Fatalf("unknown-term status=%d body=%v, want 400", status, body)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM product_terms WHERE product_id = ?`, prod).Scan(&n)
	if n != 0 {
		t.Fatalf("assignment partially applied despite unknown id: %d rows", n)
	}

	// Valid assignment → 200 + GET includes it.
	if status, _ := doJSON(t, srv, http.MethodPut, "/products/"+prod+"/terms",
		map[string]any{"term_ids": []string{cat}}, authHeader(editor)); status != http.StatusOK {
		t.Fatalf("assign status=%d, want 200", status)
	}
	_, got := doJSON(t, srv, http.MethodGet, "/products/"+prod, nil, authHeader(editor))
	if terms, _ := got["terms"].([]any); len(terms) != 1 {
		t.Fatalf("GET product terms len=%d, want 1", len(terms))
	}

	// Assigning to a nonexistent product → 404.
	if status, _ := doJSON(t, srv, http.MethodPut, "/products/"+nonexistentID+"/terms",
		map[string]any{"term_ids": []string{cat}}, authHeader(editor)); status != http.StatusNotFound {
		t.Fatalf("assign to missing product status=%d, want 404", status)
	}
}

// TestDeleteTermCascadesJunctionNotContent is the red-team check: deleting a term
// that is assigned to content removes only the junction row (content survives),
// and deleting the CONTENT removes only the junction row (term survives).
func TestDeleteTermCascadesJunctionNotContent(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.delete", "terms.manage")
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")

	art := createArticle(t, srv, editor, "Post", "Body")
	cat := createTerm(t, srv, editor, "category", "Temp", "temp", nil)
	if status, _ := doJSON(t, srv, http.MethodPut, "/articles/"+art+"/terms",
		map[string]any{"term_ids": []string{cat}}, authHeader(editor)); status != http.StatusOK {
		t.Fatalf("assign status=%d, want 200", status)
	}

	// Delete the TERM → article survives, junction row gone.
	if status, _ := doJSON(t, srv, http.MethodDelete, "/terms/"+cat, nil, authHeader(editor)); status != http.StatusNoContent {
		t.Fatalf("delete term status=%d, want 204", status)
	}
	var artCount, junc int
	db.QueryRow(`SELECT COUNT(*) FROM articles WHERE id = ?`, art).Scan(&artCount)
	db.QueryRow(`SELECT COUNT(*) FROM article_terms WHERE article_id = ?`, art).Scan(&junc)
	if artCount != 1 {
		t.Fatalf("article deleted when its term was deleted: count=%d", artCount)
	}
	if junc != 0 {
		t.Fatalf("junction row survived term delete: %d", junc)
	}

	// Now the reverse: assign a fresh term, delete the ARTICLE, term survives.
	cat2 := createTerm(t, srv, editor, "category", "Keep", "keep", nil)
	if status, _ := doJSON(t, srv, http.MethodPut, "/articles/"+art+"/terms",
		map[string]any{"term_ids": []string{cat2}}, authHeader(editor)); status != http.StatusOK {
		t.Fatalf("assign2 status=%d, want 200", status)
	}
	if status, _ := doJSON(t, srv, http.MethodDelete, "/articles/"+art, nil, authHeader(editor)); status != http.StatusNoContent {
		t.Fatalf("delete article status=%d, want 204", status)
	}
	var termCount, junc2 int
	db.QueryRow(`SELECT COUNT(*) FROM terms WHERE id = ?`, cat2).Scan(&termCount)
	db.QueryRow(`SELECT COUNT(*) FROM article_terms WHERE term_id = ?`, cat2).Scan(&junc2)
	if termCount != 1 {
		t.Fatalf("term deleted when its article was deleted: count=%d", termCount)
	}
	if junc2 != 0 {
		t.Fatalf("junction row survived article delete: %d", junc2)
	}
}
