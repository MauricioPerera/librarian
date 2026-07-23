// Package server builds the librarian HTTP handler. In this first contract it
// only exposes GET /health; it is the skeleton onto which later contracts hang
// auth and CRUD routes.
package server

import (
	"net/http"
)

// NewMux returns the HTTP handler for librarian. Routes use stdlib
// method+pattern matching (Go 1.22+ ServeMux) — no external router, sufficient
// for this base.
func NewMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	return mux
}

// handleHealth answers 200 {"status":"ok"}.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Fixed literal body; no user data, so a direct write is safe and exact.
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
