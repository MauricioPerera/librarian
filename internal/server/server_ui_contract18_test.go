package server_test

// CONTRACT-18 T3 acceptance tests — editing the fields of a dynamic content
// type from the admin UI, through the REAL form POSTs and the REAL session
// cookie.
//
// The one thing that must be proven here rather than described: the data loss
// is VISIBLE, naming the concrete field, BEFORE anything is destroyed. So the
// tests submit the removal, assert the database is untouched and the warning
// names the field, and only then submit the confirmation.

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// uiFieldIDs scrapes the hidden field_id inputs out of the edit form, together
// with the name in the same row. It is how a browser would carry the identity
// back on submit.
func uiFieldIDs(t *testing.T, client *http.Client, srv *httptest.Server, typeName string) map[string]string {
	t.Helper()
	status, body := getBody(t, client, srv.URL+"/admin/content-types/"+typeName+"/edit")
	if status != http.StatusOK {
		t.Fatalf("GET edit form: status = %d", status)
	}
	out := map[string]string{}
	for _, chunk := range strings.Split(body, `name="field_id" value="`)[1:] {
		id := chunk[:strings.IndexByte(chunk, '"')]
		rest := chunk
		nameAt := strings.Index(rest, `name="field_name" value="`)
		if nameAt < 0 {
			continue
		}
		rest = rest[nameAt+len(`name="field_name" value="`):]
		name := rest[:strings.IndexByte(rest, '"')]
		if id != "" && name != "" {
			out[name] = id
		}
	}
	return out
}

