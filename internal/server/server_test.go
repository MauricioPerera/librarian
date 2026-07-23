package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MauricioPerera/librarian/internal/server"
)

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
