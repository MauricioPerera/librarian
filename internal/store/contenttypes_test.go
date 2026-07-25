package store_test

// CONTRACT-13 T2 acceptance tests — the most important tests of this contract.
//
// They exercise the exact TWO-LAYER bug that nearly took production down in
// CONTRACT-11, now in its dynamic-table form:
//
//	layer 1 (physical): does the table still exist after a restart?
//	layer 2 (metadata): does __compat_schema still CONTAIN it after a restart?
//
// Layer 2 is the treacherous one. compat's InspectSchema PREFERS the
// __compat_schema metadata row over the live catalog, so a restart that
// rewrites that row with a schema omitting the dynamic tables makes the next
// restart believe they never existed — and makes `--dump-schema` / `compat
// copy` export the instance without those tables AND all of their rows, in
// complete silence. These tests assert both layers explicitly rather than
// assuming either.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// reviewsDef is the dynamic content type used across these tests. It uses all
// five allowed field types so the composed schema is not trivially simple.
var reviewsDef = schema.ContentTypeDefinition{
	Name: "reviews",
	Fields: []schema.FieldDefinition{
		{Name: "headline", Type: schema.FieldText},
		{Name: "score", Type: schema.FieldInteger},
		{Name: "price_paid", Type: schema.FieldDecimal},
		{Name: "verified", Type: schema.FieldBoolean},
		{Name: "read_on", Type: schema.FieldDate},
	},
}

// tableExists asks SQLite's OWN catalog (never compat's metadata) whether a
// physical table exists. Using the catalog is the point: the metadata is the
// thing under test, so it cannot also be the oracle.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("sqlite_master lookup %q: %v", name, err)
	}
	return n > 0
}

// metadataSchema reads back the canonical schema compat stores in
// __compat_schema — the row InspectSchema prefers over the live catalog.
func metadataSchema(t *testing.T, db *sql.DB) compat.Schema {
	t.Helper()
	var payload string
	if err := db.QueryRow(`SELECT "value" FROM "__compat_schema" WHERE "key" = ?`, "canonical_schema").Scan(&payload); err != nil {
		t.Fatalf("read __compat_schema: %v", err)
	}
	var s compat.Schema
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		t.Fatalf("decode __compat_schema: %v", err)
	}
	return s
}

// hasTable reports whether a compat.Schema contains a table by name.
func hasTable(s compat.Schema, name string) bool {
	for _, tbl := range s.Tables {
		if tbl.Name == name {
			return true
		}
	}
	return false
}