// postEditForm submits the edit form WITHOUT following the redirect, so the
// test sees the real status (303 on success, 200 with the warning when a
// confirmation is still missing).
func postEditForm(t *testing.T, client *http.Client, srv *httptest.Server, typeName string, form url.Values) (int, string) {
	t.Helper()
	resp, err := noRedirectJarClient(srv, client).PostForm(srv.URL+"/admin/content-types/"+typeName, form)
	if err != nil {
		t.Fatalf("POST edit form: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// dynamicColumns reads the physical columns of a dynamic type's real table.
func dynamicColumns(t *testing.T, db *sql.DB, typeName string) string {
	t.Helper()
	return strings.Join(tableColumns(t, db, "cpt_"+typeName), ",")
}

// TestAdminContentTypeEditShowsDataLossBeforeDestroyingIt is the T3 criterion.
func TestAdminContentTypeEditShowsDataLossBeforeDestroyingIt(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")

	uiCreateContentType(t, client, srv, "eventos",
		[2]string{"titulo", "text"}, [2]string{"lugar", "text"}, [2]string{"asistentes", "integer"})

	// A row with real data, so the loss is not hypothetical.
	var author string
	if err := db.QueryRow(`SELECT id FROM users WHERE email = 'admin@example.com'`).Scan(&author); err != nil {
		t.Fatalf("author: %v", err)
	}
	var rowID string
	if err := db.QueryRow(`INSERT INTO "cpt_eventos" (author_id, titulo, lugar, asistentes) VALUES (?,?,?,?) RETURNING id`,
		author, "Charla Go", "Montevideo", 40).Scan(&rowID); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	// The list offers the editor.
	_, list := getBody(t, client, srv.URL+"/admin/content-types")
	if !strings.Contains(list, `/admin/content-types/eventos/edit`) {
		t.Fatalf("the content types list has no link to the field editor: %.500q", list)
	}

	ids := uiFieldIDs(t, client, srv, "eventos")
	for _, want := range []string{"titulo", "lugar", "asistentes"} {
		if ids[want] == "" {
			t.Fatalf("the edit form does not carry the identity of %q: %v", want, ids)
		}
	}

	// SUBMIT #1: rename titulo → encabezado, add resumen, REMOVE lugar (its row
	// is submitted with an empty name).
	form := url.Values{}
	form.Add("field_id", ids["titulo"])
	form.Add("field_name", "encabezado")
	form.Add("field_type", "text")
	form.Add("field_id", ids["lugar"])
	form.Add("field_name", "") // removal
	form.Add("field_type", "text")
	form.Add("field_id", ids["asistentes"])
	form.Add("field_name", "asistentes")
	form.Add("field_type", "integer")
	form.Add("field_id", "")
	form.Add("field_name", "resumen")
	form.Add("field_type", "text")

	before := dynamicColumns(t, db, "eventos")
	status, body := postEditForm(t, client, srv, "eventos", form)
	if status != http.StatusOK {
		t.Fatalf("first submit: status = %d, want 200 with the warning (not a redirect)", status)
	}
	if !strings.Contains(body, "lugar") || !strings.Contains(body, "removed-field") {
		t.Fatalf("the warning does not name the field to be destroyed: %.900q", body)
	}
	if !strings.Contains(body, "BORRA datos") {
		t.Fatalf("the warning does not say data will be destroyed: %.900q", body)
	}
	if !strings.Contains(body, `name="confirm_remove" value="lugar"`) {
		t.Fatalf("the form does not carry the confirmation for the field it warned about: %.900q", body)
	}
	if got := dynamicColumns(t, db, "eventos"); got != before {
		t.Fatalf("THE WARNING ALREADY APPLIED THE EDIT: columns %s, want %s", got, before)
	}
	var lugar string
	if err := db.QueryRow(`SELECT lugar FROM "cpt_eventos" WHERE id = ?`, rowID).Scan(&lugar); err != nil || lugar != "Montevideo" {
		t.Fatalf("data was touched before confirmation: %q (err=%v)", lugar, err)
	}

	// SUBMIT #2: the same form plus the confirmation the page rendered.
	form.Add("confirm_remove", "lugar")
	status, body = postEditForm(t, client, srv, "eventos", form)
	if status != http.StatusSeeOther {
		t.Fatalf("confirmed submit: status = %d, want 303. body: %.900q", status, body)
	}
	if got, want := dynamicColumns(t, db, "eventos"), "id,author_id,encabezado,asistentes,resumen,_search_fold,created_at,updated_at,metadata"; got != want {
		t.Fatalf("columns after the confirmed edit = %s, want %s", got, want)
	}
	var encabezado string
	var resumen sql.NullString
	if err := db.QueryRow(`SELECT encabezado, resumen FROM "cpt_eventos" WHERE id = ?`, rowID).Scan(&encabezado, &resumen); err != nil {
		t.Fatalf("row lost: %v", err)
	}
	if encabezado != "Charla Go" {
		t.Fatalf("the rename lost its data: %q", encabezado)
	}
	if resumen.Valid {
		t.Fatalf("the added field is not NULL: %q", resumen.String)
	}
	// The listing reflects the new fields.
	_, list = getBody(t, client, srv.URL+"/admin/content-types")
	if !strings.Contains(list, "encabezado") || strings.Contains(list, ">lugar<") {
		t.Fatalf("the list does not reflect the edit: %.600q", list)
	}
	// And the generic content UI works with the new shape, with no restart.
	statusUI, contentList := getBody(t, client, srv.URL+"/admin/content/eventos")
	if statusUI != http.StatusOK || !strings.Contains(contentList, "encabezado") {
		t.Fatalf("the content listing does not use the new shape: status=%d %.600q", statusUI, contentList)
	}
	t.Logf("UI OK: warning named 'lugar' and changed nothing; the confirmed submit rebuilt the type keeping %q", encabezado)
}

// TestAdminContentTypeEditWithoutRemovalNeedsNoConfirmation: adding and
// renaming only is applied on the FIRST submit — the confirmation step exists
// for destruction, not for every edit.
func TestAdminContentTypeEditWithoutRemovalNeedsNoConfirmation(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")
	uiCreateContentType(t, client, srv, "notas", [2]string{"titulo", "text"})

	ids := uiFieldIDs(t, client, srv, "notas")
	form := url.Values{}
	form.Add("field_id", ids["titulo"])
	form.Add("field_name", "encabezado")
	form.Add("field_type", "text")
	form.Add("field_id", "")
	form.Add("field_name", "cuerpo")
	form.Add("field_type", "text")

	status, body := postEditForm(t, client, srv, "notas", form)
	if status != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (no confirmation needed). body: %.600q", status, body)
	}
	if got, want := dynamicColumns(t, db, "notas"), "id,author_id,encabezado,cuerpo,_search_fold,created_at,updated_at,metadata"; got != want {
		t.Fatalf("columns = %s, want %s", got, want)
	}
	t.Logf("NO-REMOVAL OK: applied on the first submit; columns %s", dynamicColumns(t, db, "notas"))
}

// TestAdminContentTypeEditWithoutPermissionIs403 — the write is gated by the
// same permission as creation, and the READ of the form only needs a session.
func TestAdminContentTypeEditWithoutPermissionIs403(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")
	uiCreateContentType(t, admin, srv, "notas", [2]string{"titulo", "text"})

	// A user with a session but WITHOUT content_types.manage.
	plain := loginUI(t, db, srv, "editor@example.com", "pw", "editor")
	if status, _ := getBody(t, plain, srv.URL+"/admin/content-types/notas/edit"); status != http.StatusOK {
		t.Fatalf("GET edit form without the permission = %d, want 200 (reads are session-only)", status)
	}
	form := url.Values{}
	form.Add("field_id", "")
	form.Add("field_name", "cuerpo")
	form.Add("field_type", "text")
	status, _ := postEditForm(t, plain, srv, "notas", form)
	if status != http.StatusForbidden {
		t.Fatalf("POST without the permission = %d, want 403", status)
	}
	if got := dynamicColumns(t, db, "notas"); strings.Contains(got, "cuerpo") {
		t.Fatalf("the forbidden POST changed the table: %s", got)
	}
	t.Logf("UI GATE OK: form readable with a session, write refused with 403 and nothing changed")
}

// TestAdminContentTypeEditUnknownTypeIs404HTML: an unknown type is an HTML 404
// page, never a JSON envelope and never a 500.
func TestAdminContentTypeEditUnknownTypeIs404HTML(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")

	status, body := getBody(t, client, srv.URL+"/admin/content-types/no_existe/edit")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if strings.Contains(body, `{"error"`) {
		t.Fatalf("the 404 leaked the JSON envelope: %.300q", body)
	}
	form := url.Values{}
	form.Add("field_id", "")
	form.Add("field_name", "x")
	form.Add("field_type", "text")
	if status, _ := postEditForm(t, client, srv, "no_existe", form); status != http.StatusNotFound {
		t.Fatalf("POST to an unknown type = %d, want 404", status)
	}
	t.Logf("404 OK: HTML page for both the form and the submit")
}

// TestAdminContentTypeEditCrossRenameThroughTheUI runs the red-team case
// end-to-end through the real form: `titulo`→`lugar` and `lugar`→`titulo`.
func TestAdminContentTypeEditCrossRenameThroughTheUI(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")
	uiCreateContentType(t, client, srv, "eventos", [2]string{"titulo", "text"}, [2]string{"lugar", "text"})

	var author string
	if err := db.QueryRow(`SELECT id FROM users WHERE email = 'admin@example.com'`).Scan(&author); err != nil {
		t.Fatalf("author: %v", err)
	}
	var rowID string
	if err := db.QueryRow(`INSERT INTO "cpt_eventos" (author_id, titulo, lugar) VALUES (?,?,?) RETURNING id`,
		author, "EL TITULO", "EL LUGAR").Scan(&rowID); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ids := uiFieldIDs(t, client, srv, "eventos")
	form := url.Values{}
	form.Add("field_id", ids["titulo"])
	form.Add("field_name", "lugar")
	form.Add("field_type", "text")
	form.Add("field_id", ids["lugar"])
	form.Add("field_name", "titulo")
	form.Add("field_type", "text")

	status, body := postEditForm(t, client, srv, "eventos", form)
	if status != http.StatusSeeOther {
		t.Fatalf("cross rename through the UI: status = %d, want 303. body: %.900q", status, body)
	}
	var titulo, lugar string
	if err := db.QueryRow(`SELECT titulo, lugar FROM "cpt_eventos" WHERE id = ?`, rowID).Scan(&titulo, &lugar); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if titulo != "EL LUGAR" || lugar != "EL TITULO" {
		t.Fatalf("cross rename wrong: titulo=%q lugar=%q", titulo, lugar)
	}
	t.Logf("UI CROSS-RENAME OK: titulo=%q lugar=%q, row id unchanged (%s)", titulo, lugar, rowID)
}
