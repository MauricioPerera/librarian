package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
)

// TestRoundTripExact covers the round-trip acceptance criterion: applying the
// canonical schema to a real SQLite file (not :memory:) and inspecting it back
// reconstructs the canonical schema exactly (Inspection.Exact == true).
func TestRoundTripExact(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "roundtrip.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.ApplySchema(ctx, schema.Build()); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	inspection, err := db.InspectSchema(ctx)
	if err != nil {
		t.Fatalf("inspect schema: %v", err)
	}
	if !inspection.Exact {
		t.Fatalf("inspection not exact; unresolved=%+v", inspection.Unresolved)
	}
}

// TestEnsureSchemaIdempotent covers the idempotent-startup criterion: apply,
// close, reopen the SAME file, EnsureSchema again → no error, no attempt to
// recreate existing tables.
func TestEnsureSchemaIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "idempotent.db")
	ctx := context.Background()

	// First boot: creates the schema.
	db1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	if err := store.EnsureSchema(ctx, db1); err != nil {
		t.Fatalf("ensure #1: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	// Second boot on the same file: must be a no-op, not a recreate error.
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	defer db2.Close()
	if err := store.EnsureSchema(ctx, db2); err != nil {
		t.Fatalf("ensure #2 (idempotent restart) failed: %v", err)
	}
}
