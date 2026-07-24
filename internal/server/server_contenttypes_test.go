package server_test

// CONTRACT-13 T3 acceptance tests: the JSON API that registers a DYNAMIC
// content type and creates its real table at runtime. Reuses the shared helpers
// (openAuthMux, doJSON, grant, jwtFor, authHeader) from the sibling test files
// — same package.
//
// The evidence these tests produce is deliberately NOT the HTTP response: after
// a successful POST they query the database catalog DIRECTLY to prove the real
// table exists with the expected columns, and after a rejected POST they prove
// that nothing at all was persisted or created.

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// tableColumns returns the column names of a table, in declaration order,
// straight from SQLite's own catalog via PRAGMA table_info. The table name is
// interpolated because PRAGMA takes no parameters; it is a test-controlled
// constant, never user input.
func tableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		t.Fatalf("PRAGMA table_info(%q): %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notNull, pk int
			dflt        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}
	return out
}

// sqliteTableExists reports whether a physical table exists.
func sqliteTableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("sqlite_master lookup %q: %v", name, err)
	}
	return n > 0
}

// reviewsBody is the request body used across these tests: a dynamic type
// exercising all five allowed field types.
func reviewsBody() map[string]any {
	return map[string]any{
		"name": "reviews",
		"fields": []map[string]string{
			{"name": "headline", "type": "text"},
			{"name": "score", "type": "integer"},
			{"name": "price_paid", "type": "decimal"},
			{"name": "verified", "type": "boolean"},
			{"name": "read_on", "type": "date"},
		},
	}
}

// TestContentTypesManagePermissionSeeded confirms the ONE new permission of
// this contract reaches the database through the existing idempotent,
// data-driven seed — no seed-logic change was required.
func TestContentTypesManagePermissionSeeded(t *testing.T) {
	db, _, cleanup := openAuthMux(t)
	defer cleanup()

	var id string
	if err := db.QueryRow(`SELECT id FROM permissions WHERE name = 'content_types.manage'`).Scan(&id); err != nil {
		t.Fatalf("content_types.manage not seeded: %v", err)
	}
	t.Logf("content_types.manage seeded (id=%s)", id)
}

