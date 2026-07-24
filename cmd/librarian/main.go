// Command librarian is the single headless backend binary. On startup it opens
// (or creates) the embedded libSQL database, applies the canonical schema
// idempotently, seeds the role/permission catalogs, and only then starts
// serving HTTP. The JWT signing secret is required (LIBRARIAN_JWT_SECRET);
// without it the process refuses to start.
//
// CONTRACT-04 T1 added `librarian --dump-schema [<path>] [--db <path>]`, which
// serializes the canonical schema to JSON and exits. The JSON is the generated
// artifact the compat CLI consumes as its schema_ref when exporting to
// PostgreSQL; it is never hand-maintained.
//
// CONTRACT-13 made that mode read the database. It still needs no JWT secret,
// but the canonical schema is now code PLUS the dynamic content types persisted
// in the database, so a dump that skipped the database would hand `compat copy`
// a schema silently missing those tables and all their rows. It fails loudly
// (non-zero exit) rather than emitting an incomplete schema — see dumpSchema.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/MauricioPerera/librarian/internal/config"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("librarian: %v", err)
	}
}

func run() error {
	if dumpPath, ok := dumpSchemaFlag(os.Args[1:]); ok {
		return dumpSchema(os.Args[1:], dumpPath)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if err := store.EnsureSchema(ctx, db); err != nil {
		return err
	}
	if err := store.SeedCatalogs(ctx, db.DB); err != nil {
		return err
	}

	mux, err := server.NewMux(server.Deps{DB: db.DB, JWTSecret: cfg.JWTSecret})
	if err != nil {
		return err
	}

	log.Printf("librarian: schema ready on %s, listening on %s", cfg.DBPath, cfg.Addr)
	srv := &http.Server{Addr: cfg.Addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// dumpSchema implements the --dump-schema mode.
//
// CONTRACT-13 CHANGED THIS MODE, and the change is deliberate and unavoidable.
// Until CONTRACT-12 the canonical schema was pure code, so the dump was fully
// OFFLINE: no database, no JWT secret. With dynamic content types the canonical
// schema is code PLUS the definitions persisted in the database, so a dump that
// does not read the database is, by definition, an INCOMPLETE schema — and this
// artifact is precisely the schema_ref `compat copy` uses to export, so an
// incomplete one silently leaves the dynamic tables AND ALL THEIR ROWS out of
// the export. That is the single failure mode the whole project exists to
// prevent (DEFINITION.md), so it must be impossible, not merely unlikely.
//
// The resolution, in order of how loudly it fails:
//
//   - It still does NOT require LIBRARIAN_JWT_SECRET (config.Load is still not
//     called): dumping a schema is not serving traffic. Only the database is
//     newly required.
//   - The database is located by --db <path> / --db=<path>, else LIBRARIAN_DB,
//     else the same "librarian.db" default the server uses.
//   - If that file DOES NOT EXIST, the command FAILS with a non-zero exit and
//     an explicit message. It does NOT fall back to the code-only schema and it
//     does NOT let SQLite helpfully create an empty database (which would
//     produce a plausible-looking but incomplete dump from a mistyped path —
//     exactly the silent failure being designed out).
//   - If the database exists but its registry table does not, zero dynamic
//     types is a PROOF (a definition can only live in that table), not a guess,
//     so the code-only schema is emitted and it is complete. This is what keeps
//     the command working against a pre-CONTRACT-13 database.
//   - Any other failure (unreadable file, corrupt definition row) propagates as
//     a non-zero exit. There is no path that prints a partial schema.
func dumpSchema(args []string, dumpPath string) error {
	dbPath := dbPathFlag(args)
	if dbPath == "" {
		dbPath = os.Getenv("LIBRARIAN_DB")
	}
	if dbPath == "" {
		dbPath = "librarian.db"
	}
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf(
			"--dump-schema must read the database to include the dynamic content types (a dump without them would export an incomplete schema): cannot access %q: %w — pass --db <path> or set LIBRARIAN_DB",
			dbPath, err)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database %q: %w", dbPath, err)
	}
	defer db.Close()

	defs, err := store.LoadDefinitions(context.Background(), db)
	if err != nil {
		return fmt.Errorf("read dynamic content type definitions from %q: %w", dbPath, err)
	}
	data, err := schema.JSONWith(defs)
	if err != nil {
		return fmt.Errorf("marshal schema: %w", err)
	}
	if dumpPath == "" {
		fmt.Println(string(data))
		return nil
	}
	if err := os.WriteFile(dumpPath, data, 0o644); err != nil {
		return fmt.Errorf("write schema: %w", err)
	}
	return nil
}

// dbPathFlag inspects args for --db <path> / --db=<path> and returns the value
// (empty when absent). Accepted only alongside --dump-schema; the server itself
// keeps taking its database path from LIBRARIAN_DB, unchanged.
func dbPathFlag(args []string) string {
	for i, a := range args {
		switch {
		case a == "--db":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--db="):
			return strings.TrimPrefix(a, "--db=")
		}
	}
	return ""
}

// dumpSchemaFlag inspects args for the --dump-schema mode and returns the
// optional output path (empty means stdout) and ok=true when the flag is
// present. Accepted forms:
//
//	librarian --dump-schema            # JSON to stdout
//	librarian --dump-schema path.json  # JSON to path.json
//	librarian --dump-schema=path.json  # JSON to path.json
//
// ok=false means the flag is absent and the server should boot normally.
func dumpSchemaFlag(args []string) (path string, ok bool) {
	for i, a := range args {
		switch {
		case a == "--dump-schema":
			// The next token, if present and not a flag, is the output path.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1], true
			}
			return "", true
		case strings.HasPrefix(a, "--dump-schema="):
			return strings.TrimPrefix(a, "--dump-schema="), true
		}
	}
	return "", false
}
