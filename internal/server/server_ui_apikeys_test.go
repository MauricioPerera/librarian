package server_test

// CONTRACT-09 acceptance tests: the admin API-keys UI (/admin/api-keys) — create
// (secret shown once), list (never the secret or hash), and revoke (in-place,
// idempotent, row stays). Every test that depends on the Secure session cookie
// uses the TLS server + cookie jar (openUITLS, loginUI, adminClient), the same
// reason as CONTRACT-06/07/08. Permission gating is asserted server-side,
// including red-team POSTs sent directly with the cookie.
//
// Shared helpers reused from the other server_test files: openUITLS, loginUI,
// adminClient, grant, getBody, uiDo, nonexistentID.

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// apiKeyIDByLabelDB resolves an api_keys row id by its label (newest first).
func apiKeyIDByLabelDB(t *testing.T, db *sql.DB, label string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM api_keys WHERE label = ? ORDER BY created_at DESC`, label).Scan(&id); err != nil {
		t.Fatalf("lookup api key %q: %v", label, err)
	}
	return id
}

// keyHashByLabel returns the stored key_hash for a label, so a test can assert
// the hash never appears in any listing HTML.
func keyHashByLabel(t *testing.T, db *sql.DB, label string) string {
	t.Helper()
	var h string
	if err := db.QueryRow(`SELECT key_hash FROM api_keys WHERE label = ? ORDER BY created_at DESC`, label).Scan(&h); err != nil {
		t.Fatalf("lookup key_hash %q: %v", label, err)
	}
	return h
}

// extractSecret pulls the "lbk_..." plaintext secret out of the success-page
// HTML. The secret is rendered inside <code>lbk_XXXX</code>, so it ends at the
// first character that cannot be part of a base64url secret.
func extractSecret(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "lbk_")
	if i < 0 {
		t.Fatalf("no lbk_ secret found in success page: %.400q", body)
	}
	rest := body[i:]
	end := strings.IndexAny(rest, "<\"' \n\t\r")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// createKeyUI creates a key through the real POST form and returns the response
// body (the success page containing the secret).
func createKeyUI(t *testing.T, client *http.Client, srv *httptest.Server, label, role string) string {
	t.Helper()
	resp, err := client.PostForm(srv.URL+"/admin/api-keys", url.Values{"label": {label}, "role": {role}})
	if err != nil {
		t.Fatalf("POST create key: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create key status = %d, want 200; body %.300q", resp.StatusCode, body)
	}
	return string(body)
}

// --- T2: create shows the secret once; it never reappears --------------------

// TestAdminAPIKeyCreateShowsSecretOnce covers T2: creating a key via the UI
// renders the plaintext secret ONCE with a clear warning, and neither the secret
// nor the key_hash ever appears in the listing HTML.
func TestAdminAPIKeyCreateShowsSecretOnce(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := adminClient(t, db, srv)

	body := createKeyUI(t, client, srv, "ci-key", "editor")

	// Secret shown once, with the warning marker.
	secret := extractSecret(t, body)
	if !strings.HasPrefix(secret, "lbk_") {
		t.Fatalf("secret does not look like a librarian key: %q", secret)
	}
	if !strings.Contains(body, "única vez") {
		t.Errorf("success page missing the one-time warning")
	}

	// The listing shows the label + role, but NEVER the secret or the hash.
	status, list := getBody(t, client, srv.URL+"/admin/api-keys")
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}
	if !strings.Contains(list, "ci-key") || !strings.Contains(list, "editor") {
		t.Errorf("listing missing label/role: %.400q", list)
	}
	if strings.Contains(list, "lbk_") {
		t.Errorf("listing HTML contains a plaintext secret prefix — leak")
	}
	if strings.Contains(list, secret) {
		t.Errorf("listing HTML contains the exact secret — leak")
	}
	if hash := keyHashByLabel(t, db, "ci-key"); strings.Contains(list, hash) {
		t.Errorf("listing HTML contains the key_hash — leak")
	}

	// The new-form and the list page (the only other GET routes) also never
	// contain a secret prefix.
	_, newForm := getBody(t, client, srv.URL+"/admin/api-keys/new")
	if strings.Contains(newForm, "lbk_") {
		t.Errorf("new-form HTML contains a secret prefix — leak")
	}
}

// TestAdminAPIKeyCreateUnknownRoleRejected is a red-team: a crafted POST with a
// role not in the fixed catalog is rejected 400 and mints no key.
func TestAdminAPIKeyCreateUnknownRoleRejected(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := adminClient(t, db, srv)

	resp := uiDo(t, client, http.MethodPost, srv.URL+"/admin/api-keys",
		url.Values{"label": {"evil"}, "role": {"superadmin"}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if strings.Contains(string(body), "lbk_") {
		t.Errorf("rejected create still rendered a secret")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE label = ?`, "evil").Scan(&n)
	if n != 0 {
		t.Errorf("key minted despite unknown role: %d", n)
	}
}

// --- T3 + T4: round-trip create → use → revoke → reject ----------------------