// TestCreateContentTypeCreatesRealTable is the core T3 criterion: a real
// content type created through the REAL HTTP API, then confirmed with a direct
// catalog query — including the columns ContentType() injects.
func TestCreateContentTypeCreatesRealTable(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	status, body := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create content type: status=%d body=%v, want 201", status, body)
	}
	if name, _ := body["name"].(string); name != "reviews" {
		t.Fatalf("response name=%v, want reviews", body["name"])
	}

	// The definition is persisted.
	var typeID string
	if err := db.QueryRow(`SELECT id FROM content_types WHERE name = 'reviews'`).Scan(&typeID); err != nil {
		t.Fatalf("definition not persisted: %v", err)
	}
	var fieldCount int
	if err := db.QueryRow(`SELECT count(*) FROM content_type_fields WHERE content_type_id = ?`, typeID).Scan(&fieldCount); err != nil {
		t.Fatalf("count fields: %v", err)
	}
	if fieldCount != 5 {
		t.Fatalf("persisted %d fields, want 5", fieldCount)
	}

	// THE evidence: the real table, with the injected columns, from the catalog.
	if !sqliteTableExists(t, db, "reviews") {
		t.Fatal("the real 'reviews' table was not created")
	}
	got := tableColumns(t, db, "reviews")
	want := []string{"id", "author_id", "headline", "score", "price_paid", "verified", "read_on", "created_at", "updated_at", "metadata"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("columns of 'reviews' = %v, want %v", got, want)
	}

	// And it behaves like a real content table: an insert referencing a real
	// author works, and the FK to users is enforced.
	var authorID string
	if err := db.QueryRow(`SELECT id FROM users WHERE email = 'admin@example.com'`).Scan(&authorID); err != nil {
		t.Fatalf("lookup author: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO reviews (author_id, headline, score, price_paid, verified, read_on) VALUES (?, ?, ?, ?, ?, ?)`,
		authorID, "Great book", 5, "19.99", 1, "2026-07-24"); err != nil {
		t.Fatalf("insert into the dynamic table: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM reviews`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("dynamic table row count=%d err=%v, want 1", n, err)
	}
	t.Logf("T3 OK: POST /content-types created the real table; PRAGMA table_info(reviews)=%v; 1 row inserted", got)
}

// TestCreateContentTypeWithoutPermissionIs403 covers the gating criterion: the
// dedicated permission is required, and a caller without it changes nothing.
func TestCreateContentTypeWithoutPermissionIs403(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	// editor has every OTHER permission but not content_types.manage — proving
	// the gate is the new dedicated permission, not "any admin-ish grant".
	grant(t, db, "editor", "content.create", "content.update", "content.publish", "content.delete", "users.manage", "roles.manage", "terms.manage")
	editor := jwtFor(t, db, "ed@example.com", "pw", "editor")

	status, body := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(editor))
	if status != http.StatusForbidden {
		t.Fatalf("without content_types.manage: status=%d body=%v, want 403", status, body)
	}
	if sqliteTableExists(t, db, "reviews") {
		t.Fatal("a forbidden request created the table")
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM content_types`).Scan(&n)
	if n != 0 {
		t.Fatalf("a forbidden request persisted %d definitions", n)
	}

	// Unauthenticated → 401.
	status, _ = doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status=%d, want 401", status)
	}
	t.Logf("GATING OK: 403 without content_types.manage (with every other permission granted), 401 unauthenticated, nothing created")
}

// TestCreateContentTypeHostileNamesAre400 runs the T1 battery through the REAL
// HTTP endpoint, and — the part that matters — proves each rejection left
// NOTHING behind: no definition row, no field row, no table.
func TestCreateContentTypeHostileNamesAre400(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	tablesBefore := countTables(t, db)

	cases := []struct {
		label string
		name  string
	}{
		{"empty", ""},
		{"double quote", `re"views`},
		{"quote-escape injection", `x" ; DROP TABLE "users`},
		{"semicolon", "reviews; DROP TABLE users"},
		{"spaces", "my reviews"},
		{"uppercase", "MiTipo"},
		{"unicode", "reseñas"},
		{"only digits", "123"},
		{"starts with digit", "1abc"},
		{"too long", strings.Repeat("a", 33)},
		{"reserved code table", "users"},
		{"reserved injected column", "id"},
		{"compat internal", "__compat_schema"},
		{"registry table", "content_types"},
	}
	for _, tc := range cases {
		status, body := doJSON(t, srv, http.MethodPost, "/content-types",
			map[string]any{"name": tc.name, "fields": []map[string]string{{"name": "x", "type": "text"}}},
			authHeader(admin))
		if status != http.StatusBadRequest {
			t.Fatalf("REJECT FAILED [%s] %q: status=%d body=%v, want 400", tc.label, tc.name, status, body)
		}
		t.Logf("400 [%-24s] %-34q -> %v", tc.label, tc.name, body["error"])
	}

	// A hostile FIELD name must be rejected too, and leave nothing behind.
	for _, field := range []string{`sc"ore`, "score; DROP TABLE users", "Score", "metadata", "1st"} {
		status, body := doJSON(t, srv, http.MethodPost, "/content-types",
			map[string]any{"name": "reviews", "fields": []map[string]string{{"name": field, "type": "text"}}},
			authHeader(admin))
		if status != http.StatusBadRequest {
			t.Fatalf("hostile field %q: status=%d body=%v, want 400", field, status, body)
		}
		t.Logf("400 [hostile field         ] %-34q -> %v", field, body["error"])
	}
	// An unsupported field type (v1 excludes json/vector) must be a 400 as well.
	for _, ft := range []string{"json", "vector", "bigint", ""} {
		status, body := doJSON(t, srv, http.MethodPost, "/content-types",
			map[string]any{"name": "reviews", "fields": []map[string]string{{"name": "x", "type": ft}}},
			authHeader(admin))
		if status != http.StatusBadRequest {
			t.Fatalf("field type %q: status=%d body=%v, want 400", ft, status, body)
		}
		t.Logf("400 [unsupported field type] %-34q -> %v", ft, body["error"])
	}

	// NOTHING was persisted and NOTHING was created by any of the above.
	var types, fields int
	db.QueryRow(`SELECT count(*) FROM content_types`).Scan(&types)
	db.QueryRow(`SELECT count(*) FROM content_type_fields`).Scan(&fields)
	if types != 0 || fields != 0 {
		t.Fatalf("PARTIAL STATE after rejected requests: content_types=%d, content_type_fields=%d, want 0/0", types, fields)
	}
	if after := countTables(t, db); after != tablesBefore {
		t.Fatalf("table count changed from %d to %d during rejected requests", tablesBefore, after)
	}
	// The injection attempts must not have harmed the tables they targeted.
	if !sqliteTableExists(t, db, "users") {
		t.Fatal("the 'users' table is gone — an injection attempt succeeded")
	}
	t.Logf("NO EFFECT OK: 0 definitions, 0 fields, table count unchanged at %d, 'users' intact", tablesBefore)
}

// countTables returns the number of physical tables in the database.
func countTables(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&n); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	return n
}

