package server_test

// CONTRACT-22 T3 — the end-to-end proof, on SQLite, in the DEFAULT suite.
//
// The same demonstration as the dual-engine battery
// (dualengine_contract22_test.go, which additionally runs it against a real
// PostgreSQL 17), kept here without a build tag so `go test ./...` protects it:
// a clean install refuses a permission-gated write, the bootstrap runs, and the
// SAME write succeeds over real HTTP with a token obtained from the real login
// route.
//
// The control at the end is what makes the 201 mean something: a user with no
// role still gets 403 on the same route, so the success is an authorization
// decision and not an ungated endpoint.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// cleanInstall22 is a brand new database taken through the PRODUCTION startup
// path: open, EnsureSchema, SeedCatalogs. Nothing else — which is precisely the
// state the contract measured as inadministrable.
func cleanInstall22(t *testing.T) (*compat.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(compat.SQLite, filepath.Join(t.TempDir(), "c22.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := store.EnsureSchema(ctx, st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if err := store.SeedCatalogs(ctx, st); err != nil {
		t.Fatalf("seed catalogs: %v", err)
	}
	mux, err := server.NewMux(server.Deps{Store: st, JWTSecret: "contract-22-secret"})
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return st, srv
}

// request22 performs one HTTP request against the test server and returns the
// status plus the decoded body.
func request22(t *testing.T, srv *httptest.Server, method, path, token string, payload any) (int, map[string]any) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	decoded := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode, decoded
}

// login22 obtains a bearer token through the real POST /auth/login route.
func login22(t *testing.T, srv *httptest.Server, email, password string) (int, string) {
	t.Helper()
	status, body := request22(t, srv, http.MethodPost, "/auth/login", "",
		map[string]any{"email": email, "password": password})
	if status != http.StatusOK {
		return status, ""
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("login 200 with no token: %v", body)
	}
	return status, token
}

// TestBootstrapUnblocksGatedWritesOverHTTP is the acceptance criterion of the
// contract, executed.
func TestBootstrapUnblocksGatedWritesOverHTTP(t *testing.T) {
	st, srv := cleanInstall22(t)
	ctx := context.Background()

	// --- before: nobody can even authenticate --------------------------------
	if status, _ := login22(t, srv, "admin@example.com", "correct-horse"); status != http.StatusUnauthorized {
		t.Fatalf("login on a clean install: status=%d, want 401", status)
	}
	if status, _ := request22(t, srv, http.MethodPost, "/articles", "", map[string]any{"title": "T", "body": "B"}); status != http.StatusUnauthorized {
		t.Fatalf("gated write with no identity: status=%d, want 401", status)
	}

	// --- the bootstrap -------------------------------------------------------
	result, err := auth.Bootstrap(ctx, st, "admin@example.com", "correct-horse")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// --- after: enter by HTTP and perform the writes that were impossible ----
	status, token := login22(t, srv, "admin@example.com", "correct-horse")
	if status != http.StatusOK {
		t.Fatalf("login after bootstrap: status=%d, want 200", status)
	}

	status, body := request22(t, srv, http.MethodPost, "/articles", token,
		map[string]any{"title": "Primera", "body": "Cuerpo"})
	if status != http.StatusCreated {
		t.Fatalf("POST /articles (content.create) after bootstrap: status=%d body=%v, want 201", status, body)
	}
	articleID, _ := body["id"].(string)
	if articleID == "" {
		t.Fatalf("POST /articles returned no id: %v", body)
	}

	// The route that answered 403 in PRODUCTION with role_permissions empty.
	status, body = request22(t, srv, http.MethodPost, "/content-types", token, map[string]any{
		"name": "resenas22",
		"fields": []map[string]string{
			{"name": "titular", "type": "text"},
			{"name": "puntaje", "type": "integer"},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /content-types (content_types.manage) after bootstrap: status=%d body=%v, want 201", status, body)
	}

	// And THE TOOL THAT WAS UNREACHABLE: the CONTRACT-16 role editor, gated by
	// roles.manage — the permission whose absence made the fix for hueco 1
	// impossible to apply. It is exercised for real, through the browser session
	// flow, and the resulting grant is read back with direct SQL.
	admin := loginExisting(t, srv, "admin@example.com", "correct-horse")
	resp := postPerms(t, admin, srv, "editor", "content.create")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /admin/roles/editor/permissions (roles.manage) after bootstrap: status=%d, want 303", resp.StatusCode)
	}
	if got := dbGrants(t, st.DB, "editor"); !sameSet(got, "content.create") {
		t.Fatalf("the role editor UI did not persist the grant: editor holds %v", got)
	}

	// --- the control: the gate is real ---------------------------------------
	if _, err := auth.CreateUser(ctx, st, "sinrol@example.com", "sin-rol-clave", nil); err != nil {
		t.Fatalf("create roleless user: %v", err)
	}
	_, roleless := login22(t, srv, "sinrol@example.com", "sin-rol-clave")
	if status, body := request22(t, srv, http.MethodPost, "/articles", roleless,
		map[string]any{"title": "X", "body": "Y"}); status != http.StatusForbidden {
		t.Fatalf("CONTROL: a roleless user wrote an article: status=%d body=%v, want 403", status, body)
	}

	t.Logf("HOLE CLOSED: clean install -> bootstrap(%s, %d grants) -> HTTP login -> 201 POST /articles (%s), 201 POST /content-types, 303 granting a permission through the roles.manage UI; a roleless user still gets 403",
		result.Email, len(result.Permissions), articleID)
}

// TestGatedWriteIsImpossibleWithoutBootstrap is the other half of the same
// measurement, and the one that proves the fixture is discriminating: on a clean
// install a user created with the administrator role — which is what the old
// hand-rolled deploy helper did — is refused, because the ROLE holds nothing.
// This is the production incident, reproduced over HTTP.
func TestGatedWriteIsImpossibleWithoutBootstrap(t *testing.T) {
	st, srv := cleanInstall22(t)
	ctx := context.Background()

	if _, err := auth.CreateUser(ctx, st, "admin@example.com", "correct-horse", []string{auth.BootstrapRole}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	status, token := login22(t, srv, "admin@example.com", "correct-horse")
	if status != http.StatusOK {
		t.Fatalf("login: status=%d", status)
	}
	// Reading works — which is exactly why the incident stayed invisible for
	// weeks: every verification performed in production had been a read.
	if status, _ := request22(t, srv, http.MethodGet, "/articles", token, nil); status != http.StatusOK {
		t.Fatalf("GET /articles: status=%d, want 200", status)
	}
	for _, route := range []string{"/articles", "/content-types", "/terms"} {
		status, body := request22(t, srv, http.MethodPost, route, token, map[string]any{"title": "T", "body": "B", "name": "x", "taxonomy": "tag", "slug": "x"})
		if status != http.StatusForbidden {
			t.Fatalf("POST %s on an unbootstrapped install: status=%d body=%v, want 403", route, status, body)
		}
	}
	if got := len(schema.Permissions); got == 0 {
		t.Fatal("the permission catalog is empty; this test would be vacuous")
	}
	t.Logf("MEASURED: an administrator on an unbootstrapped install READS fine and is 403 on every gated write (the production incident)")
}
