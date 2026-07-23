// Command librarian is the single headless backend binary. On startup it opens
// (or creates) the embedded libSQL database, applies the canonical schema
// idempotently, seeds the role/permission catalogs, and only then starts
// serving HTTP. The JWT signing secret is required (LIBRARIAN_JWT_SECRET);
// without it the process refuses to start.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/MauricioPerera/librarian/internal/config"
	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("librarian: %v", err)
	}
}

func run() error {
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