// TestCreateContentTypeDuplicateIs400 covers the red-team concurrency item at
// the HTTP level: a duplicate name is a clean 400 (never a 500 leaking SQL) and
// no second definition survives. The guarantee behind it is the schema-level
// UNIQUE(name), which cannot lose a race.
func TestCreateContentTypeDuplicateIs400(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("first create: status=%d body=%v", status, body)
	}
	status, body := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("duplicate create: status=%d body=%v, want 400 (never 500)", status, body)
	}
	if msg, _ := body["error"].(string); msg == "" || strings.Contains(strings.ToUpper(msg), "SQL") {
		t.Fatalf("duplicate create: want a clean message with no SQL, got %v", body)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM content_types`).Scan(&n)
	if n != 1 {
		t.Fatalf("content_types has %d rows after a duplicate attempt, want 1", n)
	}
	t.Logf("DUPLICATE OK: 400 %q, still exactly 1 definition", body["error"])
}

// TestListAndGetContentTypes covers the two read routes: they need only a valid
// identity (reading is not permission-gated), and an unknown name is a 404.
func TestListAndGetContentTypes(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")
	reader := jwtFor(t, db, "reader@example.com", "pw", "contributor") // no grants

	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%v", status, body)
	}

	// List, as a caller with NO permissions at all.
	status, body := doJSON(t, srv, http.MethodGet, "/content-types", nil, authHeader(reader))
	if status != http.StatusOK {
		t.Fatalf("list: status=%d body=%v, want 200", status, body)
	}
	list, _ := body["content_types"].([]any)
	if len(list) != 1 {
		t.Fatalf("list returned %d content types, want 1 (%v)", len(list), body)
	}

	// Detail, with its fields in declared order.
	status, body = doJSON(t, srv, http.MethodGet, "/content-types/reviews", nil, authHeader(reader))
	if status != http.StatusOK {
		t.Fatalf("get: status=%d body=%v, want 200", status, body)
	}
	fields, _ := body["fields"].([]any)
	if len(fields) != 5 {
		t.Fatalf("get returned %d fields, want 5 (%v)", len(fields), body)
	}
	var names []string
	for _, f := range fields {
		m, _ := f.(map[string]any)
		names = append(names, fmt.Sprintf("%v:%v", m["name"], m["type"]))
	}
	wantOrder := "headline:text,score:integer,price_paid:decimal,verified:boolean,read_on:date"
	if strings.Join(names, ",") != wantOrder {
		t.Fatalf("field order = %v, want %v", strings.Join(names, ","), wantOrder)
	}

	// Unknown and syntactically impossible names → 404, never 500.
	for _, name := range []string{"nope", "users", "MiTipo"} {
		status, _ := doJSON(t, srv, http.MethodGet, "/content-types/"+name, nil, authHeader(reader))
		if status != http.StatusNotFound {
			t.Fatalf("GET /content-types/%s: status=%d, want 404", name, status)
		}
	}
	// Reading is not permission-gated, but it IS authenticated.
	if status, _ := doJSON(t, srv, http.MethodGet, "/content-types", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: status=%d, want 401", status)
	}
	t.Logf("READ OK: list=1, detail fields in order %q, unknown→404, unauthenticated→401", wantOrder)
}

// TestCreateContentTypeIsCreateOnly confirms the deliberate absence of PUT and
// DELETE (v1 is create-only: compat has no ALTER TABLE and dropping a type was
// never in scope). Go's ServeMux answers 405 for a path registered under other
// methods only.
func TestCreateContentTypeIsCreateOnly(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%v", status, body)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		status, _ := doJSON(t, srv, method, "/content-types/reviews", map[string]any{"name": "reviews"}, authHeader(admin))
		if status == http.StatusOK || status == http.StatusCreated || status == http.StatusNoContent {
			t.Fatalf("%s /content-types/reviews succeeded (status=%d); v1 is create-only", method, status)
		}
		t.Logf("CREATE-ONLY OK: %s /content-types/reviews -> %d", method, status)
	}
	// The type is untouched.
	if !sqliteTableExists(t, db, "reviews") {
		t.Fatal("'reviews' table disappeared")
	}
}

// TestDynamicTypeCoexistsWithCodeContentTypes is the T4 guard at the API level:
// after a dynamic type exists, the code-defined content types keep working
// exactly as before through their own unchanged routes.
func TestDynamicTypeCoexistsWithCodeContentTypes(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create", "content.update", "content.publish", "content.delete", "terms.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create dynamic type: status=%d body=%v", status, body)
	}

	// articles: unchanged contract.
	status, body := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "After Dynamic", "body": "still works"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("POST /articles after a dynamic type exists: status=%d body=%v, want 201", status, body)
	}
	articleID, _ := body["id"].(string)

	// products: unchanged contract.
	status, body = doJSON(t, srv, http.MethodPost, "/products",
		map[string]any{"title": "Widget", "body": "d", "price": "9.99", "sku": "SKU-AFTER-13"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("POST /products after a dynamic type exists: status=%d body=%v, want 201", status, body)
	}

	// terms + assignment: unchanged contract.
	termID := createTerm(t, srv, admin, "category", "News", "news-after-13", nil)
	status, body = doJSON(t, srv, http.MethodPut, "/articles/"+articleID+"/terms",
		map[string]any{"term_ids": []string{termID}}, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("PUT /articles/{id}/terms after a dynamic type exists: status=%d body=%v, want 200", status, body)
	}

	// And the dynamic type is still listed alongside them.
	status, body = doJSON(t, srv, http.MethodGet, "/content-types", nil, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("list content types: status=%d body=%v", status, body)
	}
	t.Logf("COEXISTENCE OK: articles, products and terms all behave unchanged with a dynamic type present")
}
