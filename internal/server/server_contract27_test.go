package server_test

// CONTRACT-27 acceptance tests over REAL HTTP.
//
// What they prove that the store-level tests cannot: the full cycle a client
// actually performs (create the target, create the referrer, write rows with and
// WITHOUT the relation), that a reference to an id that does not exist is a 400
// and not a 500, that deleting a REFERENCED ROW is refused legibly, and that the
// T3 guard reaches the client as an actionable body rather than a driver error.

import (
	"net/http"
	"strings"
	"testing"
)

// autoresBody / librosBody are the two types of the whole battery: `libros`
// declares a relation `autor` to `autores`.
func autoresBody() map[string]any {
	return map[string]any{
		"name":   "autores",
		"fields": []map[string]string{{"name": "nombre", "type": "text"}},
	}
}

func librosBody() map[string]any {
	return map[string]any{
		"name":       "libros",
		"fields":     []map[string]string{{"name": "titulo", "type": "text"}},
		"references": []map[string]string{{"name": "autor", "target": "autores"}},
	}
}

// TestRelationFullCycleOverHTTP is the T5 cycle end to end.
func TestRelationFullCycleOverHTTP(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create", "content.update", "content.delete")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	// The referrer BEFORE its target: refused, with a message that says what to
	// create first. Nothing is created.
	status, body := doJSON(t, srv, http.MethodPost, "/content-types", librosBody(), authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("libros before autores: status=%d body=%v, want 400", status, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "autores") || !strings.Contains(msg, "Create") {
		t.Fatalf("the refusal does not say what to create first: %v", body)
	}
	if sqliteTableExists(t, db, "cpt_libros") {
		t.Fatal("the refused creation left a table behind")
	}
	t.Logf("ORDER OK: 400 %s", body["error"])

	// Now in the right order.
	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", autoresBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create autores: %d %v", status, body)
	}
	status, body = doJSON(t, srv, http.MethodPost, "/content-types", librosBody(), authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create libros: %d %v", status, body)
	}
	refs, ok := body["references"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("the create response does not echo the relation: %v", body)
	}
	t.Logf("CREATE OK: %v", body)

	// GET /content-types/{name} carries the relation back.
	status, body = doJSON(t, srv, http.MethodGet, "/content-types/libros", nil, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("get libros: %d %v", status, body)
	}
	got := body["references"].([]any)[0].(map[string]any)
	if got["name"] != "autor" || got["target"] != "autores" {
		t.Fatalf("the definition read back lost the relation: %v", body)
	}

	// A row in the target.
	status, autor := doJSON(t, srv, http.MethodPost, "/content/autores", map[string]any{"nombre": "Borges"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create autor: %d %v", status, autor)
	}
	autorID := autor["id"].(string)

	// A row WITH the relation, and one WITHOUT it — a relation is optional.
	status, conRef := doJSON(t, srv, http.MethodPost, "/content/libros",
		map[string]any{"titulo": "Ficciones", "autor": autorID}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create libro with the relation: %d %v", status, conRef)
	}
	if conRef["autor"] != autorID {
		t.Fatalf("the relation did not round-trip: %v", conRef)
	}
	status, sinRef := doJSON(t, srv, http.MethodPost, "/content/libros",
		map[string]any{"titulo": "Anonimo"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create libro without the relation: %d %v", status, sinRef)
	}
	if sinRef["autor"] != nil {
		t.Fatalf("an omitted relation is not null: %v", sinRef)
	}
	t.Logf("ROWS OK: with=%v without=%v", conRef["autor"], sinRef["autor"])

	// THE CONSTRAINT IS ENFORCED, and the answer is 400, not 500.
	status, body = doJSON(t, srv, http.MethodPost, "/content/libros",
		map[string]any{"titulo": "Fantasma", "autor": "99999999-9999-4999-8999-999999999999"}, authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("a relation to a non-existent id: status=%d body=%v, want 400", status, body)
	}
	t.Logf("FK OK: 400 %s", body["error"])

	// A malformed id is a 400 too (on PostgreSQL the column is `uuid`, so an
	// unvalidated value would be a driver error, i.e. a 500).
	status, body = doJSON(t, srv, http.MethodPost, "/content/libros",
		map[string]any{"titulo": "Malformado", "autor": "not-a-uuid"}, authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("a malformed relation value: status=%d body=%v, want 400", status, body)
	}

	// DELETING A REFERENCED ROW is refused by the engine. The generic CRUD layer
	// reports it as a failure; what matters here is that the row SURVIVES — the
	// reference is never silently cascaded away.
	status, body = doJSON(t, srv, http.MethodDelete, "/content/autores/"+autorID, nil, authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("deleting a referenced row: status=%d body=%v, want 400", status, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "libros") || !strings.Contains(msg, "autor") {
		t.Fatalf("the refusal does not name who points at the row: %v", body)
	}
	t.Logf("ROW DELETE OK: 400 %s", body["error"])
	status, _ = doJSON(t, srv, http.MethodGet, "/content/autores/"+autorID, nil, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("the referenced row did not survive the refused deletion: %d", status)
	}

	// The row that is NOT referenced deletes normally, so the refusal above is
	// about the reference and not about the route.
	status, otro := doJSON(t, srv, http.MethodPost, "/content/autores", map[string]any{"nombre": "Cortazar"}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create the second autor: %d %v", status, otro)
	}
	if status, _ := doJSON(t, srv, http.MethodDelete, "/content/autores/"+otro["id"].(string), nil, authHeader(admin)); status != http.StatusNoContent {
		t.Fatalf("deleting an UNreferenced row: %d, want 204", status)
	}
	t.Log("OK: only the REFERENCED row is protected")
}

// TestReferencedTypeGuardOverHTTP is T3 as a client experiences it: PUT and
// DELETE on the target both refused with 400, an actionable body, and nothing
// done — and both work again once the referrer is gone.
func TestReferencedTypeGuardOverHTTP(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", autoresBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create autores: %d %v", status, body)
	}
	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", librosBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create libros: %d %v", status, body)
	}
	_, def := doJSON(t, srv, http.MethodGet, "/content-types/autores", nil, authHeader(admin))
	fieldID := def["fields"].([]any)[0].(map[string]any)["id"].(string)

	edit := map[string]any{
		"fields": []map[string]any{{"id": fieldID, "name": "nombre_completo", "type": "text"}},
	}
	status, body := doJSON(t, srv, http.MethodPut, "/content-types/autores", edit, authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("PUT on a referenced type: status=%d body=%v, want 400", status, body)
	}
	if body["nothing_was_done"] != true {
		t.Fatalf("the refusal does not state that nothing was done: %v", body)
	}
	by := body["referenced_by"].([]any)[0].(map[string]any)
	if by["content_type"] != "libros" || by["reference"] != "autor" {
		t.Fatalf("the refusal does not name the referrer: %v", body)
	}
	t.Logf("PUT OK: 400 %s", body["error"])

	status, body = doJSON(t, srv, http.MethodDelete, "/content-types/autores",
		map[string]any{"confirm_name": "autores", "confirm_rows": 0}, authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("DELETE on a referenced type: status=%d body=%v, want 400", status, body)
	}
	if body["referenced_by"] == nil || body["nothing_was_done"] != true {
		t.Fatalf("the delete refusal is not the actionable shape: %v", body)
	}
	t.Logf("DELETE OK: 400 %s", body["error"])

	// Nothing was touched.
	if !sqliteTableExists(t, db, "cpt_autores") {
		t.Fatal("the refusals dropped the target's table")
	}
	_, def = doJSON(t, srv, http.MethodGet, "/content-types/autores", nil, authHeader(admin))
	if def["fields"].([]any)[0].(map[string]any)["name"] != "nombre" {
		t.Fatalf("the refused edit changed the type: %v", def)
	}

	// Free it, then both operations succeed.
	if status, body := doJSON(t, srv, http.MethodDelete, "/content-types/libros",
		map[string]any{"confirm_name": "libros", "confirm_rows": 0}, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("delete the referrer: %d %v", status, body)
	}
	if status, body := doJSON(t, srv, http.MethodPut, "/content-types/autores", edit, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("PUT after freeing it: %d %v", status, body)
	}
	if status, body := doJSON(t, srv, http.MethodDelete, "/content-types/autores",
		map[string]any{"confirm_name": "autores", "confirm_rows": 0}, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("DELETE after freeing it: %d %v", status, body)
	}
	if sqliteTableExists(t, db, "cpt_autores") {
		t.Fatal("the target's table survived its deletion")
	}
	t.Log("OK: after deleting the referrer the target is editable and deletable again")
}

// TestReferenceToACodeTypeIsRejected pins the scope: only DYNAMIC types can be
// referenced. `articles` is a code type and is not in the registry.
func TestReferenceToACodeTypeIsRejected(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	body := map[string]any{
		"name":       "resenas",
		"fields":     []map[string]string{{"name": "texto", "type": "text"}},
		"references": []map[string]string{{"name": "articulo", "target": "articles"}},
	}
	status, resp := doJSON(t, srv, http.MethodPost, "/content-types", body, authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("a reference to a code type: status=%d body=%v, want 400", status, resp)
	}
	if sqliteTableExists(t, db, "cpt_resenas") {
		t.Fatal("the refused creation left a table behind")
	}
	t.Logf("OK: %s", resp["error"])
}
