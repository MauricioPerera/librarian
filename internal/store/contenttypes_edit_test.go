package store_test

// CONTRACT-18 T1/T4 acceptance tests — editing the fields of an APPLIED dynamic
// content type.
//
// The evidence in this file is never the return value of the function under
// test: after every edit the assertions go to SQLite's OWN catalog (PRAGMA
// table_info / sqlite_master), to the rows themselves, and to the
// __compat_schema metadata row — the three things that can come apart, and the
// three the contract demands be proven rather than asserted in prose.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// eventosDef is the type edited across these tests: four fields of three
// different families, so a rebuild has something real to preserve.
var eventosDef = schema.ContentTypeDefinition{
	Name: "eventos",
	Fields: []schema.FieldDefinition{
		{Name: "titulo", Type: schema.FieldText},
		{Name: "lugar", Type: schema.FieldText},
		{Name: "asistentes", Type: schema.FieldInteger},
		{Name: "gratis", Type: schema.FieldBoolean},
	},
}

// openWithType opens a fresh database, applies the schema, seeds, creates
// eventosDef and returns the store plus the id of a real user (dynamic tables
// have a NOT NULL author_id with an FK to users).
func openWithType(t *testing.T, name string) (*compat.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), name)
	ctx := context.Background()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.SeedCatalogs(ctx, db.DB); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.CreateContentType(ctx, db, eventosDef); err != nil {
		t.Fatalf("create content type: %v", err)
	}
	var authorID string
	if err := db.DB.QueryRowContext(ctx,
		`INSERT INTO users (email, password_hash, status) VALUES ('a@example.com', 'x', 'active') RETURNING id`).Scan(&authorID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return db, authorID
}

// columnsOf returns the physical column names of a table, in order, from
// SQLite's own catalog.
func columnsOf(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		t.Fatalf("PRAGMA table_info(%q): %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notNull, pk int
			dflt        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		out = append(out, name)
	}
	return out
}

// metadataColumnsOf returns the columns compat's __compat_schema metadata row
// claims a table has — the view InspectSchema PREFERS over the live catalog.
func metadataColumnsOf(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	for _, tbl := range metadataSchema(t, db).Tables {
		if tbl.Name == table {
			var out []string
			for _, c := range tbl.Columns {
				out = append(out, c.Name)
			}
			return out
		}
	}
	return nil
}

// stagingTables returns every physical table using the staging prefix. The
// answer must be empty after every operation, successful or not.
func stagingTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE ?`, schema.StagingTablePrefix+"%")
	if err != nil {
		t.Fatalf("scan for staging tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	return out
}

// insertEvento inserts one row into cpt_eventos and returns its generated id.
func insertEvento(t *testing.T, db *sql.DB, authorID, titulo, lugar string, asistentes int, gratis bool) string {
	t.Helper()
	var id string
	if err := db.QueryRow(
		`INSERT INTO "cpt_eventos" (author_id, titulo, lugar, asistentes, gratis) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		authorID, titulo, lugar, asistentes, gratis).Scan(&id); err != nil {
		t.Fatalf("insert evento: %v", err)
	}
	return id
}

// fieldIDs maps field name → stored id for a content type.
func fieldIDs(t *testing.T, db *sql.DB, typeName string) map[string]string {
	t.Helper()
	_, fields, err := store.LoadContentTypeFields(context.Background(), db, typeName)
	if err != nil {
		t.Fatalf("LoadContentTypeFields(%q): %v", typeName, err)
	}
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		out[f.Name] = f.ID
	}
	return out
}

// storedFields returns "name:type:ordinal" triples in ordinal order.
func storedFields(t *testing.T, db *sql.DB, typeName string) []string {
	t.Helper()
	_, fields, err := store.LoadContentTypeFields(context.Background(), db, typeName)
	if err != nil {
		t.Fatalf("LoadContentTypeFields(%q): %v", typeName, err)
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Name+":"+string(f.Type))
	}
	return out
}

