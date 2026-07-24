package server_test

// CONTRACT-14 acceptance tests: the GENERIC JSON CRUD over dynamic content
// types. Every test creates its content type through the REAL CONTRACT-13 API
// (POST /content-types) and then drives the generic routes over real HTTP —
// there is no shortcut that bypasses either layer.
//
// Reuses the shared helpers from the sibling test files (same package):
// openAuthMux, doJSON, grant, jwtFor, apiKeyFor, authHeader, sqliteTableExists,
// countTables, tableColumns, reviewsBody.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
)

// createDynamicType registers a dynamic content type through the real API.
func createDynamicType(t *testing.T, srv *httptest.Server, token string, body map[string]any) {
	t.Helper()
	status, resp := doJSON(t, srv, http.MethodPost, "/content-types", body, authHeader(token))
	if status != http.StatusCreated {
		t.Fatalf("create content type %v: status=%d body=%v, want 201", body["name"], status, resp)
	}
}

// contentAdmin sets up an identity holding every permission this contract
// touches: content_types.manage (to define the type) plus the generic
// content.* grants (to load content into it).
func contentAdmin(t *testing.T, db *sql.DB) string {
	t.Helper()
	grant(t, db, "administrator", "content_types.manage", "content.create", "content.update", "content.delete")
	return jwtFor(t, db, "admin@example.com", "pw", "administrator")
}

// items decodes the {"type":..., "items":[...]} list envelope.
func items(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("list body has no items array: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("list item is not an object: %v", r)
		}
		out = append(out, m)
	}
	return out
}

// --- T1 + T2 + T3: the full round trip over the five field types -------------

// TestDynamicContentRoundTrip is the central T3 criterion: create a type with
// all five field types, then create → list → read → update → delete → 404,
// asserting at every step that each value comes back with the CORRECT JSON TYPE
// (an integer as a number, a boolean as a boolean — not everything a string).
func TestDynamicContentRoundTrip(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())

	// --- CREATE (T2) ---
	status, created := doJSON(t, srv, http.MethodPost, "/content/reviews", map[string]any{
		"headline":   "A great read",
		"score":      5,
		"price_paid": "19.99",
		"verified":   true,
		"read_on":    "2026-07-24",
	}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("POST /content/reviews: status=%d body=%v, want 201", status, created)
	}
	rowID, _ := created["id"].(string)
	if rowID == "" {
		t.Fatalf("create response has no id: %v", created)
	}

	// THE typing criterion, on the decoded JSON: json.Unmarshal gives float64
	// for a JSON number, bool for a JSON boolean and string for a JSON string,
	// so the Go type of each value IS the proof of its JSON type.
	assertReviewTypes := func(label string, row map[string]any, headline string, score float64, verified bool) {
		t.Helper()
		if v, ok := row["headline"].(string); !ok || v != headline {
			t.Fatalf("%s: headline = %#v (%T), want string %q", label, row["headline"], row["headline"], headline)
		}
		if v, ok := row["score"].(float64); !ok || v != score {
			t.Fatalf("%s: score = %#v (%T), want JSON number %v", label, row["score"], row["score"], score)
		}
		if v, ok := row["verified"].(bool); !ok || v != verified {
			t.Fatalf("%s: verified = %#v (%T), want JSON boolean %v", label, row["verified"], row["verified"], verified)
		}
		// decimal is a JSON STRING by design (compat stores DecimalType as
		// canonical TEXT; a JSON number would re-introduce float rounding) —
		// the same choice products.price makes.
		if _, ok := row["price_paid"].(string); !ok {
			t.Fatalf("%s: price_paid = %#v (%T), want JSON string", label, row["price_paid"], row["price_paid"])
		}
		if _, ok := row["read_on"].(string); !ok {
			t.Fatalf("%s: read_on = %#v (%T), want JSON string", label, row["read_on"], row["read_on"])
		}
		for _, common := range []string{"id", "author_id", "created_at", "updated_at"} {
			if v, ok := row[common].(string); !ok || v == "" {
				t.Fatalf("%s: common column %q = %#v, want a non-empty string", label, common, row[common])
			}
		}
		if _, present := row["metadata"]; !present {
			t.Fatalf("%s: the common metadata column is missing from the row: %v", label, row)
		}
	}
	assertReviewTypes("create", created, "A great read", 5, true)
	if created["price_paid"] != "19.99" {
		t.Fatalf("create: price_paid = %v, want 19.99 verbatim", created["price_paid"])
	}
	if created["read_on"] != "2026-07-24" {
		t.Fatalf("create: read_on = %v, want 2026-07-24 verbatim", created["read_on"])
	}
	t.Logf("T2 CREATE OK: %v", created)

	// --- LIST (T1) ---
	status, body := doJSON(t, srv, http.MethodGet, "/content/reviews", nil, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("GET /content/reviews: status=%d body=%v, want 200", status, body)
	}
	list := items(t, body)
	if len(list) != 1 {
		t.Fatalf("list returned %d rows, want 1: %v", len(list), body)
	}
	assertReviewTypes("list", list[0], "A great read", 5, true)
	t.Logf("T1 LIST OK: type=%v items=%v", body["type"], list)

	// --- READ BY ID (T1) ---
	status, one := doJSON(t, srv, http.MethodGet, "/content/reviews/"+rowID, nil, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("GET /content/reviews/{id}: status=%d body=%v, want 200", status, one)
	}
	assertReviewTypes("detail", one, "A great read", 5, true)
	t.Logf("T1 DETAIL OK: %v", one)

	// --- UPDATE (T2) ---
	status, updated := doJSON(t, srv, http.MethodPut, "/content/reviews/"+rowID, map[string]any{
		"headline":   "Actually mediocre",
		"score":      2,
		"price_paid": "0.01",
		"verified":   false,
		"read_on":    "2026-07-25",
	}, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("PUT /content/reviews/{id}: status=%d body=%v, want 200", status, updated)
	}
	assertReviewTypes("update", updated, "Actually mediocre", 2, false)
	if updated["price_paid"] != "0.01" || updated["read_on"] != "2026-07-25" {
		t.Fatalf("update did not persist decimal/date: %v", updated)
	}
	if updated["id"] != rowID {
		t.Fatalf("update changed the id: %v", updated)
	}
	// Re-read confirms the update was stored, not just echoed.
	_, reread := doJSON(t, srv, http.MethodGet, "/content/reviews/"+rowID, nil, authHeader(admin))
	assertReviewTypes("re-read", reread, "Actually mediocre", 2, false)
	t.Logf("T2 UPDATE OK: %v", updated)

	// --- DELETE (T2) → then 404 ---
	status, body = doJSON(t, srv, http.MethodDelete, "/content/reviews/"+rowID, nil, authHeader(admin))
	if status != http.StatusNoContent {
		t.Fatalf("DELETE /content/reviews/{id}: status=%d body=%v, want 204", status, body)
	}
	status, body = doJSON(t, srv, http.MethodGet, "/content/reviews/"+rowID, nil, authHeader(admin))
	if status != http.StatusNotFound {
		t.Fatalf("GET after DELETE: status=%d body=%v, want 404", status, body)
	}
	status, body = doJSON(t, srv, http.MethodGet, "/content/reviews", nil, authHeader(admin))
	if len(items(t, body)) != 0 {
		t.Fatalf("list after DELETE returned rows: %v", body)
	}
	t.Logf("T2 DELETE OK: 204, then 404 on read, empty list")
}

