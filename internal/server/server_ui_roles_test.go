package server_test

// CONTRACT-16 T3 + T4 acceptance tests: the per-role permission editor over real
// HTTP with a real session cookie. The TLS server (openUITLS) is mandatory here
// for the same reason as CONTRACT-06/07/08: the session cookie is Secure and
// net/http/cookiejar drops it over plain HTTP, so a NewServer test would never
// see the cookie come back.
//
// Shared helpers reused: openUITLS, loginUI, grant, getBody, noRedirectJarClient,
// nonexistentID, userIDByEmail, roleBlock, timeNowUTC, testSecret.

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
)

// rolesAdminClient grants roles.manage to administrator and logs in an
// administrator session able to exercise the editor.
func rolesAdminClient(t *testing.T, db *sql.DB, srv *httptest.Server, email string) *http.Client {
	t.Helper()
	grant(t, db, "administrator", "roles.manage")
	return loginUI(t, db, srv, email, "pw", "administrator")
}

// loginExisting logs in an ALREADY-created user through the real /login form and
// returns a cookie-jar client. Unlike loginUI it does not create the user, so a
// test can assign several roles first and get a session JWT carrying all of them.
func loginExisting(t *testing.T, srv *httptest.Server, email, pw string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := srv.Client()
	client.Jar = jar
	resp, err := client.PostForm(srv.URL+"/login", url.Values{"email": {email}, "password": {pw}})
	if err != nil {
		t.Fatalf("login %q: %v", email, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login %q final status = %d, want 200", email, resp.StatusCode)
	}
	return client
}

// dbGrants reads a role's grants with a DIRECT SQL query against
// role_permissions — the independent confirmation the contract asks for (never
// through the same code path that wrote them).
func dbGrants(t *testing.T, db *sql.DB, role string) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT p.name
		   FROM role_permissions rp
		   JOIN roles r       ON r.id = rp.role_id
		   JOIN permissions p ON p.id = rp.permission_id
		  WHERE r.name = ?`, role)
	if err != nil {
		t.Fatalf("query grants %q: %v", role, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// sameSet compares two name sets ignoring order.
func sameSet(got []string, want ...string) bool {
	g := append([]string(nil), got...)
	sort.Strings(g)
	sort.Strings(want)
	return strings.Join(g, ",") == strings.Join(want, ",")
}

// postPerms submits the editor form for a role WITHOUT following the redirect,
// so the test sees the real status (303 on success, 4xx on a rejection).
func postPerms(t *testing.T, client *http.Client, srv *httptest.Server, role string, perms ...string) *http.Response {
	t.Helper()
	form := url.Values{}
	for _, p := range perms {
		form.Add("permissions", p)
	}
	resp, err := noRedirectJarClient(srv, client).PostForm(
		srv.URL+"/admin/roles/"+role+"/permissions", form)
	if err != nil {
		t.Fatalf("POST permissions for %q: %v", role, err)
	}
	return resp
}

// apiStatus performs a JSON API call with a bearer token over the TLS test
// server (doJSON uses http.DefaultClient, which does not trust the test cert).
func apiStatus(t *testing.T, srv *httptest.Server, token, method, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("api %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

// tokenFor creates a user with a role and returns a real JWT for it.
func tokenFor(t *testing.T, db *sql.DB, email, role string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := auth.CreateUser(ctx, db, email, "pw", []string{role}); err != nil {
		t.Fatalf("create %q: %v", email, err)
	}
	u, err := auth.VerifyCredentials(ctx, db, email, "pw")
	if err != nil {
		t.Fatalf("verify %q: %v", email, err)
	}
	tok, err := auth.IssueJWT(testSecret, u, timeNowUTC())
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	return tok
}

// --- T3: the edit form ------------------------------------------------------

// TestRoleEditFormShowsCatalogWithCurrentGrantsChecked covers T3: the form lists
// EVERY permission of the fixed catalog, with the currently granted ones checked
// and the others unchecked.
func TestRoleEditFormShowsCatalogWithCurrentGrantsChecked(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := rolesAdminClient(t, db, srv, "roleadmin@example.com")
	grant(t, db, "editor", "content.create")

	status, body := getBody(t, client, srv.URL+"/admin/roles/editor/edit")
	if status != http.StatusOK {
		t.Fatalf("edit form status = %d, want 200", status)
	}
	// Every catalog permission is offered.
	for _, p := range []string{"content.create", "content.update", "content.publish",
		"content.delete", "users.manage", "roles.manage", "terms.manage", "content_types.manage"} {
		if !strings.Contains(body, `value="`+p+`"`) {
			t.Errorf("edit form missing catalog permission %q", p)
		}
	}
	// The granted one is checked; a non-granted one is not.
	if !strings.Contains(body, `value="content.create" checked`) {
		t.Errorf("granted content.create is not pre-checked: %.600q", body)
	}
	if strings.Contains(body, `value="content.delete" checked`) {
		t.Errorf("non-granted content.delete rendered as checked")
	}
}

// TestRolesListShowsEditLinkOnlyWithPermission covers the listing side of T3:
// the read-only view stays session-only, but the edit link appears only for a
// session that actually holds roles.manage.
func TestRolesListShowsEditLinkOnlyWithPermission(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()

	viewer := loginUI(t, db, srv, "plainviewer@example.com", "pw", "author")
	status, body := getBody(t, viewer, srv.URL+"/admin/roles")
	if status != http.StatusOK {
		t.Fatalf("viewer roles list = %d, want 200 (still session-only)", status)
	}
	if strings.Contains(body, "/admin/roles/editor/edit") {
		t.Errorf("edit link shown to a session without roles.manage")
	}

	admin := rolesAdminClient(t, db, srv, "linkadmin@example.com")
	_, body = getBody(t, admin, srv.URL+"/admin/roles")
	if !strings.Contains(body, "/admin/roles/editor/edit") {
		t.Errorf("edit link missing for a session WITH roles.manage: %.600q", body)
	}
}

// TestRoleEditWithoutPermissionIs403 covers "sin roles.manage → 403 al editar",
// on both the form and the write, sent directly with the cookie (red-team, not
// via a button), and confirms nothing changed.
func TestRoleEditWithoutPermissionIs403(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	attacker := loginUI(t, db, srv, "noperm@example.com", "pw", "author")

	status, body := getBody(t, attacker, srv.URL+"/admin/roles/editor/edit")
	if status != http.StatusForbidden {
		t.Fatalf("GET edit without roles.manage = %d, want 403", status)
	}
	if strings.Contains(body, `"error"`) {
		t.Errorf("403 body looks like a JSON envelope")
	}
	if !strings.Contains(body, "Sin permiso") {
		t.Errorf("403 body missing the HTML marker")
	}

	resp := postPerms(t, attacker, srv, "editor", "content.delete")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without roles.manage = %d, want 403", resp.StatusCode)
	}
	if got := dbGrants(t, db, "editor"); !sameSet(got, "content.create") {
		t.Errorf("grants changed despite 403: %v", got)
	}
}

// TestRoleEditUnknownRoleIs404 covers "rol inexistente → 404" on both routes,
// as HTML (never a JSON envelope, never a 500).
func TestRoleEditUnknownRoleIs404(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := rolesAdminClient(t, db, srv, "admin404@example.com")

	status, body := getBody(t, client, srv.URL+"/admin/roles/superadmin/edit")
	if status != http.StatusNotFound {
		t.Fatalf("GET unknown role = %d, want 404", status)
	}
	if strings.Contains(body, `"error"`) {
		t.Errorf("404 body looks like JSON")
	}

	resp := postPerms(t, client, srv, "superadmin", "content.create")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST unknown role = %d, want 404", resp.StatusCode)
	}
}

// TestRoleEditUnknownPermissionIs400WithoutMutating covers the crafted-body case:
// a permission outside the fixed catalog → 400, rendered as a form error, and
// role_permissions is byte-for-byte unchanged (the valid permission in the same
// body must NOT sneak in).
func TestRoleEditUnknownPermissionIs400WithoutMutating(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := rolesAdminClient(t, db, srv, "admin400@example.com")
	grant(t, db, "author", "content.create")

	resp := postPerms(t, client, srv, "author", "content.publish", "articles.nuke")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("crafted unknown permission = %d, want 400", resp.StatusCode)
	}
	// The page renders an HTML banner (<p class="error" …>), so the JSON check
	// looks for the API envelope's opening `{"error"`, not the bare word.
	if strings.Contains(string(body), `{"error"`) {
		t.Errorf("400 body looks like a JSON envelope")
	}
	if !strings.Contains(string(body), "<!DOCTYPE html>") {
		t.Errorf("400 body is not the rendered form page")
	}
	if !strings.Contains(string(body), "Permiso desconocido") {
		t.Errorf("400 body missing the form error banner: %.600q", body)
	}
	if got := dbGrants(t, db, "author"); !sameSet(got, "content.create") {
		t.Errorf("grants mutated on a rejected request: %v, want [content.create]", got)
	}
}

// --- T2 at the HTTP layer: the guard ----------------------------------------

// TestGuardHTTPRejectsSelfLockout is case (a) end-to-end: the admin holds
// roles.manage only through `administrator` and submits that role's form without
// it → 409 with the explanatory form error, nothing persisted, and they can
// still open the editor afterwards (they are not locked out).
func TestGuardHTTPRejectsSelfLockout(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := rolesAdminClient(t, db, srv, "lockme@example.com")
	// administrator currently holds roles.manage (granted by rolesAdminClient).

	resp := postPerms(t, client, srv, "administrator", "users.manage")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("self-lockout = %d, want 409", resp.StatusCode)
	}
	if strings.Contains(string(body), `{"error"`) {
		t.Errorf("guard rejection looks like a JSON envelope")
	}
	if !strings.Contains(string(body), "<!DOCTYPE html>") {
		t.Errorf("guard rejection is not the rendered form page")
	}
	if !strings.Contains(string(body), "No se guardó") || !strings.Contains(string(body), "roles.manage") {
		t.Errorf("guard rejection is not rendered as a form error: %.800q", body)
	}
	if got := dbGrants(t, db, "administrator"); !sameSet(got, "roles.manage") {
		t.Errorf("grants changed despite the guard: %v", got)
	}
	// Still administrable.
	if status, _ := getBody(t, client, srv.URL+"/admin/roles/administrator/edit"); status != http.StatusOK {
		t.Errorf("admin locked out after a rejected change: %d", status)
	}
}

// TestGuardHTTPAllowsRemovingFromAnotherRole is case (b) end-to-end.
func TestGuardHTTPAllowsRemovingFromAnotherRole(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := rolesAdminClient(t, db, srv, "other@example.com")
	grant(t, db, "editor", "roles.manage", "content.create")

	resp := postPerms(t, client, srv, "editor", "content.create")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("removing from another role = %d, want 303", resp.StatusCode)
	}
	if got := dbGrants(t, db, "editor"); !sameSet(got, "content.create") {
		t.Errorf("editor grants = %v, want [content.create]", got)
	}
}

// TestGuardHTTPTwoRolesFirstAllowedSecondRejected is case (c) plus its red-team
// follow-up, end-to-end: an admin with TWO roles that both grant roles.manage
// may strip it from one; the SECOND attempt is refused.
func TestGuardHTTPTwoRolesFirstAllowedSecondRejected(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "roles.manage")
	grant(t, db, "editor", "roles.manage")
	// A session whose JWT carries BOTH roles: create the user, assign both roles,
	// and only THEN log in, so the token's claims list them both.
	created, err := auth.CreateUser(context.Background(), db, "two@example.com", "pw", []string{"administrator"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := auth.SetUserRoles(context.Background(), db, created.ID, []string{"administrator", "editor"}); err != nil {
		t.Fatalf("assign second role: %v", err)
	}
	client := loginExisting(t, srv, "two@example.com", "pw")

	// First removal: allowed (administrator still grants roles.manage).
	resp := postPerms(t, client, srv, "editor")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first removal = %d, want 303", resp.StatusCode)
	}
	if got := dbGrants(t, db, "editor"); len(got) != 0 {
		t.Fatalf("editor grants after first removal = %v, want none", got)
	}

	// Second removal: refused — it would complete the lockout.
	resp = postPerms(t, client, srv, "administrator")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second removal = %d, want 409 (lockout in two steps must be refused)", resp.StatusCode)
	}
	if got := dbGrants(t, db, "administrator"); !sameSet(got, "roles.manage") {
		t.Errorf("administrator grants = %v, want [roles.manage] unchanged", got)
	}
}

// TestGuardHTTPAllowsUnrelatedRemovalOnOwnRole: dropping content.create from the
// admin's OWN role while keeping roles.manage is allowed — the guard is about
// roles.manage only.
func TestGuardHTTPAllowsUnrelatedRemovalOnOwnRole(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := rolesAdminClient(t, db, srv, "unrelated@example.com")
	grant(t, db, "administrator", "content.create")

	resp := postPerms(t, client, srv, "administrator", "roles.manage")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unrelated removal = %d, want 303", resp.StatusCode)
	}
	if got := dbGrants(t, db, "administrator"); !sameSet(got, "roles.manage") {
		t.Errorf("grants = %v, want [roles.manage]", got)
	}
}

// --- T4: the full flow and the REAL effect ----------------------------------

// TestRolePermissionsFullFlowAndRealEffect is the T4 end-to-end proof:
//
//	(1) open the role's editor and see its current grants checked;
//	(2) add one permission and remove another, save;
//	(3) confirm with a DIRECT role_permissions query that the set is exact;
//	(4) and — the part that matters — that the change TAKES EFFECT: a user of
//	    that role loses access (403) to the route the revoked permission gated,
//	    and regains it (201) when it is granted back through the same UI.
func TestRolePermissionsFullFlowAndRealEffect(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := rolesAdminClient(t, db, srv, "flow@example.com")

	// The author role starts with content.create + content.update.
	grant(t, db, "author", "content.create", "content.update")
	authorToken := tokenFor(t, db, "writer@example.com", "author")

	// Baseline: the author CAN create an article (content.create is granted).
	if code := apiStatus(t, srv, authorToken, http.MethodPost, "/articles",
		`{"title":"before","body":"b"}`); code != http.StatusCreated {
		t.Fatalf("baseline POST /articles = %d, want 201", code)
	}

	// (1) The editor shows the current grants checked.
	status, body := getBody(t, client, srv.URL+"/admin/roles/author/edit")
	if status != http.StatusOK {
		t.Fatalf("edit form = %d, want 200", status)
	}
	if !strings.Contains(body, `value="content.create" checked`) ||
		!strings.Contains(body, `value="content.update" checked`) {
		t.Fatalf("current grants not pre-checked: %.800q", body)
	}

	// (2) Add content.publish, remove content.create (keep content.update).
	resp := postPerms(t, client, srv, "author", "content.update", "content.publish")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/roles/author/edit" {
		t.Errorf("Location = %q, want the edit page", loc)
	}

	// (3) Direct role_permissions query: the set is EXACTLY what was asked.
	if got := dbGrants(t, db, "author"); !sameSet(got, "content.update", "content.publish") {
		t.Fatalf("persisted grants = %v, want [content.publish content.update]", got)
	}
	// And the reloaded form reflects it.
	_, body = getBody(t, client, srv.URL+"/admin/roles/author/edit")
	if !strings.Contains(body, `value="content.publish" checked`) ||
		strings.Contains(body, `value="content.create" checked`) {
		t.Errorf("reloaded form does not reflect the saved set: %.800q", body)
	}

	// (4a) REAL EFFECT: the same author token now gets 403 on the route
	// content.create gated. Nothing about the user changed — only the grant.
	if code := apiStatus(t, srv, authorToken, http.MethodPost, "/articles",
		`{"title":"after","body":"b"}`); code != http.StatusForbidden {
		t.Fatalf("after revoking content.create, POST /articles = %d, want 403", code)
	}

	// (4b) Grant it back through the SAME UI → access is restored.
	resp = postPerms(t, client, srv, "author", "content.update", "content.publish", "content.create")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("re-grant = %d, want 303", resp.StatusCode)
	}
	if got := dbGrants(t, db, "author"); !sameSet(got, "content.create", "content.update", "content.publish") {
		t.Fatalf("re-granted set = %v", got)
	}
	if code := apiStatus(t, srv, authorToken, http.MethodPost, "/articles",
		`{"title":"restored","body":"b"}`); code != http.StatusCreated {
		t.Fatalf("after re-granting content.create, POST /articles = %d, want 201", code)
	}

	// The roles listing reflects the live table too (no hardcoded catalog).
	_, listing := getBody(t, client, srv.URL+"/admin/roles")
	if !strings.Contains(roleBlock(listing, "author"), "content.publish") {
		t.Errorf("roles listing does not show the newly granted permission")
	}
}

// TestRoleEmptySetRevokesEverything: saving with no checkbox ticked is a valid
// operation that leaves the role with zero grants (and the guard does not stand
// in the way when the role is not the actor's).
func TestRoleEmptySetRevokesEverything(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := rolesAdminClient(t, db, srv, "empty@example.com")
	grant(t, db, "contributor", "content.create", "content.update")

	resp := postPerms(t, client, srv, "contributor")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("empty save = %d, want 303", resp.StatusCode)
	}
	if got := dbGrants(t, db, "contributor"); len(got) != 0 {
		t.Errorf("grants after empty save = %v, want none", got)
	}
}
