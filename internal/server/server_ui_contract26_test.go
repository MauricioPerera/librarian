package server_test

// CONTRACT-26 T3 acceptance tests — deleting a dynamic content type from the
// admin UI, through the REAL form POSTs and the REAL session cookie.
//
// The one thing that must be PROVEN here rather than described: the number of
// rows that will be destroyed is on screen BEFORE anything is destroyed, and a
// submit that does not carry exactly what was on screen destroys nothing.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postDeleteForm submits the delete confirmation WITHOUT following the redirect,
// so the test sees the real status (303 on success, 400 with the page when the
// confirmation does not match).
func postDeleteForm(t *testing.T, client *http.Client, srv *httptest.Server, typeName string, form url.Values) (int, string) {
	t.Helper()
	resp, err := noRedirectJarClient(srv, client).PostForm(srv.URL+"/admin/content-types/"+typeName+"/delete", form)
	if err != nil {
		t.Fatalf("POST delete form: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// insertUIRows loads n rows into a dynamic type directly, so the count the page
// must show is a real one.
func insertUIRows(t *testing.T, client *http.Client, srv *httptest.Server, typeName string, titles ...string) {
	t.Helper()
	for _, title := range titles {
		form := url.Values{"field_titulo": {title}, "field_lugar": {"Montevideo"}, "field_asistentes": {"10"}}
		resp, err := client.PostForm(srv.URL+"/admin/content/"+typeName, form)
		if err != nil {
			t.Fatalf("insert row %q: %v", title, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("insert row %q: status=%d %.300q", title, resp.StatusCode, body)
		}
	}
}

// TestAdminContentTypeDeleteShowsTheRowCountBeforeDestroyingIt is the T3
// criterion: two steps, and the first one SHOWS the count.
func TestAdminContentTypeDeleteShowsTheRowCountBeforeDestroyingIt(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")

	uiCreateContentType(t, client, srv, "eventos",
		[2]string{"titulo", "text"}, [2]string{"lugar", "text"}, [2]string{"asistentes", "integer"})
	insertUIRows(t, client, srv, "eventos", "Feria del libro", "Charla Go", "Taller")

	// The list offers the deletion.
	_, list := getBody(t, client, srv.URL+"/admin/content-types")
	if !strings.Contains(list, `/admin/content-types/eventos/delete`) {
		t.Fatalf("the content types list has no link to the deletion: %.600q", list)
	}

	// STEP ONE: the confirmation page. It must SAY three, name the table, and
	// carry both values in hidden inputs — and it must not have destroyed
	// anything.
	status, page := getBody(t, client, srv.URL+"/admin/content-types/eventos/delete")
	if status != http.StatusOK {
		t.Fatalf("GET delete page: %d", status)
	}
	for _, want := range []string{
		`id="row-count">3<`,
		`cpt_eventos`,
		`name="confirm_name" value="eventos"`,
		`name="confirm_rows" value="3"`,
		`no se puede deshacer`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the confirmation page does not contain %q: %.1500q", want, page)
		}
	}
	if !sqliteTableExists(t, db, "cpt_eventos") {
		t.Fatal("merely LOOKING at the confirmation page destroyed the type")
	}
	t.Log("STEP ONE OK: the page shows 3 rows, names cpt_eventos and carries confirm_name/confirm_rows; nothing destroyed")

	// A hand-crafted submit that does NOT carry what was shown is bounced back —
	// the UI is not the only guard, the store re-checks.
	for _, bad := range []url.Values{
		{},
		{"confirm_name": {"eventos"}},
		{"confirm_rows": {"3"}},
		{"confirm_name": {"eventos"}, "confirm_rows": {"0"}},
		{"confirm_name": {"otro"}, "confirm_rows": {"3"}},
	} {
		status, body := postDeleteForm(t, client, srv, "eventos", bad)
		if status != http.StatusBadRequest {
			t.Fatalf("hand-crafted submit %v: status=%d, want 400", bad, status)
		}
		if !strings.Contains(body, `id="delete-error"`) || !strings.Contains(body, `name="confirm_rows" value="3"`) {
			t.Fatalf("the bounced page does not explain itself or lost the live count: %.800q", body)
		}
		if !sqliteTableExists(t, db, "cpt_eventos") {
			t.Fatalf("a bounced submit %v destroyed the table", bad)
		}
	}
	t.Log("BOUNCE OK: 5 mismatched submits refused with 400, the live count re-shown, the table intact")

	// STEP TWO: the submit the page itself produces.
	status, body := postDeleteForm(t, client, srv, "eventos",
		url.Values{"confirm_name": {"eventos"}, "confirm_rows": {"3"}})
	if status != http.StatusSeeOther {
		t.Fatalf("confirmed submit: status=%d body=%.500q, want 303", status, body)
	}
	if sqliteTableExists(t, db, "cpt_eventos") {
		t.Fatal("the confirmed submit left the table behind")
	}
	var types, fields int
	if err := db.QueryRow(`SELECT count(*) FROM content_types`).Scan(&types); err != nil {
		t.Fatalf("count types: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM content_type_fields`).Scan(&fields); err != nil {
		t.Fatalf("count fields: %v", err)
	}
	if types != 0 || fields != 0 {
		t.Fatalf("registry left behind: types=%d fields=%d", types, fields)
	}
	_, list = getBody(t, client, srv.URL+"/admin/content-types")
	if strings.Contains(list, `content-type-eventos`) {
		t.Fatalf("the deleted type is still listed: %.600q", list)
	}
	t.Log("STEP TWO OK: 303, the table and the registry rows are gone and the list no longer shows the type")
}

// TestAdminContentTypeDeleteWithoutPermissionIs403 — the write is gated by the
// same permission as creation and editing; the confirmation page only needs a
// session (it destroys nothing).
func TestAdminContentTypeDeleteWithoutPermissionIs403(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	admin := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")
	uiCreateContentType(t, admin, srv, "notas", [2]string{"titulo", "text"})

	plain := loginUI(t, db, srv, "editor@example.com", "pw", "editor")
	if status, _ := getBody(t, plain, srv.URL+"/admin/content-types/notas/delete"); status != http.StatusOK {
		t.Fatalf("GET delete page without the permission = %d, want 200 (reads are session-only)", status)
	}
	status, _ := postDeleteForm(t, plain, srv, "notas",
		url.Values{"confirm_name": {"notas"}, "confirm_rows": {"0"}})
	if status != http.StatusForbidden {
		t.Fatalf("POST without the permission = %d, want 403", status)
	}
	if !sqliteTableExists(t, db, "cpt_notas") {
		t.Fatal("the forbidden POST destroyed the type")
	}
	t.Log("UI GATE OK: page readable with a session, the deletion refused with 403 and nothing destroyed")
}

// TestAdminContentTypeDeleteUnknownTypeIs404HTML — an unknown type is an HTML
// 404 page on both the confirmation page and the submit, never a 500.
func TestAdminContentTypeDeleteUnknownTypeIs404HTML(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")

	status, body := getBody(t, client, srv.URL+"/admin/content-types/fantasma/delete")
	if status != http.StatusNotFound || !strings.Contains(body, "<html") {
		t.Fatalf("GET unknown: status=%d html=%v", status, strings.Contains(body, "<html"))
	}
	status, body = postDeleteForm(t, client, srv, "fantasma",
		url.Values{"confirm_name": {"fantasma"}, "confirm_rows": {"0"}})
	if status != http.StatusNotFound || !strings.Contains(body, "<html") {
		t.Fatalf("POST unknown: status=%d html=%v", status, strings.Contains(body, "<html"))
	}
	t.Log("404 OK: HTML page for both the confirmation page and the submit")
}

// TestAdminContentTypeDeleteReflectsAConcurrentWrite — the page is re-read on a
// bounce, so an admin who confirms a count that moved sees the NEW one instead
// of being asked again for a number that is already wrong.
func TestAdminContentTypeDeleteReflectsAConcurrentWrite(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")

	uiCreateContentType(t, client, srv, "eventos",
		[2]string{"titulo", "text"}, [2]string{"lugar", "text"}, [2]string{"asistentes", "integer"})
	insertUIRows(t, client, srv, "eventos", "Feria del libro")

	_, page := getBody(t, client, srv.URL+"/admin/content-types/eventos/delete")
	if !strings.Contains(page, `name="confirm_rows" value="1"`) {
		t.Fatalf("the page does not carry the count of 1: %.900q", page)
	}
	// Another client loads content while the admin reads the page.
	insertUIRows(t, client, srv, "eventos", "Charla Go", "Taller")

	status, body := postDeleteForm(t, client, srv, "eventos",
		url.Values{"confirm_name": {"eventos"}, "confirm_rows": {"1"}})
	if status != http.StatusBadRequest {
		t.Fatalf("stale submit: status=%d, want 400", status)
	}
	if !strings.Contains(body, `name="confirm_rows" value="3"`) || !strings.Contains(body, `id="row-count">3<`) {
		t.Fatalf("the bounced page does not show the NEW count: %.1200q", body)
	}
	if !sqliteTableExists(t, db, "cpt_eventos") {
		t.Fatal("the stale submit destroyed the type")
	}
	t.Log("STALE COUNT OK: the submit carrying 1 was refused and the page came back showing 3")
}
