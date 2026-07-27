package server_test

// CONTRACT-18 T2/T4 acceptance tests — the JSON write path that edits the
// fields of an applied dynamic content type, over REAL HTTP.
//
// What these tests prove that the store-level ones cannot: the permission gate,
// the itemised data-loss confirmation as a client actually experiences it, that
// a validation failure never reaches the database, that the generic CRUD of
// CONTRACT-14 speaks the NEW shape immediately (no restart), and that the
// transient staging table never leaks into any response.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// contentTypeIDs reads GET /content-types/{name} and returns field name → id,
// exactly as a real client would before composing an edit.
func contentTypeIDs(t *testing.T, srv *httptest.Server, token, name string) map[string]string {
	t.Helper()
	status, body := doJSON(t, srv, http.MethodGet, "/content-types/"+name, nil, authHeader(token))
	if status != http.StatusOK {
		t.Fatalf("GET /content-types/%s: status=%d body=%v", name, status, body)
	}
	out := map[string]string{}
	fields, _ := body["fields"].([]any)
	for _, raw := range fields {
		f, _ := raw.(map[string]any)
		id, _ := f["id"].(string)
		fname, _ := f["name"].(string)
		if id == "" {
			t.Fatalf("GET /content-types/%s returned a field without an id: %v — an edit cannot express a rename", name, f)
		}
		out[fname] = id
	}
	return out
}

// TestEditContentTypeRequiresPermission is the gate: the SAME
// content_types.manage that gates creation, no new permission.
func TestEditContentTypeRequiresPermission(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	grant(t, db, "editor", "content.create", "content.update", "content.delete")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")
	editor := jwtFor(t, db, "editor@example.com", "pw", "editor")

	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, body)
	}
	ids := contentTypeIDs(t, srv, admin, "reviews")

	edit := map[string]any{"fields": []map[string]any{
		{"id": ids["headline"], "name": "headline", "type": "text"},
		{"id": ids["score"], "name": "score", "type": "integer"},
		{"id": ids["price_paid"], "name": "price_paid", "type": "decimal"},
		{"id": ids["verified"], "name": "verified", "type": "boolean"},
		{"id": ids["read_on"], "name": "read_on", "type": "date"},
		{"name": "summary", "type": "text"},
	}}
	// An identity WITHOUT content_types.manage cannot edit, even though it can
	// write content.
	if status, body := doJSON(t, srv, http.MethodPut, "/content-types/reviews", edit, authHeader(editor)); status != http.StatusForbidden {
		t.Fatalf("editor PUT: status=%d body=%v, want 403", status, body)
	}
	// No identity at all → 401.
	if status, _ := doJSON(t, srv, http.MethodPut, "/content-types/reviews", edit, nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous PUT: status=%d, want 401", status)
	}
	// And nothing happened to the table.
	if got := strings.Join(tableColumns(t, db, "cpt_reviews"), ","); strings.Contains(got, "summary") {
		t.Fatalf("a forbidden request changed the table: %s", got)
	}
	// With the permission it works.
	status, body := doJSON(t, srv, http.MethodPut, "/content-types/reviews", edit, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("admin PUT: status=%d body=%v, want 200", status, body)
	}
	if !strings.Contains(strings.Join(tableColumns(t, db, "cpt_reviews"), ","), "summary") {
		t.Fatalf("the accepted edit did not add the column: %v", tableColumns(t, db, "cpt_reviews"))
	}
	t.Logf("GATE OK: 403 for content.* only, 401 anonymous, 200 with content_types.manage; columns now %v",
		tableColumns(t, db, "cpt_reviews"))
}