// TestEditContentTypeRoundTripWithRealData is THE test of the contract: one
// edit that ADDS, RENAMES and REMOVES at the same time, over rows that have
// real values in every field, verified row by row.
func TestEditContentTypeRoundTripWithRealData(t *testing.T) {
	db, author := openWithType(t, "roundtrip.db")
	ctx := context.Background()

	type row struct {
		id                string
		titulo, lugar     string
		asistentes        int
		gratis            bool
		createdAt, author string
	}
	want := []row{
		{titulo: "Charla Go", lugar: "Montevideo", asistentes: 40, gratis: true},
		{titulo: "Taller SQL", lugar: "Rosario", asistentes: 12, gratis: false},
		{titulo: "Meetup", lugar: "Lima", asistentes: 0, gratis: true},
	}
	for i := range want {
		want[i].id = insertEvento(t, db.DB, author, want[i].titulo, want[i].lugar, want[i].asistentes, want[i].gratis)
		if err := db.DB.QueryRow(`SELECT created_at, author_id FROM "cpt_eventos" WHERE id = ?`, want[i].id).
			Scan(&want[i].createdAt, &want[i].author); err != nil {
			t.Fatalf("read created_at: %v", err)
		}
	}

	ids := fieldIDs(t, db.DB, "eventos")
	// One edit that does everything: rename titulo→encabezado, keep asistentes
	// and gratis (reordered), REMOVE lugar, ADD resumen.
	edits := []store.FieldEdit{
		{ID: ids["titulo"], Name: "encabezado", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{ID: ids["gratis"], Name: "gratis", Type: schema.FieldBoolean},
		{Name: "resumen", Type: schema.FieldText},
	}
	plan, err := store.EditContentType(ctx, db, "eventos", edits, []string{"lugar"})
	if err != nil {
		t.Fatalf("EditContentType: %v", err)
	}
	t.Logf("plan: added=%v removed=%v renamed=%v", plan.Added, plan.Removed, plan.Renamed)

	// --- the table: same NAME, new SHAPE, no staging leftovers ---
	if !tableExists(t, db.DB, "cpt_eventos") {
		t.Fatal("cpt_eventos disappeared")
	}
	if leftovers := stagingTables(t, db.DB); len(leftovers) != 0 {
		t.Fatalf("staging table(s) left behind: %v", leftovers)
	}
	gotCols := columnsOf(t, db.DB, "cpt_eventos")
	wantCols := []string{"id", "author_id", "encabezado", "asistentes", "gratis", "resumen", "created_at", "updated_at", "metadata"}
	if strings.Join(gotCols, ",") != strings.Join(wantCols, ",") {
		t.Fatalf("columns = %v, want %v", gotCols, wantCols)
	}

	// --- the rows: same ids, carried values, NULL for the new field ---
	for _, w := range want {
		var (
			encabezado sql.NullString
			asistentes sql.NullInt64
			gratis     sql.NullBool
			resumen    sql.NullString
			createdAt  string
			authorID   string
		)
		err := db.DB.QueryRow(
			`SELECT encabezado, asistentes, gratis, resumen, created_at, author_id FROM "cpt_eventos" WHERE id = ?`, w.id).
			Scan(&encabezado, &asistentes, &gratis, &resumen, &createdAt, &authorID)
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("ROW IDENTITY LOST: no row with id %s after the edit", w.id)
		}
		if err != nil {
			t.Fatalf("read row %s: %v", w.id, err)
		}
		if encabezado.String != w.titulo {
			t.Fatalf("row %s: renamed field encabezado = %q, want the old titulo %q", w.id, encabezado.String, w.titulo)
		}
		if asistentes.Int64 != int64(w.asistentes) {
			t.Fatalf("row %s: asistentes = %d, want %d", w.id, asistentes.Int64, w.asistentes)
		}
		if gratis.Bool != w.gratis {
			t.Fatalf("row %s: gratis = %v, want %v", w.id, gratis.Bool, w.gratis)
		}
		if resumen.Valid {
			t.Fatalf("row %s: the ADDED field resumen = %q, want NULL", w.id, resumen.String)
		}
		if createdAt != w.createdAt {
			t.Fatalf("row %s: created_at changed (%q → %q)", w.id, w.createdAt, createdAt)
		}
		if authorID != w.author {
			t.Fatalf("row %s: author_id changed", w.id)
		}
	}
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM "cpt_eventos"`).Scan(&n); err != nil || n != len(want) {
		t.Fatalf("row count = %d (err=%v), want %d", n, err, len(want))
	}
	// The removed column really is gone.
	if _, err := db.DB.Exec(`SELECT lugar FROM "cpt_eventos"`); err == nil {
		t.Fatal("the removed column 'lugar' still exists")
	}

	// --- the registry: renamed row KEPT ITS IDENTITY, ordinals are the new ones ---
	after := fieldIDs(t, db.DB, "eventos")
	if after["encabezado"] != ids["titulo"] {
		t.Fatalf("renamed field lost its identity: id was %s, now %s", ids["titulo"], after["encabezado"])
	}
	if got := storedFields(t, db.DB, "eventos"); strings.Join(got, ",") != "encabezado:text,asistentes:integer,gratis:boolean,resumen:text" {
		t.Fatalf("registry fields = %v", got)
	}

	// --- the metadata: agrees with the physical catalog, column for column ---
	meta := metadataColumnsOf(t, db.DB, "cpt_eventos")
	if strings.Join(meta, ",") != strings.Join(gotCols, ",") {
		t.Fatalf("__compat_schema LIES: metadata columns %v vs catalog %v", meta, gotCols)
	}
	t.Logf("ROUND-TRIP OK: %d rows kept their ids and data; columns %v; metadata agrees with the catalog", len(want), gotCols)
}

// TestEditContentTypeCrossRename is the red-team item that breaks any naive
// implementation: `titulo`→`lugar` and `lugar`→`titulo` IN THE SAME EDIT. A
// rename-one-at-a-time implementation either collides on the unique constraint
// or ends up with both columns holding the same data.
func TestEditContentTypeCrossRename(t *testing.T) {
	db, author := openWithType(t, "crossrename.db")
	ctx := context.Background()

	id := insertEvento(t, db.DB, author, "EL TITULO", "EL LUGAR", 7, true)
	ids := fieldIDs(t, db.DB, "eventos")

	edits := []store.FieldEdit{
		{ID: ids["titulo"], Name: "lugar", Type: schema.FieldText},
		{ID: ids["lugar"], Name: "titulo", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{ID: ids["gratis"], Name: "gratis", Type: schema.FieldBoolean},
	}
	if _, err := store.EditContentType(ctx, db, "eventos", edits, nil); err != nil {
		t.Fatalf("cross rename: %v", err)
	}

	var titulo, lugar string
	if err := db.DB.QueryRow(`SELECT titulo, lugar FROM "cpt_eventos" WHERE id = ?`, id).Scan(&titulo, &lugar); err != nil {
		t.Fatalf("read row: %v", err)
	}
	// The values FOLLOW THE IDENTITY: the field that used to be called titulo is
	// now called lugar, so its value ("EL TITULO") is now under lugar.
	if lugar != "EL TITULO" || titulo != "EL LUGAR" {
		t.Fatalf("CROSS RENAME WRONG: titulo=%q lugar=%q, want titulo=%q lugar=%q", titulo, lugar, "EL LUGAR", "EL TITULO")
	}
	// The registry swapped too, keeping both identities.
	after := fieldIDs(t, db.DB, "eventos")
	if after["lugar"] != ids["titulo"] || after["titulo"] != ids["lugar"] {
		t.Fatalf("registry identities not swapped: %v vs %v", after, ids)
	}
	if got := storedFields(t, db.DB, "eventos"); strings.Join(got, ",") != "lugar:text,titulo:text,asistentes:integer,gratis:boolean" {
		t.Fatalf("registry fields after cross rename = %v", got)
	}
	t.Logf("CROSS-RENAME OK: values follow identity (titulo=%q, lugar=%q) and both registry rows kept their ids", titulo, lugar)
}

// TestEditContentTypeRollbackMidwayFK forces a failure IN THE MIDDLE of the
// rebuild — after the rows have already been copied into the staging table —
// and proves the database is left exactly as it was.
//
// The failure is induced realistically: another table holds a foreign key into
// cpt_eventos, so the DROP of the original table (step 3, right after the
// forward copy) is refused by the engine.
func TestEditContentTypeRollbackMidwayFK(t *testing.T) {
	db, author := openWithType(t, "rollbackfk.db")
	ctx := context.Background()

	id := insertEvento(t, db.DB, author, "Charla", "Montevideo", 40, true)
	if _, err := db.DB.Exec(`CREATE TABLE "zz_refs" (id INTEGER PRIMARY KEY, evento_id TEXT REFERENCES "cpt_eventos"("id"))`); err != nil {
		t.Fatalf("create referencing table: %v", err)
	}
	if _, err := db.DB.Exec(`INSERT INTO "zz_refs" (id, evento_id) VALUES (1, ?)`, id); err != nil {
		t.Fatalf("insert referencing row: %v", err)
	}
	beforeCols := columnsOf(t, db.DB, "cpt_eventos")
	beforeFields := storedFields(t, db.DB, "eventos")
	beforeIDs := fieldIDs(t, db.DB, "eventos")
	beforeMeta := metadataColumnsOf(t, db.DB, "cpt_eventos")

	edits := []store.FieldEdit{
		{ID: beforeIDs["titulo"], Name: "encabezado", Type: schema.FieldText},
		{ID: beforeIDs["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{Name: "resumen", Type: schema.FieldText},
	}
	_, err := store.EditContentType(ctx, db, "eventos", edits, []string{"lugar", "gratis"})
	if err == nil {
		t.Fatal("the rebuild succeeded although DROP TABLE could not have")
	}
	t.Logf("rebuild failed as expected: %v", err)

	// THE ASSERTIONS: nothing moved.
	if got := columnsOf(t, db.DB, "cpt_eventos"); strings.Join(got, ",") != strings.Join(beforeCols, ",") {
		t.Fatalf("table shape changed despite the failure: %v, want %v", got, beforeCols)
	}
	var titulo, lugar string
	var asistentes int
	if err := db.DB.QueryRow(`SELECT titulo, lugar, asistentes FROM "cpt_eventos" WHERE id = ?`, id).Scan(&titulo, &lugar, &asistentes); err != nil {
		t.Fatalf("original row lost: %v", err)
	}
	if titulo != "Charla" || lugar != "Montevideo" || asistentes != 40 {
		t.Fatalf("original row damaged: %q/%q/%d", titulo, lugar, asistentes)
	}
	if got := storedFields(t, db.DB, "eventos"); strings.Join(got, ",") != strings.Join(beforeFields, ",") {
		t.Fatalf("registry changed despite the failure: %v, want %v", got, beforeFields)
	}
	if got := fieldIDs(t, db.DB, "eventos"); fmt.Sprint(got) != fmt.Sprint(beforeIDs) {
		t.Fatalf("field identities changed despite the failure: %v, want %v", got, beforeIDs)
	}
	if got := metadataColumnsOf(t, db.DB, "cpt_eventos"); strings.Join(got, ",") != strings.Join(beforeMeta, ",") {
		t.Fatalf("metadata changed despite the failure: %v, want %v", got, beforeMeta)
	}
	if leftovers := stagingTables(t, db.DB); len(leftovers) != 0 {
		t.Fatalf("staging table survived the rollback: %v", leftovers)
	}
	t.Logf("ROLLBACK OK: shape %v, row intact, registry %v, metadata intact, no staging leftovers", beforeCols, beforeFields)
}

// TestEditContentTypeRollbackAtMetadataStep forces the failure at the LAST step
// — after BOTH copies and the registry update — by removing the metadata table
// the final write targets. It is the complement of the FK test: the rollback
// must undo work that had already reached the registry rows.
func TestEditContentTypeRollbackAtMetadataStep(t *testing.T) {
	db, author := openWithType(t, "rollbackmeta.db")
	ctx := context.Background()

	id := insertEvento(t, db.DB, author, "Charla", "Montevideo", 40, true)
	beforeCols := columnsOf(t, db.DB, "cpt_eventos")
	beforeFields := storedFields(t, db.DB, "eventos")
	ids := fieldIDs(t, db.DB, "eventos")

	// Induce the failure at step 8 (outside the transaction, so the drop itself
	// is not rolled back and the write is guaranteed to fail).
	if _, err := db.DB.Exec(`DROP TABLE "__compat_schema"`); err != nil {
		t.Fatalf("drop metadata table: %v", err)
	}

	edits := []store.FieldEdit{
		{ID: ids["titulo"], Name: "encabezado", Type: schema.FieldText},
		{ID: ids["lugar"], Name: "lugar", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{ID: ids["gratis"], Name: "gratis", Type: schema.FieldBoolean},
		{Name: "resumen", Type: schema.FieldText},
	}
	if _, err := store.EditContentType(ctx, db, "eventos", edits, nil); err == nil {
		t.Fatal("the rebuild succeeded although the metadata write could not have")
	} else {
		t.Logf("rebuild failed as expected at the metadata step: %v", err)
	}

	if got := columnsOf(t, db.DB, "cpt_eventos"); strings.Join(got, ",") != strings.Join(beforeCols, ",") {
		t.Fatalf("table shape changed despite the failure: %v, want %v", got, beforeCols)
	}
	var titulo string
	if err := db.DB.QueryRow(`SELECT titulo FROM "cpt_eventos" WHERE id = ?`, id).Scan(&titulo); err != nil || titulo != "Charla" {
		t.Fatalf("row lost or damaged: %q (err=%v)", titulo, err)
	}
	if got := storedFields(t, db.DB, "eventos"); strings.Join(got, ",") != strings.Join(beforeFields, ",") {
		t.Fatalf("registry changed despite the failure: %v, want %v", got, beforeFields)
	}
	if got := fieldIDs(t, db.DB, "eventos"); fmt.Sprint(got) != fmt.Sprint(ids) {
		t.Fatalf("field identities changed despite the failure")
	}
	if leftovers := stagingTables(t, db.DB); len(leftovers) != 0 {
		t.Fatalf("staging table survived the rollback: %v", leftovers)
	}
	t.Logf("ROLLBACK-AT-LAST-STEP OK: table %v, registry %v, no staging leftovers", beforeCols, beforeFields)
}

// TestEditContentTypeSurvivesTwoRestarts is the CONTRACT-11 lesson applied to
// this contract: the corrupted-metadata class of bug only shows up on the
// SECOND boot, once the bad row has been read back.
func TestEditContentTypeSurvivesTwoRestarts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart-edit.db")
	ctx := context.Background()

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
	if err := store.CreateContentType(ctx, db1, eventosDef); err != nil {
		t.Fatalf("create: %v", err)
	}
	var author string
	if err := db1.DB.QueryRow(`INSERT INTO users (email, password_hash, status) VALUES ('r@example.com','x','active') RETURNING id`).Scan(&author); err != nil {
		t.Fatalf("user: %v", err)
	}
	rowID := insertEvento(t, db1.DB, author, "Charla", "Montevideo", 40, true)
	ids := fieldIDs(t, db1.DB, "eventos")
	if _, err := store.EditContentType(ctx, db1, "eventos", []store.FieldEdit{
		{ID: ids["titulo"], Name: "encabezado", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{Name: "resumen", Type: schema.FieldText},
	}, []string{"lugar", "gratis"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	wantCols := columnsOf(t, db1.DB, "cpt_eventos")
	if err := db1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	for boot := 1; boot <= 2; boot++ {
		db, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("open boot #%d: %v", boot, err)
		}
		// A restart must be a NO-OP: nothing missing, nothing to create.
		want, err := store.CanonicalSchema(ctx, db)
		if err != nil {
			t.Fatalf("canonical schema boot #%d: %v", boot, err)
		}
		inspection, err := db.InspectSchema(ctx)
		if err != nil {
			t.Fatalf("inspect boot #%d: %v", boot, err)
		}
		present := map[string]struct{}{}
		for _, tbl := range inspection.Schema.Tables {
			present[tbl.Name] = struct{}{}
		}
		var missing []string
		for _, tbl := range want.Tables {
			if _, ok := present[tbl.Name]; !ok {
				missing = append(missing, tbl.Name)
			}
		}
		if len(missing) != 0 {
			t.Fatalf("boot #%d would CREATE %v — the metadata does not match the post-edit reality", boot, missing)
		}
		if err := store.EnsureSchema(ctx, db); err != nil {
			t.Fatalf("EnsureSchema boot #%d: %v", boot, err)
		}
		if got := columnsOf(t, db.DB, "cpt_eventos"); strings.Join(got, ",") != strings.Join(wantCols, ",") {
			t.Fatalf("boot #%d: columns = %v, want %v", boot, got, wantCols)
		}
		var encabezado string
		if err := db.DB.QueryRow(`SELECT encabezado FROM "cpt_eventos" WHERE id = ?`, rowID).Scan(&encabezado); err != nil || encabezado != "Charla" {
			t.Fatalf("boot #%d: row lost (%q, err=%v)", boot, encabezado, err)
		}
		if got := storedFields(t, db.DB, "eventos"); strings.Join(got, ",") != "encabezado:text,asistentes:integer,resumen:text" {
			t.Fatalf("boot #%d: registry = %v", boot, got)
		}
		if leftovers := stagingTables(t, db.DB); len(leftovers) != 0 {
			t.Fatalf("boot #%d: staging leftovers %v", boot, leftovers)
		}
		t.Logf("boot #%d OK: nothing missing, columns %v, data intact", boot, wantCols)
		if err := db.Close(); err != nil {
			t.Fatalf("close boot #%d: %v", boot, err)
		}
	}
}

// TestEditContentTypeDumpSchemaReflectsNewShape covers the export half: the
// artifact `compat copy` consumes must describe the NEW columns and not the old
// ones, and must still compile for PostgreSQL.
func TestEditContentTypeDumpSchemaReflectsNewShape(t *testing.T) {
	db, _ := openWithType(t, "dump-edit.db")
	ctx := context.Background()

	ids := fieldIDs(t, db.DB, "eventos")
	if _, err := store.EditContentType(ctx, db, "eventos", []store.FieldEdit{
		{ID: ids["titulo"], Name: "encabezado", Type: schema.FieldText},
		{Name: "resumen", Type: schema.FieldText},
	}, []string{"lugar", "asistentes", "gratis"}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	defs, err := store.LoadDefinitions(ctx, db)
	if err != nil {
		t.Fatalf("load definitions: %v", err)
	}
	data, err := schema.JSONWith(defs)
	if err != nil {
		t.Fatalf("JSONWith: %v", err)
	}
	dump := string(data)
	for _, want := range []string{`"encabezado"`, `"resumen"`, `"cpt_eventos"`} {
		if !strings.Contains(dump, want) {
			t.Fatalf("dump does not contain %s", want)
		}
	}
	for _, unwanted := range []string{`"titulo"`, `"lugar"`, `"asistentes"`, `"gratis"`, schema.StagingTableName} {
		if strings.Contains(dump, unwanted) {
			t.Fatalf("dump still contains the removed/internal name %s", unwanted)
		}
	}
	full, err := schema.BuildWith(defs)
	if err != nil {
		t.Fatalf("BuildWith: %v", err)
	}
	if _, err := compat.CompileDDL(schema.PostgresTarget, full); err != nil {
		t.Fatalf("post-edit schema does not compile for PostgreSQL: %v", err)
	}
	t.Logf("DUMP OK: the export describes the NEW shape and compiles for PostgreSQL")
}

// TestEditContentTypeEasyPaths covers the two paths the contract insists must
// also work: a type with ZERO rows, and an edit that removes nothing.
func TestEditContentTypeEasyPaths(t *testing.T) {
	db, author := openWithType(t, "easy.db")
	ctx := context.Background()

	// (1) zero rows: pure add.
	ids := fieldIDs(t, db.DB, "eventos")
	all := []store.FieldEdit{
		{ID: ids["titulo"], Name: "titulo", Type: schema.FieldText},
		{ID: ids["lugar"], Name: "lugar", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{ID: ids["gratis"], Name: "gratis", Type: schema.FieldBoolean},
	}
	plan, err := store.EditContentType(ctx, db, "eventos", append(append([]store.FieldEdit{}, all...),
		store.FieldEdit{Name: "resumen", Type: schema.FieldText}), nil)
	if err != nil {
		t.Fatalf("edit with zero rows: %v", err)
	}
	if len(plan.Added) != 1 || len(plan.Removed) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	if got := columnsOf(t, db.DB, "cpt_eventos"); !strings.Contains(strings.Join(got, ","), "resumen") {
		t.Fatalf("resumen not added: %v", got)
	}

	// (2) rows present, edit that removes nothing (rename only).
	id := insertEvento(t, db.DB, author, "Charla", "Montevideo", 40, true)
	ids = fieldIDs(t, db.DB, "eventos")
	if _, err := store.EditContentType(ctx, db, "eventos", []store.FieldEdit{
		{ID: ids["titulo"], Name: "encabezado", Type: schema.FieldText},
		{ID: ids["lugar"], Name: "lugar", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{ID: ids["gratis"], Name: "gratis", Type: schema.FieldBoolean},
		{ID: ids["resumen"], Name: "resumen", Type: schema.FieldText},
	}, nil); err != nil {
		t.Fatalf("rename-only edit: %v", err)
	}
	var encabezado string
	if err := db.DB.QueryRow(`SELECT encabezado FROM "cpt_eventos" WHERE id = ?`, id).Scan(&encabezado); err != nil || encabezado != "Charla" {
		t.Fatalf("rename-only edit lost data: %q (err=%v)", encabezado, err)
	}

	// (3) the identical definition: a NO-OP that touches nothing.
	ids = fieldIDs(t, db.DB, "eventos")
	before := columnsOf(t, db.DB, "cpt_eventos")
	plan, err = store.EditContentType(ctx, db, "eventos", []store.FieldEdit{
		{ID: ids["encabezado"], Name: "encabezado", Type: schema.FieldText},
		{ID: ids["lugar"], Name: "lugar", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{ID: ids["gratis"], Name: "gratis", Type: schema.FieldBoolean},
		{ID: ids["resumen"], Name: "resumen", Type: schema.FieldText},
	}, nil)
	if err != nil {
		t.Fatalf("no-op edit: %v", err)
	}
	if !plan.NoOp {
		t.Fatal("an identical definition was not recognised as a no-op")
	}
	if got := columnsOf(t, db.DB, "cpt_eventos"); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("no-op edit changed the table: %v vs %v", got, before)
	}
	if got := fieldIDs(t, db.DB, "eventos"); fmt.Sprint(got) != fmt.Sprint(ids) {
		t.Fatal("no-op edit changed the field identities")
	}
	t.Logf("EASY PATHS OK: zero rows, no-removal edit and identical definition all behave")
}

// TestEditContentTypeRejections covers every refusal, and proves each one is
// decided WITHOUT writing anything.
func TestEditContentTypeRejections(t *testing.T) {
	db, author := openWithType(t, "reject.db")
	ctx := context.Background()
	insertEvento(t, db.DB, author, "Charla", "Montevideo", 40, true)
	ids := fieldIDs(t, db.DB, "eventos")
	beforeCols := columnsOf(t, db.DB, "cpt_eventos")
	beforeFields := storedFields(t, db.DB, "eventos")

	keepAll := func(extra ...store.FieldEdit) []store.FieldEdit {
		base := []store.FieldEdit{
			{ID: ids["titulo"], Name: "titulo", Type: schema.FieldText},
			{ID: ids["lugar"], Name: "lugar", Type: schema.FieldText},
			{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
			{ID: ids["gratis"], Name: "gratis", Type: schema.FieldBoolean},
		}
		return append(base, extra...)
	}

	cases := []struct {
		name    string
		typeArg string
		edits   []store.FieldEdit
		confirm []string
		check   func(error) bool
	}{
		{
			name:    "unknown content type",
			typeArg: "no_existe",
			edits:   nil,
			check:   func(err error) bool { return errors.Is(err, store.ErrContentTypeNotFound) },
		},
		{
			name:    "unknown field id",
			typeArg: "eventos",
			edits:   []store.FieldEdit{{ID: "00000000-0000-0000-0000-000000000000", Name: "x", Type: schema.FieldText}},
			check:   func(err error) bool { return errors.Is(err, store.ErrUnknownField) },
		},
		{
			name:    "same field twice",
			typeArg: "eventos",
			edits: []store.FieldEdit{
				{ID: ids["titulo"], Name: "a", Type: schema.FieldText},
				{ID: ids["titulo"], Name: "b", Type: schema.FieldText},
			},
			check: func(err error) bool { return errors.Is(err, store.ErrUnknownField) },
		},
		{
			name:    "type change",
			typeArg: "eventos",
			edits: []store.FieldEdit{
				{ID: ids["titulo"], Name: "titulo", Type: schema.FieldInteger},
			},
			check: func(err error) bool { return errors.Is(err, store.ErrFieldTypeChange) },
		},
		{
			name:    "field name collides with an injected column",
			typeArg: "eventos",
			edits:   keepAll(store.FieldEdit{Name: "updated_at", Type: schema.FieldText}),
			check:   func(err error) bool { return strings.Contains(err.Error(), "reserved") },
		},
		{
			name:    "duplicate field name",
			typeArg: "eventos",
			edits:   keepAll(store.FieldEdit{Name: "titulo", Type: schema.FieldText}),
			check:   func(err error) bool { return strings.Contains(err.Error(), "more than once") },
		},
		{
			name:    "invalid field name",
			typeArg: "eventos",
			edits:   keepAll(store.FieldEdit{Name: "Mal Nombre", Type: schema.FieldText}),
			check:   func(err error) bool { return strings.Contains(err.Error(), "is invalid") },
		},
		{
			name:    "unknown field type",
			typeArg: "eventos",
			edits:   keepAll(store.FieldEdit{Name: "raro", Type: schema.FieldType("json")}),
			check:   func(err error) bool { return strings.Contains(err.Error(), "unknown type") },
		},
		{
			name:    "removal without confirmation",
			typeArg: "eventos",
			edits: []store.FieldEdit{
				{ID: ids["titulo"], Name: "titulo", Type: schema.FieldText},
			},
			check: func(err error) bool {
				var loss store.DataLossError
				if !errors.As(err, &loss) {
					return false
				}
				return strings.Join(loss.Fields, ",") == "lugar,asistentes,gratis"
			},
		},
		{
			name:    "confirmation of a field that is not being removed",
			typeArg: "eventos",
			edits:   keepAll(),
			confirm: []string{"lugar"},
			check: func(err error) bool {
				var extra store.UnexpectedConfirmationError
				return errors.As(err, &extra)
			},
		},
	}
	for _, c := range cases {
		_, err := store.EditContentType(ctx, db, c.typeArg, c.edits, c.confirm)
		if err == nil {
			t.Fatalf("%s: the edit was ACCEPTED", c.name)
		}
		if !c.check(err) {
			t.Fatalf("%s: unexpected error %v", c.name, err)
		}
		t.Logf("%-50s rejected: %v", c.name, err)
	}

	// Nothing was written by ANY of them.
	if got := columnsOf(t, db.DB, "cpt_eventos"); strings.Join(got, ",") != strings.Join(beforeCols, ",") {
		t.Fatalf("a rejected edit changed the table: %v, want %v", got, beforeCols)
	}
	if got := storedFields(t, db.DB, "eventos"); strings.Join(got, ",") != strings.Join(beforeFields, ",") {
		t.Fatalf("a rejected edit changed the registry: %v, want %v", got, beforeFields)
	}
	if leftovers := stagingTables(t, db.DB); len(leftovers) != 0 {
		t.Fatalf("a rejected edit left staging tables: %v", leftovers)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM "cpt_eventos"`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("row count = %d (err=%v), want 1", n, err)
	}
}

