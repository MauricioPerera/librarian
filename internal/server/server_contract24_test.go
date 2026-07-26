package server_test

// CONTRACT-24 — "alive" is not "ready to serve", and an infrastructure outage
// is not a credential failure.
//
// The regression these tests hold down was MEASURED in production with the
// PostgreSQL container stopped (docs/PENDIENTES.md, hueco 7): /health answered
// 200 {"status":"ok"}, POST /auth/login answered 401, and GET /content-types
// answered 401. The outage was invisible to monitoring and presented itself as
// "wrong credentials".
//
// The database is taken down here by CLOSING THE POOL — a real *sql.DB that no
// longer has a connection to give, not a stub that returns a canned error. The
// contract's closing proof (a genuinely stopped PostgreSQL container) is in
// docs/reports/CONTRACT-24-REPORT.md; this file is the suite-resident guard that
// keeps the behavior from drifting back.

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

// TestReadyIsGreenWhileTheDatabaseAnswers pins the healthy side of the new
// route, so a probe that returned 503 unconditionally could not pass.
func TestReadyIsGreenWhileTheDatabaseAnswers(t *testing.T) {
	_, srv, cleanup := openAuthMux(t)
	defer cleanup()

	status, body := doJSON(t, srv, http.MethodGet, "/ready", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /ready with a live database: status = %d, want 200", status)
	}
	if body["status"] != "ready" {
		t.Fatalf("GET /ready body = %v, want status=ready", body)
	}
}

// TestDatabaseDownIsReportedAsInfrastructure is the contract's core assertion,
// all four observations against ONE downed database.
func TestDatabaseDownIsReportedAsInfrastructure(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	ctx := context.Background()

	// A real, valid account exists — so a 503 below cannot be mistaken for
	// "there was nobody to log in as".
	createUser24(ctx, t, db, "alice@example.com", "correct-horse")

	// Take the database away.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// 1. /health KEEPS ITS MEANING: the process is alive, so it still says so,
	//    byte for byte. Monitoring pointed at it is not silently redefined.
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(raw) != `{"status":"ok"}` {
		t.Fatalf("GET /health with the database down: %d %q, want 200 {\"status\":\"ok\"}", resp.StatusCode, raw)
	}

	// 2. The availability probe reports the outage with a STATUS CODE (a
	//    balancer reads the status line, not a field in a 200 body).
	readyStatus, readyBody := doJSON(t, srv, http.MethodGet, "/ready", nil, nil)
	if readyStatus != http.StatusServiceUnavailable {
		t.Fatalf("GET /ready with the database down: status = %d, want 503", readyStatus)
	}
	assertNoInfraDetail24(t, "GET /ready", readyBody)

	// 3. Login is 5xx and NOT 401 — the defect that made an outage look like bad
	//    credentials. Asserted with the CORRECT password of a real user, which is
	//    the case where a 401 is most obviously a lie.
	loginStatus, loginBody := doJSON(t, srv, http.MethodPost, "/auth/login",
		map[string]any{"email": "alice@example.com", "password": "correct-horse"}, nil)
	if loginStatus == http.StatusUnauthorized {
		t.Fatal("POST /auth/login with the database down answered 401 — the regression this contract closes")
	}
	if loginStatus != http.StatusServiceUnavailable {
		t.Fatalf("POST /auth/login with the database down: status = %d, want 503", loginStatus)
	}
	assertNoInfraDetail24(t, "POST /auth/login", loginBody)

	// 4. An authenticated read route, reached with a bearer token that is not a
	//    JWT, so resolution falls through to the API-key lookup that needs the
	//    database. This is the GET /content-types -> 401 of the production
	//    measurement.
	readStatus, readBody := doJSON(t, srv, http.MethodGet, "/content-types", nil,
		map[string]string{"Authorization": "Bearer lbk_some-api-key-shaped-token"})
	if readStatus == http.StatusUnauthorized {
		t.Fatal("GET /content-types with the database down answered 401 — the regression this contract closes")
	}
	if readStatus != http.StatusServiceUnavailable {
		t.Fatalf("GET /content-types with the database down: status = %d, want 503", readStatus)
	}
	assertNoInfraDetail24(t, "GET /content-types", readBody)
}