// TestAdminAPIKeyRoundTrip is the full T4 round-trip: create a key via the UI,
// capture its secret from the success page, use it as a real Bearer token
// against GET /whoami (200, "auth":"apikey"), revoke it via the UI (in-place row
// swap → Revocada, row stays), then the same /whoami call is rejected (401).
func TestAdminAPIKeyRoundTrip(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := adminClient(t, db, srv)

	body := createKeyUI(t, client, srv, "rt-key", "editor")
	secret := extractSecret(t, body)

	// Use the secret as a real Bearer token against the JSON /whoami.
	whoami := func() (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("whoami: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	code, who := whoami()
	if code != http.StatusOK {
		t.Fatalf("whoami pre-revoke status = %d, want 200; body %s", code, who)
	}
	if !strings.Contains(who, `"auth":"apikey"`) || !strings.Contains(who, "rt-key") {
		t.Errorf("whoami body not an apikey identity: %s", who)
	}

	// Revoke via the UI (hx-post → single updated row fragment).
	id := apiKeyIDByLabelDB(t, db, "rt-key")
	resp := uiDo(t, client, http.MethodPost, srv.URL+"/admin/api-keys/"+id+"/revoke", nil)
	rowBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(rowBody), "Revocada") {
		t.Errorf("revoke fragment does not show Revocada: %.300q", rowBody)
	}
	if strings.Contains(string(rowBody), "lbk_") {
		t.Errorf("revoke fragment leaked a secret")
	}

	// The same call is now rejected (revoked → 401 ErrAPIKeyRejected path).
	code, _ = whoami()
	if code != http.StatusUnauthorized {
		t.Fatalf("whoami post-revoke status = %d, want 401", code)
	}

	// The revoked key still appears in the listing (historical record), marked
	// revoked — it did not disappear.
	_, list := getBody(t, client, srv.URL+"/admin/api-keys")
	if !strings.Contains(list, "rt-key") {
		t.Errorf("revoked key disappeared from listing")
	}
	if !strings.Contains(list, "Revocada") {
		t.Errorf("listing does not mark the revoked key")
	}
}

// TestAdminAPIKeyRevokeIdempotentAndMissing covers idempotency and the 404 path:
// revoking the same key twice is a no-op success both times, and revoking a
// missing id → 404 HTML (never a 500 or JSON).
func TestAdminAPIKeyRevokeIdempotentAndMissing(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := adminClient(t, db, srv)

	createKeyUI(t, client, srv, "twice", "author")
	id := apiKeyIDByLabelDB(t, db, "twice")

	for i := 0; i < 2; i++ {
		resp := uiDo(t, client, http.MethodPost, srv.URL+"/admin/api-keys/"+id+"/revoke", nil)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("revoke #%d status = %d, want 200", i+1, resp.StatusCode)
		}
		if !strings.Contains(string(b), "Revocada") {
			t.Errorf("revoke #%d fragment not Revocada", i+1)
		}
	}

	// Missing id → 404 HTML.
	resp := uiDo(t, client, http.MethodPost, srv.URL+"/admin/api-keys/"+nonexistentID+"/revoke", nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke missing id = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(string(b), `"error"`) {
		t.Errorf("404 body looks like JSON")
	}
}

// --- Red-team: write routes gated server-side by users.manage ----------------

// TestAdminAPIKeysWriteWithoutPermission is the red-team gating check: a valid
// session WITHOUT users.manage hitting the create/revoke routes directly (with
// the cookie, not via a button) → 403 HTML, and nothing changes server-side.
func TestAdminAPIKeysWriteWithoutPermission(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()

	// A legit key created by an admin, to be the revoke target.
	admin := adminClient(t, db, srv)
	createKeyUI(t, admin, srv, "legit", "editor")
	id := apiKeyIDByLabelDB(t, db, "legit")

	// The attacker: author role, no users.manage.
	attacker := loginUI(t, db, srv, "attacker@example.com", "pw", "author")

	// POST create → 403 HTML, no key minted.
	resp := uiDo(t, attacker, http.MethodPost, srv.URL+"/admin/api-keys",
		url.Values{"label": {"sneak"}, "role": {"editor"}})
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
	db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE label = ?`, "sneak").Scan(&n)
	if n != 0 {
		t.Errorf("key minted despite missing permission — gate not server-side")
	}

	// POST revoke directly → 403, the target key stays active.
	resp = uiDo(t, attacker, http.MethodPost, srv.URL+"/admin/api-keys/"+id+"/revoke", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("attacker revoke status = %d, want 403", resp.StatusCode)
	}
	var revoked sql.NullString
	db.QueryRow(`SELECT revoked_at FROM api_keys WHERE id = ?`, id).Scan(&revoked)
	if revoked.Valid && revoked.String != "" {
		t.Errorf("key revoked despite missing permission — gate not server-side")
	}
}

// TestAdminAPIKeysNoSessionRedirects confirms the read + write routes with no
// session redirect to /login (302), never a 401/JSON.
func TestAdminAPIKeysNoSessionRedirects(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := noRedirectClient(srv)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/api-keys"},
		{http.MethodGet, "/admin/api-keys/new"},
		{http.MethodPost, "/admin/api-keys"},
		{http.MethodPost, "/admin/api-keys/" + nonexistentID + "/revoke"},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("%s %s status = %d, want 302", tc.method, tc.path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Errorf("%s %s Location = %q, want /login", tc.method, tc.path, loc)
		}
	}
}