// TestEditContentTypeWithoutItsTable covers the last red-team question: the
// definition exists but the real table does not. A rebuild must refuse loudly
// instead of "helpfully" creating the table (which would hide a database that
// is not in the state the registry claims).
func TestEditContentTypeWithoutItsTable(t *testing.T) {
	db, _ := openWithType(t, "notable.db")
	ctx := context.Background()

	if _, err := db.DB.Exec(`DROP TABLE "cpt_eventos"`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	ids := fieldIDs(t, db.DB, "eventos")
	_, err := store.EditContentType(ctx, db, "eventos", []store.FieldEdit{
		{ID: ids["titulo"], Name: "titulo", Type: schema.FieldText},
		{ID: ids["lugar"], Name: "lugar", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{ID: ids["gratis"], Name: "gratis", Type: schema.FieldBoolean},
		{Name: "resumen", Type: schema.FieldText},
	}, nil)
	if !errors.Is(err, store.ErrMissingTable) {
		t.Fatalf("error = %v, want ErrMissingTable", err)
	}
	t.Logf("MISSING TABLE OK: %v", err)
}

// TestEditContentTypeRefusesALeftoverStagingTable proves the other precondition
// fails loudly rather than dropping something that is already there.
func TestEditContentTypeRefusesALeftoverStagingTable(t *testing.T) {
	db, _ := openWithType(t, "squat.db")
	ctx := context.Background()

	if _, err := db.DB.Exec(`CREATE TABLE "` + schema.StagingTableName + `" (x INTEGER)`); err != nil {
		t.Fatalf("squat staging table: %v", err)
	}
	ids := fieldIDs(t, db.DB, "eventos")
	_, err := store.EditContentType(ctx, db, "eventos", []store.FieldEdit{
		{ID: ids["titulo"], Name: "encabezado", Type: schema.FieldText},
		{ID: ids["lugar"], Name: "lugar", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{ID: ids["gratis"], Name: "gratis", Type: schema.FieldBoolean},
	}, nil)
	if !errors.Is(err, store.ErrStagingTableExists) {
		t.Fatalf("error = %v, want ErrStagingTableExists", err)
	}
	// The squatted table is untouched — the refusal did not drop it.
	if !tableExists(t, db.DB, schema.StagingTableName) {
		t.Fatal("the pre-existing staging table was dropped by the refusal")
	}
	t.Logf("STAGING PRECONDITION OK: %v", err)
}

// TestEditWithManyRowsIsLinearAndComplete answers the red-team question about a
// type with MANY rows: the copy is O(2n) and that is accepted, but it must not
// lose or duplicate a single row.
func TestEditWithManyRowsIsLinearAndComplete(t *testing.T) {
	db, author := openWithType(t, "manyrows.db")
	ctx := context.Background()

	const rows = 2000
	tx, err := db.DB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := tx.Exec(`INSERT INTO "cpt_eventos" (author_id, titulo, lugar, asistentes, gratis) VALUES (?, ?, ?, ?, ?)`,
			author, fmt.Sprintf("t%d", i), "L", i, i%2 == 0); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	ids := fieldIDs(t, db.DB, "eventos")
	if _, err := store.EditContentType(ctx, db, "eventos", []store.FieldEdit{
		{ID: ids["titulo"], Name: "encabezado", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{Name: "resumen", Type: schema.FieldText},
	}, []string{"lugar", "gratis"}); err != nil {
		t.Fatalf("edit with %d rows: %v", rows, err)
	}
	var n, distinct, matched int
	if err := db.DB.QueryRow(`SELECT count(*), count(DISTINCT id) FROM "cpt_eventos"`).Scan(&n, &distinct); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != rows || distinct != rows {
		t.Fatalf("after the edit: %d rows, %d distinct ids, want %d/%d", n, distinct, rows, rows)
	}
	if err := db.DB.QueryRow(`SELECT count(*) FROM "cpt_eventos" WHERE encabezado = 't' || asistentes`).Scan(&matched); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if matched != rows {
		t.Fatalf("only %d of %d rows kept the titulo↔asistentes correspondence", matched, rows)
	}
	t.Logf("MANY ROWS OK: %d rows copied twice (O(2n), accepted), all ids distinct, all values in correspondence", rows)
}