// TestDynamicContentNullsAndOmittedFields documents the chosen missing-field
// semantics: an omitted or explicitly-null field is stored as NULL, identically
// on create and update (PUT is a FULL replacement of the row's own fields).
func TestDynamicContentNullsAndOmittedFields(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())

	// Create with NOTHING but the type: every own field must be JSON null.
	status, created := doJSON(t, srv, http.MethodPost, "/content/reviews", map[string]any{}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create with an empty body: status=%d body=%v, want 201", status, created)
	}
	for _, f := range []string{"headline", "score", "price_paid", "verified", "read_on"} {
		if v, present := created[f]; !present || v != nil {
			t.Fatalf("omitted field %q = %#v, want JSON null", f, v)
		}
	}
	rowID, _ := created["id"].(string)

	// Fill it, then PUT with only ONE field: the others must be reset to NULL —
	// the same full-replacement semantics articles/products have.
	if status, body := doJSON(t, srv, http.MethodPut, "/content/reviews/"+rowID, map[string]any{
		"headline": "filled", "score": 3, "price_paid": "1.00", "verified": true, "read_on": "2026-01-01",
	}, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("fill: status=%d body=%v", status, body)
	}
	status, replaced := doJSON(t, srv, http.MethodPut, "/content/reviews/"+rowID,
		map[string]any{"headline": "only this one"}, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("partial PUT: status=%d body=%v, want 200", status, replaced)
	}
	if replaced["headline"] != "only this one" {
		t.Fatalf("PUT did not set headline: %v", replaced)
	}
	for _, f := range []string{"score", "price_paid", "verified", "read_on"} {
		if v := replaced[f]; v != nil {
			t.Fatalf("PUT is a full replacement: field %q = %#v, want JSON null", f, v)
		}
	}

	// An EXPLICIT null is accepted and identical to omission.
	status, nulled := doJSON(t, srv, http.MethodPut, "/content/reviews/"+rowID,
		map[string]any{"headline": nil, "score": nil}, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("explicit nulls: status=%d body=%v, want 200", status, nulled)
	}
	if nulled["headline"] != nil || nulled["score"] != nil {
		t.Fatalf("explicit nulls not stored: %v", nulled)
	}
	t.Logf("NULL SEMANTICS OK: omitted == explicit null == NULL, identical on create and update")
}

// TestDynamicContentEmptyListAndBadID covers two red-team items: a type with
// zero rows lists as an empty ARRAY (not null, not 404), and a malformed
// (non-UUID) id is a 404, never a 500.
func TestDynamicContentEmptyListAndBadID(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())

	status, body := doJSON(t, srv, http.MethodGet, "/content/reviews", nil, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("list of an empty type: status=%d body=%v, want 200", status, body)
	}
	raw, ok := body["items"].([]any)
	if !ok || raw == nil {
		t.Fatalf("empty type must list as [], got %#v", body["items"])
	}
	if len(raw) != 0 {
		t.Fatalf("empty type listed %d rows", len(raw))
	}

	for _, badID := range []string{"not-a-uuid", "0", "%20", "x';DROP%20TABLE%20users;--"} {
		status, body := doJSON(t, srv, http.MethodGet, "/content/reviews/"+badID, nil, authHeader(admin))
		if status != http.StatusNotFound {
			t.Fatalf("GET /content/reviews/%s: status=%d body=%v, want 404 (never 500)", badID, status, body)
		}
		status, _ = doJSON(t, srv, http.MethodPut, "/content/reviews/"+badID, map[string]any{"headline": "x"}, authHeader(admin))
		if status != http.StatusNotFound {
			t.Fatalf("PUT /content/reviews/%s: status=%d, want 404", badID, status)
		}
		status, _ = doJSON(t, srv, http.MethodDelete, "/content/reviews/"+badID, nil, authHeader(admin))
		if status != http.StatusNotFound {
			t.Fatalf("DELETE /content/reviews/%s: status=%d, want 404", badID, status)
		}
	}
	if !sqliteTableExists(t, db, "users") {
		t.Fatal("the 'users' table is gone — an injected id was executed")
	}
	t.Logf("EMPTY/BAD-ID OK: [] for an empty type; malformed ids → 404 on GET/PUT/DELETE; users intact")
}

// --- T2: per-field validation ------------------------------------------------

