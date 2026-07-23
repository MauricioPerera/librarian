// Command librarian is the single headless backend binary. On startup it opens
// (or creates) the embedded libSQL database, applies the canonical schema
// idempotently, and only then starts serving HTTP.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
)

const (
	defaultAddr = ":8080"
	defaultDB   = "librarian.db"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("librarian: %v", err)
	}
}

func run() error {
	addr := os.Getenv("LIBRARIAN_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	dbPath := os.Getenv("LIBRARIAN_DB")
	if dbPath == "" {
		dbPath = defaultDB
	}

	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := store.EnsureSchema(context.Background(), db); err != nil {
		return err
	}

	log.Printf("librarian: schema ready on %s, listening on %s", dbPath, addr)
	srv := &http.Server{Addr: addr, Handler: server.NewMux()}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
