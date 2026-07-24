package store

// contenttypes.go implements CONTRACT-13 T2/T3 on the persistence side:
//
//   - CanonicalSchema: the composed "code + dynamic" schema, THE definition of
//     what this instance's schema is. Used by EnsureSchema, by
//     `librarian --dump-schema`, and by the compat.InferFeatures audit contract
//     — the three places that previously treated schema.Build() as complete.
//   - LoadContentTypeDefinitions: reads the persisted definitions back.
//   - CreateContentType: persists a definition AND creates its real table in a
//     SINGLE transaction, so the two can never come apart.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// ErrDuplicateContentType is returned when a content type with that name
// already exists. The real guarantee is the schema-level UNIQUE(name) on
// content_types (an application-level pre-check can lose a concurrent race);
// this sentinel merely lets the server translate the constraint violation into
// a clean 400 instead of leaking a raw SQL error as a 500 — the exact pattern
// products.sku and terms(taxonomy_id, slug) already use.
var ErrDuplicateContentType = errors.New("content type already exists")

// ErrContentTypeNotFound is returned by FetchContentType when no definition
// with that name exists (→ 404).
var ErrContentTypeNotFound = errors.New("content type not found")

// FromDB wraps an existing *sql.DB in a compat.Store bound to librarian's
// SQLite target. compat.Store is a two-field struct ({Target, DB}) with both
// fields exported, so this is an honest, allocation-only adapter — NOT a second
// connection: it shares the very same pool (which compat pins to a single
// connection for the foreign-keys pragma). It exists so the HTTP layer, which
// only ever carries a *sql.DB, can go through the exact same schema-application
// path as startup without changing server.Deps or NewMux's signature (the
// public contract of contracts 01-12 stays untouched).
func FromDB(db *sql.DB) *compat.Store {
	return &compat.Store{Target: schema.SQLiteTarget, DB: db}
}

// registryPresent reports whether the content_types registry table physically
// exists in the database.
//
// This deliberately consults SQLite's own catalog (sqlite_master) and NOT
// compat's InspectSchema: InspectSchema prefers the __compat_schema metadata
// row, which is exactly the thing EnsureSchema is about to rewrite. Asking the
// physical catalog is the only answer that cannot be circular.
//
// librarian's runtime engine is always SQLite (store.Open → compat.OpenSQLite);
// any other engine is a programming error and fails loudly rather than silently
// reporting "no registry" (which would produce an incomplete canonical schema).
func registryPresent(ctx context.Context, store *compat.Store) (bool, error) {
	if store.Target.Engine != compat.SQLite {
		return false, fmt.Errorf("content-type registry lookup: unsupported engine %q (librarian's runtime engine is SQLite)", store.Target.Engine)
	}
	var n int
	err := store.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		schema.ContentTypesTable,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("look up %s in sqlite_master: %w", schema.ContentTypesTable, err)
	}
	return n > 0, nil
}

