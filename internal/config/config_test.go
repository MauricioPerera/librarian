package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/config"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
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
	t.Setenv("LIBRARIAN_ENGINE", "")
	t.Setenv("LIBRARIAN_DB", "")
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
	if cfg.DBPath() != "librarian.db" {
		t.Fatalf("dbpath = %q, want librarian.db", cfg.DBPath())
	}
	// CONTRACT-21 T2: the default engine is SQLite — what every deployed
	// instance runs today — and it is only the default when nothing contradicts
	// it (see TestResolveEngine).
	if cfg.Engine != compat.SQLite {
		t.Fatalf("engine = %q, want %q", cfg.Engine, compat.SQLite)
	}
}

// TestResolveEngine is the acceptance test of CONTRACT-21 T2's central
// requirement: the engine choice is UNAMBIGUOUS, and every ambiguous or invalid
// configuration FAILS with a message that says what was expected — it never
// falls back to SQLite when the intent was PostgreSQL, because that fallback
// would come up healthy while serving an empty local file.
func TestResolveEngine(t *testing.T) {
	const pgDSN = "postgres://u:p@host:5432/librarian?sslmode=disable"

	cases := []struct {
		name       string
		engineVar  string
		dsnVar     string
		wantEngine compat.Engine
		wantDSN    string
		wantErr    string // substring the error must contain; "" = must succeed
	}{
		{
			name:       "unset defaults to sqlite with the default file",
			wantEngine: compat.SQLite, wantDSN: "librarian.db",
		},
		{
			name:       "unset with a file path is sqlite on that path",
			dsnVar:     "/var/lib/librarian/librarian.db",
			wantEngine: compat.SQLite, wantDSN: "/var/lib/librarian/librarian.db",
		},
		{
			name:      "explicit sqlite is sqlite",
			engineVar: "sqlite", dsnVar: "data.db",
			wantEngine: compat.SQLite, wantDSN: "data.db",
		},
		{
			name:      "explicit postgres with a URL is postgres",
			engineVar: "postgres", dsnVar: pgDSN,
			wantEngine: compat.Postgres, wantDSN: pgDSN,
		},
		{
			name:      "case and spacing do not matter",
			engineVar: "  PostgreSQL  ", dsnVar: pgDSN,
			wantEngine: compat.Postgres, wantDSN: pgDSN,
		},
		{
			name:      "libpq keyword form is recognised as postgres",
			engineVar: "postgres", dsnVar: "host=db.internal dbname=librarian user=librarian",
			wantEngine: compat.Postgres, wantDSN: "host=db.internal dbname=librarian user=librarian",
		},
		// THE FAILURE THIS CONTRACT EXISTS TO PREVENT, in both directions.
		{
			name:    "a postgres DSN with no engine set REFUSES to boot on sqlite",
			dsnVar:  pgDSN,
			wantErr: "refusing to start on SQLite with a PostgreSQL DSN",
		},
		{
			name:      "a postgres DSN with engine=sqlite REFUSES too",
			engineVar: "sqlite", dsnVar: pgDSN,
			wantErr: "refusing to start on SQLite with a PostgreSQL DSN",
		},
		{
			name:      "engine=postgres with no DSN fails, it does not invent one",
			engineVar: "postgres",
			wantErr:   "requires LIBRARIAN_DB",
		},
		{
			name:      "engine=postgres with a file path fails naming what was expected",
			engineVar: "postgres", dsnVar: "librarian.db",
			wantErr: "expected a postgres:// or postgresql:// URL",
		},
		{
			name:      "a typo in the scheme does NOT silently become sqlite",
			engineVar: "postgres", dsnVar: "postgress://u:p@host:5432/librarian",
			wantErr: "expected a postgres:// or postgresql:// URL",
		},
		{
			name:      "an unknown engine name fails listing the accepted ones",
			engineVar: "mysql", dsnVar: "whatever",
			wantErr: "not a supported engine",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EngineVar, tc.engineVar)
			t.Setenv(config.DSNVar, tc.dsnVar)
			engine, dsn, err := config.ResolveEngine()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("engine=%q dsn=%q was accepted (engine=%q, dsn=%q); want an error containing %q",
						tc.engineVar, tc.dsnVar, engine, dsn, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				t.Logf("REFUSED as required: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if engine != tc.wantEngine || dsn != tc.wantDSN {
				t.Fatalf("got (%q, %q), want (%q, %q)", engine, dsn, tc.wantEngine, tc.wantDSN)
			}
		})
	}
}