// TestDynamicTypeSurvivesRestartCycle is THE test of this contract: create a
// dynamic content type, then simulate a real service restart (close the file,
// reopen it, run EnsureSchema again) and assert that BOTH layers survived —
// twice, because the CONTRACT-11 class of bug only manifests on the SECOND
// restart, once the corrupted metadata has been read back.
func TestDynamicTypeSurvivesRestartCycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	ctx := context.Background()

	// --- boot #1: normal startup, then an admin defines a dynamic type. ---
	db1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	if err := store.EnsureSchema(ctx, db1); err != nil {
		t.Fatalf("ensure #1: %v", err)
	}
	if err := store.SeedCatalogs(ctx, db1.DB); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.CreateContentType(ctx, db1, reviewsDef); err != nil {
		t.Fatalf("create content type: %v", err)
	}
	if !tableExists(t, db1.DB, reviewsDef.TableName()) {
		t.Fatalf("boot #1: dynamic table %q was not created", reviewsDef.TableName())
	}
	// CONTRACT-17: the REAL table is the prefixed one, and the bare public name
	// must not exist as a table at all.
	if tableExists(t, db1.DB, reviewsDef.Name) {
		t.Fatalf("boot #1: an UNPREFIXED table %q exists", reviewsDef.Name)
	}
	if !hasTable(metadataSchema(t, db1.DB), reviewsDef.TableName()) {
		t.Fatalf("boot #1: __compat_schema does not contain %q", reviewsDef.TableName())
	}
	// The code tables must still be in the metadata too — the reduced-metadata
	// trap works in both directions.
	if !hasTable(metadataSchema(t, db1.DB), "users") {
		t.Fatal("boot #1: __compat_schema lost the code table 'users'")
	}
	t.Logf("boot #1 OK: table %q exists=%v, unprefixed %q exists=%v, metadata tables=%d",
		reviewsDef.TableName(), tableExists(t, db1.DB, reviewsDef.TableName()),
		reviewsDef.Name, tableExists(t, db1.DB, reviewsDef.Name), len(metadataSchema(t, db1.DB).Tables))
	if err := db1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	// --- restart #1: exactly what the service does on boot. ---
	db2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	if err := store.EnsureSchema(ctx, db2); err != nil {
		t.Fatalf("RESTART FAILED: EnsureSchema #2 errored: %v", err)
	}
	if !tableExists(t, db2.DB, reviewsDef.TableName()) {
		t.Fatalf("restart #1: dynamic table %q disappeared", reviewsDef.TableName())
	}
	meta2 := metadataSchema(t, db2.DB)
	if !hasTable(meta2, reviewsDef.TableName()) {
		t.Fatalf("restart #1: __compat_schema NO LONGER contains 'reviews' — a `compat copy` would now silently omit that table and all its rows. Tables in metadata: %d", len(meta2.Tables))
	}
	if !hasTable(meta2, "users") || !hasTable(meta2, "products") {
		t.Fatal("restart #1: __compat_schema lost code tables")
	}
	t.Logf("restart #1 OK: table exists, metadata still contains 'reviews' (%d tables)", len(meta2.Tables))
	if err := db2.Close(); err != nil {
		t.Fatalf("close #2: %v", err)
	}

	// --- restart #2: the one that would fail if restart #1 corrupted the
	// metadata (the corrupted row is only READ BACK on the next boot). ---
	db3, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open #3: %v", err)
	}
	defer db3.Close()
	if err := store.EnsureSchema(ctx, db3); err != nil {
		t.Fatalf("SECOND RESTART FAILED: EnsureSchema #3 errored: %v", err)
	}
	if !tableExists(t, db3.DB, reviewsDef.TableName()) {
		t.Fatalf("restart #2: dynamic table %q disappeared", reviewsDef.TableName())
	}
	meta3 := metadataSchema(t, db3.DB)
	if !hasTable(meta3, reviewsDef.TableName()) {
		t.Fatalf("restart #2: __compat_schema no longer contains %q", reviewsDef.TableName())
	}
	// The dynamic definition itself must still be readable, and still compose.
	defs, err := store.LoadDefinitions(ctx, db3)
	if err != nil {
		t.Fatalf("load definitions after restart: %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "reviews" || len(defs[0].Fields) != len(reviewsDef.Fields) {
		t.Fatalf("definitions after restart = %+v, want the reviews definition with %d fields", defs, len(reviewsDef.Fields))
	}
	t.Logf("restart #2 OK: table exists, metadata contains 'reviews' (%d tables), definition reloaded with %d fields",
		len(meta3.Tables), len(defs[0].Fields))
}

// TestDumpSchemaIncludesDynamicType is the second half of T2: the artifact
// `compat copy` consumes as its schema_ref must include the dynamic table. If
// it did not, the export would silently drop that table and every row in it.
func TestDumpSchemaIncludesDynamicType(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dump.db")
	ctx := context.Background()

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.CreateContentType(ctx, db, reviewsDef); err != nil {
		t.Fatalf("create content type: %v", err)
	}

	// This is exactly what `librarian --dump-schema` does.
	defs, err := store.LoadDefinitions(ctx, db)
	if err != nil {
		t.Fatalf("load definitions: %v", err)
	}
	data, err := schema.JSONWith(defs)
	if err != nil {
		t.Fatalf("JSONWith: %v", err)
	}
	var dumped compat.Schema
	if err := json.Unmarshal(data, &dumped); err != nil {
		t.Fatalf("unmarshal dump: %v", err)
	}
	if !hasTable(dumped, reviewsDef.TableName()) {
		t.Fatalf("EXPORT WOULD OMIT DATA: --dump-schema output does not contain the dynamic table %q", reviewsDef.TableName())
	}
	if hasTable(dumped, reviewsDef.Name) {
		t.Fatalf("--dump-schema contains the UNPREFIXED table %q", reviewsDef.Name)
	}
	// Every code table must still be there (the composition must not replace).
	for _, tbl := range schema.Build().Tables {
		if !hasTable(dumped, tbl.Name) {
			t.Fatalf("dump lost code table %q", tbl.Name)
		}
	}
	// And the dump must carry the dynamic type's OWN columns, not just its name.
	var cols []string
	for _, tbl := range dumped.Tables {
		if tbl.Name == reviewsDef.TableName() {
			for _, c := range tbl.Columns {
				cols = append(cols, c.Name)
			}
		}
	}
	for _, want := range []string{"id", "author_id", "headline", "score", "price_paid", "verified", "read_on", "created_at", "updated_at", "metadata"} {
		if !strings.Contains(strings.Join(cols, ","), want) {
			t.Fatalf("dumped %q is missing column %q; got %v", reviewsDef.TableName(), want, cols)
		}
	}
	// It must compile for the EXPORT target too, not only for SQLite.
	if _, err := compat.CompileDDL(schema.PostgresTarget, dumped); err != nil {
		t.Fatalf("dumped schema does not compile for PostgreSQL (the export destination): %v", err)
	}
	t.Logf("DUMP OK: %d tables including dynamic %q %v; compiles for PostgreSQL", len(dumped.Tables), reviewsDef.TableName(), cols)
}

