//go:build dualengine

package store_test

// CONTRACT-21 T3 — the battery that proves internal/store is dual-engine, and
// specifically that the ATOMICITY of CreateContentType and EditContentType
// survives on BOTH engines.
//
// It follows the CONTRACT-19/20 pattern and is excluded from the default suite
// by the same build tag, because it needs a live PostgreSQL 17 with pgvector:
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineStore -count=1 -v ./internal/store
//
// Without COMPAT_POSTGRES_DSN it SKIPS rather than passing vacuously.
//
// WHY THIS ONE MATTERS MORE THAN THE OTHER TWO. CONTRACT-19 and CONTRACT-20
// migrated statements whose failure mode is "a wrong answer". This package
// migrates statements that run inside TRANSACTIONS THAT MIX DDL, application SQL
// and metadata, and whose failure mode is a database left half-changed. The two
// engines do NOT behave the same after an error inside a transaction —
// PostgreSQL poisons the whole transaction (25P02), SQLite does not — so
// "it rolls back the same way" is a claim that has to be MEASURED on both, which
// is what the rollback scenarios below do. Both engines run the SAME scenario
// and the two transcripts must match line for line; driver error TEXT is
// deliberately never recorded (it legitimately differs), only whether an error
// occurred and, above all, THE STATE OF THE DATABASE AFTERWARDS.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

const pgDSNEnv = "COMPAT_POSTGRES_DSN"

// eventosC21 is the type the edit scenarios rebuild: four fields of three
// families, so a rebuild has real data to preserve.
var eventosC21 = schema.ContentTypeDefinition{
	Name: "eventos",
	Fields: []schema.FieldDefinition{
		{Name: "titulo", Type: schema.FieldText},
		{Name: "lugar", Type: schema.FieldText},
		{Name: "asistentes", Type: schema.FieldInteger},
		{Name: "gratis", Type: schema.FieldBoolean},
	},
}

// collationPairA/B are two type names chosen so that BYTE order and the
// en_US.utf8 COLLATION order DISAGREE — the divergence CONTRACT-20 measured,
// applied to the one listing this package owns.
//
//	bytes     : `_` is 0x5F and `a` is 0x61, so "blog_notas" < "bloganuncios"
//	collation : punctuation has no primary weight, so "blognotas" vs
//	            "bloganuncios" compares `a` < `n` → "bloganuncios" first
//
// The order of THIS listing is the order of the tables in the composed canonical
// schema, which is what `--dump-schema` emits and what ApplySchema creates in.
// If it diverged, the two engines would produce different canonical schemas from
// the same content types.
const (
	collationPairA = "blog_notas"
	collationPairB = "bloganuncios"
)

// TestDualEngineStore runs the whole scenario on a real SQLite file and a real
// PostgreSQL 17 schema and requires the transcripts to match line for line.
func TestDualEngineStore(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteTranscript := runStoreScenario(t, "sqlite", openSQLiteForStore, "")
	pgTranscript := runStoreScenario(t, "postgres", openPostgresForStore, dsn)

	t.Logf("transcript (%d lines, identical on both engines):\n%s",
		len(sqliteTranscript), strings.Join(sqliteTranscript, "\n"))

	if len(sqliteTranscript) != len(pgTranscript) {
		t.Fatalf("transcript length differs: sqlite=%d postgres=%d", len(sqliteTranscript), len(pgTranscript))
	}
	diverged := 0
	for i := range sqliteTranscript {
		if sqliteTranscript[i] != pgTranscript[i] {
			diverged++
			t.Errorf("line %d diverges:\n  sqlite  : %s\n  postgres: %s", i+1, sqliteTranscript[i], pgTranscript[i])
		}
	}
	if diverged == 0 {
		t.Logf("OK: %d observations identical on SQLite and PostgreSQL 17", len(sqliteTranscript))
	}
}

// --- engine fixtures ---------------------------------------------------------

// engineOpener returns a FRESH, EMPTY database for the engine — "clean install",
// which is the premise of this contract — plus a teardown.
type engineOpener func(t *testing.T, dsn string) (*compat.Store, func())

