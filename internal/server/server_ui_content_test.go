package server_test

// CONTRACT-15 acceptance tests: the generic admin UI for dynamic content types.
//
// Everything here goes through the REAL HTTP surface with a REAL session cookie
// (openUITLS + loginUI, so httptest.NewTLSServer — a Secure cookie is dropped by
// the cookie jar over plain HTTP), and NEVER through the JSON API: the point of
// this contract is that an admin who only has a browser can do the whole job.
// Reuses loginUI, getBody, uiDo, grant, openUITLS, noRedirectClient and
// nonexistentID from the sibling test files (same package).

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- T1: the mechanism that makes the dynamic sidebar unforgettable ----------

// TestPageDataIsBuiltOnlyByTheConstructor is the GUARD that turns "a new page
// forgot the dynamic menu entries" from a silent bug into a red test.
//
// The sidebar's dynamic half lives in pageData.dynamic, which only h.page(r, …)
// (ui.go) fills. A future page that hand-builds a `pageData{…}` literal would
// compile fine and render a sidebar missing every dynamic content type — the
// exact silent failure the contract warns about. This test reads the package's
// own source and fails if any file other than ui.go writes such a literal, so
// the omission is caught by `go test ./...` instead of by a confused admin.
func TestPageDataIsBuiltOnlyByTheConstructor(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "ui.go" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(src), "pageData{") {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("pageData{...} literal outside ui.go in %v — build page view models with h.page(r, title) "+
			"or the dynamic content types silently vanish from that page's sidebar", offenders)
	}
}