// TestEditContentTypeDataLossNeedsItemisedConfirmation is the heart of T2: a
// removal is refused UNTIL the caller names the field, and the refusal itself
// tells the caller exactly what would be lost.
func TestEditContentTypeDataLossNeedsItemisedConfirmation(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	if status, body := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, body)
	}
	ids := contentTypeIDs(t, srv, admin, "reviews")
	// Keep only headline and score: three fields would be destroyed.
	edit := map[string]any{"fields": []map[string]any{
		{"id": ids["headline"], "name": "headline", "type": "text"},
		{"id": ids["score"], "name": "score", "type": "integer"},
	}}

	status, body := doJSON(t, srv, http.MethodPut, "/content-types/reviews", edit, authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("unconfirmed removal: status=%d body=%v, want 400", status, body)
	}
	msg, _ := body["error"].(string)
	for _, field := range []string{"price_paid", "verified", "read_on"} {
		if !strings.Contains(msg, field) {
			t.Fatalf("the refusal does not name %q: %q", field, msg)
		}
	}
	if _, ok := body["removes_data_of"]; !ok {
		t.Fatalf("the refusal carries no machine-readable list of what would be lost: %v", body)
	}
	// Nothing was done.
	before := strings.Join(tableColumns(t, db, "cpt_reviews"), ",")
	if !strings.Contains(before, "price_paid") {
		t.Fatalf("the refused edit already changed the table: %s", before)
	}

	// A PARTIAL confirmation is still refused — it is per field, not a flag.
	edit["confirm_remove"] = []string{"price_paid"}
	status, body = doJSON(t, srv, http.MethodPut, "/content-types/reviews", edit, authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("partial confirmation: status=%d body=%v, want 400", status, body)
	}

	// Confirming a field that is NOT being removed is refused too (stale client).
	edit["confirm_remove"] = []string{"price_paid", "verified", "read_on", "headline"}
	status, body = doJSON(t, srv, http.MethodPut, "/content-types/reviews", edit, authHeader(admin))
	if status != http.StatusBadRequest {
		t.Fatalf("stale confirmation: status=%d body=%v, want 400", status, body)
	}

	// The exact list → accepted.
	edit["confirm_remove"] = []string{"price_paid", "verified", "read_on"}
	status, body = doJSON(t, srv, http.MethodPut, "/content-types/reviews", edit, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("confirmed removal: status=%d body=%v, want 200", status, body)
	}
	got := tableColumns(t, db, "cpt_reviews")
	want := "id,author_id,headline,score,_search_fold,created_at,updated_at,metadata"
	if strings.Join(got, ",") != want {
		t.Fatalf("columns after the confirmed removal = %v, want %s", got, want)
	}
	t.Logf("CONFIRMATION OK: refused unconfirmed and partially confirmed, accepted the exact list; columns now %v", got)
}

// TestEditContentTypeRejections covers the validation surface over HTTP: every
// one is a 400 with an actionable message and NOTHING is written.
func TestEditContentTypeHTTPRejections(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")
	if status, _ := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatal("create failed")
	}
	ids := contentTypeIDs(t, srv, admin, "reviews")
	keep := []map[string]any{
		{"id": ids["headline"], "name": "headline", "type": "text"},
		{"id": ids["score"], "name": "score", "type": "integer"},
		{"id": ids["price_paid"], "name": "price_paid", "type": "decimal"},
		{"id": ids["verified"], "name": "verified", "type": "boolean"},
		{"id": ids["read_on"], "name": "read_on", "type": "date"},
	}
	with := func(extra ...map[string]any) map[string]any {
		return map[string]any{"fields": append(append([]map[string]any{}, keep...), extra...)}
	}

	cases := []struct {
		name   string
		path   string
		body   map[string]any
		status int
		msg    string
	}{
		{"unknown content type", "/content-types/no_existe", with(), http.StatusNotFound, "not found"},
		{"invalid field name", "/content-types/reviews", with(map[string]any{"name": "Mal Nombre", "type": "text"}), http.StatusBadRequest, "is invalid"},
		{"duplicate field", "/content-types/reviews", with(map[string]any{"name": "headline", "type": "text"}), http.StatusBadRequest, "more than once"},
		{"reserved field name", "/content-types/reviews", with(map[string]any{"name": "metadata", "type": "text"}), http.StatusBadRequest, "reserved"},
		{"unknown field type", "/content-types/reviews", with(map[string]any{"name": "raro", "type": "json"}), http.StatusBadRequest, "unknown type"},
		{"unknown field id", "/content-types/reviews",
			map[string]any{"fields": []map[string]any{{"id": "00000000-0000-0000-0000-000000000000", "name": "x", "type": "text"}}},
			http.StatusBadRequest, "unknown field"},
		{"type change", "/content-types/reviews",
			map[string]any{"fields": []map[string]any{{"id": ids["score"], "name": "score", "type": "text"}}},
			http.StatusBadRequest, "changing the type"},
		{"renaming the type itself", "/content-types/reviews",
			map[string]any{"name": "otra_cosa", "fields": keep}, http.StatusBadRequest, "cannot be changed"},
	}
	for _, c := range cases {
		status, body := doJSON(t, srv, http.MethodPut, c.path, c.body, authHeader(admin))
		if status != c.status {
			t.Fatalf("%s: status=%d body=%v, want %d", c.name, status, body, c.status)
		}
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, c.msg) {
			t.Fatalf("%s: message %q does not contain %q", c.name, msg, c.msg)
		}
		t.Logf("%-28s → %d %s", c.name, status, msg)
	}
	// The type is untouched by every one of them.
	if got := strings.Join(tableColumns(t, db, "cpt_reviews"), ","); got != "id,author_id,headline,score,price_paid,verified,read_on,_search_fold,created_at,updated_at,metadata" {
		t.Fatalf("a rejected edit changed the table: %s", got)
	}
}

