package server_test

// CONTRACT-08 acceptance tests: the admin users UI (/admin/users) and the
// read-only roles/permissions view (/admin/roles). Every test that depends on
// the Secure session cookie uses the TLS server + cookie jar (openUITLS,
// loginUI), the same reason as CONTRACT-06/07. Permission gating is asserted
// server-side, including a red-team POST sent directly with the cookie.
//
// Shared helpers reused from the other server_test files: openUITLS, loginUI,
// grant, getBody, uiDo, noRedirectJarClient, nonexistentID.

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
)

// adminClient grants users.manage to the administrator role and logs in an
// administrator session that can exercise the write routes.
func adminClient(t *testing.T, db *sql.DB, srv *httptest.Server) *http.Client {
	t.Helper()
	grant(t, db, "administrator", "users.manage")
	return loginUI(t, db, srv, "admin@example.com", "pw", "administrator")
}

// jsonLoginStatus POSTs the JSON /auth/login and returns just the status code.
func jsonLoginStatus(t *testing.T, srv *httptest.Server, email, pw string) int {
	t.Helper()
	code, _ := jsonLoginStatusMsg(t, srv, email, pw)
	return code
}

// jsonLoginStatusMsg POSTs the JSON /auth/login and returns the status code plus
// the response body (used to compare the generic error message across cases).
func jsonLoginStatusMsg(t *testing.T, srv *httptest.Server, email, pw string) (int, string) {
	t.Helper()
	resp, err := srv.Client().Post(srv.URL+"/auth/login", "application/json",
		strings.NewReader(`{"email":`+quote(email)+`,"password":`+quote(pw)+`}`))
	if err != nil {
		t.Fatalf("json login: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

// quote wraps a string as a JSON string literal (the inputs here are plain test
// emails/passwords with no escaping needs, but this keeps the body valid JSON).
func quote(s string) string {
	return `"` + s + `"`
}

// userIDByEmail resolves a user's id for the detail/status/roles routes.
func userIDByEmail(t *testing.T, db *sql.DB, email string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM users WHERE email = ?`, email).Scan(&id); err != nil {
		t.Fatalf("lookup user %q: %v", email, err)
	}
	return id
}

// --- T2: create + list + detail --------------------------------------------

// TestAdminUserCreateAppearsInListAndDetail covers T2: an admin creates a user
// through the real POST form; the user shows in the list (email, active status,
// roles) and on its detail page.
func TestAdminUserCreateAppearsInListAndDetail(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := adminClient(t, db, srv)

	// Empty list first (only the admin itself exists → no "new@" yet).
	status, body := getBody(t, client, srv.URL+"/admin/users")
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}

	// Create via the real form POST (checkbox roles editor + author).
	if _, err := client.PostForm(srv.URL+"/admin/users", url.Values{
		"email":    {"new@example.com"},
		"password": {"s3cret-pw"},
		"roles":    {"editor", "author"},
	}); err != nil {
		t.Fatalf("POST create: %v", err)
	}

	// Appears in the list with email + active badge.
	status, body = getBody(t, client, srv.URL+"/admin/users")
	if status != http.StatusOK {
		t.Fatalf("list-after status = %d, want 200", status)
	}
	if !strings.Contains(body, "new@example.com") {
		t.Errorf("list missing created email: %.300q", body)
	}
	if !strings.Contains(body, "badge-status-active") {
		t.Errorf("list missing active status badge")
	}

	// Created active, with the two roles, in the DB.
	u, found, err := auth.GetUser(context.Background(), db, userIDByEmail(t, db, "new@example.com"))
	if err != nil || !found {
		t.Fatalf("GetUser: found=%v err=%v", found, err)
	}
	if u.Status != "active" {
		t.Errorf("status = %q, want active", u.Status)
	}
	if len(u.Roles) != 2 {
		t.Errorf("roles = %v, want 2", u.Roles)
	}

	// Detail page shows email, status, and the roles.
	status, body = getBody(t, client, srv.URL+"/admin/users/"+u.ID)
	if status != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", status)
	}
	for _, want := range []string{"new@example.com", "editor", "author"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail missing %q: %.400q", want, body)
		}
	}
}

// TestAdminUserCreateUnknownRoleRejected is a red-team: a crafted POST assigning
// a role not in the fixed catalog is rejected with 400 and creates no user.
func TestAdminUserCreateUnknownRoleRejected(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := adminClient(t, db, srv)

	resp, err := noRedirectJarClient(srv, client).PostForm(srv.URL+"/admin/users", url.Values{
		"email":    {"evil@example.com"},
		"password": {"pw"},
		"roles":    {"superadmin"}, // not in schema.Roles
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE email = ?`, "evil@example.com").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("user created despite unknown role: %d", n)
	}
}

// --- T3: change status + roles + 404 ----------------------------------------

// TestAdminUserStatusChange covers T3: an admin changes a user's status via
// hx-post; the server replies HX-Redirect and the DB reflects the new status.
func TestAdminUserStatusChange(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := adminClient(t, db, srv)
	created, err := auth.CreateUser(context.Background(), db, "target@example.com", "pw", []string{"author"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	resp := uiDo(t, client, http.MethodPost, srv.URL+"/admin/users/"+created.ID+"/status",
		url.Values{"status": {"suspended"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status change = %d, want 200", resp.StatusCode)
	}
	if loc := resp.Header.Get("HX-Redirect"); loc != "/admin/users/"+created.ID {
		t.Errorf("HX-Redirect = %q, want detail", loc)
	}
	resp.Body.Close()

	var got string
	if err := db.QueryRow(`SELECT status FROM users WHERE id = ?`, created.ID).Scan(&got); err != nil {
		t.Fatalf("select status: %v", err)
	}
	if got != "suspended" {
		t.Errorf("persisted status = %q, want suspended", got)
	}

	// Missing id → 404 HTML.
	resp = uiDo(t, client, http.MethodPost, srv.URL+"/admin/users/"+nonexistentID+"/status",
		url.Values{"status": {"active"}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status missing id = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(string(body), `"error"`) {
		t.Errorf("404 body looks like JSON")
	}
}

// TestAdminUserRolesChange covers T3: replacing a user's roles via hx-post is
// persisted, and a missing id → 404.
func TestAdminUserRolesChange(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := adminClient(t, db, srv)
	created, err := auth.CreateUser(context.Background(), db, "roles@example.com", "pw", []string{"author"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	resp := uiDo(t, client, http.MethodPost, srv.URL+"/admin/users/"+created.ID+"/roles",
		url.Values{"roles": {"editor", "contributor"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("roles change = %d, want 200", resp.StatusCode)
	}
	if loc := resp.Header.Get("HX-Redirect"); loc != "/admin/users/"+created.ID {
		t.Errorf("HX-Redirect = %q, want detail", loc)
	}
	resp.Body.Close()

	u, _, _ := auth.GetUser(context.Background(), db, created.ID)
	if !containsAll(u.Roles, "editor", "contributor") || len(u.Roles) != 2 {
		t.Errorf("roles = %v, want {editor, contributor}", u.Roles)
	}

	// Missing id → 404.
	resp = uiDo(t, client, http.MethodPost, srv.URL+"/admin/users/"+nonexistentID+"/roles",
		url.Values{"roles": {"editor"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("roles missing id = %d, want 404", resp.StatusCode)
	}
}

// TestAdminUserDetailMissingIs404 covers the detail-route 404 for a missing id.
func TestAdminUserDetailMissingIs404(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := adminClient(t, db, srv)

	status, body := getBody(t, client, srv.URL+"/admin/users/"+nonexistentID)
	if status != http.StatusNotFound {
		t.Fatalf("detail missing = %d, want 404", status)
	}
	if strings.Contains(body, `"error"`) {
		t.Errorf("404 detail body looks like JSON")
	}
}

// --- T4: roles/permissions read-only view -----------------------------------

// TestAdminRolesViewReflectsRealGrants covers T4: the view reads the live
// role_permissions table. We grant a specific permission to a role and confirm
// it appears (not a hardcoded catalog), and that the view needs only a session.
func TestAdminRolesViewReflectsRealGrants(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	// A plain session (author, no users.manage) can read the roles view.
	client := loginUI(t, db, srv, "viewer@example.com", "pw", "author")

	// Before granting: editor has no content.publish yet.
	_, before := getBody(t, client, srv.URL+"/admin/roles")
	editorBlock := roleBlock(before, "editor")
	if strings.Contains(editorBlock, "content.publish") {
		t.Fatalf("editor unexpectedly already shows content.publish: %q", editorBlock)
	}

	// Grant it in the real table, then confirm the view reflects it.
	grant(t, db, "editor", "content.publish")
	status, after := getBody(t, client, srv.URL+"/admin/roles")
	if status != http.StatusOK {
		t.Fatalf("roles view status = %d, want 200", status)
	}
	if !strings.Contains(after, "content.publish") {
		t.Errorf("roles view does not reflect granted permission: %.500q", after)
	}
	if !strings.Contains(roleBlock(after, "editor"), "content.publish") {
		t.Errorf("granted permission not shown under the editor row")
	}
}

// --- T5: round-trip login rejection + red-team gating -----------------------

// TestAdminUserRoundTripLoginRejection is the full T5 round-trip: create a user
// via the UI → assign a role → suspend via the UI → the suspended user's login
// (JSON /auth/login) is rejected with the generic message → reactivate → login
// succeeds. It proves the UI status toggle is wired to the real auth fix.
func TestAdminUserRoundTripLoginRejection(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	admin := adminClient(t, db, srv)

	// Create via UI.
	if _, err := admin.PostForm(srv.URL+"/admin/users", url.Values{
		"email":    {"rt@example.com"},
		"password": {"rt-pw"},
		"roles":    {"author"},
	}); err != nil {
		t.Fatalf("create via UI: %v", err)
	}
	uid := userIDByEmail(t, db, "rt@example.com")

	// The new user can log in (JSON) while active.
	if code := jsonLoginStatus(t, srv, "rt@example.com", "rt-pw"); code != http.StatusOK {
		t.Fatalf("active login status = %d, want 200", code)
	}

	// Suspend via the UI.
	uiDo(t, admin, http.MethodPost, srv.URL+"/admin/users/"+uid+"/status",
		url.Values{"status": {"suspended"}}).Body.Close()

	// Suspended → login rejected with the generic message (anti-enumeration).
	code, msg := jsonLoginStatusMsg(t, srv, "rt@example.com", "rt-pw")
	if code != http.StatusUnauthorized {
		t.Fatalf("suspended login status = %d, want 401", code)
	}
	unknownCode, unknownMsg := jsonLoginStatusMsg(t, srv, "ghost@example.com", "rt-pw")
	if unknownCode != http.StatusUnauthorized {
		t.Fatalf("unknown-user login = %d, want 401", unknownCode)
	}
	if msg != unknownMsg {
		t.Errorf("suspended msg %q differs from unknown-user msg %q — enumeration leak", msg, unknownMsg)
	}

	// Reactivate via the UI → login succeeds again.
	uiDo(t, admin, http.MethodPost, srv.URL+"/admin/users/"+uid+"/status",
		url.Values{"status": {"active"}}).Body.Close()
	if code := jsonLoginStatus(t, srv, "rt@example.com", "rt-pw"); code != http.StatusOK {
		t.Fatalf("reactivated login status = %d, want 200", code)
	}
}

// TestAdminUsersWriteWithoutPermissionServerSide is the red-team gating check: a
// valid session WITHOUT users.manage hitting the write routes directly (with the
// cookie, not via a button) → 403 HTML, and nothing changes server-side.
func TestAdminUsersWriteWithoutPermissionServerSide(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	// A target user for the status attempt.
	target, err := auth.CreateUser(context.Background(), db, "victim@example.com", "pw", []string{"author"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	// The attacker: author role, no users.manage.
	attacker := loginUI(t, db, srv, "attacker@example.com", "pw", "author")

	// POST create → 403 HTML.
	resp := uiDo(t, attacker, http.MethodPost, srv.URL+"/admin/users",
		url.Values{"email": {"sneak@example.com"}, "password": {"pw"}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("attacker create status = %d, want 403", resp.StatusCode)
	}
	if strings.Contains(string(body), `"error"`) {
		t.Errorf("403 body looks like JSON envelope")
	}
	if !strings.Contains(string(body), "Sin permiso") {
		t.Errorf("403 body missing HTML marker")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM users WHERE email = ?`, "sneak@example.com").Scan(&n)
	if n != 0 {
		t.Errorf("user created despite missing permission — gate not server-side")
	}

	// POST status directly → 403, status unchanged.
	resp = uiDo(t, attacker, http.MethodPost, srv.URL+"/admin/users/"+target.ID+"/status",
		url.Values{"status": {"suspended"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("attacker status change = %d, want 403", resp.StatusCode)
	}
	var st string
	db.QueryRow(`SELECT status FROM users WHERE id = ?`, target.ID).Scan(&st)
	if st != "active" {
		t.Errorf("status changed to %q despite missing permission", st)
	}
}

// --- local helpers ----------------------------------------------------------

// containsAll reports whether s contains every one of vs.
func containsAll(s []string, vs ...string) bool {
	for _, v := range vs {
		found := false
		for _, x := range s {
			if x == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// roleBlock returns the substring of the roles-view HTML for one role row, so a
// permission assertion is scoped to that role (the row is <tr id="role-NAME">…).
func roleBlock(html, role string) string {
	marker := `<tr id="role-` + role + `"`
	i := strings.Index(html, marker)
	if i < 0 {
		return ""
	}
	rest := html[i:]
	if end := strings.Index(rest, "</tr>"); end >= 0 {
		return rest[:end]
	}
	return rest
}
