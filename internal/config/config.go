// Package config loads librarian's runtime configuration from the environment
// and validates the invariants the auth contract requires. Most importantly,
// the JWT signing secret MUST come from LIBRARIAN_JWT_SECRET and MUST be
// non-empty — Load fails (fail-closed) if it is absent or empty, so the server
// never starts with a default or hardcoded secret.
package config

import (
	"errors"
	"os"
)

// Config is the validated runtime configuration handed to the server.
type Config struct {
	Addr      string
	DBPath    string
	JWTSecret string
}

// Load reads configuration from the environment and validates it. It returns
// an error (rather than a default) when LIBRARIAN_JWT_SECRET is missing or
// empty.
func Load() (Config, error) {
	cfg := Config{
		Addr:      os.Getenv("LIBRARIAN_ADDR"),
		DBPath:    os.Getenv("LIBRARIAN_DB"),
		JWTSecret: os.Getenv("LIBRARIAN_JWT_SECRET"),
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "librarian.db"
	}
	if cfg.JWTSecret == "" {
		return Config{}, errors.New("LIBRARIAN_JWT_SECRET must be set and non-empty (no default secret is provided)")
	}
	return cfg, nil
}
