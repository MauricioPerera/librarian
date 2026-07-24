package server_test

// CONTRACT-07 acceptance tests: the admin articles UI (/admin/articles). Every
// test that depends on the Secure session cookie surviving a round-trip uses the
// TLS server + cookie jar (same reason as CONTRACT-06: a Secure cookie is
// dropped over plain HTTP). Permission gating is asserted server-side, including
// a red-team hx-delete sent directly with the cookie (not via the button).

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
)

// loginUI creates a user with the given role and logs it in through the real
// /login form, returning a cookie-jar client already carrying the session. It
// follows redirects (default) so the jar captures the Set-Cookie.
func loginUI(t *testing.T, db *sql.DB, srv *httptest.Server, email, pw, role string) *http.Client {
	t.Helper()
	if _, err := auth.CreateUser(context.Background(), db, email, pw, []string{role}); err != nil {
		t.Fatalf("create user %q: %v", email, err)
	}
	jar, _ := cookiejar.New(nil)
	client := srv.Client()
	client.Jar = jar
	resp, err := client.PostForm(srv.URL+"/login", url.Values{"email": {email}, "password": {pw}})
	if err != nil {
		t.Fatalf("login %q: %v", email, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login %q final status = %d, want 200 (home)", email, resp.StatusCode)
	}
	return client
}

// getBody GETs a path with the given client and returns status + body.
func getBody(t *testing.T, client *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := client.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// noRedirectJarClient returns the srv client with the given jar but redirects
// disabled, so a test can inspect a 3xx directly while still sending the cookie.
func noRedirectJarClient(srv *httptest.Server, client *http.Client) *http.Client {
	c := srv.Client()
	c.Jar = client.Jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// --- T1: requireSessionPermission middleware --------------------------------

// TestAdminNoSessionRedirectsToLogin covers the no-session branch on both a read
// and a write route: 302 → /login, never a 401 or JSON.
func TestAdminNoSessionRedirectsToLogin(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := noRedirectClient(srv)

	for _, u := range []string{"/admin/articles", "/admin/articles/new"} {
		resp, err := client.Get(srv.URL + u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("GET %s status = %d, want 302", u, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Errorf("GET %s Location = %q, want /login", u, loc)
		}
	}

	// A write route (POST create) with no session also redirects to /login,
	// never a 401.
	resp, err := client.PostForm(srv.URL+"/admin/articles", url.Values{"title": {"x"}, "body": {"y"}})
	if err != nil {
		t.Fatalf("POST create no-session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("POST create no-session status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("POST create no-session Location = %q, want /login", loc)
	}
}

// TestAdminSessionWithoutPermissionIs403 covers the valid-session-but-no-perm
// branch: a 403 HTML page, NOT a JSON envelope, NOT a 500.
func TestAdminSessionWithoutPermissionIs403(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	// "author" role has NO grants → session valid, permission absent.
	client := loginUI(t, db, srv, "noperm@example.com", "pw", "author")

	resp, err := client.PostForm(srv.URL+"/admin/articles", url.Values{"title": {"x"}, "body": {"y"}})
	if err != nil {
		t.Fatalf("POST create: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html (not JSON)", ct)
	}
	if strings.Contains(string(body), `"error"`) {
		t.Errorf("403 body looks like JSON envelope, want HTML: %.80q", body)
	}
	if !strings.Contains(string(body), "Sin permiso") {
		t.Errorf("403 body missing HTML marker: %.120q", body)
	}
}

// TestAdminSessionWithPermissionPasses covers the happy path of the middleware:
// a session WITH content.create can POST and gets the 303 redirect to the list.
func TestAdminSessionWithPermissionPasses(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	resp, err := noRedirectJarClient(srv, client).PostForm(srv.URL+"/admin/articles",
		url.Values{"title": {"Hello"}, "body": {"World"}})
	if err != nil {
		t.Fatalf("POST create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/articles" {
		t.Errorf("Location = %q, want /admin/articles", loc)
	}
}

// --- T2: list + create ------------------------------------------------------

// TestAdminCreateAppearsInList covers T2: create via the real POST form, then
// the new article shows in GET /admin/articles with its title and draft status.
func TestAdminCreateAppearsInList(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	// Empty list first.
	status, body := getBody(t, client, srv.URL+"/admin/articles")
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}
	if !strings.Contains(body, "No hay artículos") {
		t.Errorf("empty list missing empty-state marker")
	}

	// Create via the real form POST.
	if _, err := client.PostForm(srv.URL+"/admin/articles",
		url.Values{"title": {"Mi Artículo"}, "body": {"Contenido de prueba"}}); err != nil {
		t.Fatalf("POST create: %v", err)
	}

	// It appears in the list, as a draft.
	status, body = getBody(t, client, srv.URL+"/admin/articles")
	if status != http.StatusOK {
		t.Fatalf("list-after status = %d, want 200", status)
	}
	if !strings.Contains(body, "Mi Artículo") {
		t.Errorf("list missing created title: %.200q", body)
	}
	if !strings.Contains(body, "Borrador") {
		t.Errorf("list missing draft badge")
	}

	// The author is the session user.
	var author string
	if err := db.QueryRow(`SELECT u.email FROM articles a JOIN users u ON u.id = a.author_id WHERE a.title = ?`,
		"Mi Artículo").Scan(&author); err != nil {
		t.Fatalf("author lookup: %v", err)
	}
	if author != "ed@example.com" {
		t.Errorf("author = %q, want ed@example.com", author)
	}
}

// TestAdminCreateValidationReRendersForm covers the validation branch: an empty
// title re-renders the form with the same message the JSON API uses, no row
// created.
func TestAdminCreateValidationReRendersForm(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	resp, err := client.PostForm(srv.URL+"/admin/articles", url.Values{"title": {""}, "body": {"B"}})
	if err != nil {
		t.Fatalf("POST create: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "title and body are required") {
		t.Errorf("missing validation message: %.160q", body)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("row created despite validation failure: %d", n)
	}
}

// --- T3: edit / publish / delete + 404 --------------------------------------

// TestAdminEditForm covers the preloaded edit form and its 404 for a missing id.
func TestAdminEditForm(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	id := uiCreate(t, client, srv, "Editable", "Cuerpo")

	status, body := getBody(t, client, srv.URL+"/admin/articles/"+id+"/edit")
	if status != http.StatusOK {
		t.Fatalf("edit-form status = %d, want 200", status)
	}
	if !strings.Contains(body, "Editable") {
		t.Errorf("edit form not preloaded with title: %.200q", body)
	}
	if !strings.Contains(body, `hx-put="/admin/articles/`+id+`"`) {
		t.Errorf("edit form missing hx-put target")
	}

	// Missing id → 404 HTML.
	status, body = getBody(t, client, srv.URL+"/admin/articles/"+nonexistentID+"/edit")
	if status != http.StatusNotFound {
		t.Fatalf("edit-form missing status = %d, want 404", status)
	}
	if strings.Contains(body, `"error"`) {
		t.Errorf("404 edit body looks like JSON")
	}
}

// TestAdminUpdate covers hx-put update: HX-Redirect on success, persisted change,
// and 404 for a missing id.
func TestAdminUpdate(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	id := uiCreate(t, client, srv, "Old", "OldBody")

	resp := uiDo(t, client, http.MethodPut, srv.URL+"/admin/articles/"+id,
		url.Values{"title": {"New"}, "body": {"NewBody"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200", resp.StatusCode)
	}
	if loc := resp.Header.Get("HX-Redirect"); loc != "/admin/articles" {
		t.Errorf("HX-Redirect = %q, want /admin/articles", loc)
	}
	resp.Body.Close()
	var title, body string
	if err := db.QueryRow(`SELECT title, body FROM articles WHERE id = ?`, id).Scan(&title, &body); err != nil {
		t.Fatalf("select: %v", err)
	}
	if title != "New" || body != "NewBody" {
		t.Errorf("persisted = (%q,%q), want (New,NewBody)", title, body)
	}

	// Missing id → 404.
	resp = uiDo(t, client, http.MethodPut, srv.URL+"/admin/articles/"+nonexistentID,
		url.Values{"title": {"x"}, "body": {"y"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update missing status = %d, want 404", resp.StatusCode)
	}
}

// TestAdminPublish covers hx-post publish: 200 + the updated row fragment now
// shows Publicado, and the DB reflects it. Missing id → 404.
func TestAdminPublish(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.publish")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	id := uiCreate(t, client, srv, "ToPublish", "Body")

	resp := uiDo(t, client, http.MethodPost, srv.URL+"/admin/articles/"+id+"/publish", nil)
	frag, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(frag), "Publicado") {
		t.Errorf("publish fragment missing published badge: %.200q", frag)
	}
	if !strings.Contains(string(frag), `id="article-`+id+`"`) {
		t.Errorf("publish fragment is not the row for this id")
	}
	var pub sql.NullString
	if err := db.QueryRow(`SELECT published_at FROM articles WHERE id = ?`, id).Scan(&pub); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !pub.Valid || pub.String == "" {
		t.Errorf("published_at not set in DB")
	}

	// Missing id → 404.
	resp = uiDo(t, client, http.MethodPost, srv.URL+"/admin/articles/"+nonexistentID+"/publish", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("publish missing status = %d, want 404", resp.StatusCode)
	}
}

// TestAdminDelete covers hx-delete: 200 empty body + row gone. Missing id → 404.
func TestAdminDelete(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.delete")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	id := uiCreate(t, client, srv, "ToDelete", "Body")

	resp := uiDo(t, client, http.MethodDelete, srv.URL+"/admin/articles/"+id, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp.StatusCode)
	}
	if exists(t, db, id) {
		t.Errorf("row still present after delete")
	}

	// Missing id → 404.
	resp = uiDo(t, client, http.MethodDelete, srv.URL+"/admin/articles/"+nonexistentID, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing status = %d, want 404", resp.StatusCode)
	}
}

// TestAdminMalformedIDIsNotFound is the red-team 404 check: a non-UUID id on any
// write route → 404 HTML, never 500/panic.
func TestAdminMalformedIDIsNotFound(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.update", "content.publish", "content.delete")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	cases := []struct {
		method, path string
	}{
		{http.MethodPut, srv.URL + "/admin/articles/not-a-uuid"},
		{http.MethodPost, srv.URL + "/admin/articles/x'OR'1'='1/publish"},
		{http.MethodDelete, srv.URL + "/admin/articles/abc"},
	}
	for _, c := range cases {
		resp := uiDo(t, client, c.method, c.path, url.Values{"title": {"x"}, "body": {"y"}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404 (not 500)", c.method, c.path, resp.StatusCode)
		}
	}
}

// --- T4: round-trip + red-team server-side gating ---------------------------

// TestAdminRoundTrip is the full end-to-end: login → create → appears → edit →
// publish → delete → gone.
func TestAdminRoundTrip(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.update", "content.publish", "content.delete")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	id := uiCreate(t, client, srv, "RoundTrip", "Body")
	if _, body := getBody(t, client, srv.URL+"/admin/articles"); !strings.Contains(body, "RoundTrip") {
		t.Fatalf("created article not in list")
	}

	// Edit.
	uiDo(t, client, http.MethodPut, srv.URL+"/admin/articles/"+id,
		url.Values{"title": {"RoundTrip 2"}, "body": {"Body 2"}}).Body.Close()
	if _, body := getBody(t, client, srv.URL+"/admin/articles"); !strings.Contains(body, "RoundTrip 2") {
		t.Fatalf("edited title not in list")
	}

	// Publish.
	uiDo(t, client, http.MethodPost, srv.URL+"/admin/articles/"+id+"/publish", nil).Body.Close()
	if _, body := getBody(t, client, srv.URL+"/admin/articles"); !strings.Contains(body, "Publicado") {
		t.Fatalf("published badge not in list")
	}

	// Delete.
	uiDo(t, client, http.MethodDelete, srv.URL+"/admin/articles/"+id, nil).Body.Close()
	_, body := getBody(t, client, srv.URL+"/admin/articles")
	if strings.Contains(body, "RoundTrip") {
		t.Fatalf("article still in list after delete")
	}
}

// TestAdminDeleteWithoutPermissionServerSide is the explicit red-team: a valid
// session WITHOUT content.delete sending hx-delete DIRECTLY (with the cookie,
// not via the button) → 403 HTML, and the row survives. The gate is server-side.
func TestAdminDeleteWithoutPermissionServerSide(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	// The victim editor (has create) makes a row.
	grant(t, db, "editor", "content.create")
	editor := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	id := uiCreate(t, editor, srv, "Protected", "Body")

	// The attacker session: author role, no content.delete.
	attacker := loginUI(t, db, srv, "attacker@example.com", "pw", "author")
	resp := uiDo(t, attacker, http.MethodDelete, srv.URL+"/admin/articles/"+id, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("direct hx-delete without perm status = %d, want 403", resp.StatusCode)
	}
	if strings.Contains(string(body), `"error"`) {
		t.Errorf("403 body looks like JSON envelope")
	}
	if !exists(t, db, id) {
		t.Errorf("row deleted despite missing permission — gate is not server-side")
	}
}

// --- helpers ----------------------------------------------------------------

// uiCreate creates an article through the real POST form and returns its id,
// looked up by title (the tests keep titles unique per case).
func uiCreate(t *testing.T, client *http.Client, srv *httptest.Server, title, body string) string {
	t.Helper()
	resp, err := client.PostForm(srv.URL+"/admin/articles", url.Values{"title": {title}, "body": {body}})
	if err != nil {
		t.Fatalf("uiCreate POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// PostForm follows the 303 to GET /admin/articles → 200.
		t.Fatalf("uiCreate final status = %d, want 200", resp.StatusCode)
	}
	return uiCreatedID(t, client, srv, title)
}

// uiCreatedID resolves the id of an article by scraping the list HTML for the
// row anchor of the given title — proving the created row is really in the UI,
// without a DB peek. It parses the id from the edit link next to the title.
func uiCreatedID(t *testing.T, client *http.Client, srv *httptest.Server, title string) string {
	t.Helper()
	_, body := getBody(t, client, srv.URL+"/admin/articles")
	// Find the <tr id="article-UUID"> whose cell-title matches. Simpler: the
	// list renders one row per article; locate the title, then the nearest
	// preceding tr id. We scan tr blocks.
	rows := strings.Split(body, `<tr id="article-`)
	for _, chunk := range rows[1:] {
		id := chunk[:strings.IndexByte(chunk, '"')]
		if strings.Contains(chunk, ">"+title+"<") {
			return id
		}
	}
	t.Fatalf("created article %q not found in list HTML", title)
	return ""
}

// uiDo sends a form-encoded request (PUT/POST/DELETE) with the client's cookie,
// redirects disabled so the raw status/headers are visible — exactly how htmx
// issues the verb. The caller closes the body.
func uiDo(t *testing.T, client *http.Client, method, u string, form url.Values) *http.Response {
	t.Helper()
	var reqBody io.Reader
	if form != nil {
		reqBody = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, u, reqBody)
	if err != nil {
		t.Fatalf("new %s %s: %v", method, u, err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	return resp
}