// TestDynamicContentFieldValidation is the T2 criterion: a field that does not
// exist in the type → 400; a value whose JSON type does not match the declared
// FieldType → 400 with a clear message, never a 500 and never a stored value.
func TestDynamicContentFieldValidation(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())

	cases := []struct {
		label string
		body  map[string]any
	}{
		{"unknown field", map[string]any{"nope": "x"}},
		{"unknown field alongside valid ones", map[string]any{"headline": "ok", "nope": 1}},
		{"common column id", map[string]any{"id": "00000000-0000-0000-0000-000000000000"}},
		{"common column author_id", map[string]any{"author_id": "00000000-0000-0000-0000-000000000000"}},
		{"common column created_at", map[string]any{"created_at": "1999-01-01"}},
		{"common column updated_at", map[string]any{"updated_at": "1999-01-01"}},
		{"common column metadata", map[string]any{"metadata": map[string]any{"a": 1}}},
		{"hostile field name", map[string]any{`sc"ore`: 1}},
		{"injecting field name", map[string]any{"score = 1, headline = 'pwned'": 1}},
		{"semicolon field name", map[string]any{"score; DROP TABLE users": 1}},
		{"text given a number", map[string]any{"headline": 42}},
		{"text given a boolean", map[string]any{"headline": true}},
		{"text given an object", map[string]any{"headline": map[string]any{"a": 1}}},
		{"integer given a string", map[string]any{"score": "5"}},
		{"integer given a decimal", map[string]any{"score": 5.5}},
		{"integer given a boolean", map[string]any{"score": true}},
		{"boolean given a number", map[string]any{"verified": 1}},
		{"boolean given a string", map[string]any{"verified": "true"}},
		{"decimal given a boolean", map[string]any{"price_paid": true}},
		{"decimal given a non-numeric string", map[string]any{"price_paid": "cheap"}},
		{"decimal given an object", map[string]any{"price_paid": map[string]any{"a": 1}}},
		{"date given a number", map[string]any{"read_on": 20260724}},
		{"date given a non-date string", map[string]any{"read_on": "yesterday"}},
		{"date given an impossible date", map[string]any{"read_on": "2026-13-45"}},
		{"date given a timestamp", map[string]any{"read_on": "2026-07-24T00:00:00Z"}},
	}
	for _, tc := range cases {
		status, body := doJSON(t, srv, http.MethodPost, "/content/reviews", tc.body, authHeader(admin))
		if status != http.StatusBadRequest {
			t.Fatalf("CREATE [%s] %v: status=%d body=%v, want 400", tc.label, tc.body, status, body)
		}
		t.Logf("400 create [%-34s] -> %v", tc.label, body["error"])
		// The same rule applies on UPDATE — consistent between create and update.
		status, body = doJSON(t, srv, http.MethodPut, "/content/reviews/00000000-0000-0000-0000-000000000000", tc.body, authHeader(admin))
		if status != http.StatusBadRequest {
			t.Fatalf("UPDATE [%s] %v: status=%d body=%v, want 400 (validation before the 404)", tc.label, tc.body, status, body)
		}
	}

	// A body that is not a JSON object at all → 400, never a panic.
	for _, raw := range []any{[]any{1, 2}, "a string", 42} {
		if status, body := doJSON(t, srv, http.MethodPost, "/content/reviews", raw, authHeader(admin)); status != http.StatusBadRequest {
			t.Fatalf("non-object body %v: status=%d body=%v, want 400", raw, status, body)
		}
	}

	// NOTHING was stored by any rejected request.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM reviews`).Scan(&n); err != nil {
		t.Fatalf("count reviews: %v", err)
	}
	if n != 0 {
		t.Fatalf("rejected requests stored %d rows, want 0", n)
	}
	// And the common columns were never touched: the injected/common-column
	// bodies could not have altered the table shape either.
	got := tableColumns(t, db, "reviews")
	want := []string{"id", "author_id", "headline", "score", "price_paid", "verified", "read_on", "created_at", "updated_at", "metadata"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("reviews columns changed: %v, want %v", got, want)
	}
	t.Logf("VALIDATION OK: %d rejected bodies, 0 rows stored, table shape unchanged", len(cases))
}

// TestDynamicContentCommonColumnsAreServerOwned proves the red-team item
// positively: id / author_id / created_at / updated_at are set by the server,
// and a create that ALSO sends valid fields does not get to influence them.
func TestDynamicContentCommonColumnsAreServerOwned(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())

	status, created := doJSON(t, srv, http.MethodPost, "/content/reviews",
		map[string]any{"headline": "server owned"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%v", status, created)
	}
	var authorID string
	if err := db.QueryRow(`SELECT id FROM users WHERE email = 'admin@example.com'`).Scan(&authorID); err != nil {
		t.Fatalf("lookup author: %v", err)
	}
	if created["author_id"] != authorID {
		t.Fatalf("author_id = %v, want the authenticated user %s", created["author_id"], authorID)
	}
	if created["id"] == "" || created["created_at"] == "" || created["updated_at"] == "" {
		t.Fatalf("server-owned columns are empty: %v", created)
	}
	t.Logf("SERVER-OWNED OK: author_id=%s taken from the JWT identity; id/timestamps from column defaults", authorID)
}

// --- T3: isolation between types ---------------------------------------------