// uiCreateContentType creates a dynamic content type through the REAL form POST
// and asserts it succeeded (303 → the list, followed by the client).
func uiCreateContentType(t *testing.T, client *http.Client, srv *httptest.Server, name string, fields ...[2]string) {
	t.Helper()
	form := url.Values{"name": {name}}
	for _, f := range fields {
		form.Add("field_name", f[0])
		form.Add("field_type", f[1])
	}
	resp, err := client.PostForm(srv.URL+"/admin/content-types", form)
	if err != nil {
		t.Fatalf("create content type %q: %v", name, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create content type %q: status = %d, want 200 after redirect: %.300q", name, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "content-type-"+name) {
		t.Fatalf("content type %q not present in the list after creation: %.400q", name, body)
	}
}

// uiContentRowIDs scrapes the row ids out of a generic listing page.
func uiContentRowIDs(t *testing.T, client *http.Client, srv *httptest.Server, typeName string) []string {
	t.Helper()
	_, body := getBody(t, client, srv.URL+"/admin/content/"+typeName)
	var ids []string
	for _, chunk := range strings.Split(body, `<tr id="content-`)[1:] {
		ids = append(ids, chunk[:strings.IndexByte(chunk, '"')])
	}
	return ids
}

// TestDynamicTypeAppearsInSidebarOfEveryAuthenticatedPage is T1's acceptance:
// a type created mid-session shows up in the sidebar of EVERY authenticated
// page — not just the one that happens to know about content types — with no
// restart, because h.page reads the definitions from the database per request.
func TestDynamicTypeAppearsInSidebarOfEveryAuthenticatedPage(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage")
	client := loginUI(t, db, srv, "nav@example.com", "pw", "editor")

	pages := []string{
		"/", "/admin/articles", "/admin/articles/new", "/admin/products",
		"/admin/terms", "/admin/users", "/admin/users/new", "/admin/roles",
		"/admin/api-keys", "/admin/content-types", "/admin/content-types/new",
	}

	// Before: no dynamic entry anywhere, but the static "Tipos de contenido"
	// section is already there.
	for _, p := range pages {
		_, body := getBody(t, client, srv.URL+p)
		if strings.Contains(body, `href="/admin/content/recetas"`) {
			t.Fatalf("page %s already shows the dynamic type before it exists", p)
		}
		if !strings.Contains(body, `href="/admin/content-types"`) {
			t.Errorf("page %s is missing the static 'Tipos de contenido' menu entry", p)
		}
	}

	// Create it through the UI, with the SAME live session.
	uiCreateContentType(t, client, srv, "recetas", [2]string{"titulo", "text"})

	// After: every authenticated page's sidebar lists it, with its submenu.
	for _, p := range append(pages, "/admin/content/recetas", "/admin/content/recetas/new") {
		_, body := getBody(t, client, srv.URL+p)
		for _, want := range []string{`href="/admin/content/recetas"`, ">recetas<"} {
			if !strings.Contains(body, want) {
				t.Errorf("page %s sidebar is missing %q", p, want)
			}
		}
	}

	// The submenu only renders for the ACTIVE section (layout.html), so check it
	// on a page inside the type's own namespace.
	_, body := getBody(t, client, srv.URL+"/admin/content/recetas")
	for _, want := range []string{"Todo el contenido", `href="/admin/content/recetas/new"`} {
		if !strings.Contains(body, want) {
			t.Errorf("active dynamic section is missing its submenu entry %q", want)
		}
	}
}

// --- T2: the content-type management UI --------------------------------------

// TestAdminContentTypesListAndCreate covers the happy path of T2: the empty
// list, the create form with its htmx "add field" control, the creation itself,
// and the real table it produced.
func TestAdminContentTypesListAndCreate(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage")
	client := loginUI(t, db, srv, "ct@example.com", "pw", "editor")

	status, body := getBody(t, client, srv.URL+"/admin/content-types")
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}
	if !strings.Contains(body, "No hay tipos de contenido") {
		t.Errorf("empty list marker missing: %.300q", body)
	}

	status, body = getBody(t, client, srv.URL+"/admin/content-types/new")
	if status != http.StatusOK {
		t.Fatalf("new-form status = %d, want 200", status)
	}
	for _, want := range []string{`name="field_name"`, `name="field_type"`, `hx-get="/admin/content-types/new/field"`, `value="decimal"`} {
		if !strings.Contains(body, want) {
			t.Errorf("create form missing %q", want)
		}
	}

	// The htmx fragment that grows the form returns exactly one more row.
	status, body = getBody(t, client, srv.URL+"/admin/content-types/new/field")
	if status != http.StatusOK {
		t.Fatalf("field-row fragment status = %d, want 200", status)
	}
	if strings.Count(body, `name="field_name"`) != 1 || strings.Contains(body, "<html") {
		t.Errorf("field-row fragment is not a single bare row: %.300q", body)
	}

	uiCreateContentType(t, client, srv, "libros",
		[2]string{"titulo", "text"},
		[2]string{"paginas", "integer"},
		// A blank row must be ignored, not become a field.
		[2]string{"", "text"},
	)

	_, body = getBody(t, client, srv.URL+"/admin/content-types")
	for _, want := range []string{"libros", "titulo", "paginas", "integer"} {
		if !strings.Contains(body, want) {
			t.Errorf("list missing %q after creation: %.400q", want, body)
		}
	}
	// Exactly the two named fields were persisted (the blank row was dropped).
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM content_type_fields f
		JOIN content_types t ON t.id = f.content_type_id WHERE t.name = ?`, "libros").Scan(&n); err != nil {
		t.Fatalf("count fields: %v", err)
	}
	if n != 2 {
		t.Errorf("persisted %d fields, want 2 (the blank row must be ignored)", n)
	}
	// And the real table exists.
	if !tableExists(t, db, "libros") {
		t.Errorf("table libros was not created")
	}
}

// TestAdminContentTypeInvalidNameReRendersForm: an invalid identifier
// re-renders the form with the error — never a 500, never raw JSON, nothing
// persisted.
func TestAdminContentTypeInvalidNameReRendersForm(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage")
	client := loginUI(t, db, srv, "ct@example.com", "pw", "editor")

	for _, bad := range []string{"Recetas", "mi tipo", "1tipo", "users", "recetas; DROP TABLE users"} {
		resp, err := client.PostForm(srv.URL+"/admin/content-types",
			url.Values{"name": {bad}, "field_name": {"campo"}, "field_type": {"text"}})
		if err != nil {
			t.Fatalf("POST %q: %v", bad, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("name %q: status = %d, want 400", bad, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("name %q: Content-Type = %q, want text/html", bad, ct)
		}
		if strings.Contains(string(body), `{"error"`) {
			t.Errorf("name %q: body looks like the raw JSON envelope", bad)
		}
		if !strings.Contains(string(body), `name="name"`) {
			t.Errorf("name %q: the form was not re-rendered: %.300q", bad, body)
		}
		if !strings.Contains(string(body), "class=\"error\"") {
			t.Errorf("name %q: no error banner in the re-rendered form", bad)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM content_types`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d content types persisted despite every name being invalid", n)
	}
	// The system table named in one of the hostile attempts is intact.
	if !tableExists(t, db, "users") {
		t.Fatal("users table is gone")
	}
}