// TestCanonicalSchemaWithoutRegistry confirms the honest bootstrap case: a
// database whose registry table does not exist yet (a fresh file, or one
// created by a pre-CONTRACT-13 binary) composes to exactly the code schema.
// That is a PROOF of zero dynamic types, not a silent fallback — a definition
// can only live in that table.
func TestCanonicalSchemaWithoutRegistry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	ctx := context.Background()

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Nothing applied at all: the registry table does not exist.
	full, err := store.CanonicalSchema(ctx, db)
	if err != nil {
		t.Fatalf("CanonicalSchema on an empty database: %v", err)
	}
	if len(full.Tables) != len(schema.Build().Tables) {
		t.Fatalf("canonical schema of a registry-less database has %d tables, want %d (code only)",
			len(full.Tables), len(schema.Build().Tables))
	}
	t.Logf("registry-less database composes to the code schema exactly (%d tables)", len(full.Tables))
}

// TestUpgradeFromPreContract13Database is the real production upgrade path: a
// database created by the PREVIOUS binary (no registry tables at all) is
// restarted with this one. EnsureSchema must add just the two registry tables,
// leave every existing table and row alone, and leave the metadata complete.
func TestUpgradeFromPreContract13Database(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upgrade.db")
	ctx := context.Background()

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Build the "old" database: the current code schema MINUS the two registry
	// tables — byte-for-byte what a pre-CONTRACT-13 binary would have created.
	var old compat.Schema
	for _, tbl := range schema.Build().Tables {
		if tbl.Name == schema.ContentTypesTable || tbl.Name == schema.ContentTypeFieldsTable {
			continue
		}
		old.Tables = append(old.Tables, tbl)
	}
	if err := db.ApplySchema(ctx, old); err != nil {
		t.Fatalf("apply pre-CONTRACT-13 schema: %v", err)
	}
	if err := store.SeedCatalogs(ctx, db.DB); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, status) VALUES ('legacy@example.com', 'x', 'active')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	// The upgrade restart.
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("UPGRADE FAILED: EnsureSchema on a pre-CONTRACT-13 database errored: %v", err)
	}
	for _, name := range []string{schema.ContentTypesTable, schema.ContentTypeFieldsTable} {
		if !tableExists(t, db.DB, name) {
			t.Fatalf("upgrade did not create registry table %q", name)
		}
	}
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM users WHERE email = 'legacy@example.com'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pre-existing row lost during upgrade (count=%d, err=%v)", n, err)
	}
	meta := metadataSchema(t, db.DB)
	if !hasTable(meta, "users") || !hasTable(meta, schema.ContentTypesTable) {
		t.Fatalf("post-upgrade metadata incomplete (%d tables)", len(meta.Tables))
	}

	// And an admin can immediately define a dynamic type on the upgraded DB.
	if err := store.CreateContentType(ctx, db, reviewsDef); err != nil {
		t.Fatalf("create content type on upgraded database: %v", err)
	}
	// Followed by another restart, which must still be clean.
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema after upgrade + dynamic type: %v", err)
	}
	if !hasTable(metadataSchema(t, db.DB), reviewsDef.TableName()) {
		t.Fatalf("metadata lost %q after the post-upgrade restart", reviewsDef.TableName())
	}
	t.Logf("UPGRADE OK: registry added, legacy row intact, dynamic type created and surviving a restart")
}