// openSQLiteForStore is the production path on a brand new file.
func openSQLiteForStore(t *testing.T, _ string) (*compat.Store, func()) {
	t.Helper()
	st, err := store.Open(compat.SQLite, filepath.Join(t.TempDir(), "dual21.db"))
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	return st, func() { _ = st.Close() }
}

// openPostgresForStore creates a FRESH, uniquely named schema so a run can never
// collide with leftovers, and drops it afterwards. Unlike the CONTRACT-19/20
// batteries — which had to apply the schema with compat's own ApplySchema
// because internal/store was still SQLite-only — this one uses the REAL
// production path (store.EnsureSchema / store.SeedCatalogs), because making that
// path work on PostgreSQL is the entire subject of this contract.
func openPostgresForStore(t *testing.T, dsn string) (*compat.Store, func()) {
	t.Helper()
	ctx := context.Background()
	schemaName := fmt.Sprintf("librarian_c21_%d", time.Now().UnixNano())

	admin, err := compat.OpenPostgres(schema.PostgresVersion, dsn)
	if err != nil {
		t.Fatalf("postgres open (admin): %v", err)
	}
	if _, err := admin.DB.ExecContext(ctx, `CREATE SCHEMA "`+schemaName+`"`); err != nil {
		_ = admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	scoped, err := store.Open(compat.Postgres, withSearchPathC21(dsn, schemaName))
	if err != nil {
		t.Fatalf("postgres open (scoped): %v", err)
	}
	var current string
	if err := scoped.DB.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	if current != schemaName {
		t.Fatalf("current_schema() = %q, want %q — the scoped connection is not isolated", current, schemaName)
	}
	return scoped, func() {
		_ = scoped.Close()
		if _, err := admin.DB.ExecContext(context.Background(), `DROP SCHEMA "`+schemaName+`" CASCADE`); err != nil {
			t.Logf("drop schema %s: %v", schemaName, err)
		}
		_ = admin.Close()
	}
}

// withSearchPathC21 pins every connection of the pool to one schema. `public`
// stays as a SECOND entry only so the pgvector `vector` TYPE resolves; the run's
// own schema stays first and current_schema() is asserted right after
// connecting.
func withSearchPathC21(dsn, schemaName string) string {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "search_path=" + schemaName + ",public"
}

// --- the scenario ------------------------------------------------------------

func runStoreScenario(t *testing.T, engineLabel string, open engineOpener, dsn string) []string {
	t.Helper()
	ctx := context.Background()
	st, cleanup := open(t, dsn)
	defer cleanup()

	var lines []string
	record := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	errWord := func(err error) string {
		if err == nil {
			return "none"
		}
		return "yes"
	}

	// --- 1. THE CLEAN INSTALL: the production startup path, twice ------------
	//
	// Two boots, not one: the CONTRACT-11/13 lesson is that the metadata class of
	// bug only shows up on the SECOND boot, once the row written by the first has
	// been read back.
	record("boot#1 ensureSchema err=%s", errWord(store.EnsureSchema(ctx, st)))
	record("boot#1 seedCatalogs err=%s", errWord(store.SeedCatalogs(ctx, st)))
	record("boot#2 ensureSchema err=%s", errWord(store.EnsureSchema(ctx, st)))
	record("boot#2 seedCatalogs err=%s", errWord(store.SeedCatalogs(ctx, st)))
	record("catalogs roles=%d permissions=%d taxonomies=%d",
		countRowsC21(t, st, "roles"), countRowsC21(t, st, "permissions"), countRowsC21(t, st, "taxonomies"))

	base, err := store.CanonicalSchema(ctx, st)
	if err != nil {
		t.Fatalf("[%s] canonical schema: %v", engineLabel, err)
	}
	record("clean install tables=%d views=%d routines=%d dynamic=0",
		len(base.Tables), len(base.Views), len(base.Routines))

	// The two catalog probes, on a table that certainly exists and one that
	// certainly does not. compat.TableExists asks the ENGINE catalog, which is
	// the only oracle that cannot be circular with the metadata EnsureSchema
	// rewrites.
	record("tableExists users=%v absent=%v",
		tableExistsC21(t, st, "users"), tableExistsC21(t, st, "no_such_table_here"))
	// RED-TEAM: a VIEW must NOT answer true — otherwise a content type could be
	// created over the name of an existing view and the diff would disagree with
	// the DDL.
	record("tableExists view(user_role_names)=%v", tableExistsC21(t, st, "user_role_names"))

	// --- 2. CreateContentType: the happy path --------------------------------
	authorID := seedAuthorC21(t, st)
	record("author created=%v", authorID != "")

	record("createContentType eventos err=%s", errWord(store.CreateContentType(ctx, st, eventosC21)))
	record("after create table-exists=%v registry-types=%d registry-fields=%d",
		tableExistsC21(t, st, eventosC21.TableName()),
		countRowsC21(t, st, schema.ContentTypesTable), countRowsC21(t, st, schema.ContentTypeFieldsTable))
	// RED-TEAM: does TableExists see a table created inside the transaction that
	// just committed? (The probe is deliberately never issued INSIDE that
	// transaction — see the note at the end of this scenario.)
	record("metadata knows the dynamic table=%v", metadataHasTableC21(t, st, eventosC21.TableName()))
	record("stored fields=%v", storedFieldNamesC21(t, st, "eventos"))

	// --- 3. Duplicate name → the sentinel, not a 500 -------------------------
	//
	// THE REGRESSION THIS CATCHES: the duplicate used to be detected by matching
	// SQLite's message text, so on PostgreSQL it would never have fired.
	dupErr := store.CreateContentType(ctx, st, eventosC21)
	record("createContentType duplicate err=%s sentinel=%v types=%d",
		errWord(dupErr), errorsIsDuplicateC21(dupErr), countRowsC21(t, st, schema.ContentTypesTable))

	// --- 4. ATOMICITY of CreateContentType -----------------------------------
	//
	// The failure is induced the same way the SQLite-only test induces it: a
	// table with the target name already exists physically but is unknown to
	// compat's metadata, so the diff says "missing" and the CREATE TABLE then
	// fails HALFWAY THROUGH the transaction — after the definition rows have
	// already been inserted.
	squatted := schema.ContentTypeDefinition{
		Name:   "resenas",
		Fields: []schema.FieldDefinition{{Name: "texto", Type: schema.FieldText}},
	}
	execC21(t, st, `CREATE TABLE "`+squatted.TableName()+`" ("x" INTEGER)`)
	atomicErr := store.CreateContentType(ctx, st, squatted)
	record("createContentType squatted err=%s", errWord(atomicErr))
	record("ATOMIC create: types=%d fields=%d metadata-has-squatted=%v",
		countRowsC21(t, st, schema.ContentTypesTable),
		countRowsC21(t, st, schema.ContentTypeFieldsTable),
		metadataHasTableC21(t, st, squatted.TableName()))
	execC21(t, st, `DROP TABLE "`+squatted.TableName()+`"`)

	// --- 5. The ORDER of the definitions, over a collation-divergent pair ----
	for _, name := range []string{collationPairB, collationPairA} {
		def := schema.ContentTypeDefinition{
			Name:   name,
			Fields: []schema.FieldDefinition{{Name: "cuerpo", Type: schema.FieldText}},
		}
		if err := store.CreateContentType(ctx, st, def); err != nil {
			t.Fatalf("[%s] create %q: %v", engineLabel, name, err)
		}
	}
	defs, err := store.LoadDefinitions(ctx, st)
	if err != nil {
		t.Fatalf("[%s] load definitions: %v", engineLabel, err)
	}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	record("definitions order=%v", names)
	// PROOF THAT THE FIXTURE IS DISCRIMINATING (the CONTRACT-20B lesson: a
	// comparison whose data cannot produce a difference proves nothing). This is
	// LOGGED, never recorded, precisely because it is the raw `ORDER BY t.name`
	// the two engines disagree about — recording it would make the transcripts
	// differ by design.
	t.Logf("[%s] RAW SQL ORDER BY name = %v  (Go-imposed order = %v)",
		engineLabel, rawTypeNameOrderC21(t, st), names)
	composed, err := store.CanonicalSchema(ctx, st)
	if err != nil {
		t.Fatalf("[%s] composed schema: %v", engineLabel, err)
	}
	record("composed schema dynamic-table-order=%v", dynamicTableNamesC21(composed))

	// --- 6. --dump-schema: the artifact `compat copy` consumes ---------------
	//
	// The SAME content types on the two engines must produce the SAME canonical
	// JSON, byte for byte. That is what makes the two dumps comparable and the
	// export faithful, and it is a red-team question of this contract.
	dump, err := schema.JSONWith(defs)
	if err != nil {
		t.Fatalf("[%s] dump schema: %v", engineLabel, err)
	}
	record("dump-schema bytes=%d sha-like-prefix=%s", len(dump), firstLineC21(dump))
	record("dump-schema digest=%s", digestC21(dump))

	// --- 7. EditContentType: add + rename + remove, over real rows -----------
	rowID := insertEventoC21(t, st, authorID, "Charla", "Montevideo", 40, true)
	beforeIDs := fieldIDsC21(t, st, "eventos")
	edits := []store.FieldEdit{
		{ID: beforeIDs["titulo"], Name: "encabezado", Type: schema.FieldText},
		{ID: beforeIDs["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{Name: "resumen", Type: schema.FieldText},
	}
	plan, editErr := store.EditContentType(ctx, st, "eventos", edits, []string{"lugar", "gratis"})
	record("editContentType err=%s added=%v removed=%v renamed=%v",
		errWord(editErr), plan.Added, plan.Removed, plan.Renamed)
	record("after edit fields=%v", storedFieldNamesC21(t, st, "eventos"))
	record("after edit carried row encabezado=%q asistentes=%d resumen-null=%v",
		textColumnC21(t, st, eventosC21.TableName(), "encabezado", rowID),
		intColumnC21(t, st, eventosC21.TableName(), "asistentes", rowID),
		isNullColumnC21(t, st, eventosC21.TableName(), "resumen", rowID))
	record("after edit identities preserved=%v",
		fieldIDsC21(t, st, "eventos")["encabezado"] == beforeIDs["titulo"])

	// --- 8. THE CROSS-RENAME: a↔b in one edit -------------------------------
	ids := fieldIDsC21(t, st, "eventos")
	cross := []store.FieldEdit{
		{ID: ids["encabezado"], Name: "resumen", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{ID: ids["resumen"], Name: "encabezado", Type: schema.FieldText},
	}
	_, crossErr := store.EditContentType(ctx, st, "eventos", cross, nil)
	record("crossRename err=%s fields=%v", errWord(crossErr), storedFieldNamesC21(t, st, "eventos"))
	record("crossRename data followed identity resumen=%q",
		textColumnC21(t, st, eventosC21.TableName(), "resumen", rowID))

	// --- 9. ROLLBACK MIDWAY: the DROP TABLE cannot happen -------------------
	//
	// A second table with a foreign key onto the dynamic table makes step 3 of
	// the rebuild (DROP the original) fail — AFTER the staging table has been
	// created and the rows copied into it. Both engines refuse the drop, for
	// different reasons and with different messages; what has to be identical is
	// what is left behind, which is: nothing.
	execC21(t, st, `CREATE TABLE "zz_refs" ("id" TEXT PRIMARY KEY, "evento_id" TEXT REFERENCES "`+eventosC21.TableName()+`"("id"))`)
	execArgsC21(t, st, `INSERT INTO "zz_refs" ("id", "evento_id") VALUES (`+
		compat.Placeholder(st.Target.Engine, 1)+`, `+compat.Placeholder(st.Target.Engine, 2)+`)`, "r1", rowID)

	beforeFields := storedFieldNamesC21(t, st, "eventos")
	beforeIdentities := fieldIDsC21(t, st, "eventos")
	beforeMeta := metadataColumnsC21(t, st, eventosC21.TableName())
	ids = fieldIDsC21(t, st, "eventos")
	doomed := []store.FieldEdit{
		{ID: ids["resumen"], Name: "resumen_v2", Type: schema.FieldText},
		{Name: "extra", Type: schema.FieldText},
	}
	_, dropErr := store.EditContentType(ctx, st, "eventos", doomed, []string{"asistentes", "encabezado"})
	record("ROLLBACK-midway err=%s", errWord(dropErr))
	record("ROLLBACK-midway fields=%v identities-intact=%v",
		storedFieldNamesC21(t, st, "eventos"),
		fmt.Sprint(fieldIDsC21(t, st, "eventos")) == fmt.Sprint(beforeIdentities))
	record("ROLLBACK-midway table-columns=%v", tableColumnsC21(t, st, eventosC21.TableName()))
	record("ROLLBACK-midway row-intact resumen=%q", textColumnC21(t, st, eventosC21.TableName(), "resumen", rowID))
	record("ROLLBACK-midway metadata-intact=%v staging-left=%v",
		fmt.Sprint(metadataColumnsC21(t, st, eventosC21.TableName())) == fmt.Sprint(beforeMeta),
		tableExistsC21(t, st, schema.StagingTableName))
	record("ROLLBACK-midway fields-unchanged=%v", fmt.Sprint(storedFieldNamesC21(t, st, "eventos")) == fmt.Sprint(beforeFields))
	// The transaction was rolled back, so the connection must be usable again —
	// on PostgreSQL an aborted transaction that was NOT rolled back would make
	// every later statement fail with 25P02.
	record("ROLLBACK-midway connection usable=%v", tableExistsC21(t, st, "users"))
	execC21(t, st, `DROP TABLE "zz_refs"`)

	// --- 10. ROLLBACK AT THE LAST STEP: the metadata write ------------------
	//
	// The complement: the failure lands at step 8, AFTER both copies and the
	// registry update. The rollback has to undo work that already reached the
	// registry rows and the physical table.
	execC21(t, st, `DROP TABLE "__compat_schema"`)
	ids = fieldIDsC21(t, st, "eventos")
	lastStep := []store.FieldEdit{
		{ID: ids["resumen"], Name: "resumen", Type: schema.FieldText},
		{ID: ids["asistentes"], Name: "asistentes", Type: schema.FieldInteger},
		{ID: ids["encabezado"], Name: "encabezado", Type: schema.FieldText},
		{Name: "nuevo", Type: schema.FieldText},
	}
	_, metaErr := store.EditContentType(ctx, st, "eventos", lastStep, nil)
	record("ROLLBACK-metadata err=%s", errWord(metaErr))
	record("ROLLBACK-metadata fields=%v identities-intact=%v",
		storedFieldNamesC21(t, st, "eventos"),
		fmt.Sprint(fieldIDsC21(t, st, "eventos")) == fmt.Sprint(ids))
	record("ROLLBACK-metadata table-columns=%v", tableColumnsC21(t, st, eventosC21.TableName()))
	record("ROLLBACK-metadata row-intact resumen=%q", textColumnC21(t, st, eventosC21.TableName(), "resumen", rowID))
	record("ROLLBACK-metadata staging-left=%v", tableExistsC21(t, st, schema.StagingTableName))
	record("ROLLBACK-metadata connection usable=%v", tableExistsC21(t, st, "users"))

	// RED-TEAM, answered explicitly rather than in prose: every catalog probe of
	// this package runs on the POOL, outside any open transaction, so it can
	// never observe (or fail to observe) a table created in a transaction that
	// has not committed. On SQLite it MUST be that way — compat pins the pool to
	// one connection, so probing while a transaction is open would deadlock — and
	// on PostgreSQL it is the same code. What the probe does see, immediately
	// after a commit, is recorded above ("after create table-exists=true").
	record("probes run outside transactions=%v", true)

	return lines
}

// --- helpers -----------------------------------------------------------------

func errorsIsDuplicateC21(err error) bool {
	return err != nil && strings.Contains(err.Error(), store.ErrDuplicateContentType.Error())
}

func execC21(t *testing.T, st *compat.Store, statement string) {
	t.Helper()
	if _, err := st.DB.ExecContext(context.Background(), statement); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

func execArgsC21(t *testing.T, st *compat.Store, statement string, args ...any) {
	t.Helper()
	if _, err := st.DB.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

func countRowsC21(t *testing.T, st *compat.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM "`+table+`"`).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func tableExistsC21(t *testing.T, st *compat.Store, name string) bool {
	t.Helper()
	present, err := st.TableExists(context.Background(), name)
	if err != nil {
		t.Fatalf("TableExists(%q): %v", name, err)
	}
	return present
}

// seedAuthorC21 inserts one user (dynamic tables have a NOT NULL author_id with
// an FK to users) and returns its id.
func seedAuthorC21(t *testing.T, st *compat.Store) string {
	t.Helper()
	engine := st.Target.Engine
	id := fmt.Sprintf("%08x-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffff, time.Now().UnixNano()&0xffffffffffff)
	var roleID string
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT "id" FROM "roles" WHERE "name" = `+compat.Placeholder(engine, 1), "administrator").Scan(&roleID); err != nil {
		t.Fatalf("role lookup: %v", err)
	}
	stmt := `INSERT INTO "users" ("id", "email", "password_hash", "status", "created_at", "updated_at") VALUES (` +
		compat.Placeholder(engine, 1) + `, ` + compat.Placeholder(engine, 2) + `, ` + compat.Placeholder(engine, 3) + `, ` +
		compat.Placeholder(engine, 4) + `, ` + compat.Placeholder(engine, 5) + `, ` + compat.Placeholder(engine, 6) + `)`
	stamp := "2026-01-01T00:00:00.000000000Z"
	if _, err := st.DB.ExecContext(context.Background(), stmt,
		id, "autor@example.com", "x", "active", stamp, stamp); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func insertEventoC21(t *testing.T, st *compat.Store, author, titulo, lugar string, asistentes int, gratis bool) string {
	t.Helper()
	engine := st.Target.Engine
	id := "11111111-2222-4333-8444-555555555555"
	// `gratis` is BOOLEAN, which is INTEGER on SQLite and BOOLEAN on PostgreSQL:
	// the value is bound as a Go bool, which each driver renders natively.
	stmt := `INSERT INTO "` + eventosC21.TableName() + `" ("id", "author_id", "created_at", "updated_at", "titulo", "lugar", "asistentes", "gratis") VALUES (` +
		compat.Placeholder(engine, 1) + `, ` + compat.Placeholder(engine, 2) + `, ` + compat.Placeholder(engine, 3) + `, ` +
		compat.Placeholder(engine, 4) + `, ` + compat.Placeholder(engine, 5) + `, ` + compat.Placeholder(engine, 6) + `, ` +
		compat.Placeholder(engine, 7) + `, ` + compat.Placeholder(engine, 8) + `)`
	stamp := "2026-01-02T00:00:00.000000000Z"
	if _, err := st.DB.ExecContext(context.Background(), stmt,
		id, author, stamp, stamp, titulo, lugar, asistentes, gratis); err != nil {
		t.Fatalf("insert evento: %v", err)
	}
	return id
}

func storedFieldNamesC21(t *testing.T, st *compat.Store, typeName string) []string {
	t.Helper()
	_, fields, err := store.LoadContentTypeFields(context.Background(), st, typeName)
	if err != nil {
		t.Fatalf("LoadContentTypeFields(%q): %v", typeName, err)
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Name+":"+string(f.Type))
	}
	return out
}

func fieldIDsC21(t *testing.T, st *compat.Store, typeName string) map[string]string {
	t.Helper()
	_, fields, err := store.LoadContentTypeFields(context.Background(), st, typeName)
	if err != nil {
		t.Fatalf("LoadContentTypeFields(%q): %v", typeName, err)
	}
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		out[f.Name] = f.ID
	}
	return out
}

func textColumnC21(t *testing.T, st *compat.Store, table, column, id string) string {
	t.Helper()
	var v *string
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT "`+column+`" FROM "`+table+`" WHERE "id" = `+compat.Placeholder(st.Target.Engine, 1), id).Scan(&v); err != nil {
		t.Fatalf("read %s.%s: %v", table, column, err)
	}
	if v == nil {
		return "<null>"
	}
	return *v
}

func intColumnC21(t *testing.T, st *compat.Store, table, column, id string) int64 {
	t.Helper()
	var v *int64
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT "`+column+`" FROM "`+table+`" WHERE "id" = `+compat.Placeholder(st.Target.Engine, 1), id).Scan(&v); err != nil {
		t.Fatalf("read %s.%s: %v", table, column, err)
	}
	if v == nil {
		return -1
	}
	return *v
}

func isNullColumnC21(t *testing.T, st *compat.Store, table, column, id string) bool {
	t.Helper()
	var isNull bool
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT "`+column+`" IS NULL FROM "`+table+`" WHERE "id" = `+compat.Placeholder(st.Target.Engine, 1), id).Scan(&isNull); err != nil {
		t.Fatalf("null check %s.%s: %v", table, column, err)
	}
	return isNull
}

// tableColumnsC21 returns the PHYSICAL column names of a table, in physical
// order, asking each engine's own catalog. It is the oracle for "the rebuild
// left the table exactly as it was".
func tableColumnsC21(t *testing.T, st *compat.Store, table string) []string {
	t.Helper()
	var query string
	var args []any
	switch st.Target.Engine {
	case compat.SQLite:
		query = `SELECT name FROM pragma_table_info(?) ORDER BY cid`
		args = []any{table}
	default:
		query = `SELECT a.attname FROM pg_attribute a
		           JOIN pg_class c ON c.oid = a.attrelid
		           JOIN pg_namespace n ON n.oid = c.relnamespace
		          WHERE n.nspname = current_schema() AND c.relname = $1 AND a.attnum > 0 AND NOT a.attisdropped
		          ORDER BY a.attnum`
		args = []any{table}
	}
	rows, err := st.DB.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out = append(out, name)
	}
	return out
}

// metadataSchemaC21 reads back the canonical schema compat stores in
// __compat_schema — the row InspectSchema prefers over the live catalog, and the
// single most damaging thing that can be left lying.
func metadataSchemaC21(t *testing.T, st *compat.Store) compat.Schema {
	t.Helper()
	var payload string
	err := st.DB.QueryRowContext(context.Background(),
		`SELECT "value" FROM "__compat_schema" WHERE "key" = `+compat.Placeholder(st.Target.Engine, 1),
		"canonical_schema").Scan(&payload)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var got compat.Schema
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	return got
}

func metadataHasTableC21(t *testing.T, st *compat.Store, name string) bool {
	t.Helper()
	for _, table := range metadataSchemaC21(t, st).Tables {
		if table.Name == name {
			return true
		}
	}
	return false
}

func metadataColumnsC21(t *testing.T, st *compat.Store, name string) []string {
	t.Helper()
	for _, table := range metadataSchemaC21(t, st).Tables {
		if table.Name != name {
			continue
		}
		out := make([]string, 0, len(table.Columns))
		for _, c := range table.Columns {
			out = append(out, c.Name)
		}
		return out
	}
	return nil
}

func dynamicTableNamesC21(s compat.Schema) []string {
	var out []string
	for _, table := range s.Tables {
		if strings.HasPrefix(table.Name, schema.DynamicTablePrefix) {
			out = append(out, table.Name)
		}
	}
	return out
}

func firstLineC21(data []byte) string {
	text := string(data)
	if len(text) > 24 {
		return text[:24]
	}
	return text
}

// digestC21 is a tiny order-sensitive checksum of the dump. It exists so the
// transcript carries a single value that changes if ANY byte of the canonical
// JSON differs between the engines, without pasting the whole document twice.
func digestC21(data []byte) string {
	var h uint64 = 1469598103934665603
	for _, b := range data {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}

// rawTypeNameOrderC21 asks the database for the type names ordered by the SQL
// `ORDER BY name` that LoadContentTypeDefinitions keeps as its stable base. It
// exists only to be logged: it is the value the two engines are EXPECTED to
// disagree about, and seeing them disagree is what proves the Go-side ordering
// is doing work rather than being a no-op over a harmless fixture.
func rawTypeNameOrderC21(t *testing.T, st *compat.Store) []string {
	t.Helper()
	rows, err := st.DB.QueryContext(context.Background(),
		`SELECT "name" FROM "`+schema.ContentTypesTable+`" ORDER BY "name"`)
	if err != nil {
		t.Fatalf("raw order: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	return out
}
