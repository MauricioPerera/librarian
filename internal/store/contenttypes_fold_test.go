package store_test

// CONTRACT-34 store-level tests — the folded search column across the
// operations that reshape a dynamic type's table.
//
// This file exists because the third writer of the column, the CONTRACT-18
// rebuild, cannot be reached from the panel for the types the search actually
// serves: a relation TARGET is refused by CONTRACT-27's guard (measured in
// internal/server, TestRebuildIsRefusedForARelationTarget). The rebuild itself is
// nonetheless correct and must stay so, because a folded type that nothing
// references can be edited today and any folded type could be edited tomorrow.
//
// The evidence is always the stored value read back from the table, never the
// return of the function under test: a fold that lies is invisible from the API
// and only shows up as a search that finds the wrong row.
//
// Reuses openWithType, columnsOf and eventosDef from contenttypes_edit_test.go.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// insertFoldedEvento writes one row through raw SQL, folding the searched field
// the way the generic CRUD layer does. It is the fixture, not the thing under
// test: what is tested is what the REBUILD leaves behind. (The sibling file's
// insertEvento predates the column and would leave the fold NULL, which would
// make every assertion below pass for the wrong reason.)
func insertFoldedEvento(t *testing.T, db *compat.Store, authorID, titulo string, asistentes int) string {
	t.Helper()
	var id string
	if err := db.DB.QueryRowContext(context.Background(),
		`INSERT INTO "cpt_eventos" (author_id, titulo, lugar, asistentes, gratis, "_search_fold")
		 VALUES (?,?,?,?,?,?) RETURNING id`,
		authorID, titulo, "Montevideo", asistentes, true, schema.FoldSearchText(titulo)).Scan(&id); err != nil {
		t.Fatalf("insert evento %q: %v", titulo, err)
	}
	return id
}

// foldOf reads the stored fold of one row.
func foldOf(t *testing.T, db *compat.Store, id string) any {
	t.Helper()
	var fold *string
	if err := db.DB.QueryRowContext(context.Background(),
		`SELECT "_search_fold" FROM "cpt_eventos" WHERE id = ?`, id).Scan(&fold); err != nil {
		t.Fatalf("read fold of %q: %v", id, err)
	}
	if fold == nil {
		return nil
	}
	return *fold
}