// TestCreateContentTypeIsAtomic covers the red-team item: if the CREATE TABLE
// fails, the DEFINITION must not remain persisted. A type registered without
// its table is the worst reachable state — every later EnsureSchema would
// compose a schema describing a table that does not exist.
//
// The failure is induced realistically: a table with the target name already
// exists physically but is unknown to compat's metadata (e.g. left behind by an
// operator), so the diff says "missing" and the CREATE TABLE then fails.
func TestCreateContentTypeIsAtomic(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "atomic.db")
	ctx := context.Background()

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// Squat the name with a raw table compat's metadata knows nothing about.
	if _, err := db.DB.ExecContext(ctx, `CREATE TABLE "`+reviewsDef.TableName()+`" (x INTEGER)`); err != nil {
		t.Fatalf("squat table: %v", err)
	}

	err = store.CreateContentType(ctx, db, reviewsDef)
	if err == nil {
		t.Fatal("CreateContentType succeeded even though CREATE TABLE could not have")
	}
	t.Logf("create failed as expected: %v", err)

	// THE ASSERTION: nothing was persisted.
	var types, fields int
	if err := db.DB.QueryRow(`SELECT count(*) FROM ` + schema.ContentTypesTable).Scan(&types); err != nil {
		t.Fatalf("count content_types: %v", err)
	}
	if err := db.DB.QueryRow(`SELECT count(*) FROM ` + schema.ContentTypeFieldsTable).Scan(&fields); err != nil {
		t.Fatalf("count content_type_fields: %v", err)
	}
	if types != 0 || fields != 0 {
		t.Fatalf("PARTIAL STATE: content_types=%d, content_type_fields=%d rows persisted despite the failure — want 0/0", types, fields)
	}
	// And the metadata was not touched either.
	if hasTable(metadataSchema(t, db.DB), reviewsDef.TableName()) {
		t.Fatalf("PARTIAL STATE: __compat_schema records %q despite the failed creation", reviewsDef.TableName())
	}
	t.Logf("ATOMIC OK: 0 definition rows, 0 field rows, metadata untouched after the failed creation")
}

// TestCreateContentTypeDuplicateName confirms the duplicate-name outcome is the
// dedicated sentinel (→ 400 at the HTTP layer, never a 500) and that no second
// definition is persisted. The real guarantee is the schema-level UNIQUE(name);
// this test also exercises it directly, bypassing the friendly pre-check, by
// asserting the constraint exists in the applied table.
func TestCreateContentTypeDuplicateName(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dup.db")
	ctx := context.Background()

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.CreateContentType(ctx, db, reviewsDef); err != nil {
		t.Fatalf("create #1: %v", err)
	}
	err = store.CreateContentType(ctx, db, reviewsDef)
	if err == nil {
		t.Fatal("duplicate content type name was accepted")
	}
	if !errors.Is(err, store.ErrDuplicateContentType) {
		t.Fatalf("duplicate error = %v, want ErrDuplicateContentType", err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM ` + schema.ContentTypesTable).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("content_types has %d rows after a duplicate attempt, want 1", n)
	}
	// The constraint itself (not the application pre-check) is what wins a race:
	// insert directly and confirm the database rejects it.
	_, rawErr := db.DB.ExecContext(ctx, `INSERT INTO `+schema.ContentTypesTable+` (name) VALUES ('reviews')`)
	if rawErr == nil {
		t.Fatal("a direct duplicate INSERT succeeded — UNIQUE(name) is not enforced, so two concurrent creations could both win")
	}
	t.Logf("DUPLICATE OK: sentinel=%v; schema-level UNIQUE also rejects a direct insert: %v", err, rawErr)
}

// TestCreateContentTypeRejectsHostileNames confirms the store layer applies the
// T1 gate itself (defence in depth: the HTTP layer validates first, but a
// non-HTTP caller must not be able to bypass it) and persists nothing.
func TestCreateContentTypeRejectsHostileNames(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hostile.db")
	ctx := context.Background()

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// NOTE (CONTRACT-17): "users" is deliberately NOT in this list any more — a
	// type named like a code table is now legal because it produces "cpt_users".
	// A name too long for the PREFIXED table still is hostile.
	for _, name := range []string{`x"; DROP TABLE users; --`, "MiTipo", "1abc", "", "my type", strings.Repeat("a", 64), strings.Repeat("a", schema.MaxTypeNameLength+1)} {
		if err := store.CreateContentType(ctx, db, schema.ContentTypeDefinition{Name: name}); err == nil {
			t.Fatalf("store accepted hostile name %q", name)
		}
	}
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM ` + schema.ContentTypesTable).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d definitions persisted despite every name being hostile", n)
	}
	if err := db.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&n); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	t.Logf("HOSTILE OK: 0 definitions persisted; database still has %d tables (code schema only)", n)
}