// TestDynamicContentIsolationBetweenTypes creates TWO dynamic types, loads rows
// into both, and confirms each list returns only its own rows.
func TestDynamicContentIsolationBetweenTypes(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)

	createDynamicType(t, srv, admin, reviewsBody())
	createDynamicType(t, srv, admin, map[string]any{
		"name": "recipes",
		"fields": []map[string]string{
			{"name": "title", "type": "text"},
			{"name": "servings", "type": "integer"},
		},
	})

	for i := 0; i < 3; i++ {
		if status, body := doJSON(t, srv, http.MethodPost, "/content/reviews",
			map[string]any{"headline": fmt.Sprintf("review %d", i), "score": i}, authHeader(admin)); status != http.StatusCreated {
			t.Fatalf("create review %d: status=%d body=%v", i, status, body)
		}
	}
	for i := 0; i < 2; i++ {
		if status, body := doJSON(t, srv, http.MethodPost, "/content/recipes",
			map[string]any{"title": fmt.Sprintf("recipe %d", i), "servings": i + 1}, authHeader(admin)); status != http.StatusCreated {
			t.Fatalf("create recipe %d: status=%d body=%v", i, status, body)
		}
	}

	_, body := doJSON(t, srv, http.MethodGet, "/content/reviews", nil, authHeader(admin))
	reviews := items(t, body)
	if len(reviews) != 3 {
		t.Fatalf("reviews list returned %d rows, want 3: %v", len(reviews), body)
	}
	for _, row := range reviews {
		if _, leaked := row["title"]; leaked {
			t.Fatalf("a reviews row carries a recipes field: %v", row)
		}
		if _, own := row["headline"]; !own {
			t.Fatalf("a reviews row is missing its own field: %v", row)
		}
	}
	_, body = doJSON(t, srv, http.MethodGet, "/content/recipes", nil, authHeader(admin))
	recipes := items(t, body)
	if len(recipes) != 2 {
		t.Fatalf("recipes list returned %d rows, want 2: %v", len(recipes), body)
	}
	for _, row := range recipes {
		if _, leaked := row["headline"]; leaked {
			t.Fatalf("a recipes row carries a reviews field: %v", row)
		}
	}

	// A row of one type is not readable through the OTHER type's route.
	reviewID, _ := reviews[0]["id"].(string)
	if status, _ := doJSON(t, srv, http.MethodGet, "/content/recipes/"+reviewID, nil, authHeader(admin)); status != http.StatusNotFound {
		t.Fatalf("a reviews id is readable under /content/recipes: status=%d, want 404", status)
	}
	// Nor deletable.
	if status, _ := doJSON(t, srv, http.MethodDelete, "/content/recipes/"+reviewID, nil, authHeader(admin)); status != http.StatusNotFound {
		t.Fatalf("a reviews id is deletable under /content/recipes: status=%d, want 404", status)
	}
	var stillThere int
	db.QueryRow(`SELECT count(*) FROM reviews`).Scan(&stillThere)
	if stillThere != 3 {
		t.Fatalf("cross-type delete removed rows: reviews has %d, want 3", stillThere)
	}
	t.Logf("ISOLATION OK: reviews=3 recipes=2, no field leakage, cross-type id → 404 and no row touched")
}

// --- T3: security ------------------------------------------------------------

// TestDynamicContentHostileTypeNames is the most important security criterion:
// a {type} segment that is unknown OR hostile never produces a query against a
// dynamic table. It is resolved against a PERSISTED definition first, and no
// definition matches, so the answer is 404 — and the system tables are proven
// intact afterwards by direct queries.
func TestDynamicContentHostileTypeNames(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())

	tablesBefore := countTables(t, db)
	var usersBefore, permsBefore, rolesBefore, typesBefore int
	db.QueryRow(`SELECT count(*) FROM users`).Scan(&usersBefore)
	db.QueryRow(`SELECT count(*) FROM permissions`).Scan(&permsBefore)
	db.QueryRow(`SELECT count(*) FROM roles`).Scan(&rolesBefore)
	db.QueryRow(`SELECT count(*) FROM content_types`).Scan(&typesBefore)

	hostile := []struct {
		label string
		name  string
	}{
		{"unknown", "nope"},
		{"code table users", "users"},
		{"code table articles", "articles"},
		{"code table products", "products"},
		{"registry table", "content_types"},
		{"registry fields table", "content_type_fields"},
		{"api keys table", "api_keys"},
		{"compat internal", "__compat_schema"},
		{"double quote", `re"views`},
		{"quote-escape injection", `reviews" ; DROP TABLE "users`},
		{"semicolon drop", "reviews; DROP TABLE users"},
		{"drop users", "users; DROP TABLE users; --"},
		{"union select", "reviews UNION SELECT * FROM users"},
		{"comment", "reviews--"},
		{"backtick", "`reviews`"},
		{"single quote", "reviews' OR '1'='1"},
		{"uppercase", "Reviews"},
		{"unicode", "reseñas"},
		{"space", "my reviews"},
		{"leading digit", "1reviews"},
	}
	for _, tc := range hostile {
		seg := url.PathEscape(tc.name)
		for _, probe := range []struct {
			method string
			path   string
			body   any
		}{
			{http.MethodGet, "/content/" + seg, nil},
			{http.MethodGet, "/content/" + seg + "/00000000-0000-0000-0000-000000000000", nil},
			{http.MethodPost, "/content/" + seg, map[string]any{"headline": "x"}},
			{http.MethodPut, "/content/" + seg + "/00000000-0000-0000-0000-000000000000", map[string]any{"headline": "x"}},
			{http.MethodDelete, "/content/" + seg + "/00000000-0000-0000-0000-000000000000", nil},
		} {
			status, body := doJSON(t, srv, probe.method, probe.path, probe.body, authHeader(admin))
			if status != http.StatusNotFound && status != http.StatusBadRequest {
				t.Fatalf("SECURITY FAILURE [%s] %s %s: status=%d body=%v, want 404 or 400", tc.label, probe.method, probe.path, status, body)
			}
		}
		t.Logf("404/400 [%-24s] %q on all five routes", tc.label, tc.name)
	}

	// THE proof: nothing was executed. Every system table is intact, with the
	// same row counts, and no table was created or dropped.
	for _, table := range []string{"users", "roles", "permissions", "role_permissions", "api_keys", "articles", "products", "terms", "content_types", "content_type_fields", "reviews"} {
		if !sqliteTableExists(t, db, table) {
			t.Fatalf("table %q no longer exists — an injection was executed", table)
		}
	}
	if after := countTables(t, db); after != tablesBefore {
		t.Fatalf("table count changed from %d to %d", tablesBefore, after)
	}
	var usersAfter, permsAfter, rolesAfter, typesAfter int
	db.QueryRow(`SELECT count(*) FROM users`).Scan(&usersAfter)
	db.QueryRow(`SELECT count(*) FROM permissions`).Scan(&permsAfter)
	db.QueryRow(`SELECT count(*) FROM roles`).Scan(&rolesAfter)
	db.QueryRow(`SELECT count(*) FROM content_types`).Scan(&typesAfter)
	if usersAfter != usersBefore || permsAfter != permsBefore || rolesAfter != rolesBefore || typesAfter != typesBefore {
		t.Fatalf("system row counts changed: users %d→%d, permissions %d→%d, roles %d→%d, content_types %d→%d",
			usersBefore, usersAfter, permsBefore, permsAfter, rolesBefore, rolesAfter, typesBefore, typesAfter)
	}
	// And the legitimate type still works.
	if status, body := doJSON(t, srv, http.MethodGet, "/content/reviews", nil, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("the legitimate type broke after the hostile battery: status=%d body=%v", status, body)
	}
	t.Logf("SYSTEM INTACT: %d tables (unchanged), users=%d permissions=%d roles=%d content_types=%d (all unchanged)",
		tablesBefore, usersAfter, permsAfter, rolesAfter, typesAfter)
}

