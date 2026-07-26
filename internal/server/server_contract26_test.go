package server_test

// CONTRACT-26 T2/T4 acceptance tests — the JSON write path that DELETES a
// dynamic content type, over REAL HTTP.
//
// What these prove that the store-level tests cannot: the permission gate, the
// guard as a client actually experiences it (an unconfirmed DELETE is the way a
// client LEARNS the row count), that a refused request never touches the
// database, that the type's content routes go to 404 afterwards, and that the
// name is reusable immediately without a restart.

import (
	"net/http"
	"strings"
	"testing"
)

// eventosBody is the type these tests create, destroy and re-create.
func eventosBody() map[string]any {
	return map[string]any{
		"name": "eventos",
		"fields": []map[string]string{
			{"name": "titulo", "type": "text"},
			{"name": "lugar", "type": "text"},
			{"name": "asistentes", "type": "integer"},
		},
	}
}

// TestDeleteContentTypeRequiresPermission is the gate: the SAME
// content_types.manage that gates creating and editing — NO new permission.
func TestDeleteContentTypeRequiresPermission(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	grant(t, db, "editor", "content.create", "content.update", "content.delete")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")
	editor := jwtFor(t, db, "editor@example.com", "pw", "editor")

	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", eventosBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, body)
	}
	confirm := map[string]any{"confirm_name": "eventos", "confirm_rows": 0}

	// An identity that can write CONTENT cannot delete a TYPE.
	if status, body := doJSON(t, srv, http.MethodDelete, "/content-types/eventos", confirm, authHeader(editor)); status != http.StatusForbidden {
		t.Fatalf("editor DELETE: status=%d body=%v, want 403", status, body)
	}
	// No identity at all → 401.
	if status, _ := doJSON(t, srv, http.MethodDelete, "/content-types/eventos", confirm, nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous DELETE: status=%d, want 401", status)
	}
	if !sqliteTableExists(t, db, "cpt_eventos") {
		t.Fatal("a forbidden request destroyed the table")
	}

	status, body := doJSON(t, srv, http.MethodDelete, "/content-types/eventos", confirm, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("admin DELETE: status=%d body=%v, want 200", status, body)
	}
	if sqliteTableExists(t, db, "cpt_eventos") {
		t.Fatal("the accepted deletion left the table behind")
	}
	t.Logf("GATE OK: 403 for content.* only, 401 anonymous, 200 with content_types.manage; response %v", body)
}

// TestDeleteContentTypeGuardOverHTTP is the heart of T2 as a client lives it:
// the unconfirmed DELETE is REFUSED and is simultaneously the only place the row
// count comes from, and every wrong confirmation is refused with nothing done.
func TestDeleteContentTypeGuardOverHTTP(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", eventosBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, body)
	}
	for i, titulo := range []string{"Feria", "Charla Go", "Taller"} {
		row := map[string]any{"titulo": titulo, "lugar": "Montevideo", "asistentes": 10 + i}
		if status, body := doJSON(t, srv, http.MethodPost, "/content/eventos", row, authHeader(admin)); status != http.StatusCreated {
			t.Fatalf("insert row %d: %d %v", i, status, body)
		}
	}

	// STEP ONE: the DELETE with NO confirmation. It is refused, and it is what
	// tells the client how many rows are at stake.
	status, body := doJSON(t, srv, http.MethodDelete, "/content-types/eventos", nil, authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("unconfirmed DELETE: status=%d body=%v, want 400", status, body)
	}
	rows, ok := body["rows"].(float64)
	if !ok || rows != 3 {
		t.Fatalf("the refusal does not report the row count: %v", body)
	}
	if body["confirm_name"] != "eventos" || body["confirm_rows"].(float64) != 3 || body["nothing_was_done"] != true {
		t.Fatalf("the refusal does not hand back the confirmation to resend: %v", body)
	}
	t.Logf("unconfirmed DELETE  -> 400 rows=%v confirm_name=%v confirm_rows=%v", body["rows"], body["confirm_name"], body["confirm_rows"])

	// Every wrong confirmation, each one refused with the type intact.
	wrong := []struct {
		label   string
		payload map[string]any
	}{
		{"a boolean, the shape this contract refuses", map[string]any{"confirm": true}},
		{"the name only", map[string]any{"confirm_name": "eventos"}},
		{"the count only", map[string]any{"confirm_rows": 3}},
		{"another type's name", map[string]any{"confirm_name": "articles", "confirm_rows": 3}},
		{"a wrong count", map[string]any{"confirm_name": "eventos", "confirm_rows": 2}},
		{"zero on a type with three rows", map[string]any{"confirm_name": "eventos", "confirm_rows": 0}},
	}
	for _, c := range wrong {
		status, body := doJSON(t, srv, http.MethodDelete, "/content-types/eventos", c.payload, authHeader(admin))
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%v, want 400", c.label, status, body)
		}
		if body["rows"].(float64) != 3 || body["nothing_was_done"] != true {
			t.Fatalf("%s: %v", c.label, body)
		}
		t.Logf("%-42s -> 400 %s", c.label, body["error"])
	}
	if !sqliteTableExists(t, db, "cpt_eventos") {
		t.Fatal("a refused deletion destroyed the table")
	}
	if _, listed := doJSON(t, srv, http.MethodGet, "/content/eventos", nil, authHeader(admin)); len(listed["items"].([]any)) != 3 {
		t.Fatalf("a refused deletion changed the content: %v", listed["items"])
	}

	// STEP TWO: the exact confirmation the refusal handed back.
	status, body = doJSON(t, srv, http.MethodDelete, "/content-types/eventos",
		map[string]any{"confirm_name": "eventos", "confirm_rows": 3}, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("confirmed DELETE: status=%d body=%v", status, body)
	}
	if body["rows_deleted"].(float64) != 3 || body["deleted"] != true || body["table"] != "cpt_eventos" {
		t.Fatalf("the receipt does not say what went: %v", body)
	}
	t.Logf("confirmed DELETE    -> 200 %v", body)

	// The catalog, the registry and the routes all agree.
	if sqliteTableExists(t, db, "cpt_eventos") {
		t.Fatal("the table survived")
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/content-types/eventos", nil, authHeader(admin)); status != http.StatusNotFound {
		t.Fatalf("GET /content-types/eventos after the deletion: %d, want 404", status)
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/content/eventos", nil, authHeader(admin)); status != http.StatusNotFound {
		t.Fatalf("GET /content/eventos after the deletion: %d, want 404", status)
	}
	if status, _ := doJSON(t, srv, http.MethodDelete, "/content-types/eventos",
		map[string]any{"confirm_name": "eventos", "confirm_rows": 0}, authHeader(admin)); status != http.StatusNotFound {
		t.Fatalf("deleting twice: %d, want 404", status)
	}
	status, list := doJSON(t, srv, http.MethodGet, "/content-types", nil, authHeader(admin))
	if status != http.StatusOK || len(list["content_types"].([]any)) != 0 {
		t.Fatalf("the listing still shows the type: %v", list)
	}
	t.Log("AFTER OK: /content-types/eventos and /content/eventos are 404, the listing is empty, a second DELETE is 404")
}

