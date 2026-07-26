package store_test

// CONTRACT-27 acceptance tests at the STORE level, on SQLite (the dual-engine
// half lives in dualengine_contract27_test.go, behind the `dualengine` tag).
//
// They cover the four things this contract can get wrong without failing to
// compile:
//
//	T1 — the reference is a REAL foreign key in the composed table, and the
//	     registry table that holds it is additive.
//	T2 — the target must exist FIRST, with a message that says so.
//	T3 — editing and deleting a REFERENCED type are refused BEFORE anything is
//	     touched, naming the referrer; and after the referrer is deleted the
//	     target becomes editable and deletable again.
//	T5 — a restart cycle keeps both layers (physical table + __compat_schema),
//	     and `--dump-schema` orders the tables so a target always precedes its
//	     referrer.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// autoresC27 is the TARGET type, and libros the referrer. The names are chosen
// so that the referrer sorts BEFORE its target alphabetically ("libros" < …no):
// see librosC27's comment — the alphabetical trap is exercised explicitly by
// TestDumpSchemaOrdersTargetsBeforeReferrers.
var autoresC27 = schema.ContentTypeDefinition{
	Name: "autores",
	Fields: []schema.FieldDefinition{
		{Name: "nombre", Type: schema.FieldText},
	},
}

// librosC27 references autores. `libros` sorts AFTER `autores`, so this pair
// alone would pass even with the old name-only ordering; the pair that breaks it
// is in TestDumpSchemaOrdersTargetsBeforeReferrers.
var librosC27 = schema.ContentTypeDefinition{
	Name: "libros",
	Fields: []schema.FieldDefinition{
		{Name: "titulo", Type: schema.FieldText},
	},
	References: []schema.ReferenceDefinition{
		{Name: "autor", Target: "autores"},
	},
}