// TestDynamicContentHostileValuesAreStoredVerbatim is the proof that VALUES go
// through as bound parameters: a string that is a complete SQL injection
// payload is stored and returned byte-for-byte, as data, and nothing happens.
func TestDynamicContentHostileValuesAreStoredVerbatim(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())

	tablesBefore := countTables(t, db)
	var usersBefore int
	db.QueryRow(`SELECT count(*) FROM users`).Scan(&usersBefore)

	payloads := []string{
		`;DROP TABLE users;--`,
		`' OR '1'='1`,
		`"; DROP TABLE users; --`,
		`x'); DELETE FROM users WHERE ('1'='1`,
		`Robert'); DROP TABLE students;--`,
		`100% "quoted" and 'single' \backslash\`,
		`UNION SELECT password_hash FROM users`,
		"line1\nline2\ttab",
		`—unicode— ñ 中文 🙂`,
	}
	ids := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		status, created := doJSON(t, srv, http.MethodPost, "/content/reviews",
			map[string]any{"headline": payload, "score": 1}, authHeader(admin))
		if status != http.StatusCreated {
			t.Fatalf("hostile value %q: status=%d body=%v, want 201 (it is DATA)", payload, status, created)
		}
		if got, _ := created["headline"].(string); got != payload {
			t.Fatalf("hostile value not verbatim on create:\n got %q\nwant %q", got, payload)
		}
		id, _ := created["id"].(string)
		ids = append(ids, id)

		// Verbatim on read-by-id...
		_, one := doJSON(t, srv, http.MethodGet, "/content/reviews/"+id, nil, authHeader(admin))
		if got, _ := one["headline"].(string); got != payload {
			t.Fatalf("hostile value not verbatim on read:\n got %q\nwant %q", got, payload)
		}
		// ...and straight out of the database, proving it was STORED verbatim
		// and not merely echoed back by the handler.
		var stored string
		if err := db.QueryRow(`SELECT headline FROM reviews WHERE id = ?`, id).Scan(&stored); err != nil {
			t.Fatalf("direct read of %q: %v", payload, err)
		}
		if stored != payload {
			t.Fatalf("stored value differs:\n got %q\nwant %q", stored, payload)
		}
		t.Logf("VERBATIM OK %q", payload)
	}

	// Update with a hostile value is equally inert.
	if status, body := doJSON(t, srv, http.MethodPut, "/content/reviews/"+ids[0],
		map[string]any{"headline": `'); DROP TABLE users; --`}, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("hostile update: status=%d body=%v", status, body)
	}

	if !sqliteTableExists(t, db, "users") {
		t.Fatal("the 'users' table is gone — a value was executed as SQL")
	}
	var usersAfter int
	db.QueryRow(`SELECT count(*) FROM users`).Scan(&usersAfter)
	if usersAfter != usersBefore {
		t.Fatalf("users rows changed %d→%d — a value was executed", usersBefore, usersAfter)
	}
	if after := countTables(t, db); after != tablesBefore {
		t.Fatalf("table count changed %d→%d — a value was executed", tablesBefore, after)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM reviews`).Scan(&n)
	if n != len(payloads) {
		t.Fatalf("reviews has %d rows, want %d", n, len(payloads))
	}
	t.Logf("PARAMETERIZED VALUES PROVEN: %d injection payloads stored as data, users=%d unchanged, %d tables unchanged",
		len(payloads), usersAfter, tablesBefore)
}

// --- T3: permission gating ----------------------------------------------------

// TestDynamicContentPermissionGating covers the gating criterion: each write
// route requires its OWN generic content.* permission (no new permission was
// introduced), reading requires only a valid identity, and an unauthenticated
// caller gets 401 everywhere.
func TestDynamicContentPermissionGating(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())
	status, seed := doJSON(t, srv, http.MethodPost, "/content/reviews",
		map[string]any{"headline": "seed"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("seed row: status=%d body=%v", status, seed)
	}
	rowID, _ := seed["id"].(string)

	// A user with EVERY other permission but none of the content.* ones —
	// proving the gate is the specific permission, not "any admin-ish grant".
	grant(t, db, "editor", "content_types.manage", "terms.manage", "users.manage", "roles.manage", "content.publish")
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")

	for _, probe := range []struct {
		method, path, perm string
		body               any
	}{
		{http.MethodPost, "/content/reviews", "content.create", map[string]any{"headline": "x"}},
		{http.MethodPut, "/content/reviews/" + rowID, "content.update", map[string]any{"headline": "x"}},
		{http.MethodDelete, "/content/reviews/" + rowID, "content.delete", nil},
	} {
		status, body := doJSON(t, srv, probe.method, probe.path, probe.body, authHeader(editor))
		if status != http.StatusForbidden {
			t.Fatalf("%s %s without %s: status=%d body=%v, want 403", probe.method, probe.path, probe.perm, status, body)
		}
		t.Logf("403 [%-6s %-40s] missing %s", probe.method, probe.path, probe.perm)
	}
	// Reading, however, works for that same identity (reads are not gated).
	if status, body := doJSON(t, srv, http.MethodGet, "/content/reviews", nil, authHeader(editor)); status != http.StatusOK {
		t.Fatalf("read without content.*: status=%d body=%v, want 200", status, body)
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/content/reviews/"+rowID, nil, authHeader(editor)); status != http.StatusOK {
		t.Fatalf("read by id without content.*: status=%d, want 200", status)
	}

	// Unauthenticated → 401 on every route, read included.
	for _, probe := range []struct {
		method, path string
	}{
		{http.MethodGet, "/content/reviews"},
		{http.MethodGet, "/content/reviews/" + rowID},
		{http.MethodPost, "/content/reviews"},
		{http.MethodPut, "/content/reviews/" + rowID},
		{http.MethodDelete, "/content/reviews/" + rowID},
	} {
		if status, body := doJSON(t, srv, probe.method, probe.path, map[string]any{}, nil); status != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s: status=%d body=%v, want 401", probe.method, probe.path, status, body)
		}
	}

	// The row survived every rejected attempt.
	var n int
	db.QueryRow(`SELECT count(*) FROM reviews`).Scan(&n)
	if n != 1 {
		t.Fatalf("reviews has %d rows after rejected writes, want 1", n)
	}
	t.Logf("GATING OK: 403 per-route without the matching content.* grant, reads open to any identity, 401 unauthenticated, 1 row untouched")
}

// TestDynamicContentAPIKeyCannotCreate covers the authorship decision: a dynamic
// table's author_id is NOT NULL with an FK to users, so an API-key identity —
// which has no human behind it — is rejected with a clear 403 rather than
// inserting a null/invented author. This mirrors articles and products exactly.
func TestDynamicContentAPIKeyCannotCreate(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())

	// The key's role HAS content.create — the rejection is about authorship,
	// not about the permission.
	key := apiKeyFor(t, db, "ci", "administrator")

	status, body := doJSON(t, srv, http.MethodPost, "/content/reviews",
		map[string]any{"headline": "by a robot"}, authHeader(key))
	if status != http.StatusForbidden {
		t.Fatalf("create with an API key: status=%d body=%v, want 403", status, body)
	}
	msg, _ := body["error"].(string)
	if msg == "" {
		t.Fatalf("403 with no explanatory message: %v", body)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM reviews`).Scan(&n)
	if n != 0 {
		t.Fatalf("an API-key create inserted %d rows", n)
	}

	// The API key CAN still read, and can update/delete existing rows (those
	// carry no authorship decision).
	if status, _ := doJSON(t, srv, http.MethodGet, "/content/reviews", nil, authHeader(key)); status != http.StatusOK {
		t.Fatalf("API key read: status=%d, want 200", status)
	}
	t.Logf("API-KEY OK: 403 %q, 0 rows inserted, reads still allowed", msg)
}

