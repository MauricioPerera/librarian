package server_test

// CONTRACT-06 acceptance tests for the browser-facing UI. Anything that depends
// on the Secure session cookie surviving a round-trip uses httptest.NewTLSServer
// + srv.Client(): a Secure cookie is dropped by net/http/cookiejar over plain
// HTTP, so a NewServer (HTTP) test would never see the cookie return and would
// fail for a non-bug reason.

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
)

// openUITLS mirrors openAuthMux but returns an HTTPS test server, required for
// any test exercising the Secure session cookie.
func openUITLS(t *testing.T) (*sql.DB, *httptest.Server, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "serverui.db")
	sdb, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if err := store.EnsureSchema(ctx, sdb); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.SeedCatalogs(ctx, sdb.DB); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux, err := server.NewMux(server.Deps{DB: sdb.DB, JWTSecret: testSecret})
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewTLSServer(mux)
	cleanup := func() {
		srv.Close()
		_ = sdb.Close()
	}
	return sdb.DB, srv, cleanup
}

// noRedirectClient returns the TLS-trusting client for srv but with redirect
// following disabled, so a test can inspect the 3xx status, Location, and any
// Set-Cookie header directly.
func noRedirectClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return c
}

// findCookie returns the named cookie from a response, or nil.
func findCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// --- T1: embedded static assets --------------------------------------------

