package store_test

import (
	"context"
	"database/sql"
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

// TestSeedCatalogsIdempotent covers the CONTRACT-02 T1 acceptance criterion:
// seeding the role/permission catalogs twice on the same file neither
// duplicates rows nor fails. SELECT count(*) FROM roles is equal before and
// after the second seed, and equals len(schema.Roles); same for permissions.
func TestSeedCatalogsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seed.db")
	ctx := context.Background()

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	if err := store.SeedCatalogs(ctx, db.DB); err != nil {
		t.Fatalf("seed #1: %v", err)
	}
	rolesAfter1 := count(t, db.DB, "SELECT count(*) FROM roles")
	permsAfter1 := count(t, db.DB, "SELECT count(*) FROM permissions")
	if rolesAfter1 != len(schema.Roles) {
		t.Fatalf("roles after seed #1 = %d, want %d", rolesAfter1, len(schema.Roles))
	}
	if permsAfter1 != len(schema.Permissions) {
		t.Fatalf("permissions after seed #1 = %d, want %d", permsAfter1, len(schema.Permissions))
	}

	// Second seed on the same file: must be a no-op (no duplicate rows, no error).
	if err := store.SeedCatalogs(ctx, db.DB); err != nil {
		t.Fatalf("seed #2 (idempotent) failed: %v", err)
	}
	rolesAfter2 := count(t, db.DB, "SELECT count(*) FROM roles")
	permsAfter2 := count(t, db.DB, "SELECT count(*) FROM permissions")
	if rolesAfter2 != rolesAfter1 {
		t.Fatalf("roles after seed #2 = %d, want %d (no duplicates)", rolesAfter2, rolesAfter1)
	}
	if permsAfter2 != permsAfter1 {
		t.Fatalf("permissions after seed #2 = %d, want %d (no duplicates)", permsAfter2, permsAfter1)
	}
}

// count returns the integer result of a single-row SELECT count(*) query.
func count(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}