// TestEditContentTypeThenGenericCRUD is the end-to-end criterion: content
// created BEFORE the edit is still readable after it, under the new field
// names, through the SAME generic routes, with NO restart — and the ids in the
// URLs still resolve.
func TestEditContentTypeThenGenericCRUD(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create", "content.update")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	if status, _ := doJSON(t, srv, http.MethodPost, "/content-types", map[string]any{
		"name": "eventos",
		"fields": []map[string]string{
			{"name": "titulo", "type": "text"},
			{"name": "lugar", "type": "text"},
			{"name": "asistentes", "type": "integer"},
		},
	}, authHeader(admin)); status != http.StatusCreated {
		t.Fatal("create type failed")
	}
	status, created := doJSON(t, srv, http.MethodPost, "/content/eventos",
		map[string]any{"titulo": "Charla Go", "lugar": "Montevideo", "asistentes": 40}, authHeader(admin))
	if status != http.StatusCreated {
		t.Fatalf("create content: %d %v", status, created)
	}
	rowID, _ := created["id"].(string)
	if rowID == "" {
		t.Fatalf("no id in %v", created)
	}

	ids := contentTypeIDs(t, srv, admin, "eventos")
	status, body := doJSON(t, srv, http.MethodPut, "/content-types/eventos", map[string]any{
		"fields": []map[string]any{
			{"id": ids["titulo"], "name": "encabezado", "type": "text"},
			{"id": ids["asistentes"], "name": "asistentes", "type": "integer"},
			{"name": "resumen", "type": "text"},
		},
		"confirm_remove": []string{"lugar"},
	}, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("edit: %d %v", status, body)
	}
	renamed, _ := body["renamed"].([]any)
	if len(renamed) != 1 {
		t.Fatalf("response does not report exactly one rename: %v", body["renamed"])
	}
	if pair, _ := renamed[0].(map[string]any); pair["from"] != "titulo" || pair["to"] != "encabezado" {
		t.Fatalf("reported rename = %v, want titulo→encabezado", renamed[0])
	}
	if removed, _ := body["removed"].([]any); len(removed) != 1 || removed[0] != "lugar" {
		t.Fatalf("reported removals = %v, want [lugar]", body["removed"])
	}
	if added, _ := body["added"].([]any); len(added) != 1 || added[0] != "resumen" {
		t.Fatalf("reported additions = %v, want [resumen]", body["added"])
	}

	// THE criterion: same URL, same id, new shape, no restart.
	status, row := doJSON(t, srv, http.MethodGet, "/content/eventos/"+rowID, nil, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("GET the pre-edit row: %d %v", status, row)
	}
	if row["id"] != rowID {
		t.Fatalf("row id changed: %v vs %v", row["id"], rowID)
	}
	if row["encabezado"] != "Charla Go" {
		t.Fatalf("renamed field lost its value: %v", row["encabezado"])
	}
	if row["asistentes"] != float64(40) {
		t.Fatalf("surviving field lost its value: %v", row["asistentes"])
	}
	if v, ok := row["resumen"]; !ok || v != nil {
		t.Fatalf("added field should be null, got %v (present=%v)", v, ok)
	}
	if _, ok := row["lugar"]; ok {
		t.Fatalf("removed field is still returned: %v", row)
	}
	// Writing through the new shape works immediately too.
	status, updated := doJSON(t, srv, http.MethodPut, "/content/eventos/"+rowID,
		map[string]any{"encabezado": "Charla Go 2", "asistentes": 41, "resumen": "hola"}, authHeader(admin))
	if status != http.StatusOK {
		t.Fatalf("update with the new shape: %d %v", status, updated)
	}
	if updated["resumen"] != "hola" || updated["encabezado"] != "Charla Go 2" {
		t.Fatalf("update did not stick: %v", updated)
	}
	// A body still using the OLD name is now an unknown field → 400.
	if status, _ := doJSON(t, srv, http.MethodPut, "/content/eventos/"+rowID,
		map[string]any{"titulo": "x"}, authHeader(admin)); status != http.StatusBadRequest {
		t.Fatalf("a body using the removed field name = %d, want 400", status)
	}
	t.Logf("CRUD-AFTER-EDIT OK: row %s kept its id, its renamed value and accepts the new shape without a restart", rowID)
}

