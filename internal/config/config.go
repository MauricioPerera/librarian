// Package config loads librarian's runtime configuration from the environment
// and validates the invariants the auth contract requires. Most importantly,
// the JWT signing secret MUST come from LIBRARIAN_JWT_SECRET and MUST be
// non-empty — Load fails (fail-closed) if it is absent or empty, so the server
// never starts with a default or hardcoded secret.
//
// CONTRACT-21 T2 adds the OTHER fail-closed decision of this project: WHICH
// ENGINE this instance runs on. See ResolveEngine.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// Environment variable names, spelled once.
const (
	// EngineVar names the engine explicitly: "sqlite" (the default) or
	// "postgres".
	EngineVar = "LIBRARIAN_ENGINE"
	// DSNVar carries the connection string: a file path on SQLite, a PostgreSQL
	// URL on PostgreSQL. It keeps its historical name — a deployed instance
	// already sets it, and there is only ever ONE connection string, so a second
	// variable would be a second source of truth for the same thing.
	DSNVar = "LIBRARIAN_DB"
)

// DefaultSQLitePath is the database file used when LIBRARIAN_DB is unset. It
// only applies to SQLite: there is no defensible default for a PostgreSQL DSN,
// so PostgreSQL requires the variable.
const DefaultSQLitePath = "librarian.db"

// Config is the validated runtime configuration handed to the server.
//
// Engine and DSN travel TOGETHER and are always produced by ResolveEngine, so
// they cannot be set from different places: store.Open takes both and hands back
// a compat.Store whose Target is derived from the engine, which is what makes
// "a PostgreSQL connection carrying a SQLite target" unrepresentable.
type Config struct {
	Addr      string
	Engine    compat.Engine
	DSN       string
	JWTSecret string
}

// DBPath is the DSN under its historical name, kept so log lines and error
// messages that talk about "the database" read the same on both engines.
func (c Config) DBPath() string { return c.DSN }

// Load reads configuration from the environment and validates it. It returns
// an error (rather than a default) when LIBRARIAN_JWT_SECRET is missing or
// empty, and when the engine selection is ambiguous or invalid.
func Load() (Config, error) {
	engine, dsn, err := ResolveEngine()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Addr:      os.Getenv("LIBRARIAN_ADDR"),
		Engine:    engine,
		DSN:       dsn,
		JWTSecret: os.Getenv("LIBRARIAN_JWT_SECRET"),
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.JWTSecret == "" {
		return Config{}, errors.New("LIBRARIAN_JWT_SECRET must be set and non-empty (no default secret is provided)")
	}
	return cfg, nil
}

// ResolveEngine decides which engine this instance runs on, and on which DSN.
//
// THE FORM, AND WHY (CONTRACT-21 T2). The choice is an EXPLICIT variable
// (LIBRARIAN_ENGINE), CROSS-CHECKED against the shape of the DSN. Neither half
// alone satisfies the contract's requirement that the choice be unambiguous:
//
//   - Inferring the engine from the DSN alone was rejected. It reads well for
//     `postgres://…` but the SQLite side is "anything else", so a typo
//     (`postgress://`, a DSN that lost its scheme through a broken template, an
//     empty variable) silently means "SQLite", and SQLite will HAPPILY CREATE
//     the file. The process then comes up healthy, serving an empty database,
//     while the operator believes it is on PostgreSQL. That is the exact silent
//     failure the contract names, and it is unrecoverable in the sense that
//     matters: nothing is broken enough to notice.
//   - An explicit variable alone was rejected too, for the mirror reason: an
//     operator who sets LIBRARIAN_DB to a PostgreSQL URL and forgets
//     LIBRARIAN_ENGINE gets the same empty-SQLite outcome, and this time the
//     configuration says out loud what was intended.
//
// So: the variable decides, and a DSN that CONTRADICTS it is a hard error naming
// what was expected. The result is that every wrong configuration fails at
// startup with a message, and none of them boots onto the wrong engine.
//
// The default (variable absent or empty) is SQLite, which is what every deployed
// instance runs today — but only when the DSN does not look like PostgreSQL, so
// the default can never be the thing that swallows a PostgreSQL intent.
func ResolveEngine() (compat.Engine, string, error) {
	raw := strings.TrimSpace(os.Getenv(EngineVar))
	dsn := strings.TrimSpace(os.Getenv(DSNVar))
	looksPostgres := LooksLikePostgresDSN(dsn)

	switch strings.ToLower(raw) {
	case "", "sqlite", "libsql":
		if looksPostgres {
			return "", "", fmt.Errorf(
				"%s is a PostgreSQL connection URL but %s is %s: refusing to start on SQLite with a PostgreSQL DSN, because that would silently create an empty local database file and serve from it. Set %s=postgres, or point %s at a file path",
				DSNVar, EngineVar, describeEngineVar(raw), EngineVar, DSNVar)
		}
		if dsn == "" {
			dsn = DefaultSQLitePath
		}
		return compat.SQLite, dsn, nil
	case "postgres", "postgresql":
		if dsn == "" {
			return "", "", fmt.Errorf(
				"%s=postgres requires %s to be set to a PostgreSQL connection URL (for example postgres://user:password@host:5432/librarian?sslmode=disable); there is no default DSN for PostgreSQL",
				EngineVar, DSNVar)
		}
		if !looksPostgres {
			return "", "", fmt.Errorf(
				"%s=postgres but %s (%q) is not a PostgreSQL connection URL: expected a postgres:// or postgresql:// URL, or a libpq keyword/value string containing host= or dbname=",
				EngineVar, DSNVar, dsn)
		}
		return compat.Postgres, dsn, nil
	default:
		return "", "", fmt.Errorf(
			"%s=%q is not a supported engine: expected %q (the default) or %q",
			EngineVar, raw, "sqlite", "postgres")
	}
}

// describeEngineVar renders the state of LIBRARIAN_ENGINE for the error message
// above, so "unset" and "set to sqlite" are told apart — they are different
// mistakes and deserve different reading.
func describeEngineVar(raw string) string {
	if raw == "" {
		return "not set (which defaults to sqlite)"
	}
	return fmt.Sprintf("%q", raw)
}

// LooksLikePostgresDSN reports whether dsn is addressed at PostgreSQL.
//
// It recognises the two forms the pgx driver accepts: the URL form
// (postgres:// or postgresql://) and the libpq keyword/value form
// ("host=… dbname=…"). It is deliberately a SHAPE test and not a validity test:
// its job is to catch the engine/DSN contradiction, not to pre-validate a
// connection string — that is the driver's job, and its failure is loud.
func LooksLikePostgresDSN(dsn string) bool {
	lower := strings.ToLower(strings.TrimSpace(dsn))
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		return true
	}
	// libpq keyword/value form. Requiring one of these two keys (rather than any
	// `=`) keeps a Windows path or a file name with an equals sign from being
	// mistaken for a connection string.
	for _, key := range []string{"host=", "dbname="} {
		if strings.HasPrefix(lower, key) || strings.Contains(lower, " "+key) {
			return true
		}
	}
	return false
}
