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
	srv := httptest.NewServer(server.NewMux())
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
