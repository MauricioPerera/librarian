package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
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

// TestEnsureSchemaAddsOnlyMissingTable simulates the real scenario a redeploy
// hits when the code adds a new content-type table to an already-deployed
// database (CONTRACT-11: adding "products" to a database created before it
// existed). A database with every table EXCEPT one is built directly (bypassing
// EnsureSchema, to control exactly which table is absent); EnsureSchema must
// then create ONLY that missing table, not fail trying to re-CREATE TABLE the
// ones that already exist (compat's ApplySchema has no "IF NOT EXISTS" — the
// full-schema re-apply this test guards against would crash a real service on
// startup after any schema-adding change).
func TestEnsureSchemaAddsOnlyMissingTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "partial.db")
	ctx := context.Background()

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	full := schema.Build()
	var partial compat.Schema
	for _, table := range full.Tables {
		if table.Name != "products" {
			partial.Tables = append(partial.Tables, table)
		}
	}
	if err := db.ApplySchema(ctx, partial); err != nil {
		t.Fatalf("apply partial schema (everything but products): %v", err)
	}

	// The real regression: this must NOT try to re-CREATE the tables applied
	// above, only add the missing "products".
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema on a database missing one table failed: %v", err)
	}

	inspection, err := db.InspectSchema(ctx)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	var sawProducts, sawUsers bool
	for _, table := range inspection.Schema.Tables {
		if table.Name == "products" {
			sawProducts = true
		}
		if table.Name == "users" {
			sawUsers = true
		}
	}
	if !sawProducts {
		t.Fatal("products table was not created")
	}
	if !sawUsers {
		t.Fatal("pre-existing users table went missing")
	}

	// Idempotency still holds on top of the incremental apply: a second
	// EnsureSchema call (every table now present) must remain a no-op.
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema second call (now fully applied) failed: %v", err)
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
	taxAfter1 := count(t, db.DB, "SELECT count(*) FROM taxonomies")
	if rolesAfter1 != len(schema.Roles) {
		t.Fatalf("roles after seed #1 = %d, want %d", rolesAfter1, len(schema.Roles))
	}
	if permsAfter1 != len(schema.Permissions) {
		t.Fatalf("permissions after seed #1 = %d, want %d", permsAfter1, len(schema.Permissions))
	}
	// CONTRACT-12 T1: taxonomies is the third code-fixed catalog, seeded the same
	// idempotent way. After the first seed there must be exactly len(Taxonomies).
	if taxAfter1 != len(schema.Taxonomies) {
		t.Fatalf("taxonomies after seed #1 = %d, want %d", taxAfter1, len(schema.Taxonomies))
	}

	// Second seed on the same file: must be a no-op (no duplicate rows, no error).
	if err := store.SeedCatalogs(ctx, db.DB); err != nil {
		t.Fatalf("seed #2 (idempotent) failed: %v", err)
	}
	rolesAfter2 := count(t, db.DB, "SELECT count(*) FROM roles")
	permsAfter2 := count(t, db.DB, "SELECT count(*) FROM permissions")
	taxAfter2 := count(t, db.DB, "SELECT count(*) FROM taxonomies")
	if rolesAfter2 != rolesAfter1 {
		t.Fatalf("roles after seed #2 = %d, want %d (no duplicates)", rolesAfter2, rolesAfter1)
	}
	if permsAfter2 != permsAfter1 {
		t.Fatalf("permissions after seed #2 = %d, want %d (no duplicates)", permsAfter2, permsAfter1)
	}
	if taxAfter2 != taxAfter1 {
		t.Fatalf("taxonomies after seed #2 = %d, want %d (no duplicates)", taxAfter2, taxAfter1)
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

// TestEnsureSchemaRecreatesViews covers the CONTRACT-19 upgrade path, which is
// the one way this contract could break an ALREADY DEPLOYED instance: such a
// database has every TABLE already, so an EnsureSchema that only acted when a
// table was missing would never create the new views and every auth read would
// fail after the deploy. Dropping the views and running EnsureSchema again
// simulates exactly that database, and the views must come back.
func TestEnsureSchemaRecreatesViews(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "views.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	views := schema.Build().Views
	if len(views) == 0 {
		t.Fatal("the canonical schema declares no views — CONTRACT-19 T1 is missing")
	}
	for _, view := range views {
		if _, err := db.DB.ExecContext(ctx, `DROP VIEW "`+view.Name+`"`); err != nil {
			t.Fatalf("drop view %s: %v", view.Name, err)
		}
	}
	if got := count(t, db.DB, `SELECT count(*) FROM sqlite_master WHERE type = 'view'`); got != 0 {
		t.Fatalf("views still present after drop: %d", got)
	}

	// The restart of an already-deployed instance: no table is missing.
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	if got, want := count(t, db.DB, `SELECT count(*) FROM sqlite_master WHERE type = 'view'`), len(views); got != want {
		t.Fatalf("views after re-ensure = %d, want %d", got, want)
	}
	// A third run must be a no-op success, not a "view already exists" failure.
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("third ensure (idempotency): %v", err)
	}
	for _, view := range views {
		if _, err := db.DB.ExecContext(ctx, `SELECT count(*) FROM "`+view.Name+`"`); err != nil {
			t.Fatalf("view %s is not queryable: %v", view.Name, err)
		}
	}
}