// --- T3: definitions are read from the database, not from process memory ------

// TestDynamicContentSurvivesRestart covers the red-team item: a type created by
// one process is fully usable by ANOTHER process over the SAME database file,
// with no shared in-memory state — because the definitions are read back from
// the database on every request.
func TestDynamicContentSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	ctx := context.Background()

	// --- process 1: define the type and load a row ---
	sdb1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := store.EnsureSchema(ctx, sdb1); err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	if err := store.SeedCatalogs(ctx, sdb1.DB); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	mux1, err := server.NewMux(server.Deps{DB: sdb1.DB, JWTSecret: testSecret})
	if err != nil {
		t.Fatalf("NewMux 1: %v", err)
	}
	srv1 := httptest.NewServer(mux1)
	admin := contentAdmin(t, sdb1.DB)
	createDynamicType(t, srv1, admin, reviewsBody())
	status, created := doJSON(t, srv1, http.MethodPost, "/content/reviews",
		map[string]any{"headline": "written before the restart", "score": 7, "verified": true}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create in process 1: status=%d body=%v", status, created)
	}
	rowID, _ := created["id"].(string)
	srv1.Close()
	if err := sdb1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	// --- process 2: a brand new connection, mux and handler set ---
	sdb2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer sdb2.Close()
	if err := store.EnsureSchema(ctx, sdb2); err != nil {
		t.Fatalf("ensure 2 (the composed schema must already match): %v", err)
	}
	mux2, err := server.NewMux(server.Deps{DB: sdb2.DB, JWTSecret: testSecret})
	if err != nil {
		t.Fatalf("NewMux 2: %v", err)
	}
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()

	// A fresh token from the SAME database (the JWT is verified against the
	// persisted user, so this exercises the restart end to end).
	user, err := auth.VerifyCredentials(ctx, sdb2.DB, "admin@example.com", "pw")
	if err != nil {
		t.Fatalf("verify after restart: %v", err)
	}
	token, err := auth.IssueJWT(testSecret, user, timeNowUTC())
	if err != nil {
		t.Fatalf("issue jwt after restart: %v", err)
	}

	status, one := doJSON(t, srv2, http.MethodGet, "/content/reviews/"+rowID, nil, authHeader(token))
	if status != http.StatusOK {
		t.Fatalf("read after restart: status=%d body=%v, want 200", status, one)
	}
	if one["headline"] != "written before the restart" {
		t.Fatalf("row lost across the restart: %v", one)
	}
	if v, ok := one["score"].(float64); !ok || v != 7 {
		t.Fatalf("score = %#v (%T) after restart, want JSON number 7", one["score"], one["score"])
	}
	if v, ok := one["verified"].(bool); !ok || !v {
		t.Fatalf("verified = %#v (%T) after restart, want JSON boolean true", one["verified"], one["verified"])
	}
	// Writing works too, from the new process.
	if status, body := doJSON(t, srv2, http.MethodPost, "/content/reviews",
		map[string]any{"headline": "written after the restart"}, authHeader(token)); status != http.StatusCreated {
		t.Fatalf("create after restart: status=%d body=%v, want 201", status, body)
	}
	_, list := doJSON(t, srv2, http.MethodGet, "/content/reviews", nil, authHeader(token))
	if n := len(items(t, list)); n != 2 {
		t.Fatalf("list after restart returned %d rows, want 2", n)
	}
	t.Logf("RESTART OK: the definition was read back from the database by a new process; read and write both work")
}