// TestAdminContentTypeDuplicateNameReRendersForm: a duplicate name re-renders
// the form with a clear message and creates nothing new.
func TestAdminContentTypeDuplicateNameReRendersForm(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage")
	client := loginUI(t, db, srv, "ct@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "notas", [2]string{"cuerpo", "text"})

	resp, err := client.PostForm(srv.URL+"/admin/content-types",
		url.Values{"name": {"notas"}, "field_name": {"otro"}, "field_type": {"text"}})
	if err != nil {
		t.Fatalf("POST duplicate: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Ya existe un tipo de contenido con ese nombre") {
		t.Errorf("duplicate message missing: %.300q", body)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM content_types WHERE name = ?`, "notas").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("duplicate produced %d rows, want 1", n)
	}
}

// TestAdminContentTypeWithoutPermissionIs403: a valid session WITHOUT
// content_types.manage → 403 HTML and no table created. The gate is
// server-side, and no new permission was introduced.
func TestAdminContentTypeWithoutPermissionIs403(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := loginUI(t, db, srv, "noperm@example.com", "pw", "author") // no grants

	resp, err := client.PostForm(srv.URL+"/admin/content-types",
		url.Values{"name": {"prohibido"}, "field_name": {"x"}, "field_type": {"text"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(string(body), "Sin permiso") {
		t.Errorf("403 body missing the HTML marker: %.200q", body)
	}
	if tableExists(t, db, "prohibido") {
		t.Errorf("table created despite the missing permission")
	}
}

// TestAdminContentTypeWithZeroFields is the red-team item: a type with NO
// declared fields is creatable from the UI and its (empty, then populated)
// listing works.
func TestAdminContentTypeWithZeroFields(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ct@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "marcadores")

	status, body := getBody(t, client, srv.URL+"/admin/content/marcadores")
	if status != http.StatusOK {
		t.Fatalf("listing of a field-less type: status = %d, want 200", status)
	}
	if !strings.Contains(body, "No hay contenido") {
		t.Errorf("empty-state marker missing: %.300q", body)
	}

	resp, err := client.PostForm(srv.URL+"/admin/content/marcadores", url.Values{})
	if err != nil {
		t.Fatalf("create row: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create row on a field-less type: status = %d, want 200", resp.StatusCode)
	}
	if ids := uiContentRowIDs(t, client, srv, "marcadores"); len(ids) != 1 {
		t.Errorf("field-less type has %d rows, want 1", len(ids))
	}
}

// TestAdminContentTypeManyFieldsDoesNotExplode is the other red-team item: a
// type with many fields still renders a usable page (inside a horizontally
// scrollable container), it does not 500.
func TestAdminContentTypeManyFieldsDoesNotExplode(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ct@example.com", "pw", "editor")

	fields := make([][2]string, 0, 24)
	for i := 0; i < 24; i++ {
		fields = append(fields, [2]string{"campo_" + string(rune('a'+i%26)) + strconvItoa(i), "text"})
	}
	uiCreateContentType(t, client, srv, "ancho", fields...)

	// The create form renders every one of the 24 controls.
	status, body := getBody(t, client, srv.URL+"/admin/content/ancho/new")
	if status != http.StatusOK {
		t.Fatalf("wide create form status = %d, want 200", status)
	}
	for _, f := range fields {
		if !strings.Contains(body, `name="`+f[0]+`"`) {
			t.Errorf("wide create form is missing the control for %q", f[0])
		}
	}

	// A row makes the listing render its 24 columns — inside the scroll box.
	form := url.Values{}
	for _, f := range fields {
		form.Set(f[0], "v")
	}
	resp, err := client.PostForm(srv.URL+"/admin/content/ancho", form)
	if err != nil {
		t.Fatalf("create wide row: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create wide row status = %d, want 200", resp.StatusCode)
	}
	status, body = getBody(t, client, srv.URL+"/admin/content/ancho")
	if status != http.StatusOK {
		t.Fatalf("wide listing status = %d, want 200 (not 500)", status)
	}
	if !strings.Contains(body, "table-scroll") {
		t.Errorf("wide listing is not inside a horizontally scrollable container")
	}
	if got := strings.Count(body, "<th>"); got != len(fields)+2 {
		t.Errorf("wide listing rendered %d headers, want %d", got, len(fields)+2)
	}
}

// --- T3: the generic content UI ----------------------------------------------

// TestAdminContentUnknownTypeIsHTML404 covers the security perimeter of T3: an
// unknown, a system-table and a hostile {type} are all 404 HTML on every route,
// and the system tables are untouched afterwards.
func TestAdminContentUnknownTypeIsHTML404(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.update", "content.delete")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	hostile := []string{
		"noexiste",
		"users",                     // a real table, but NOT a dynamic type
		"articles",                  // idem
		"content_types",             // the registry itself
		"users; DROP TABLE users--", // an injection attempt in the identifier slot
		"'; DROP TABLE users; --",
		"../users",
	}
	for _, typeName := range hostile {
		seg := url.PathEscape(typeName)
		for _, p := range []string{"/admin/content/" + seg, "/admin/content/" + seg + "/new", "/admin/content/" + seg + "/" + nonexistentID + "/edit"} {
			status, body := getBody(t, client, srv.URL+p)
			if status != http.StatusNotFound {
				t.Errorf("GET %s status = %d, want 404", p, status)
			}
			if strings.Contains(body, `{"error"`) {
				t.Errorf("GET %s body looks like the raw JSON envelope", p)
			}
			if !strings.Contains(body, "No encontrado") {
				t.Errorf("GET %s is not the HTML 404 page: %.200q", p, body)
			}
		}
		resp, err := client.PostForm(srv.URL+"/admin/content/"+seg, url.Values{"x": {"1"}})
		if err != nil {
			t.Fatalf("POST %q: %v", typeName, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST /admin/content/%s status = %d, want 404", seg, resp.StatusCode)
		}
		resp = uiDo(t, client, http.MethodDelete, srv.URL+"/admin/content/"+seg+"/"+nonexistentID, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("DELETE /admin/content/%s status = %d, want 404", seg, resp.StatusCode)
		}
	}

	// Every system table survived the whole battery.
	for _, tbl := range []string{"users", "articles", "products", "terms", "roles", "permissions", "api_keys", "content_types", "content_type_fields"} {
		if !tableExists(t, db, tbl) {
			t.Fatalf("system table %q disappeared during the hostile-{type} battery", tbl)
		}
	}
	var users int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("users still unreadable: %v", err)
	}
}

// TestAdminContentCreateWithoutPermissionIs403: a valid session WITHOUT
// content.create → 403 HTML at creating content, and no row inserted.
func TestAdminContentCreateWithoutPermissionIs403(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage")
	owner := loginUI(t, db, srv, "ct@example.com", "pw", "editor")
	uiCreateContentType(t, owner, srv, "recetas", [2]string{"titulo", "text"})

	attacker := loginUI(t, db, srv, "attacker@example.com", "pw", "author") // no content.*
	resp, err := attacker.PostForm(srv.URL+"/admin/content/recetas", url.Values{"titulo": {"colado"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Sin permiso") {
		t.Errorf("403 is not the HTML page: %.200q", body)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM recetas`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows inserted despite the missing permission", n)
	}
}

// TestAdminContentValidationReRendersForm: a wrong value for a declared type
// re-renders the form with the message and inserts nothing — the SAME validator
// the JSON API uses (bindValue), reached through the form.
func TestAdminContentValidationReRendersForm(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	uiCreateContentType(t, client, srv, "medidas",
		[2]string{"cantidad", "integer"}, [2]string{"cuando", "date"})

	cases := []url.Values{
		{"cantidad": {"abc"}, "cuando": {"2026-01-01"}},
		{"cantidad": {"1.5"}, "cuando": {"2026-01-01"}},
		{"cantidad": {"3"}, "cuando": {"no-es-fecha"}},
	}
	for _, form := range cases {
		resp, err := client.PostForm(srv.URL+"/admin/content/medidas", form)
		if err != nil {
			t.Fatalf("POST %v: %v", form, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: status = %d, want 400", form, resp.StatusCode)
		}
		if !strings.Contains(string(body), "Revisá los campos") {
			t.Errorf("%v: validation banner missing: %.300q", form, body)
		}
		// The admin's input is preserved in the re-rendered form.
		if !strings.Contains(string(body), `value="`+form.Get("cantidad")+`"`) {
			t.Errorf("%v: the typed value was not preserved", form)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM medidas`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows inserted despite validation failures", n)
	}
}

// --- T4: the complete admin flow, browser-only -------------------------------

// TestAdminDynamicContentFullFlow is THE acceptance test of this contract: from
// a database with no dynamic types, an admin with only a browser creates a type,
// sees it in the sidebar, opens its listing, creates a row using ALL FIVE field
// types, sees it listed, edits it and deletes it. No JSON API call anywhere.
func TestAdminDynamicContentFullFlow(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update", "content.delete")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "editor")

	// 1. Create the type from the UI, with one field of every allowed type.
	uiCreateContentType(t, client, srv, "recetas",
		[2]string{"titulo", "text"},
		[2]string{"raciones", "integer"},
		[2]string{"costo", "decimal"},
		[2]string{"publicada", "boolean"},
		[2]string{"fecha", "date"},
	)

	// 2. It is in the sidebar of an unrelated authenticated page.
	if _, body := getBody(t, client, srv.URL+"/admin/articles"); !strings.Contains(body, `href="/admin/content/recetas"`) {
		t.Fatalf("the new type is not in the sidebar of /admin/articles")
	}

	// 3. Its listing exists and renders the declared columns.
	status, body := getBody(t, client, srv.URL+"/admin/content/recetas")
	if status != http.StatusOK {
		t.Fatalf("listing status = %d, want 200", status)
	}
	if !strings.Contains(body, "No hay contenido") {
		t.Errorf("empty-state marker missing")
	}

	// 4. Create a row with all five field types (checkbox CHECKED here: the
	//    hidden companion + the box, exactly what a browser submits).
	resp, err := client.PostForm(srv.URL+"/admin/content/recetas", url.Values{
		"titulo":    {"Tarta de manzana"},
		"raciones":  {"8"},
		"costo":     {"12.50"},
		"publicada": {"false", "true"},
		"fecha":     {"2026-07-24"},
	})
	if err != nil {
		t.Fatalf("create row: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create row status = %d, want 200 after redirect", resp.StatusCode)
	}

	// 5. It shows in the listing with every value.
	_, body = getBody(t, client, srv.URL+"/admin/content/recetas")
	for _, want := range []string{"Tarta de manzana", "8", "12.50", "sí", "2026-07-24"} {
		if !strings.Contains(body, want) {
			t.Errorf("listing missing %q: %.500q", want, body)
		}
	}
	ids := uiContentRowIDs(t, client, srv, "recetas")
	if len(ids) != 1 {
		t.Fatalf("listing has %d rows, want 1", len(ids))
	}
	rowID := ids[0]

	// The stored values have the right SQL types (decimal as canonical text,
	// boolean as 1, date as canonical YYYY-MM-DD).
	var titulo, costo, fecha string
	var raciones, publicada int64
	if err := db.QueryRow(`SELECT titulo, raciones, costo, publicada, fecha FROM recetas WHERE id = ?`, rowID).
		Scan(&titulo, &raciones, &costo, &publicada, &fecha); err != nil {
		t.Fatalf("select stored row: %v", err)
	}
	if titulo != "Tarta de manzana" || raciones != 8 || costo != "12.50" || publicada != 1 || fecha != "2026-07-24" {
		t.Errorf("stored = (%q,%d,%q,%d,%q)", titulo, raciones, costo, publicada, fecha)
	}

	// 6. The edit form is preloaded with the stored values.
	status, body = getBody(t, client, srv.URL+"/admin/content/recetas/"+rowID+"/edit")
	if status != http.StatusOK {
		t.Fatalf("edit form status = %d, want 200", status)
	}
	for _, want := range []string{`value="Tarta de manzana"`, `value="8"`, `value="12.50"`, `value="2026-07-24"`, "checked", `hx-put="/admin/content/recetas/` + rowID + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form missing %q", want)
		}
	}

	// 7. Edit it (unchecking the boolean this time).
	resp = uiDo(t, client, http.MethodPut, srv.URL+"/admin/content/recetas/"+rowID, url.Values{
		"titulo":    {"Tarta de pera"},
		"raciones":  {"6"},
		"costo":     {"9.99"},
		"publicada": {"false"},
		"fecha":     {"2026-08-01"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200", resp.StatusCode)
	}
	if loc := resp.Header.Get("HX-Redirect"); loc != "/admin/content/recetas" {
		t.Errorf("HX-Redirect = %q, want /admin/content/recetas", loc)
	}
	_, body = getBody(t, client, srv.URL+"/admin/content/recetas")
	for _, want := range []string{"Tarta de pera", "9.99", "no"} {
		if !strings.Contains(body, want) {
			t.Errorf("listing after edit missing %q", want)
		}
	}

	// 8. Delete it.
	resp = uiDo(t, client, http.MethodDelete, srv.URL+"/admin/content/recetas/"+rowID, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp.StatusCode)
	}
	if ids := uiContentRowIDs(t, client, srv, "recetas"); len(ids) != 0 {
		t.Errorf("row survived the delete: %v", ids)
	}

	// 9. A missing id is still a 404 on both write verbs.
	resp = uiDo(t, client, http.MethodPut, srv.URL+"/admin/content/recetas/"+nonexistentID, url.Values{"titulo": {"x"}, "publicada": {"false"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT missing id status = %d, want 404", resp.StatusCode)
	}
	resp = uiDo(t, client, http.MethodDelete, srv.URL+"/admin/content/recetas/not-a-uuid", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE malformed id status = %d, want 404", resp.StatusCode)
	}
}

// TestAdminContentUncheckedCheckboxStoresFalseNotNull is the explicit check of
// the trap: an UNCHECKED checkbox must store false (0), NOT NULL. The browser
// submits only the hidden companion in that case.
func TestAdminContentUncheckedCheckboxStoresFalseNotNull(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	uiCreateContentType(t, client, srv, "banderas",
		[2]string{"nombre", "text"}, [2]string{"activa", "boolean"})

	// Exactly what a browser sends for an unchecked box: the hidden "false" only.
	resp, err := client.PostForm(srv.URL+"/admin/content/banderas",
		url.Values{"nombre": {"sin marcar"}, "activa": {"false"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()

	var activa sql.NullInt64
	if err := db.QueryRow(`SELECT activa FROM banderas WHERE nombre = ?`, "sin marcar").Scan(&activa); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !activa.Valid {
		t.Fatalf("an unchecked checkbox stored NULL — the hidden-companion fix is not working")
	}
	if activa.Int64 != 0 {
		t.Errorf("unchecked checkbox stored %d, want 0 (false)", activa.Int64)
	}

	// And a CHECKED box stores 1.
	resp, err = client.PostForm(srv.URL+"/admin/content/banderas",
		url.Values{"nombre": {"marcada"}, "activa": {"false", "true"}})
	if err != nil {
		t.Fatalf("create checked: %v", err)
	}
	resp.Body.Close()
	if err := db.QueryRow(`SELECT activa FROM banderas WHERE nombre = ?`, "marcada").Scan(&activa); err != nil {
		t.Fatalf("select checked: %v", err)
	}
	if !activa.Valid || activa.Int64 != 1 {
		t.Errorf("checked checkbox stored %v, want 1", activa)
	}
}

// TestAdminContentEditFormDistinguishesFalseFromNull is the red-team item about
// preloading: a stored NULL and a stored false must be TELLABLE APART in the
// edit form, even though a checkbox has only two states.
func TestAdminContentEditFormDistinguishesFalseFromNull(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	uiCreateContentType(t, client, srv, "tris",
		[2]string{"nombre", "text"}, [2]string{"activa", "boolean"})

	// A false row, through the form.
	resp, err := client.PostForm(srv.URL+"/admin/content/tris",
		url.Values{"nombre": {"falsa"}, "activa": {"false"}})
	if err != nil {
		t.Fatalf("create false: %v", err)
	}
	resp.Body.Close()
	// A NULL row — only reachable from outside the form, which is exactly the
	// state the edit form has to be honest about.
	resp, err = client.PostForm(srv.URL+"/admin/content/tris", url.Values{"nombre": {"nula"}, "activa": {"false"}})
	if err != nil {
		t.Fatalf("create null-to-be: %v", err)
	}
	resp.Body.Close()
	if _, err := db.Exec(`UPDATE tris SET activa = NULL WHERE nombre = ?`, "nula"); err != nil {
		t.Fatalf("force NULL: %v", err)
	}

	idOf := func(name string) string {
		var id string
		if err := db.QueryRow(`SELECT id FROM tris WHERE nombre = ?`, name).Scan(&id); err != nil {
			t.Fatalf("id of %q: %v", name, err)
		}
		return id
	}

	_, falseBody := getBody(t, client, srv.URL+"/admin/content/tris/"+idOf("falsa")+"/edit")
	_, nullBody := getBody(t, client, srv.URL+"/admin/content/tris/"+idOf("nula")+"/edit")

	if strings.Contains(falseBody, "sin valor / NULL") {
		t.Errorf("a stored false is wrongly labelled as NULL")
	}
	if !strings.Contains(nullBody, "sin valor / NULL") {
		t.Errorf("a stored NULL boolean is not distinguishable from false in the edit form")
	}
	// Neither is pre-checked (both are falsy), which is why the marker is needed.
	if strings.Contains(falseBody, `value="true" checked`) || strings.Contains(nullBody, `value="true" checked`) {
		t.Errorf("a falsy boolean must not render as checked")
	}

	// The listing tells them apart too: "no" vs the em dash.
	_, list := getBody(t, client, srv.URL+"/admin/content/tris")
	if !strings.Contains(list, ">no<") || !strings.Contains(list, ">—<") {
		t.Errorf("the listing does not distinguish false from NULL: %.600q", list)
	}
}

// TestAdminContentEmptyTextStoresNull documents the other half of the NULL
// story: an empty control means "no value" on every non-boolean type.
func TestAdminContentEmptyTextStoresNull(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	uiCreateContentType(t, client, srv, "opcionales",
		[2]string{"etiqueta", "text"}, [2]string{"cuenta", "integer"})

	resp, err := client.PostForm(srv.URL+"/admin/content/opcionales",
		url.Values{"etiqueta": {""}, "cuenta": {""}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()

	var etiqueta sql.NullString
	var cuenta sql.NullInt64
	if err := db.QueryRow(`SELECT etiqueta, cuenta FROM opcionales`).Scan(&etiqueta, &cuenta); err != nil {
		t.Fatalf("select: %v", err)
	}
	if etiqueta.Valid || cuenta.Valid {
		t.Errorf("empty controls stored (%v,%v), want NULL for both", etiqueta, cuenta)
	}
}

// TestAdminContentNoSessionRedirectsToLogin: every generic content route with no
// session → 302 /login, never JSON, never a 404 that would leak type existence.
func TestAdminContentNoSessionRedirectsToLogin(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := noRedirectClient(srv)

	for _, u := range []string{"/admin/content-types", "/admin/content-types/new", "/admin/content/lo-que-sea", "/admin/content/lo-que-sea/new"} {
		resp, err := client.Get(srv.URL + u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("GET %s status = %d, want 302", u, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Errorf("GET %s Location = %q, want /login", u, loc)
		}
	}
}

// --- helpers -----------------------------------------------------------------

// tableExists asks SQLite's own catalog whether a physical table is present.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("sqlite_master lookup for %q: %v", name, err)
	}
	return n > 0
}

// strconvItoa is a tiny local itoa so the test file needs no extra import just
// to build 24 distinct field names.
func strconvItoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