// newStoreC27 opens a fresh SQLite database through the production path.
func newStoreC27(t *testing.T) *compat.Store {
	t.Helper()
	db, err := store.Open(compat.SQLite, filepath.Join(t.TempDir(), "c27.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.SeedCatalogs(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

// TestReferencesRegistryTableIsCreatedAndFieldsTableIsUntouched is the T1
// acceptance criterion: the new table exists after a normal EnsureSchema, and
// content_type_fields is EXACTLY the table it was — same columns, same
// constraints — because altering it is the thing EnsureSchema cannot do.
func TestReferencesRegistryTableIsCreatedAndFieldsTableIsUntouched(t *testing.T) {
	db := newStoreC27(t)

	if !tableExists(t, db.DB, schema.ContentTypeReferencesTable) {
		t.Fatalf("EnsureSchema did not create %q", schema.ContentTypeReferencesTable)
	}
	if !hasTable(metadataSchema(t, db.DB), schema.ContentTypeReferencesTable) {
		t.Fatalf("__compat_schema does not contain %q", schema.ContentTypeReferencesTable)
	}

	// content_type_fields: unchanged, asserted against the engine's own DDL text.
	var ddl string
	if err := db.DB.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`,
		schema.ContentTypeFieldsTable).Scan(&ddl); err != nil {
		t.Fatalf("read the DDL of %q: %v", schema.ContentTypeFieldsTable, err)
	}
	for _, forbidden := range []string{"target_type_id", "reference"} {
		if strings.Contains(ddl, forbidden) {
			t.Errorf("%q gained %q — this contract must not change an existing table", schema.ContentTypeFieldsTable, forbidden)
		}
	}
	// The CHECK vocabulary is still exactly the five scalar families.
	for _, want := range schema.FieldTypeNames() {
		if !strings.Contains(ddl, want) {
			t.Errorf("the CHECK on %q lost the value %q", schema.ContentTypeFieldsTable, want)
		}
	}
	t.Logf("OK: %q created; %q DDL unchanged (%d bytes)", schema.ContentTypeReferencesTable, schema.ContentTypeFieldsTable, len(ddl))
}

// TestCreateWithReferenceProducesARealForeignKey is the heart of T1/T5: the
// composed table has an FK, verified against SQLite's OWN catalog, not against
// the schema we composed.
func TestCreateWithReferenceProducesARealForeignKey(t *testing.T) {
	ctx := context.Background()
	db := newStoreC27(t)

	if err := store.CreateContentType(ctx, db, autoresC27); err != nil {
		t.Fatalf("create autores: %v", err)
	}
	if err := store.CreateContentType(ctx, db, librosC27); err != nil {
		t.Fatalf("create libros: %v", err)
	}

	type fk struct{ table, from, to, onDelete string }
	rows, err := db.DB.Query(`SELECT "table", "from", "to", "on_delete" FROM pragma_foreign_key_list('cpt_libros')`)
	if err != nil {
		t.Fatalf("pragma_foreign_key_list: %v", err)
	}
	defer rows.Close()
	var found []fk
	for rows.Next() {
		var f fk
		if err := rows.Scan(&f.table, &f.from, &f.to, &f.onDelete); err != nil {
			t.Fatalf("scan fk: %v", err)
		}
		found = append(found, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate fks: %v", err)
	}
	var relation *fk
	for i := range found {
		if found[i].from == "autor" {
			relation = &found[i]
		}
	}
	if relation == nil {
		t.Fatalf("cpt_libros has no foreign key on the column 'autor'; catalog says: %+v", found)
	}
	if relation.table != "cpt_autores" || relation.to != "id" {
		t.Errorf("the FK points at %s(%s), want cpt_autores(id)", relation.table, relation.to)
	}
	if relation.onDelete != "RESTRICT" {
		t.Errorf("ON DELETE is %q, want RESTRICT (never CASCADE — see CONTRACT-27)", relation.onDelete)
	}
	t.Logf("OK: cpt_libros.autor → %s(%s) ON DELETE %s, read from the engine catalog", relation.table, relation.to, relation.onDelete)

	// And the definition reads back with its reference.
	def, err := store.FetchContentType(ctx, db, "libros")
	if err != nil {
		t.Fatalf("fetch libros: %v", err)
	}
	if len(def.References) != 1 || def.References[0].Name != "autor" || def.References[0].Target != "autores" {
		t.Fatalf("the persisted definition lost its reference: %+v", def.References)
	}
}

// TestCreateRejectsAReferenceToAMissingTarget is T2: the ORDER refusal, and it
// must be a named sentinel (→ 400) rather than a driver error.
func TestCreateRejectsAReferenceToAMissingTarget(t *testing.T) {
	ctx := context.Background()
	db := newStoreC27(t)

	err := store.CreateContentType(ctx, db, librosC27)
	if !errors.Is(err, store.ErrUnknownReferenceTarget) {
		t.Fatalf("want ErrUnknownReferenceTarget, got %v", err)
	}
	for _, want := range []string{"libros", "autor", "autores", "Create"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %s", want, err)
		}
	}
	// NOTHING was created.
	if tableExists(t, db.DB, "cpt_libros") {
		t.Fatal("a table was created despite the refusal")
	}
	t.Logf("OK: %v", err)
}

// TestSelfReferenceIsRejectedAsAKnownLimitation is the decided v1 restriction,
// and the message must say it is a limitation rather than blame the request.
func TestSelfReferenceIsRejectedAsAKnownLimitation(t *testing.T) {
	def := schema.ContentTypeDefinition{
		Name:       "nodos",
		References: []schema.ReferenceDefinition{{Name: "padre", Target: "nodos"}},
	}
	err := def.Validate()
	if err == nil {
		t.Fatal("a self-reference was accepted")
	}
	if !strings.Contains(err.Error(), "known limitation") {
		t.Errorf("the message does not present it as a known limitation: %s", err)
	}
	t.Logf("OK: %v", err)
}

// TestReferenceNameCannotCollideWithAField closes the duplicate-column hole:
// without this the collision would surface as an opaque compat error at apply
// time instead of a clean rejection at validation time.
func TestReferenceNameCannotCollideWithAField(t *testing.T) {
	def := schema.ContentTypeDefinition{
		Name:       "libros",
		Fields:     []schema.FieldDefinition{{Name: "autor", Type: schema.FieldText}},
		References: []schema.ReferenceDefinition{{Name: "autor", Target: "autores"}},
	}
	if err := def.Validate(); err == nil {
		t.Fatal("a reference colliding with a field name was accepted")
	} else {
		t.Logf("OK: %v", err)
	}
}

// TestReferencedTypeCannotBeEditedOrDeletedAndIsFreedAfterwards is T3, the
// point where this contract is made or ruined. It asserts three things in one
// arc: the refusal names the referrer, NOTHING was touched, and deleting the
// referrer frees the target for both operations.
func TestReferencedTypeCannotBeEditedOrDeletedAndIsFreedAfterwards(t *testing.T) {
	ctx := context.Background()
	db := newStoreC27(t)

	if err := store.CreateContentType(ctx, db, autoresC27); err != nil {
		t.Fatalf("create autores: %v", err)
	}
	if err := store.CreateContentType(ctx, db, librosC27); err != nil {
		t.Fatalf("create libros: %v", err)
	}

	autoresID, autoresFields, err := store.LoadContentTypeFields(ctx, db, "autores")
	if err != nil {
		t.Fatalf("load autores fields: %v", err)
	}

	// --- the EDIT is refused --------------------------------------------------
	edits := []store.FieldEdit{
		{ID: autoresFields[0].ID, Name: "nombre_completo", Type: schema.FieldText},
	}
	_, err = store.EditContentType(ctx, db, "autores", edits, nil)
	var referenced store.ReferencedTypeError
	if !errors.As(err, &referenced) {
		t.Fatalf("editing a referenced type: want ReferencedTypeError, got %v", err)
	}
	if referenced.Operation != "edit" || len(referenced.Referrers) != 1 ||
		referenced.Referrers[0].TypeName != "libros" || referenced.Referrers[0].ReferenceName != "autor" {
		t.Fatalf("the refusal does not name the referrer correctly: %+v", referenced)
	}
	for _, want := range []string{"libros", "autor", "CASCADE", "DELETE"} {
		if !strings.Contains(referenced.Error(), want) {
			t.Errorf("the edit refusal does not mention %q: %s", want, referenced.Error())
		}
	}
	// NOTHING was touched: the field is still called `nombre`.
	_, after, err := store.LoadContentTypeFields(ctx, db, "autores")
	if err != nil {
		t.Fatalf("reload autores fields: %v", err)
	}
	if len(after) != 1 || after[0].Name != "nombre" || after[0].ID != autoresFields[0].ID {
		t.Fatalf("the refused edit changed the registry: %+v", after)
	}
	if !tableExists(t, db.DB, "cpt_autores") {
		t.Fatal("the refused edit dropped the real table")
	}
	t.Logf("OK edit refusal: %v", referenced.Error())

	// --- the DELETE is refused ------------------------------------------------
	_, err = store.DeleteContentType(ctx, db, "autores",
		store.DeleteConfirmation{Name: "autores", Rows: 0, RowsStated: true})
	referenced = store.ReferencedTypeError{}
	if !errors.As(err, &referenced) {
		t.Fatalf("deleting a referenced type: want ReferencedTypeError, got %v", err)
	}
	if referenced.Operation != "delete" {
		t.Fatalf("the refusal names the wrong operation: %q", referenced.Operation)
	}
	if !tableExists(t, db.DB, "cpt_autores") {
		t.Fatal("the refused deletion dropped the real table")
	}
	if _, _, err := store.LoadContentTypeFields(ctx, db, "autores"); err != nil {
		t.Fatalf("the refused deletion removed the definition: %v", err)
	}
	t.Logf("OK delete refusal: %v", referenced.Error())
	_ = autoresID

	// --- free the target, then BOTH operations work ---------------------------
	if _, err := store.DeleteContentType(ctx, db, "libros",
		store.DeleteConfirmation{Name: "libros", Rows: 0, RowsStated: true}); err != nil {
		t.Fatalf("delete the referrer: %v", err)
	}
	if _, err := store.EditContentType(ctx, db, "autores", edits, nil); err != nil {
		t.Fatalf("after freeing it, the edit still failed: %v", err)
	}
	if _, err := store.DeleteContentType(ctx, db, "autores",
		store.DeleteConfirmation{Name: "autores", Rows: 0, RowsStated: true}); err != nil {
		t.Fatalf("after freeing it, the deletion still failed: %v", err)
	}
	if tableExists(t, db.DB, "cpt_autores") {
		t.Fatal("the target's table survived its deletion")
	}
	t.Log("OK: after deleting the referrer, the target is editable AND deletable again")
}

// TestEditKeepsTheReferenceColumnAndItsData is the quiet data-loss check: an
// edit rebuilds the table, and a rebuild that forgot the FK columns would leave
// them NULL — or drop them — without anything failing.
func TestEditKeepsTheReferenceColumnAndItsData(t *testing.T) {
	ctx := context.Background()
	db := newStoreC27(t)

	if err := store.CreateContentType(ctx, db, autoresC27); err != nil {
		t.Fatalf("create autores: %v", err)
	}
	if err := store.CreateContentType(ctx, db, librosC27); err != nil {
		t.Fatalf("create libros: %v", err)
	}
	// One author, one book pointing at it. author_id is NOT NULL with an FK to
	// users, so a real user id is needed; the seeded catalogs have none, so the
	// row is written with a user created here.
	userID := insertUserC27(t, db)
	if _, err := db.DB.Exec(
		`INSERT INTO "cpt_autores" ("id", "author_id", "nombre") VALUES (?, ?, ?)`,
		"11111111-1111-4111-8111-111111111111", userID, "Borges"); err != nil {
		t.Fatalf("insert autor: %v", err)
	}
	if _, err := db.DB.Exec(
		`INSERT INTO "cpt_libros" ("id", "author_id", "titulo", "autor") VALUES (?, ?, ?, ?)`,
		"22222222-2222-4222-8222-222222222222", userID, "Ficciones",
		"11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatalf("insert libro: %v", err)
	}

	_, fields, err := store.LoadContentTypeFields(ctx, db, "libros")
	if err != nil {
		t.Fatalf("load libros fields: %v", err)
	}
	// Rename the scalar field — a full rebuild of cpt_libros.
	if _, err := store.EditContentType(ctx, db, "libros",
		[]store.FieldEdit{{ID: fields[0].ID, Name: "titulo_completo", Type: schema.FieldText}}, nil); err != nil {
		t.Fatalf("edit libros: %v", err)
	}

	var autor string
	if err := db.DB.QueryRow(`SELECT "autor" FROM "cpt_libros" WHERE "id" = ?`,
		"22222222-2222-4222-8222-222222222222").Scan(&autor); err != nil {
		t.Fatalf("read the reference back after the rebuild: %v", err)
	}
	if autor != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("the rebuild lost the reference value: got %q", autor)
	}
	// And the FK is still there after the rebuild.
	var fks int
	if err := db.DB.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list('cpt_libros') WHERE "from" = 'autor'`).Scan(&fks); err != nil {
		t.Fatalf("pragma after rebuild: %v", err)
	}
	if fks != 1 {
		t.Fatalf("the rebuilt table has %d foreign keys on 'autor', want 1", fks)
	}
	t.Log("OK: the rebuild preserved the reference column, its value and its foreign key")
}

// insertUserC27 creates one user so content rows can satisfy author_id's FK.
func insertUserC27(t *testing.T, db *compat.Store) string {
	t.Helper()
	id := "33333333-3333-4333-8333-333333333333"
	if _, err := db.DB.Exec(
		`INSERT INTO "users" ("id", "email", "password_hash", "status") VALUES (?, ?, ?, ?)`,
		id, "c27@example.test", "x", "active"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// TestForeignKeyIsEnforcedOnSQLite proves the FK is not decorative: inserting a
// reference to an id that does not exist is refused BY THE ENGINE.
func TestForeignKeyIsEnforcedOnSQLite(t *testing.T) {
	ctx := context.Background()
	db := newStoreC27(t)
	if err := store.CreateContentType(ctx, db, autoresC27); err != nil {
		t.Fatalf("create autores: %v", err)
	}
	if err := store.CreateContentType(ctx, db, librosC27); err != nil {
		t.Fatalf("create libros: %v", err)
	}
	userID := insertUserC27(t, db)
	_, err := db.DB.Exec(
		`INSERT INTO "cpt_libros" ("id", "author_id", "titulo", "autor") VALUES (?, ?, ?, ?)`,
		"44444444-4444-4444-8444-444444444444", userID, "Fantasma",
		"99999999-9999-4999-8999-999999999999")
	if err == nil {
		t.Fatal("the engine accepted a reference to an id that does not exist")
	}
	t.Logf("OK: the engine refused it (%v)", err)
}

// TestDumpSchemaOrdersTargetsBeforeReferrers is THE red-team item: the export
// must be APPLICABLE, which means every FK target precedes its referrer in the
// table list. The pair is chosen so the ALPHABETICAL order is wrong: `alpha`
// references `zeta`, so a name sort puts the referrer first.
func TestDumpSchemaOrdersTargetsBeforeReferrers(t *testing.T) {
	ctx := context.Background()
	db := newStoreC27(t)

	zeta := schema.ContentTypeDefinition{Name: "zeta", Fields: []schema.FieldDefinition{{Name: "x", Type: schema.FieldText}}}
	alpha := schema.ContentTypeDefinition{
		Name:       "alpha",
		Fields:     []schema.FieldDefinition{{Name: "y", Type: schema.FieldText}},
		References: []schema.ReferenceDefinition{{Name: "z", Target: "zeta"}},
	}
	if err := store.CreateContentType(ctx, db, zeta); err != nil {
		t.Fatalf("create zeta: %v", err)
	}
	if err := store.CreateContentType(ctx, db, alpha); err != nil {
		t.Fatalf("create alpha: %v", err)
	}

	defs, err := store.LoadDefinitions(ctx, db)
	if err != nil {
		t.Fatalf("load definitions: %v", err)
	}
	payload, err := schema.JSONWith(defs)
	if err != nil {
		t.Fatalf("dump schema: %v", err)
	}
	var dumped compat.Schema
	if err := json.Unmarshal(payload, &dumped); err != nil {
		t.Fatalf("decode dump: %v", err)
	}
	position := map[string]int{}
	for i, tbl := range dumped.Tables {
		position[tbl.Name] = i
	}
	if position["cpt_zeta"] > position["cpt_alpha"] {
		t.Fatalf("the dump puts cpt_alpha (position %d) BEFORE its FK target cpt_zeta (position %d): applying it would fail",
			position["cpt_alpha"], position["cpt_zeta"])
	}
	t.Logf("OK: cpt_zeta at %d precedes its referrer cpt_alpha at %d (alphabetical order would have been the opposite)",
		position["cpt_zeta"], position["cpt_alpha"])

	// The dump must also be APPLIABLE for both engines, which is what compiling
	// it proves: compat emits the CREATE TABLEs in this order.
	for _, target := range []compat.Target{schema.SQLiteTarget, schema.PostgresTarget} {
		statements, err := compat.CompileDDL(target, dumped)
		if err != nil {
			t.Fatalf("compile the dumped schema for %s: %v", target.Engine, err)
		}
		var zetaAt, alphaAt = -1, -1
		for i, s := range statements {
			if strings.Contains(s, `"cpt_zeta"`) && zetaAt < 0 {
				zetaAt = i
			}
			if strings.Contains(s, `"cpt_alpha"`) && alphaAt < 0 {
				alphaAt = i
			}
		}
		if zetaAt < 0 || alphaAt < 0 || zetaAt > alphaAt {
			t.Fatalf("%s: cpt_zeta first appears at %d and cpt_alpha at %d", target.Engine, zetaAt, alphaAt)
		}
		t.Logf("OK %s: %d statements, cpt_zeta created at %d before cpt_alpha at %d", target.Engine, len(statements), zetaAt, alphaAt)
	}
}

// TestRelationSurvivesARestartCycle is the CONTRACT-13 restart test applied to a
// relation: both layers (physical FK + __compat_schema) must survive twice.
func TestRelationSurvivesARestartCycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "c27restart.db")
	ctx := context.Background()

	db1, err := store.Open(compat.SQLite, dbPath)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	if err := store.EnsureSchema(ctx, db1); err != nil {
		t.Fatalf("ensure #1: %v", err)
	}
	if err := store.CreateContentType(ctx, db1, autoresC27); err != nil {
		t.Fatalf("create autores: %v", err)
	}
	if err := store.CreateContentType(ctx, db1, librosC27); err != nil {
		t.Fatalf("create libros: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	for boot := 2; boot <= 3; boot++ {
		db, err := store.Open(compat.SQLite, dbPath)
		if err != nil {
			t.Fatalf("open #%d: %v", boot, err)
		}
		if err := store.EnsureSchema(ctx, db); err != nil {
			t.Fatalf("ensure #%d: %v", boot, err)
		}
		var fks int
		if err := db.DB.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list('cpt_libros') WHERE "from" = 'autor'`).Scan(&fks); err != nil {
			t.Fatalf("boot #%d pragma: %v", boot, err)
		}
		if fks != 1 {
			t.Fatalf("boot #%d: the FK is gone (%d)", boot, fks)
		}
		meta := metadataSchema(t, db.DB)
		if !hasTable(meta, "cpt_libros") || !hasTable(meta, "cpt_autores") || !hasTable(meta, schema.ContentTypeReferencesTable) {
			t.Fatalf("boot #%d: __compat_schema lost a table", boot)
		}
		defs, err := store.LoadDefinitions(ctx, db)
		if err != nil {
			t.Fatalf("boot #%d load: %v", boot, err)
		}
		var seen bool
		for _, d := range defs {
			if d.Name == "libros" && len(d.References) == 1 && d.References[0].Target == "autores" {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("boot #%d: the reference is no longer in the registry: %+v", boot, defs)
		}
		t.Logf("boot #%d OK: FK present, metadata has %d tables, the reference is in the registry", boot, len(meta.Tables))
		if err := db.Close(); err != nil {
			t.Fatalf("close #%d: %v", boot, err)
		}
	}
}

// TestLoadDefinitionsToleratesAnAbsentReferencesTable is the UPGRADE PATH check,
// and it is the failure that would have taken every deployed installation down
// at boot: LoadDefinitions runs INSIDE EnsureSchema, before the missing tables
// are created, so it must answer "zero relations" rather than erroring when
// content_type_references does not exist yet.
func TestLoadDefinitionsToleratesAnAbsentReferencesTable(t *testing.T) {
	ctx := context.Background()
	db := newStoreC27(t)
	if err := store.CreateContentType(ctx, db, autoresC27); err != nil {
		t.Fatalf("create autores: %v", err)
	}
	// Simulate a database created by a pre-CONTRACT-27 binary FAITHFULLY: the
	// table is absent AND __compat_schema does not mention it (InspectSchema
	// prefers that metadata over the live catalog, so dropping the table alone
	// would leave the diff believing it is still there — which is a property of
	// EnsureSchema, not of this contract).
	if _, err := db.DB.Exec(`DROP TABLE "` + schema.ContentTypeReferencesTable + `"`); err != nil {
		t.Fatalf("drop the references table: %v", err)
	}
	legacy := metadataSchema(t, db.DB)
	kept := legacy.Tables[:0]
	for _, tbl := range legacy.Tables {
		if tbl.Name != schema.ContentTypeReferencesTable {
			kept = append(kept, tbl)
		}
	}
	legacy.Tables = kept
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal the legacy metadata: %v", err)
	}
	if _, err := db.DB.Exec(
		`UPDATE "__compat_schema" SET "value" = ? WHERE "key" = ?`, string(payload), "canonical_schema"); err != nil {
		t.Fatalf("write the legacy metadata: %v", err)
	}
	defs, err := store.LoadDefinitions(ctx, db)
	if err != nil {
		t.Fatalf("LoadDefinitions on a database without the references table: %v", err)
	}
	if len(defs) != 1 || len(defs[0].References) != 0 {
		t.Fatalf("want one definition with no references, got %+v", defs)
	}
	// And EnsureSchema puts it back, additively.
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema after the drop: %v", err)
	}
	if !tableExists(t, db.DB, schema.ContentTypeReferencesTable) {
		t.Fatal("EnsureSchema did not re-create the references table")
	}
	t.Log("OK: an installation without the references table boots and gains it")
}
