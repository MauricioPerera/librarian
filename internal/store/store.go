// Package store owns opening the embedded libSQL/SQLite database and applying
// the canonical schema idempotently at startup.
package store

import (
	"context"
	"database/sql"
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

// EnsureSchema applies the canonical librarian schema if it is not already
// present. It is idempotent: on a database that already has the tables it
// returns nil without attempting to recreate them.
//
// Idempotency is decided by inspecting the live catalog: if every table of the
// canonical schema is already present, EnsureSchema is a no-op. Otherwise the
// schema is applied. (compat's ApplySchema uses plain CREATE TABLE, which would
// fail on a second run — so the guard is required for the "stop and restart on
// the same file" contract.)
func EnsureSchema(ctx context.Context, store *compat.Store) error {
	want := schema.Build()

	applied, err := schemaApplied(ctx, store, want)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}
	if err := store.ApplySchema(ctx, want); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// schemaApplied reports whether every table of want already exists in the
// store's live catalog.
func schemaApplied(ctx context.Context, store *compat.Store, want compat.Schema) (bool, error) {
	inspection, err := store.InspectSchema(ctx)
	if err != nil {
		return false, fmt.Errorf("inspect schema: %w", err)
	}
	present := make(map[string]struct{}, len(inspection.Schema.Tables))
	for _, table := range inspection.Schema.Tables {
		present[table.Name] = struct{}{}
	}
	for _, table := range want.Tables {
		if _, ok := present[table.Name]; !ok {
			return false, nil
		}
	}
	return true, nil
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
