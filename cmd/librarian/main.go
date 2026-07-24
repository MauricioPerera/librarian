// Command librarian is the single headless backend binary. On startup it opens
// (or creates) the embedded libSQL database, applies the canonical schema
// idempotently, seeds the role/permission catalogs, and only then starts
// serving HTTP. The JWT signing secret is required (LIBRARIAN_JWT_SECRET);
// without it the process refuses to start.
//
// CONTRACT-04 T1 adds an offline mode: `librarian --dump-schema [<path>]`
// serializes the canonical schema (schema.Build()) to JSON and exits, without
// a database or a JWT secret. The JSON is the generated artifact the compat CLI
// consumes as its schema_ref when exporting to PostgreSQL — Go stays the only
// source of truth, the JSON is never hand-maintained.
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
	// Offline schema dump (CONTRACT-04 T1). Handled before config.Load() so it
	// needs no JWT secret and no database — the schema is pure code. This is the
	// generated artifact `compat copy` consumes as its schema_ref.
	if dumpPath, ok := dumpSchemaFlag(os.Args[1:]); ok {
		data, err := schema.JSON()
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