// TestCredentialFailuresAreUnchangedAndIndistinguishable is the guard on the
// property the contract forbids weakening. With the database UP, an unknown
// email and a real email with a wrong password must still produce the same 401
// with the same body — the anti-enumeration collapse — and that body must be
// exactly the one shipped before this contract.
func TestCredentialFailuresAreUnchangedAndIndistinguishable(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	ctx := context.Background()

	createUser24(ctx, t, db, "alice@example.com", "correct-horse")

	wrongPassword, wrongBody := doJSON(t, srv, http.MethodPost, "/auth/login",
		map[string]any{"email": "alice@example.com", "password": "WRONG"}, nil)
	unknownUser, unknownBody := doJSON(t, srv, http.MethodPost, "/auth/login",
		map[string]any{"email": "ghost@example.com", "password": "WRONG"}, nil)

	if wrongPassword != http.StatusUnauthorized || unknownUser != http.StatusUnauthorized {
		t.Fatalf("credential failures: wrong password = %d, unknown user = %d, want 401 for both",
			wrongPassword, unknownUser)
	}
	// The exact envelope and message that shipped before CONTRACT-24, spelled
	// out here so a future edit to the message fails this test.
	if wrongBody["error"] != "invalid credentials" {
		t.Fatalf("wrong-password body = %v, want {\"error\":\"invalid credentials\"}", wrongBody)
	}
	if len(wrongBody) != len(unknownBody) || wrongBody["error"] != unknownBody["error"] {
		t.Fatalf("unknown user (%v) is distinguishable from wrong password (%v)", unknownBody, wrongBody)
	}
}

// TestBrowserLoginDoesNotBlameTheUserForAnOutage covers the UI half. The HTML
// login answered 401 with "Email o contraseña incorrectos" while the database
// was down, which sends a human off to reset a password that was never checked.
// It must now answer 5xx with a message that does not mention credentials —
// while a genuinely wrong password keeps the 401 and the original message.
func TestBrowserLoginDoesNotBlameTheUserForAnOutage(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	ctx := context.Background()

	createUser24(ctx, t, db, "alice@example.com", "correct-horse")

	// Database up, wrong password: unchanged 401 + the credential message.
	status, page := postLoginForm24(t, srv, "alice@example.com", "WRONG")
	if status != http.StatusUnauthorized {
		t.Fatalf("POST /login with a wrong password: status = %d, want 401", status)
	}
	if !strings.Contains(page, "Email o contraseña incorrectos") {
		t.Fatalf("POST /login with a wrong password did not render the credential message: %q", page)
	}

	// Database down: 5xx, and NOT the credential message.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	status, page = postLoginForm24(t, srv, "alice@example.com", "correct-horse")
	if status == http.StatusUnauthorized {
		t.Fatal("POST /login with the database down answered 401 — it blames the user for an outage")
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("POST /login with the database down: status = %d, want 503", status)
	}
	if strings.Contains(page, "Email o contraseña incorrectos") {
		t.Fatalf("POST /login with the database down still blames the credentials: %q", page)
	}
	if !strings.Contains(page, "no está disponible") {
		t.Fatalf("POST /login with the database down did not say the service is unavailable: %q", page)
	}
}

// postLoginForm24 submits the HTML login form (a real form POST, not JSON) and
// returns the status and the rendered page. Redirects are not followed, so a
// successful login's 303 would be visible rather than swallowed.
func postLoginForm24(t *testing.T, srv *httptest.Server, email, password string) (int, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	form := url.Values{"email": {email}, "password": {password}}
	resp, err := client.PostForm(srv.URL+"/login", form)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// createUser24 creates an active user with the editor role, so the login tests
// exercise a real account rather than an empty users table.
func createUser24(ctx context.Context, t *testing.T, db *sql.DB, email, password string) {
	t.Helper()
	if _, err := auth.CreateUser(ctx, storeFor(db), email, password, []string{"editor"}); err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
}

// assertNoInfraDetail24 enforces the contract's no-detail rule: an
// infrastructure response says one fixed thing and carries nothing about the
// driver, the DSN, the host, or whether the database exists.
func assertNoInfraDetail24(t *testing.T, what string, body map[string]any) {
	t.Helper()
	if body["error"] != "service unavailable" {
		t.Fatalf("%s body = %v, want exactly {\"error\":\"service unavailable\"}", what, body)
	}
	if len(body) != 1 {
		t.Fatalf("%s body carries extra fields: %v", what, body)
	}
}