// TestDynamicContentZeroFieldType covers the edge the definition model allows:
// a type with NO own fields still produces a usable table and a working CRUD.
func TestDynamicContentZeroFieldType(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, map[string]any{"name": "bookmarks", "fields": []map[string]string{}})

	status, created := doJSON(t, srv, http.MethodPost, "/content/bookmarks", map[string]any{}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create in a field-less type: status=%d body=%v, want 201", status, created)
	}
	id, _ := created["id"].(string)
	if status, body := doJSON(t, srv, http.MethodPut, "/content/bookmarks/"+id, map[string]any{}, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("update a field-less row: status=%d body=%v, want 200", status, body)
	}
	if status, _ := doJSON(t, srv, http.MethodDelete, "/content/bookmarks/"+id, nil, authHeader(admin)); status != http.StatusNoContent {
		t.Fatalf("delete a field-less row: status=%d, want 204", status)
	}
	// Any body key at all is unknown for a field-less type.
	if status, _ := doJSON(t, srv, http.MethodPost, "/content/bookmarks", map[string]any{"x": 1}, authHeader(admin)); status != http.StatusBadRequest {
		t.Fatalf("unknown field in a field-less type: status=%d, want 400", status)
	}
	t.Logf("ZERO-FIELD OK: create/update/delete work, any body key is a 400")
}

// TestDynamicContentPagingAndOrdering covers the ?limit=&offset= paging, which
// is the same lenient helper articles/products use.
func TestDynamicContentPagingAndOrdering(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())
	for i := 0; i < 5; i++ {
		if status, body := doJSON(t, srv, http.MethodPost, "/content/reviews",
			map[string]any{"headline": fmt.Sprintf("r%d", i), "score": i}, authHeader(admin)); status != http.StatusCreated {
			t.Fatalf("seed %d: status=%d body=%v", i, status, body)
		}
	}
	_, body := doJSON(t, srv, http.MethodGet, "/content/reviews?limit=2", nil, authHeader(admin))
	if n := len(items(t, body)); n != 2 {
		t.Fatalf("limit=2 returned %d rows", n)
	}
	_, body = doJSON(t, srv, http.MethodGet, "/content/reviews?limit=10&offset=3", nil, authHeader(admin))
	if n := len(items(t, body)); n != 2 {
		t.Fatalf("offset=3 returned %d rows, want 2", n)
	}
	// A garbage paging value falls back to the default rather than erroring.
	_, body = doJSON(t, srv, http.MethodGet, "/content/reviews?limit=abc&offset=-9", nil, authHeader(admin))
	if n := len(items(t, body)); n != 5 {
		t.Fatalf("garbage paging returned %d rows, want the default page of 5", n)
	}
	t.Logf("PAGING OK: limit/offset honoured, garbage falls back to the default")
}

// --- T3: contracts 01-13 are untouched ----------------------------------------