// TestDeleteContentTypeNameIsReusableOverHTTP: the name is free again and the
// new table is EMPTY — the check that catches a deletion which only removed the
// registry rows.
func TestDeleteContentTypeNameIsReusableOverHTTP(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	if status, _ := doJSON(t, srv, http.MethodPost, "/content-types", eventosBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatal("create")
	}
	if status, body := doJSON(t, srv, http.MethodPost, "/content/eventos",
		map[string]any{"titulo": "Feria", "lugar": "Montevideo", "asistentes": 120}, authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("insert: %d %v", status, body)
	}
	if status, body := doJSON(t, srv, http.MethodDelete, "/content-types/eventos",
		map[string]any{"confirm_name": "eventos", "confirm_rows": 1}, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("delete: %d %v", status, body)
	}

	// The SAME name, a DIFFERENT shape, with no restart in between.
	again := map[string]any{"name": "eventos", "fields": []map[string]string{
		{"name": "encabezado", "type": "text"},
		{"name": "cupos", "type": "integer"},
	}}
	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", again, authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("re-create: %d %v", status, body)
	}
	if got := strings.Join(tableColumns(t, db, "cpt_eventos"), ","); got != "id,author_id,encabezado,cupos,created_at,updated_at,metadata" {
		t.Fatalf("re-created shape: %s", got)
	}
	status, list := doJSON(t, srv, http.MethodGet, "/content/eventos", nil, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("GET /content/eventos: %d", status)
	}
	if items, _ := list["items"].([]any); len(items) != 0 {
		t.Fatalf("the re-created type carries %d rows of the old one: %v", len(items), items)
	}
	// And the new shape accepts content immediately.
	if status, body := doJSON(t, srv, http.MethodPost, "/content/eventos",
		map[string]any{"encabezado": "Nuevo", "cupos": 5}, authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("insert into the re-created type: %d %v", status, body)
	}
	t.Log("REUSE OK: the name was free immediately, the new table is empty and accepts the new shape without a restart")
}

// TestDeleteContentTypeUnknownIs404 keeps the failure modes clean: an unknown
// name — including one that could never be a legal type name — is a 404, never a
// 500 and never a leaked SQL error.
func TestDeleteContentTypeUnknownIs404(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	for _, name := range []string{"no_such_type", "articles", "cpt_eventos", "Eventos"} {
		status, body := doJSON(t, srv, http.MethodDelete, "/content-types/"+name,
			map[string]any{"confirm_name": name, "confirm_rows": 0}, authHeader(admin))
		if status != http.StatusNotFound {
			t.Fatalf("DELETE /content-types/%s: status=%d body=%v, want 404", name, status, body)
		}
		if msg, _ := body["error"].(string); msg != "content type not found" {
			t.Fatalf("DELETE /content-types/%s leaked %q", name, msg)
		}
	}
	// The CODE tables are not deletable through this route — they are not
	// dynamic types, so they are simply not found.
	if !sqliteTableExists(t, db, "articles") || !sqliteTableExists(t, db, "products") {
		t.Fatal("a code table was dropped through the content-type route")
	}
	t.Log("404 OK: unknown names, code-table names and the real table name all answer 404 with nothing touched")
}
