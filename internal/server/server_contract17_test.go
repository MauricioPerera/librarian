package server_test

// CONTRACT-17 acceptance tests: dynamic content types live in their OWN table
// namespace (schema.DynamicTablePrefix), while every PUBLIC surface keeps using
// the bare name the admin chose.
//
// These tests exist to catch the two SYMMETRIC bugs a prefix can introduce:
//
//	A) a query built with the bare name  -> "the type exists but its content
//	   never appears" (a table that does not exist, or worse, a CODE table);
//	B) the prefixed name leaking upwards -> the admin sees "cpt_eventos" in a
//	   URL, a JSON payload or the sidebar, and CONTRACT-13/14/15's public
//	   contract silently changed.
//
// So every assertion here is made twice: the real table is checked directly in
// SQLite's catalog, and every byte the server sends back is scanned for the
// prefix.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
)

// rawJSON performs a request and returns the status and the RAW response body.
// doJSON decodes into a map, which would hide a leaked identifier that is not a
// value; the point here is to scan the literal bytes the client receives.
func rawJSON(t *testing.T, srv *httptest.Server, method, path string, body any, headers map[string]string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// assertNoPrefix fails if a response body ever mentions the internal prefix.
func assertNoPrefix(t *testing.T, label, body string) {
	t.Helper()
	if strings.Contains(body, schema.DynamicTablePrefix) {
		t.Fatalf("LEAK [%s]: the response exposes the internal prefix %q: %.400q", label, schema.DynamicTablePrefix, body)
	}
}

// TestContract17JSONRoundTripUsesThePublicName is the T3 criterion on the JSON
// surface: the WHOLE life cycle of a dynamic type and one of its rows, driven
// exclusively with the bare public name, while the data actually lands in the
// PREFIXED table and nothing but the bare name is ever sent back.
func TestContract17JSONRoundTripUsesThePublicName(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create", "content.update", "content.delete")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	// 1. Create the type with the PUBLIC name.
	status, body := rawJSON(t, srv, http.MethodPost, "/content-types", map[string]any{
		"name": "eventos",
		"fields": []map[string]string{
			{"name": "titulo", "type": "text"},
			{"name": "asistentes", "type": "integer"},
		},
	}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create type: status=%d body=%s", status, body)
	}
	if !strings.Contains(body, `"eventos"`) {
		t.Fatalf("the definition response does not carry the public name: %s", body)
	}
	assertNoPrefix(t, "POST /content-types", body)

	// 2. The REAL table is the prefixed one, and the bare name is NOT a table.
	if !sqliteTableExists(t, db, "cpt_eventos") {
		t.Fatal("the real table 'cpt_eventos' was not created")
	}
	if sqliteTableExists(t, db, "eventos") {
		t.Fatal("an UNPREFIXED table 'eventos' exists")
	}
	// The registry stores the PUBLIC name — the prefix is derived, never stored.
	var persisted string
	if err := db.QueryRow(`SELECT name FROM content_types`).Scan(&persisted); err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if persisted != "eventos" {
		t.Fatalf("content_types.name = %q, want the public name %q", persisted, "eventos")
	}

	// 3. The definitions API keeps returning the public name.
	for _, path := range []string{"/content-types", "/content-types/eventos"} {
		status, body := rawJSON(t, srv, http.MethodGet, path, nil, authHeader(admin))
		if status != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, status, body)
		}
		if !strings.Contains(body, `"eventos"`) {
			t.Fatalf("GET %s does not mention the public name: %s", path, body)
		}
		assertNoPrefix(t, "GET "+path, body)
	}
	// And the PREFIXED name is not a public identifier at all.
	if status, _ := rawJSON(t, srv, http.MethodGet, "/content-types/cpt_eventos", nil, authHeader(admin)); status != http.StatusNotFound {
		t.Fatalf("GET /content-types/cpt_eventos: status=%d, want 404", status)
	}
	if status, _ := rawJSON(t, srv, http.MethodGet, "/content/cpt_eventos", nil, authHeader(admin)); status != http.StatusNotFound {
		t.Fatalf("GET /content/cpt_eventos: status=%d, want 404", status)
	}

	// 4. CREATE a row through /content/{public name}.
	status, body = rawJSON(t, srv, http.MethodPost, "/content/eventos",
		map[string]any{"titulo": "Feria del libro", "asistentes": 120}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create row: status=%d body=%s", status, body)
	}
	assertNoPrefix(t, "POST /content/eventos", body)
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode created row: %v", err)
	}
	rowID, _ := created["id"].(string)
	if rowID == "" {
		t.Fatalf("created row has no id: %s", body)
	}

	// THE evidence: the row is in the PREFIXED table.
	var titulo string
	var asistentes int64
	if err := db.QueryRow(`SELECT titulo, asistentes FROM cpt_eventos WHERE id = ?`, rowID).Scan(&titulo, &asistentes); err != nil {
		t.Fatalf("the row is not in cpt_eventos: %v", err)
	}
	if titulo != "Feria del libro" || asistentes != 120 {
		t.Fatalf("stored row = (%q,%d)", titulo, asistentes)
	}

	// 5. LIST, 6. READ, 7. UPDATE, 8. DELETE — all with the bare name.
	status, body = rawJSON(t, srv, http.MethodGet, "/content/eventos", nil, authHeader(admin))
	if status != http.StatusOK || !strings.Contains(body, "Feria del libro") {
		t.Fatalf("list: status=%d body=%s", status, body)
	}
	assertNoPrefix(t, "GET /content/eventos", body)

	status, body = rawJSON(t, srv, http.MethodGet, "/content/eventos/"+rowID, nil, authHeader(admin))
	if status != http.StatusOK || !strings.Contains(body, "Feria del libro") {
		t.Fatalf("detail: status=%d body=%s", status, body)
	}
	assertNoPrefix(t, "GET /content/eventos/{id}", body)

	status, body = rawJSON(t, srv, http.MethodPut, "/content/eventos/"+rowID,
		map[string]any{"titulo": "Feria del libro 2027", "asistentes": 300}, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("update: status=%d body=%s", status, body)
	}
	assertNoPrefix(t, "PUT /content/eventos/{id}", body)
	if err := db.QueryRow(`SELECT titulo, asistentes FROM cpt_eventos WHERE id = ?`, rowID).Scan(&titulo, &asistentes); err != nil {
		t.Fatalf("re-read updated row: %v", err)
	}
	if titulo != "Feria del libro 2027" || asistentes != 300 {
		t.Fatalf("updated row = (%q,%d)", titulo, asistentes)
	}

	if status, body := rawJSON(t, srv, http.MethodDelete, "/content/eventos/"+rowID, nil, authHeader(admin)); status != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", status, body)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM cpt_eventos`).Scan(&left); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if left != 0 {
		t.Fatalf("cpt_eventos still has %d rows after DELETE", left)
	}
	t.Logf("T3 OK: full round-trip on /content/eventos; data in cpt_eventos; no response ever mentioned %q", schema.DynamicTablePrefix)
}