// LoadContentTypeDefinitions reads every persisted dynamic content-type
// definition, with its fields in ordinal order, from a database whose registry
// table is known to exist. Ordering by name then ordinal makes the result
// deterministic.
//
// Every row is re-validated through the T1 gate before being returned. A row
// that fails (only possible if the registry was written outside the API) is a
// HARD ERROR, never a skipped entry: silently dropping a persisted type is what
// would make the dump — and therefore the export — incomplete.
func LoadContentTypeDefinitions(ctx context.Context, db *sql.DB) ([]schema.ContentTypeDefinition, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT t.name, COALESCE(f.name, ''), COALESCE(f.field_type, '')
		   FROM `+schema.ContentTypesTable+` t
		   LEFT JOIN `+schema.ContentTypeFieldsTable+` f ON f.content_type_id = t.id
		  ORDER BY t.name, f.ordinal`,
	)
	if err != nil {
		return nil, fmt.Errorf("query content type definitions: %w", err)
	}
	defer rows.Close()

	var (
		out   []schema.ContentTypeDefinition
		index = map[string]int{}
	)
	for rows.Next() {
		var typeName, fieldName, fieldType string
		if err := rows.Scan(&typeName, &fieldName, &fieldType); err != nil {
			return nil, fmt.Errorf("scan content type definition: %w", err)
		}
		i, ok := index[typeName]
		if !ok {
			out = append(out, schema.ContentTypeDefinition{Name: typeName})
			i = len(out) - 1
			index[typeName] = i
		}
		// The LEFT JOIN yields one all-empty field row for a type with no fields.
		if fieldName == "" {
			continue
		}
		out[i].Fields = append(out[i].Fields, schema.FieldDefinition{
			Name: fieldName,
			Type: schema.FieldType(fieldType),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate content type definitions: %w", err)
	}
	for _, d := range out {
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("persisted content type definition %q is invalid: %w", d.Name, err)
		}
	}
	return out, nil
}

// LoadDefinitions returns the persisted dynamic definitions, or an empty slice
// when the registry table does not exist yet.
//
// "Registry table absent ⇒ zero dynamic types" is a PROOF, not an assumption: a
// definition can only be persisted in that table, so a database without it
// cannot have one. This is the only case in which an empty result is legitimate
// — every OTHER failure (unreadable database, corrupt row, wrong engine)
// propagates as an error, so no caller can ever be handed a silently incomplete
// picture.
func LoadDefinitions(ctx context.Context, store *compat.Store) ([]schema.ContentTypeDefinition, error) {
	present, err := registryPresent(ctx, store)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	return LoadContentTypeDefinitions(ctx, store.DB)
}

// CanonicalSchema returns THE canonical schema of this database: the code
// tables (schema.Build()) composed with one table per persisted dynamic
// content-type definition. It is the single answer to "what is this instance's
// schema?", and it is what every one of the three former schema.Build() call
// sites now uses.
func CanonicalSchema(ctx context.Context, store *compat.Store) (compat.Schema, error) {
	defs, err := LoadDefinitions(ctx, store)
	if err != nil {
		return compat.Schema{}, fmt.Errorf("load dynamic content type definitions: %w", err)
	}
	full, err := schema.BuildWith(defs)
	if err != nil {
		return compat.Schema{}, fmt.Errorf("compose canonical schema: %w", err)
	}
	return full, nil
}

// FetchContentType returns one persisted definition by name, or
// ErrContentTypeNotFound. The name is NOT validated first on purpose: a lookup
// of a syntactically impossible name simply finds nothing (404), and the value
// is bound as a parameter, never interpolated.
func FetchContentType(ctx context.Context, db *sql.DB, name string) (schema.ContentTypeDefinition, error) {
	defs, err := LoadContentTypeDefinitions(ctx, db)
	if err != nil {
		return schema.ContentTypeDefinition{}, err
	}
	for _, d := range defs {
		if d.Name == name {
			return d, nil
		}
	}
	return schema.ContentTypeDefinition{}, ErrContentTypeNotFound
}

// CreateContentType persists a dynamic content-type definition AND creates its
// real table, ATOMICALLY — all of it or none of it.
//
// WHY ATOMICITY IS THE WHOLE POINT (red-team item of the contract): a
// definition persisted WITHOUT its table is the worst reachable state. Every
// subsequent EnsureSchema would compose a canonical schema containing a table
// that does not exist, the generic CRUD layer would query a missing table, and
// `compat copy` would be handed a schema_ref describing a table it cannot read.
// The mirror state — a table with no definition — is just as bad in the other
// direction: the table would be invisible to the composed schema and therefore
// silently excluded from every export, together with its data.
//
// HOW: everything happens inside ONE database transaction. SQLite executes DDL
// transactionally, so the CREATE TABLE, the two INSERTs and the
// __compat_schema metadata update commit or roll back together. The steps are
// deliberately the SAME machinery EnsureSchema uses — compose, diff against
// what exists (missingTables), compile with compat.CompileDDL, write the FULL
// composed metadata — rather than a loose ApplySchema, so there is exactly one
// notion of "apply a table" in the project. compat.Store.ApplySchema itself is
// not called because it opens its own transaction (and compat pins the SQLite
// pool to a single connection, so a nested transaction would deadlock); its
// body is reproduced here statement-for-statement instead.
//
// Concurrency: two admins racing on the SAME name are decided by the
// schema-level UNIQUE(name) on content_types, which cannot lose the race — the
// loser's whole transaction (table included) rolls back and it gets a 400. The
// caller (the HTTP layer) additionally serializes schema-creating requests with
// a mutex so two DIFFERENT names cannot interleave their diff-and-create steps.
func CreateContentType(ctx context.Context, store *compat.Store, def schema.ContentTypeDefinition) error {
	// 1. The T1 gate, before the name touches anything at all.
	if err := def.Validate(); err != nil {
		return err
	}

	// 2. Compose the schema this database WILL have, and let compat validate the
	//    whole thing (FK targets, column families, duplicate columns) up front.
	existing, err := LoadDefinitions(ctx, store)
	if err != nil {
		return fmt.Errorf("load existing content type definitions: %w", err)
	}
	for _, d := range existing {
		if d.Name == def.Name {
			// Fast, friendly path. The authoritative check is still the UNIQUE
			// constraint below (this one can lose a race; that one cannot).
			return ErrDuplicateContentType
		}
	}
	want, err := schema.BuildWith(append(append([]schema.ContentTypeDefinition{}, existing...), def))
	if err != nil {
		return err
	}

	// 3. Diff against what already exists — the same incremental step
	//    EnsureSchema performs. It must resolve to exactly the one new table;
	//    anything else means the database is not in the state we believe it to
	//    be in, and creating tables blindly at that point is unsafe.
	missing, err := missingTables(ctx, store, want)
	if err != nil {
		return err
	}
	if len(missing) != 1 || missing[0].Name != def.Name {
		names := make([]string, 0, len(missing))
		for _, t := range missing {
			names = append(names, t.Name)
		}
		return fmt.Errorf("refusing to create content type %q: expected exactly that one table to be missing, found %v", def.Name, names)
	}

	// 4. Compile the DDL BEFORE opening the transaction, so a compilation
	//    failure (a name compat rejects despite the T1 gate, an unsupported
	//    type) costs nothing and cannot leave anything half-done.
	statements, err := compat.CompileDDL(store.Target, compat.Schema{Tables: missing})
	if err != nil {
		return fmt.Errorf("compile DDL for content type %q: %w", def.Name, err)
	}

	// 5. One transaction: definition rows + CREATE TABLE + full metadata.
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var typeID string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO `+schema.ContentTypesTable+` (name) VALUES (?) RETURNING id`,
		def.Name,
	).Scan(&typeID)
	if isDuplicateContentTypeViolation(err) {
		return ErrDuplicateContentType
	}
	if err != nil {
		return fmt.Errorf("insert content type %q: %w", def.Name, err)
	}
	for i, f := range def.Fields {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+schema.ContentTypeFieldsTable+` (content_type_id, name, field_type, ordinal) VALUES (?, ?, ?, ?)`,
			typeID, f.Name, string(f.Type), i,
		); err != nil {
			return fmt.Errorf("insert field %q of content type %q: %w", f.Name, def.Name, err)
		}
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create table for content type %q: %w", def.Name, err)
		}
	}
	// The FULL composed schema (code + every dynamic type, including the new
	// one) — never the reduced one — so InspectSchema's canonical-metadata view
	// stays accurate and the next restart neither loses nor re-creates anything.
	if err := writeFullSchemaMetadata(ctx, tx, want); err != nil {
		return fmt.Errorf("record full schema metadata: %w", err)
	}
	return tx.Commit()
}

// isDuplicateContentTypeViolation reports whether err is the UNIQUE(name)
// constraint failure on content_types. SQLite (modernc driver) phrases it as
// "UNIQUE constraint failed: content_types.name". Matching the message keeps
// this dependency-free and is the same technique isUniqueSKUViolation /
// isUniqueSlugViolation already use. The constraint is the real guarantee; this
// only turns the DB error into a clean 400.
func isDuplicateContentTypeViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") &&
		strings.Contains(msg, schema.ContentTypesTable+".name")
}
