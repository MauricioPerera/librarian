package config_test

import (
	"os"
	"testing"

	"github.com/MauricioPerera/librarian/internal/config"
)

// TestLoadRejectsEmptySecret covers the CONTRACT-02 T3 fail-closed criterion:
// with LIBRARIAN_JWT_SECRET set to empty, Load fails explicitly — the server
// cannot start with no secret or a default secret.
func TestLoadRejectsEmptySecret(t *testing.T) {
	t.Setenv("LIBRARIAN_JWT_SECRET", "")
	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error when LIBRARIAN_JWT_SECRET is empty, got nil")
	}
}

// TestLoadRejectsAbsentSecret covers the "absent" variant explicitly. We clear
// the var for the duration of the test and restore it after.
func TestLoadRejectsAbsentSecret(t *testing.T) {
	prev, had := os.LookupEnv("LIBRARIAN_JWT_SECRET")
	os.Unsetenv("LIBRARIAN_JWT_SECRET")
	t.Cleanup(func() {
		if had {
			os.Setenv("LIBRARIAN_JWT_SECRET", prev)
		}
	})
	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error when LIBRARIAN_JWT_SECRET is absent, got nil")
	}
}

// TestLoadAcceptsSecret confirms a non-empty secret loads and keeps defaults.
func TestLoadAcceptsSecret(t *testing.T) {
	t.Setenv("LIBRARIAN_JWT_SECRET", "a-real-secret")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.JWTSecret != "a-real-secret" {
		t.Fatalf("secret = %q", cfg.JWTSecret)
	}
	if cfg.Addr != ":8080" {
		t.Fatalf("addr = %q, want :8080", cfg.Addr)
	}
	if cfg.DBPath != "librarian.db" {
		t.Fatalf("dbpath = %q, want librarian.db", cfg.DBPath)
	}
}