// TestUIStaticAssetsEmbedded confirms the binary serves the JS and CSS from its
// own embedded copy with the correct Content-Type and no runtime network call.
// The served bytes are compared to the on-disk vendored file to prove it is the
// real embedded asset, not a proxied/fetched one.
func TestUIStaticAssetsEmbedded(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()
	client := srv.Client()

	// htmx.min.js
	resp, err := client.Get(srv.URL + "/static/htmx.min.js")
	if err != nil {
		t.Fatalf("get js: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("js status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("js Content-Type = %q", ct)
	}
	if !strings.HasPrefix(string(body), "var htmx=") {
		t.Errorf("js body does not look like htmx: %.40q", string(body))
	}
	onDisk, err := os.ReadFile("assets/htmx.min.js")
	if err != nil {
		t.Fatalf("read vendored htmx: %v", err)
	}
	if string(body) != string(onDisk) {
		t.Errorf("served js differs from vendored file (%d vs %d bytes)", len(body), len(onDisk))
	}

	// app.css
	resp, err = client.Get(srv.URL + "/static/app.css")
	if err != nil {
		t.Fatalf("get css: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("css status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("css Content-Type = %q", ct)
	}
	if len(body) == 0 {
		t.Error("css body empty")
	}
}

// --- T2: login / logout -----------------------------------------------------

// TestUILoginSuccessSetsCookie covers the valid-login path: a form POST yields a
// 303 to / and a session cookie flagged HttpOnly + Secure + SameSite=Strict,
// Path=/.
func TestUILoginSuccessSetsCookie(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	if _, err := auth.CreateUser(context.Background(), db, "ui@example.com", "hunter2", []string{"editor"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	form := url.Values{"email": {"ui@example.com"}, "password": {"hunter2"}}
	resp, err := noRedirectClient(srv).PostForm(srv.URL+"/login", form)
	if err != nil {
		t.Fatalf("post login: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
	c := findCookie(resp, "librarian_session")
	if c == nil {
		t.Fatal("no librarian_session cookie set")
	}
	if c.Value == "" {
		t.Error("session cookie value empty")
	}
	if !c.HttpOnly {
		t.Error("cookie not HttpOnly")
	}
	if !c.Secure {
		t.Error("cookie not Secure")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
}

// TestUILoginInvalidGenericError covers the anti-enumeration criterion: wrong
// password AND unknown user re-render the form with the SAME generic message and
// set no cookie.
func TestUILoginInvalidGenericError(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	if _, err := auth.CreateUser(context.Background(), db, "real@example.com", "correct", []string{"author"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	client := noRedirectClient(srv)

	// Wrong password for an existing user.
	resp, err := client.PostForm(srv.URL+"/login", url.Values{"email": {"real@example.com"}, "password": {"WRONG"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	wrongBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if findCookie(resp, "librarian_session") != nil {
		t.Error("cookie set on failed login (wrong password)")
	}

	// Unknown user.
	resp, err = client.PostForm(srv.URL+"/login", url.Values{"email": {"ghost@example.com"}, "password": {"WRONG"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	unknownBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if findCookie(resp, "librarian_session") != nil {
		t.Error("cookie set on failed login (unknown user)")
	}

	const want = "Email o contraseña incorrectos."
	if !strings.Contains(string(wrongBody), want) {
		t.Errorf("wrong-password body missing generic error")
	}
	if !strings.Contains(string(unknownBody), want) {
		t.Errorf("unknown-user body missing generic error")
	}
	// Both bodies must be byte-identical — any divergence is an enumeration leak.
	if string(wrongBody) != string(unknownBody) {
		t.Error("wrong-password and unknown-user responses differ — enumeration leak")
	}
}

// TestUILogoutClearsCookie covers logout: a deletion cookie (MaxAge < 0) and a
// 303 to /login.
func TestUILogoutClearsCookie(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/logout", nil)
	resp, err := noRedirectClient(srv).Do(req)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
	c := findCookie(resp, "librarian_session")
	if c == nil {
		t.Fatal("no librarian_session cookie on logout")
	}
	if c.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want < 0 (deletion)", c.MaxAge)
	}
}

// --- T3: protected home -----------------------------------------------------

// TestUIHomeNoCookieRedirects covers the unauthenticated home: no cookie → 302
// to /login (never a 401 JSON).
func TestUIHomeNoCookieRedirects(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()

	resp, err := noRedirectClient(srv).Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

// TestUIHomeInvalidCookieRedirects covers a garbage cookie value → 302 to
// /login, same as absent.
func TestUIHomeInvalidCookieRedirects(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: "librarian_session", Value: "not-a-jwt"})
	resp, err := noRedirectClient(srv).Do(req)
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

// --- T4: full round-trip ----------------------------------------------------

// TestUIRoundTrip is the end-to-end criterion: POST /login (valid) → cookie →
// follow redirect → GET / authenticated → 200 with the caller's email.
func TestUIRoundTrip(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	if _, err := auth.CreateUser(context.Background(), db, "round@example.com", "trip-pw", []string{"editor"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	jar, _ := cookiejar.New(nil)
	client := srv.Client()
	client.Jar = jar // follow redirects (default) so login → / happens in one call

	resp, err := client.PostForm(srv.URL+"/login", url.Values{"email": {"round@example.com"}, "password": {"trip-pw"}})
	if err != nil {
		t.Fatalf("login round-trip: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200 (home)", resp.StatusCode)
	}
	if !strings.Contains(string(body), "round@example.com") {
		t.Errorf("home body does not confirm identity (email missing)")
	}

	// And a direct GET / with the jar's cookie also lands on the home page.
	resp, err = client.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get / with cookie: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "round@example.com") {
		t.Errorf("GET / body missing email")
	}
}

// --- Red-team: forged and expired session cookies ---------------------------

// TestUIForgedJWTCookieRejected puts a JWT signed with a DIFFERENT secret in the
// session cookie. VerifyJWT must reject it by signature — the result is a clean
// 302 to /login, not a panic or a 500.
func TestUIForgedJWTCookieRejected(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()

	forged, err := auth.IssueJWT("a-totally-different-secret", &auth.User{
		ID: "u1", Email: "attacker@example.com", Roles: []string{"admin"},
	}, time.Now())
	if err != nil {
		t.Fatalf("forge token: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: "librarian_session", Value: forged})
	resp, err := noRedirectClient(srv).Do(req)
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 (forged rejected)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

// TestUIExpiredJWTCookieRejected puts a correctly-signed but EXPIRED JWT in the
// cookie (issued 48h ago, 24h TTL). It must be treated exactly like an absent
// cookie: 302 to /login.
func TestUIExpiredJWTCookieRejected(t *testing.T) {
	_, srv, cleanup := openUITLS(t)
	defer cleanup()

	expired, err := auth.IssueJWT(testSecret, &auth.User{
		ID: "u1", Email: "stale@example.com", Roles: []string{"editor"},
	}, time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("issue expired: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: "librarian_session", Value: expired})
	resp, err := noRedirectClient(srv).Do(req)
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 (expired rejected)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

// --- Regression: JSON API routes unaffected by the UI addition --------------

// TestUIJSONRoutesUnaffected confirms the pre-existing JSON surface behaves
// exactly as before with the header-based Authorization flow and NO cookie:
// POST /auth/login returns a token, GET /whoami echoes the JWT identity. This is
// the explicit "existing routes still work" check the contract asks for.
func TestUIJSONRoutesUnaffected(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := auth.CreateUser(ctx, db, "api@example.com", "api-pw", []string{"editor", "author"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	client := srv.Client()

	// POST /auth/login (JSON) → token.
	loginResp, err := client.Post(srv.URL+"/auth/login", "application/json",
		strings.NewReader(`{"email":"api@example.com","password":"api-pw"}`))
	if err != nil {
		t.Fatalf("json login: %v", err)
	}
	loginBody, _ := io.ReadAll(loginResp.Body)
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("json login status = %d, body = %s", loginResp.StatusCode, loginBody)
	}
	// No session cookie must be set by the JSON login path.
	if findCookie(loginResp, "librarian_session") != nil {
		t.Error("JSON /auth/login unexpectedly set a session cookie")
	}
	// Extract token without a JSON dep beyond the stdlib already imported.
	tokenMarker := `"token":"`
	idx := strings.Index(string(loginBody), tokenMarker)
	if idx < 0 {
		t.Fatalf("no token in JSON login body: %s", loginBody)
	}
	rest := string(loginBody)[idx+len(tokenMarker):]
	token := rest[:strings.IndexByte(rest, '"')]

	// GET /whoami with the header (no cookie) → JWT identity echoed.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	whoResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	whoBody, _ := io.ReadAll(whoResp.Body)
	whoResp.Body.Close()
	if whoResp.StatusCode != http.StatusOK {
		t.Fatalf("whoami status = %d, body = %s", whoResp.StatusCode, whoBody)
	}
	for _, want := range []string{`"auth":"jwt"`, `"email":"api@example.com"`} {
		if !strings.Contains(string(whoBody), want) {
			t.Errorf("whoami body missing %s: %s", want, whoBody)
		}
	}
}
