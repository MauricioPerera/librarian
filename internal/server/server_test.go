package server_test

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// storeFor pairs a test's *sql.DB handle with the engine it is bound to, which
// is what internal/auth takes since CONTRACT-19 (a bare *sql.DB cannot answer
// "which engine is this?", and QueryRoutine/CallRoutine/Placeholder all need
// the answer). Every test database in this package is the embedded SQLite file
// store.Open creates, so the target is schema.SQLiteTarget — the same pairing
// server.NewMux performs internally.
func storeFor(db *sql.DB) *compat.Store {
	return &compat.Store{Target: schema.SQLiteTarget, DB: db}
}

// TestHealth covers the HTTP acceptance criterion: GET /health → 200 with body
// {"status":"ok"}. Uses a real httptest server + client (no lingering
// foreground process).
func TestHealth(t *testing.T) {
	mux, err := server.NewMux(server.Deps{JWTSecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); got != `{"status":"ok"}` {
		t.Fatalf("body = %q, want {\"status\":\"ok\"}", got)
	}
}

// TestNewMuxRejectsEmptySecret covers the fail-closed invariant at the handler
// layer: NewMux refuses to build when the JWT secret is empty, so the server
// cannot be wired without one. (The same invariant is enforced at startup by
// config.Load; this guards the handler construction path independently.)
func TestNewMuxRejectsEmptySecret(t *testing.T) {
	if _, err := server.NewMux(server.Deps{JWTSecret: ""}); err == nil {
		t.Fatal("expected NewMux to reject an empty JWT secret, got nil")
	}
}