// TestContract17TypeNamedLikeCodeTableIsHarmless is the T4 red-team scenario
// made real over HTTP: an admin creates a type named exactly like a CODE table
// (`users`, legal since CONTRACT-17 relaxed that reservation), stores a row in
// it, and NOTHING about the real users table — or the login that depends on it
// — changes. This is hueco 3 turned from "the service would not start" into a
// non-event.
func TestContract17TypeNamedLikeCodeTableIsHarmless(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	var usersBefore int
	if err := db.QueryRow(`SELECT count(*) FROM users`).Scan(&usersBefore); err != nil {
		t.Fatalf("count users: %v", err)
	}
	usersColumnsBefore := tableColumns(t, db, "users")

	status, body := rawJSON(t, srv, http.MethodPost, "/content-types", map[string]any{
		"name":   "users",
		"fields": []map[string]string{{"name": "apodo", "type": "text"}},
	}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("a type named like a code table was rejected: status=%d body=%s", status, body)
	}
	if !sqliteTableExists(t, db, "cpt_users") {
		t.Fatal("the dynamic table 'cpt_users' was not created")
	}

	// A row in the DYNAMIC users.
	status, body = rawJSON(t, srv, http.MethodPost, "/content/users",
		map[string]any{"apodo": "el bibliotecario"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create row in the dynamic 'users': status=%d body=%s", status, body)
	}

	// The REAL users table is untouched: same columns, same rows.
	if got := strings.Join(tableColumns(t, db, "users"), ","); got != strings.Join(usersColumnsBefore, ",") {
		t.Fatalf("the real users table changed shape: %v", got)
	}
	var usersAfter, dynamicRows int
	if err := db.QueryRow(`SELECT count(*) FROM users`).Scan(&usersAfter); err != nil {
		t.Fatalf("count users after: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM cpt_users`).Scan(&dynamicRows); err != nil {
		t.Fatalf("count cpt_users: %v", err)
	}
	if usersAfter != usersBefore || dynamicRows != 1 {
		t.Fatalf("users=%d (want %d), cpt_users=%d (want 1)", usersAfter, usersBefore, dynamicRows)
	}

	// And the capability that depends on the real users table still works.
	status, body = rawJSON(t, srv, http.MethodPost, "/auth/login",
		map[string]string{"email": "admin@example.com", "password": "pw"}, nil)
	if status != http.StatusOK || !strings.Contains(body, "token") {
		t.Fatalf("login broke after creating a type named 'users': status=%d body=%s", status, body)
	}
	t.Logf("T4 OK: dynamic type 'users' -> table cpt_users (1 row); real users table intact (%d rows) and login still 200", usersAfter)
}

// TestContract17AdminUIUsesThePublicName is the T3 criterion on the UI, with a
// REAL session cookie (hence openUITLS): the sidebar, the listing and the forms
// all speak the public name, and no rendered byte contains the prefix.
func TestContract17AdminUIUsesThePublicName(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update", "content.delete")
	client := loginUI(t, db, srv, "ui17@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "eventos",
		[2]string{"titulo", "text"},
		[2]string{"asistentes", "integer"},
	)
	if !tableExists(t, db, "cpt_eventos") {
		t.Fatal("the UI did not create the prefixed table 'cpt_eventos'")
	}
	if tableExists(t, db, "eventos") {
		t.Fatal("an UNPREFIXED table 'eventos' exists")
	}

	// The sidebar of an unrelated page links to the PUBLIC name.
	status, body := getBody(t, client, srv.URL+"/admin/articles")
	if status != http.StatusOK {
		t.Fatalf("/admin/articles status=%d", status)
	}
	if !strings.Contains(body, `href="/admin/content/eventos"`) || !strings.Contains(body, ">eventos<") {
		t.Fatalf("the sidebar does not show the public name: %.600q", body)
	}
	assertNoPrefix(t, "sidebar", body)

	// Create a row through the UI form, addressed by the public name.
	resp, err := client.PostForm(srv.URL+"/admin/content/eventos", url.Values{
		"titulo": {"Club de lectura"}, "asistentes": {"12"},
	})
	if err != nil {
		t.Fatalf("create row: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create row status=%d, want 200 after redirect", resp.StatusCode)
	}
	var stored string
	if err := db.QueryRow(`SELECT titulo FROM cpt_eventos`).Scan(&stored); err != nil {
		t.Fatalf("the UI row is not in cpt_eventos: %v", err)
	}
	if stored != "Club de lectura" {
		t.Fatalf("stored titulo = %q", stored)
	}

	// The listing and the type list render the public name and nothing else.
	for _, path := range []string{"/admin/content/eventos", "/admin/content-types", "/admin/content/eventos/new"} {
		status, body := getBody(t, client, srv.URL+path)
		if status != http.StatusOK {
			t.Fatalf("GET %s status=%d", path, status)
		}
		if !strings.Contains(body, "eventos") {
			t.Fatalf("GET %s does not mention the public name", path)
		}
		assertNoPrefix(t, "GET "+path, body)
	}
	// The prefixed name is not a UI route either.
	if status, _ := getBody(t, client, srv.URL+"/admin/content/cpt_eventos"); status != http.StatusNotFound {
		t.Fatalf("/admin/content/cpt_eventos status=%d, want 404", status)
	}
	t.Logf("T3 UI OK: sidebar + listing + forms use %q; real table is cpt_eventos; no rendered byte contains %q", "eventos", schema.DynamicTablePrefix)
}

// TestContract17EnsureSchemaIsIdempotentWithPrefixedTables is the restart cycle
// at the HTTP layer: after creating a type through the API, running the very
// same EnsureSchema the service runs on boot must be a no-op — no error, no
// attempt to re-create the prefixed table, and the composed metadata unchanged.
func TestContract17EnsureSchemaIsIdempotentWithPrefixedTables(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	if status, body := rawJSON(t, srv, http.MethodPost, "/content-types", map[string]any{
		"name": "boletines", "fields": []map[string]string{{"name": "titulo", "type": "text"}},
	}, authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create type: status=%d body=%s", status, body)
	}
	before := countTables(t, db)

	if err := store.EnsureSchema(context.Background(), storeFor(db)); err != nil {
		t.Fatalf("EnsureSchema after creating a dynamic type: %v", err)
	}
	if after := countTables(t, db); after != before {
		t.Fatalf("table count changed from %d to %d across EnsureSchema", before, after)
	}
	if !sqliteTableExists(t, db, "cpt_boletines") {
		t.Fatal("cpt_boletines disappeared across EnsureSchema")
	}
	// A second run must be just as quiet.
	if err := store.EnsureSchema(context.Background(), storeFor(db)); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}
	t.Logf("RESTART OK: EnsureSchema twice after creating 'boletines'; cpt_boletines intact, %d tables unchanged", before)
}