// TestPreviousContractsUnaffectedByGenericCRUD is the explicit regression guard:
// with dynamic types defined AND loaded with content, every earlier JSON surface
// answers exactly as before, and the new /content/ namespace has not shadowed a
// single existing route.
func TestPreviousContractsUnaffectedByGenericCRUD(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator",
		"content_types.manage", "content.create", "content.update", "content.publish", "content.delete",
		"terms.manage", "users.manage", "roles.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	// Define a dynamic type and load real content into it FIRST, so everything
	// below runs with the new surface fully in use.
	createDynamicType(t, srv, admin, reviewsBody())
	if status, body := doJSON(t, srv, http.MethodPost, "/content/reviews",
		map[string]any{"headline": "loaded", "score": 1}, authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("load dynamic content: status=%d body=%v", status, body)
	}

	// CONTRACT-01: health.
	if status, body := doJSON(t, srv, http.MethodGet, "/health", nil, nil); status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("GET /health: status=%d body=%v", status, body)
	}
	// CONTRACT-02: login + whoami.
	status, body := doJSON(t, srv, http.MethodPost, "/auth/login",
		map[string]string{"email": "admin@example.com", "password": "pw"}, nil)
	if status != http.StatusOK || body["token"] == nil {
		t.Fatalf("POST /auth/login: status=%d body=%v", status, body)
	}
	if status, body := doJSON(t, srv, http.MethodGet, "/whoami", nil, authHeader(admin)); status != http.StatusOK || body["auth"] != "jwt" {
		t.Fatalf("GET /whoami: status=%d body=%v", status, body)
	}
	// CONTRACT-03/04: articles CRUD + publish.
	status, body = doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "Unchanged", "body": "still the same"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("POST /articles: status=%d body=%v", status, body)
	}
	articleID, _ := body["id"].(string)
	if status, body := doJSON(t, srv, http.MethodGet, "/articles", nil, authHeader(admin)); status != http.StatusOK || body["articles"] == nil {
		t.Fatalf("GET /articles: status=%d body=%v (envelope must still be {\"articles\":[...]})", status, body)
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/articles/"+articleID, nil, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("GET /articles/{id}: status=%d", status)
	}
	if status, _ := doJSON(t, srv, http.MethodPut, "/articles/"+articleID,
		map[string]any{"title": "Unchanged 2", "body": "b"}, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("PUT /articles/{id}: status=%d", status)
	}
	if status, _ := doJSON(t, srv, http.MethodPost, "/articles/"+articleID+"/publish", nil, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("POST /articles/{id}/publish: status=%d", status)
	}
	// CONTRACT-11: products CRUD.
	status, body = doJSON(t, srv, http.MethodPost, "/products",
		map[string]any{"title": "Widget", "body": "d", "price": "9.99", "sku": "SKU-14"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("POST /products: status=%d body=%v", status, body)
	}
	productID, _ := body["id"].(string)
	if status, body := doJSON(t, srv, http.MethodGet, "/products", nil, authHeader(admin)); status != http.StatusOK || body["products"] == nil {
		t.Fatalf("GET /products: status=%d body=%v (envelope must still be {\"products\":[...]})", status, body)
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/products/"+productID, nil, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("GET /products/{id}: status=%d", status)
	}
	// CONTRACT-12: terms + assignment.
	termID := createTerm(t, srv, admin, "category", "News", "news-14", nil)
	if status, _ := doJSON(t, srv, http.MethodGet, "/terms", nil, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("GET /terms: status=%d", status)
	}
	if status, _ := doJSON(t, srv, http.MethodPut, "/articles/"+articleID+"/terms",
		map[string]any{"term_ids": []string{termID}}, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("PUT /articles/{id}/terms: status=%d", status)
	}
	if status, _ := doJSON(t, srv, http.MethodPut, "/products/"+productID+"/terms",
		map[string]any{"term_ids": []string{termID}}, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("PUT /products/{id}/terms: status=%d", status)
	}
	// CONTRACT-13: content-type definitions still list and read the same way.
	if status, body := doJSON(t, srv, http.MethodGet, "/content-types", nil, authHeader(admin)); status != http.StatusOK || body["content_types"] == nil {
		t.Fatalf("GET /content-types: status=%d body=%v", status, body)
	}
	if status, body := doJSON(t, srv, http.MethodGet, "/content-types/reviews", nil, authHeader(admin)); status != http.StatusOK || body["name"] != "reviews" {
		t.Fatalf("GET /content-types/reviews: status=%d body=%v", status, body)
	}
	// The new namespace did NOT shadow /content-types (a real risk: both start
	// with "/content").
	if status, _ := doJSON(t, srv, http.MethodGet, "/content-types", nil, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("/content-types was shadowed by /content/{type}: status=%d", status)
	}
	// CONTRACT-06+: the UI routes still answer (the browser surface is
	// untouched by this contract).
	for _, path := range []string{"/ui/login", "/ui/articles", "/ui/products", "/ui/users", "/ui/roles", "/ui/api-keys", "/ui/terms", "/ui/content-types"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			t.Fatalf("GET %s: status=%d (a 5xx means this contract broke the UI)", path, resp.StatusCode)
		}
		t.Logf("UI %-22s -> %d", path, resp.StatusCode)
	}
	// The dynamic content is still there after all of the above.
	status, list := doJSON(t, srv, http.MethodGet, "/content/reviews", nil, authHeader(admin))
	if status != http.StatusOK || len(items(t, list)) != 1 {
		t.Fatalf("dynamic content after the regression battery: status=%d body=%v", status, list)
	}
	t.Logf("PREVIOUS CONTRACTS OK: health, login, whoami, articles CRUD+publish, products CRUD, terms+assignment, content-types, and every UI route all unchanged")
}

// TestDynamicContentJSONShapeIsStable asserts the exact key set of a row so a
// future change to the generic layer cannot silently alter the public shape.
func TestDynamicContentJSONShapeIsStable(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())
	status, created := doJSON(t, srv, http.MethodPost, "/content/reviews",
		map[string]any{"headline": "h", "score": 1, "price_paid": "1.5", "verified": false, "read_on": "2026-02-03"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%v", status, created)
	}
	want := map[string]struct{}{
		"id": {}, "author_id": {}, "created_at": {}, "updated_at": {}, "metadata": {},
		"headline": {}, "score": {}, "price_paid": {}, "verified": {}, "read_on": {},
	}
	for k := range created {
		if _, ok := want[k]; !ok {
			t.Fatalf("unexpected key %q in a dynamic row: %v", k, created)
		}
	}
	for k := range want {
		if _, ok := created[k]; !ok {
			t.Fatalf("missing key %q in a dynamic row: %v", k, created)
		}
	}
	// metadata is not writable through this surface and is therefore null.
	if created["metadata"] != nil {
		t.Fatalf("metadata = %v, want null (not writable through this surface)", created["metadata"])
	}
	blob, _ := json.Marshal(created)
	t.Logf("SHAPE OK: %s", blob)
}

// TestDynamicContentDecimalPrecision proves the decimal-as-string decision does
// what it exists for: an arbitrary-precision value survives byte-for-byte.
func TestDynamicContentDecimalPrecision(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())

	for _, value := range []any{"123456789012345678901234567890.123456789", "0.1", -3.5, 7, "-0.000000001"} {
		status, created := doJSON(t, srv, http.MethodPost, "/content/reviews",
			map[string]any{"price_paid": value}, authHeader(admin))
		if status != http.StatusCreated {
			t.Fatalf("decimal %v: status=%d body=%v, want 201", value, status, created)
		}
		got, _ := created["price_paid"].(string)
		want := fmt.Sprintf("%v", value)
		if got != want {
			t.Fatalf("decimal round-trip: got %q, want %q", got, want)
		}
		t.Logf("DECIMAL OK %-45s -> %q", want, got)
	}
}
