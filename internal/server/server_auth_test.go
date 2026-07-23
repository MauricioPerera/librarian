package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
)

// timeNowUTC returns the current time in UTC — a thin helper so JWT issuance in
// tests reads consistently and is easy to swap for a fixed clock if needed.
func timeNowUTC() time.Time {
	return time.Now().UTC()
}

const testSecret = "contract-02-test-jwt-secret"

// openAuthMux opens a temp DB, applies schema + seeds the role catalog, and
// returns the *sql.DB handle, a ready-to-serve httptest.Server wired with the
// auth routes, and a cleanup func that closes both. Tests MUST defer the
// cleanup so the SQLite file handle is released before t.TempDir's RemoveAll
// runs (otherwise Windows refuses to delete the locked file).
func openAuthMux(t *testing.T) (*sql.DB, *httptest.Server, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "serverauth.db")
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
	srv := httptest.NewServer(mux)
	cleanup := func() {
		srv.Close()
		_ = sdb.Close()
	}
	return sdb.DB, srv, cleanup
}

// roleID looks up a role id by name.
func roleID(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM roles WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatalf("lookup role %q: %v", name, err)
	}
	return id
}

// doJSON sends a request with a JSON body and returns the status + decoded
// envelope. body may be nil.
func doJSON(t *testing.T, srv *httptest.Server, method, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// --- T3: POST /auth/login ---------------------------------------------------

// TestLoginSuccess covers the login acceptance criterion: valid credentials
// return 200 + a JWT that parses with the expected claims.
func TestLoginSuccess(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := auth.CreateUser(ctx, db, "alice@example.com", "correct-horse", []string{"editor", "author"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	status, body := doJSON(t, srv, http.MethodPost, "/auth/login",
		map[string]string{"email": "alice@example.com", "password": "correct-horse"}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v, want 200", status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("no token in response: %v", body)
	}

	// The JWT must be parseable with the expected claims.
	claims, err := auth.VerifyJWT(testSecret, token)
	if err != nil {
		t.Fatalf("verify jwt: %v", err)
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("email claim = %q", claims.Email)
	}
	if len(claims.Roles) != 2 {
		t.Errorf("roles claim = %v, want 2", claims.Roles)
	}
}

// TestLoginInvalidCredentials covers the 401 path: wrong password AND unknown
// user both return 401 with the same generic error envelope.
func TestLoginInvalidCredentials(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := auth.CreateUser(ctx, db, "bob@example.com", "s3cret", []string{"author"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Wrong password.
	status, body := doJSON(t, srv, http.MethodPost, "/auth/login",
		map[string]string{"email": "bob@example.com", "password": "WRONG"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong-password status = %d, want 401", status)
	}
	wrongMsg, _ := body["error"].(string)

	// Unknown user.
	status, body = doJSON(t, srv, http.MethodPost, "/auth/login",
		map[string]string{"email": "ghost@example.com", "password": "WRONG"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unknown-user status = %d, want 401", status)
	}
	missingMsg, _ := body["error"].(string)

	if wrongMsg != missingMsg {
		t.Fatalf("error differs — enumeration leak: wrong=%q missing=%q", wrongMsg, missingMsg)
	}
	if wrongMsg == "" {
		t.Fatal("error envelope missing")
	}
}

// --- T5: GET /whoami --------------------------------------------------------

// TestWhoamiJWT covers the JWT path of the demo endpoint.
func TestWhoamiJWT(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := auth.CreateUser(ctx, db, "carol@example.com", "pw", []string{"editor"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	user, err := auth.VerifyCredentials(ctx, db, "carol@example.com", "pw")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	token, err := auth.IssueJWT(testSecret, user, timeNowUTC())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	status, body := doJSON(t, srv, http.MethodGet, "/whoami", nil,
		map[string]string{"Authorization": "Bearer " + token})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v, want 200", status, body)
	}
	if body["auth"] != "jwt" {
		t.Errorf("auth = %v, want jwt", body["auth"])
	}
	if body["email"] != "carol@example.com" {
		t.Errorf("email = %v", body["email"])
	}
	if body["user_id"] != user.ID {
		t.Errorf("user_id = %v, want %q", body["user_id"], user.ID)
	}
}

// TestWhoamiAPIKey covers the API-key path of the demo endpoint.
func TestWhoamiAPIKey(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	ctx := context.Background()

	rid := roleID(t, db, "editor")
	secret, err := auth.MintAPIKey(ctx, db, "ci-runner", rid)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	status, body := doJSON(t, srv, http.MethodGet, "/whoami", nil,
		map[string]string{"Authorization": "Bearer " + secret})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v, want 200", status, body)
	}
	if body["auth"] != "apikey" {
		t.Errorf("auth = %v, want apikey", body["auth"])
	}
	if body["label"] != "ci-runner" {
		t.Errorf("label = %v, want ci-runner", body["label"])
	}
	if body["role_id"] != rid {
		t.Errorf("role_id = %v, want %q", body["role_id"], rid)
	}
}

// TestWhoamiRevokedAPIKeyRejected confirms a revoked key hits 401 even though
// the format is valid.
func TestWhoamiRevokedAPIKeyRejected(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	ctx := context.Background()

	rid := roleID(t, db, "editor")
	secret, err := auth.MintAPIKey(ctx, db, "revoked-key", rid)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := auth.RevokeAPIKey(ctx, db, secret); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	status, _ := doJSON(t, srv, http.MethodGet, "/whoami", nil,
		map[string]string{"Authorization": "Bearer " + secret})
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked-key status = %d, want 401", status)
	}
}

// TestWhoamiNoCredentials covers the 401 path: no Authorization header.
func TestWhoamiNoCredentials(t *testing.T) {
	_, srv, cleanup := openAuthMux(t)
	defer cleanup()

	status, _ := doJSON(t, srv, http.MethodGet, "/whoami", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("no-cred status = %d, want 401", status)
	}
}

// TestWhoamiGarbageToken covers a malformed bearer value that is neither a
// valid JWT nor a known API key.
func TestWhoamiGarbageToken(t *testing.T) {
	_, srv, cleanup := openAuthMux(t)
	defer cleanup()

	status, _ := doJSON(t, srv, http.MethodGet, "/whoami", nil,
		map[string]string{"Authorization": "Bearer not-a-jwt-or-key"})
	if status != http.StatusUnauthorized {
		t.Fatalf("garbage status = %d, want 401", status)
	}
}
