// Package store owns opening the embedded libSQL/SQLite database and applying
// the canonical schema idempotently at startup.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// Open opens (or creates) the SQLite database at path and returns a compat
// Store bound to the SQLite engine. path is a real file path (not ":memory:")
// for real server use; callers own Close.
func Open(path string) (*compat.Store, error) {
	return compat.OpenSQLite(schema.SQLiteVersion, path)
}

// EnsureSchema applies the canonical librarian schema, creating ONLY the
// tables that do not exist yet. It is idempotent (a restart on a database that
// already has every table is a no-op) AND incremental (a restart after a code
// change that adds a NEW content type creates just that table, leaving every
// existing table and its data untouched).
//
// This matters for a real deployed instance: compat's ApplySchema compiles
// and runs a plain CREATE TABLE per table with no "IF NOT EXISTS" — applying
// the FULL canonical schema against a database that already has some (but not
// all) of its tables fails outright on the first pre-existing table, which
// would crash the service on startup after any schema-adding change (found
// verifying CONTRACT-11, which adds a second content type to an already
// deployed database — a full-schema re-apply would have taken production
// down). Filtering to only the missing tables before calling ApplySchema is
// the fix: it is the exact same DDL for each new table, just scoped to what
// is actually absent.
func EnsureSchema(ctx context.Context, store *compat.Store) error {
	want := schema.Build()

	missing, err := missingTables(ctx, store, want)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	if err := store.ApplySchema(ctx, compat.Schema{Tables: missing}); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// ApplySchema above records compat's own "canonical_schema" metadata (the
	// __compat_schema table InspectSchema prefers over introspecting the live
	// catalog — see InspectSchema's source) — but it records it for the
	// REDUCED schema passed in (only the tables just created), not the full
	// canonical schema. Left as-is, the NEXT restart would read that narrow
	// metadata back, believe every pre-existing table (users, articles, ...)
	// is missing again, and crash trying to re-CREATE them. Overwrite that row
	// with the full `want` schema so InspectSchema's canonical-metadata view
	// stays accurate after an incremental apply, not just after a from-scratch
	// one. Same table/key/upsert shape compat's own writeSchemaMetadata uses
	// (unexported, so replicated here rather than touching compat).
	if err := writeFullSchemaMetadata(ctx, store, want); err != nil {
		return fmt.Errorf("record full schema metadata: %w", err)
	}
	return nil
}

// writeFullSchemaMetadata upserts the full canonical schema into compat's own
// __compat_schema metadata table, replacing whatever a prior (possibly
// reduced) ApplySchema call left there.
func writeFullSchemaMetadata(ctx context.Context, store *compat.Store, want compat.Schema) error {
	payload, err := json.Marshal(want)
	if err != nil {
		return err
	}
	_, err = store.DB.ExecContext(ctx,
		`INSERT INTO "__compat_schema" ("key", "value") VALUES (?, ?)
		 ON CONFLICT("key") DO UPDATE SET "value" = excluded."value"`,
		"canonical_schema", string(payload),
	)
	return err
}

// missingTables returns the tables of want that are not yet present in the
// store's live catalog, in want's declared order (so a table's FK targets that
// are themselves also missing are still created before it, same as a
// from-scratch apply).
func missingTables(ctx context.Context, store *compat.Store, want compat.Schema) ([]compat.Table, error) {
	inspection, err := store.InspectSchema(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect schema: %w", err)
	}
	present := make(map[string]struct{}, len(inspection.Schema.Tables))
	for _, table := range inspection.Schema.Tables {
		present[table.Name] = struct{}{}
	}
	var missing []compat.Table
	for _, table := range want.Tables {
		if _, ok := present[table.Name]; !ok {
			missing = append(missing, table)
		}
	}
	return missing, nil
}

// SeedCatalogs inserts the fixed role and permission catalogs (schema.Roles,
// schema.Permissions) into the live database if a row for a given name does not
// already exist. It is idempotent: running it twice on the same file neither
// duplicates rows nor fails. The id column is left to its DEFAULT
// gen_random_uuid() — UUIDs are not generated in Go.
//
// All SQL is parameterized; the only interpolated value is the table name,
// which is a fixed internal constant, not user input.
func SeedCatalogs(ctx context.Context, db *sql.DB) error {
	if err := seedNames(ctx, db, "roles", schema.Roles); err != nil {
		return fmt.Errorf("seed roles: %w", err)
	}
	if err := seedNames(ctx, db, "permissions", schema.Permissions); err != nil {
		return fmt.Errorf("seed permissions: %w", err)
	}
	// CONTRACT-12: taxonomies is a third code-fixed catalog, seeded exactly like
	// roles/permissions via the same idempotent seedNames helper (INSERT ...
	// ON CONFLICT(name) DO NOTHING). This is the ONLY change to this file for
	// CONTRACT-12 — EnsureSchema and its incremental-apply machinery are left
	// entirely untouched.
	if err := seedNames(ctx, db, "taxonomies", schema.Taxonomies); err != nil {
		return fmt.Errorf("seed taxonomies: %w", err)
	}
	return nil
}

// seedNames inserts each name with INSERT ... ON CONFLICT(name) DO NOTHING,
// supported by both SQLite (3.24+) and PostgreSQL. table is a fixed internal
// constant (never user input), so it is interpolated into the statement text;
// name is always bound as a parameter.
func seedNames(ctx context.Context, db *sql.DB, table string, names []string) error {
	stmt := `INSERT INTO ` + table + ` (name) VALUES (?) ON CONFLICT(name) DO NOTHING`
	for _, name := range names {
		if _, err := db.ExecContext(ctx, stmt, name); err != nil {
			return fmt.Errorf("insert %q: %w", name, err)
		}
	}
	return nil
}