// TestCreateContentTypeIsBornFolded is T1 at the level that decides everything
// downstream: the column and the registry marker are written by ONE transaction,
// so "the registry says folded" and "the table has the column" cannot disagree.
func TestCreateContentTypeIsBornFolded(t *testing.T) {
	db, _ := openWithType(t, "born.db")
	ctx := context.Background()

	cols := columnsOf(t, db.DB, "cpt_eventos")
	found := false
	for _, c := range cols {
		if c == schema.SearchFoldColumn {
			found = true
		}
	}
	if !found {
		t.Fatalf("a newly created type has no folded column: %v", cols)
	}
	def, err := store.FetchContentType(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !def.Folded {
		t.Fatalf("the registry does not mark the new type as folded")
	}
	// The marker is a row, and there is exactly one of it — the UNIQUE constraint
	// is what makes "folded" a membership rather than a counter.
	var n int
	if err := db.DB.QueryRowContext(ctx, `SELECT count(*) FROM content_type_folds`).Scan(&n); err != nil {
		t.Fatalf("count markers: %v", err)
	}
	if n != 1 {
		t.Fatalf("fold markers = %d, want 1", n)
	}
}

// TestUpgradeFromAnInstallationWithoutTheFoldRegister is THE upgrade path, and
// it is the test that makes T2's "nothing automatic at startup" verifiable
// instead of merely promised.
//
// It reproduces what every deployed database looks like the first time this
// binary runs against it: a type whose table has no folded column, and no fold
// register at all. The startup path must then (a) create the register, because
// it is a MISSING TABLE and that is the one thing EnsureSchema does; (b) leave
// the existing table exactly as it is, because EnsureSchema never alters one;
// and (c) read the type back as UNFOLDED, so the routine it composes matches the
// columns the table really has. Two consecutive starts, not one — the second is
// what caught the CONTRACT-11 bug.
func TestUpgradeFromAnInstallationWithoutTheFoldRegister(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := store.Open(compat.SQLite, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Build the "old" database the way TestUpgradeFromPreContract13Database does:
	// the schema the PREVIOUS binary would have produced — an UNFOLDED dynamic
	// type, and no fold register at all. It is reconstructed rather than surgically
	// undone because compat's InspectSchema prefers the __compat_schema metadata
	// over the physical catalog, so a database that merely lost a table would still
	// be REMEMBERED as having it and the upgrade step would be skipped.
	legacy := eventosDef // Folded is false: exactly a pre-CONTRACT-34 definition.
	composed, err := schema.BuildWith([]schema.ContentTypeDefinition{legacy})
	if err != nil {
		t.Fatalf("compose the old schema: %v", err)
	}
	var old compat.Schema
	for _, tbl := range composed.Tables {
		if tbl.Name == schema.ContentTypeFoldsTable {
			continue
		}
		old.Tables = append(old.Tables, tbl)
	}
	if err := db.ApplySchema(ctx, old); err != nil {
		t.Fatalf("apply the pre-CONTRACT-34 schema: %v", err)
	}
	if err := store.SeedCatalogs(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const legacyTypeID = "11111111-1111-4111-8111-111111111111"
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO content_types (id, name, created_at) VALUES (?,?,?)`,
		legacyTypeID, legacy.Name, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert the legacy registry row: %v", err)
	}
	for i, f := range legacy.Fields {
		if _, err := db.DB.ExecContext(ctx,
			`INSERT INTO content_type_fields (id, content_type_id, name, field_type, ordinal) VALUES (?,?,?,?,?)`,
			fmt.Sprintf("22222222-2222-4222-8222-00000000000%d", i), legacyTypeID, f.Name, string(f.Type), i); err != nil {
			t.Fatalf("insert the legacy field row %q: %v", f.Name, err)
		}
	}
	before := columnsOf(t, db.DB, "cpt_eventos")
	for _, c := range before {
		if c == schema.SearchFoldColumn {
			t.Fatalf("the reconstructed legacy table already has the folded column: %v", before)
		}
	}

	for i := 1; i <= 2; i++ {
		if err := store.EnsureSchema(ctx, db); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		var n int
		if err := db.DB.QueryRowContext(ctx, `SELECT count(*) FROM content_type_folds`).Scan(&n); err != nil {
			t.Fatalf("start %d: the register was not created: %v", i, err)
		}
		if n != 0 {
			t.Fatalf("start %d: the register has %d rows — startup marked a type as folded on its own", i, n)
		}
		if got := columnsOf(t, db.DB, "cpt_eventos"); len(got) != len(before) {
			t.Fatalf("start %d: startup ALTERED an existing table: %v, was %v", i, got, before)
		}
		def, err := store.FetchContentType(ctx, db, "eventos")
		if err != nil {
			t.Fatalf("start %d: fetch: %v", i, err)
		}
		if def.Folded {
			t.Fatalf("start %d: the pre-existing type is reported as folded", i)
		}
		// And the composed schema agrees with the physical table, which is what
		// makes every read of the type keep working.
		table, err := schema.DynamicTable(def)
		if err != nil {
			t.Fatalf("start %d: compose: %v", i, err)
		}
		for _, c := range table.Columns {
			if c.Name == schema.SearchFoldColumn {
				t.Fatalf("start %d: the composed table declares a column the real one lacks", i)
			}
		}
	}
}

// TestRebuildRecomputesTheFold is T3 for the rebuild, over the four reshapes
// that can change what the searched text IS. In every one of them the fold must
// describe the table as it ends up, not as it was.
func TestRebuildRecomputesTheFold(t *testing.T) {
	ctx := context.Background()

	t.Run("rename carries the values and refolds them", func(t *testing.T) {
		db, author := openWithType(t, "rename.db")
		id := insertFoldedEvento(t, db, author, "CHARLA GO", 40)
		_, stored, err := store.LoadContentTypeFields(ctx, db, "eventos")
		if err != nil {
			t.Fatalf("load fields: %v", err)
		}
		edits := make([]store.FieldEdit, 0, len(stored))
		for _, f := range stored {
			name := f.Name
			if name == "titulo" {
				name = "encabezado"
			}
			edits = append(edits, store.FieldEdit{ID: f.ID, Name: name, Type: f.Type})
		}
		if _, err := store.EditContentType(ctx, db, "eventos", edits, nil); err != nil {
			t.Fatalf("rename: %v", err)
		}
		// The value moved to a differently-named column and the fold followed it.
		if got := foldOf(t, db, id); got != "charla go" {
			t.Fatalf("fold after the rename = %v, want %q", got, "charla go")
		}
	})

	t.Run("removing the searched field blanks the fold", func(t *testing.T) {
		db, author := openWithType(t, "remove.db")
		id := insertFoldedEvento(t, db, author, "CHARLA GO", 40)
		_, stored, err := store.LoadContentTypeFields(ctx, db, "eventos")
		if err != nil {
			t.Fatalf("load fields: %v", err)
		}
		var edits []store.FieldEdit
		var removed []string
		for _, f := range stored {
			if f.Name == "titulo" {
				removed = append(removed, f.Name)
				continue
			}
			edits = append(edits, store.FieldEdit{ID: f.ID, Name: f.Name, Type: f.Type})
		}
		if _, err := store.EditContentType(ctx, db, "eventos", edits, removed); err != nil {
			t.Fatalf("remove: %v", err)
		}
		// The first field is now `lugar` (text), so the type is still searchable —
		// by a DIFFERENT column — and the fold is the fold of THAT one. A copied
		// fold would have kept offering the row under a title it no longer has.
		if got := foldOf(t, db, id); got != "montevideo" {
			t.Fatalf("fold after removing the searched field = %v, want %q", got, "montevideo")
		}
	})

	t.Run("a non-text first field leaves the fold NULL", func(t *testing.T) {
		db, author := openWithType(t, "nonsearchable.db")
		id := insertFoldedEvento(t, db, author, "CHARLA GO", 40)
		_, stored, err := store.LoadContentTypeFields(ctx, db, "eventos")
		if err != nil {
			t.Fatalf("load fields: %v", err)
		}
		byName := map[string]store.PersistedField{}
		for _, f := range stored {
			byName[f.Name] = f
		}
		// Reorder so the INTEGER field comes first: the type stops being searchable
		// (schema.DynamicSearchField refuses a non-text first field, because strpos
		// rejects one on PostgreSQL while instr coerces it on SQLite).
		edits := []store.FieldEdit{
			{ID: byName["asistentes"].ID, Name: "asistentes", Type: schema.FieldInteger},
			{ID: byName["titulo"].ID, Name: "titulo", Type: schema.FieldText},
			{ID: byName["lugar"].ID, Name: "lugar", Type: schema.FieldText},
			{ID: byName["gratis"].ID, Name: "gratis", Type: schema.FieldBoolean},
		}
		if _, err := store.EditContentType(ctx, db, "eventos", edits, nil); err != nil {
			t.Fatalf("reorder: %v", err)
		}
		if got := foldOf(t, db, id); got != nil {
			t.Fatalf("fold of a type with nothing searchable = %v, want NULL", got)
		}
	})

	t.Run("a new first field leaves the fold NULL for existing rows", func(t *testing.T) {
		db, author := openWithType(t, "added.db")
		id := insertFoldedEvento(t, db, author, "CHARLA GO", 40)
		_, stored, err := store.LoadContentTypeFields(ctx, db, "eventos")
		if err != nil {
			t.Fatalf("load fields: %v", err)
		}
		edits := []store.FieldEdit{{ID: "", Name: "clave", Type: schema.FieldText}}
		for _, f := range stored {
			edits = append(edits, store.FieldEdit{ID: f.ID, Name: f.Name, Type: f.Type})
		}
		if _, err := store.EditContentType(ctx, db, "eventos", edits, nil); err != nil {
			t.Fatalf("add a first field: %v", err)
		}
		// The searched column is now `clave`, which is NULL for every pre-existing
		// row — so the honest fold is NULL, not the old title. This is the case the
		// contract's red-team asks about: the row is invisible to the search until
		// it is written again, and that is CORRECT, because it has no searchable
		// text at all.
		if got := foldOf(t, db, id); got != nil {
			t.Fatalf("fold after a new searched field was added = %v, want NULL", got)
		}
	})
}

// TestRebuildKeepsTheTypeFolded guards the quietest way this could break: the
// rebuild re-creates the table from the plan's definition, so a plan that forgot
// to carry Folded would DROP the system column and every search over the type
// would then ask for a column the routine declares and the table lacks.
func TestRebuildKeepsTheTypeFolded(t *testing.T) {
	db, _ := openWithType(t, "keeps.db")
	ctx := context.Background()

	_, stored, err := store.LoadContentTypeFields(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("load fields: %v", err)
	}
	edits := []store.FieldEdit{{ID: "", Name: "resumen", Type: schema.FieldText}}
	for _, f := range stored {
		edits = append(edits, store.FieldEdit{ID: f.ID, Name: f.Name, Type: f.Type})
	}
	if _, err := store.EditContentType(ctx, db, "eventos", edits, nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	cols := columnsOf(t, db.DB, "cpt_eventos")
	found := false
	for _, c := range cols {
		if c == schema.SearchFoldColumn {
			found = true
		}
	}
	if !found {
		t.Fatalf("the rebuild dropped the folded column: %v", cols)
	}
	def, err := store.FetchContentType(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !def.Folded {
		t.Fatalf("the type stopped being marked as folded after an edit")
	}
	// And no staging table survived, the CONTRACT-18 invariant this file inherits.
	for _, c := range columnsOf(t, db.DB, schema.StagingTableName) {
		t.Fatalf("the staging table survived the rebuild (column %q)", c)
	}
}