// TestEditContentTypeNeverLeaksTheStagingTable: the transient table is an
// internal detail and must appear in NO response and in NO listing.
func TestEditContentTypeNeverLeaksTheStagingTable(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")
	if status, _ := doJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin)); status != http.StatusCreated {
		t.Fatal("create failed")
	}
	ids := contentTypeIDs(t, srv, admin, "reviews")
	if status, body := doJSON(t, srv, http.MethodPut, "/content-types/reviews", map[string]any{
		"fields": []map[string]any{
			{"id": ids["headline"], "name": "titular", "type": "text"},
			{"name": "extra", "type": "text"},
		},
		"confirm_remove": []string{"score", "price_paid", "verified", "read_on"},
	}, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("edit: %d %v", status, body)
	}

	for _, path := range []string{"/content-types", "/content-types/reviews", "/content/reviews"} {
		_, raw := rawJSON(t, srv, http.MethodGet, path, nil, authHeader(admin))
		assertNoPrefix(t, path, raw)
		if strings.Contains(raw, "cptmp") {
			t.Fatalf("%s leaks the staging table: %s", path, raw)
		}
	}
	// And no staging table survives in the database.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE 'cptmp_%'`).Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d staging table(s) left behind", n)
	}
	t.Logf("NO LEAK OK: no response mentions the staging table and none survives in the catalog")
}

// TestCreateContentTypeResponseShapeUnchanged guards the public contract of
// CONTRACT-13: the create response and the LIST still carry no `id` key (the
// field was added with omitempty, and only the single-type GET fills it).
func TestCreateContentTypeResponseShapeUnchanged(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := jwtFor(t, db, "admin@example.com", "pw", "administrator")

	_, created := rawJSON(t, srv, http.MethodPost, "/content-types", reviewsBody(), authHeader(admin))
	if strings.Contains(created, `"id"`) {
		t.Fatalf("the CREATE response shape changed: %s", created)
	}
	_, list := rawJSON(t, srv, http.MethodGet, "/content-types", nil, authHeader(admin))
	if strings.Contains(list, `"id"`) {
		t.Fatalf("the LIST response shape changed: %s", list)
	}
	_, one := rawJSON(t, srv, http.MethodGet, "/content-types/reviews", nil, authHeader(admin))
	if !strings.Contains(one, `"id"`) {
		t.Fatalf("the single-type GET must expose ids (an edit cannot express a rename without them): %s", one)
	}
	t.Logf("SHAPE OK: create/list unchanged, single GET carries ids")
}
