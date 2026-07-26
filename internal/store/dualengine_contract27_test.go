//go:build dualengine

package store_test

// CONTRACT-27 T5 — the relation battery on BOTH engines.
//
// It follows the CONTRACT-21/26 pattern exactly and reuses their fixtures
// (openSQLiteForStore / openPostgresForStore). Excluded from the default suite
// by the same build tag, because it needs a live PostgreSQL 17 with pgvector:
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineRelations -count=1 -v ./internal/store
//
// Without COMPAT_POSTGRES_DSN it SKIPS rather than passing vacuously.
//
// WHY IT HAS TO RUN ON BOTH, and it is not a formality for this contract. A
// foreign key is the one construct whose ENFORCEMENT is off by default on one of
// the two engines (SQLite needs `PRAGMA foreign_keys=ON`; compat pins it on the
// connection, which is a claim this battery MEASURES rather than trusts), whose
// referential action compiles to different catalog text, and whose violation is
// reported with a completely different error shape. The transcript records only
// engine-neutral OBSERVATIONS — never driver message text — so any divergence is
// a divergence of BEHAVIOUR.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// The pair of types the whole battery uses. `alpha` references `zeta` so the
// DEPENDENCY order and the ALPHABETICAL order disagree — the red-team item that
// breaks `--dump-schema` in silence.
var (
	zetaC27 = schema.ContentTypeDefinition{
		Name:   "zeta",
		Fields: []schema.FieldDefinition{{Name: "nombre", Type: schema.FieldText}},
	}
	alphaC27 = schema.ContentTypeDefinition{
		Name:       "alpha",
		Fields:     []schema.FieldDefinition{{Name: "titulo", Type: schema.FieldText}},
		References: []schema.ReferenceDefinition{{Name: "destino", Target: "zeta"}},
	}
)

func TestDualEngineRelations(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteTranscript := runRelationScenario(t, "sqlite", openSQLiteForStore, "")
	pgTranscript := runRelationScenario(t, "postgres", openPostgresForStore, dsn)

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

// TestDualEngineDumpedSchemaAppliesOnPostgres is THE red-team item, answered end
// to end rather than by inspecting an order: a SQLite instance that has a
// relation is dumped exactly the way `librarian --dump-schema` dumps it, and the
// dump is APPLIED to a brand-new, empty PostgreSQL schema.
//
// If the tables were emitted in name order, `cpt_alpha` would be created before
// `cpt_zeta` and PostgreSQL would refuse it — `relation "cpt_zeta" does not
// exist` — which is exactly how an export breaks in silence: the dump compiles,
// validates and looks complete, and only the destination finds out.
func TestDualEngineDumpedSchemaAppliesOnPostgres(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}
	ctx := context.Background()

	// --- the SOURCE: a real SQLite instance with a relation ------------------
	source, closeSource := openSQLiteForStore(t, "")
	defer closeSource()
	if err := store.EnsureSchema(ctx, source); err != nil {
		t.Fatalf("source ensure: %v", err)
	}
	if err := store.CreateContentType(ctx, source, zetaC27); err != nil {
		t.Fatalf("create zeta: %v", err)
	}
	if err := store.CreateContentType(ctx, source, alphaC27); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	defs, err := store.LoadDefinitions(ctx, source)
	if err != nil {
		t.Fatalf("load definitions: %v", err)
	}
	payload, err := schema.JSONWith(defs)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	var dumped compat.Schema
	if err := json.Unmarshal(payload, &dumped); err != nil {
		t.Fatalf("decode the dump: %v", err)
	}
	t.Logf("dump: %d tables, cpt_zeta at %d, cpt_alpha at %d",
		len(dumped.Tables), tableIndexC27(dumped, "cpt_zeta"), tableIndexC27(dumped, "cpt_alpha"))

	// --- the DESTINATION: an empty PostgreSQL schema -------------------------
	destination, closeDestination := openPostgresForStore(t, dsn)
	defer closeDestination()
	// openPostgresForStore returns a connection to an EMPTY schema; apply the
	// dump exactly as `compat copy` would, in the order the dump declares.
	if err := destination.ApplySchema(ctx, dumped); err != nil {
		t.Fatalf("applying the dumped schema to PostgreSQL failed: %v", err)
	}
	for _, table := range []string{"cpt_zeta", "cpt_alpha", schema.ContentTypeReferencesTable} {
		present, err := destination.TableExists(ctx, table)
		if err != nil {
			t.Fatalf("probe %q: %v", table, err)
		}
		if !present {
			t.Fatalf("the applied schema is missing the table %q", table)
		}
	}
	// And the foreign key travelled with it.
	inspection, err := destination.InspectSchema(ctx)
	if err != nil {
		t.Fatalf("inspect the destination: %v", err)
	}
	got := describeFKC27(inspection.Schema, "cpt_alpha", "destino")
	if !strings.Contains(got, "cpt_zeta") || !strings.Contains(got, "restrict") {
		t.Fatalf("the exported foreign key did not survive: %s", got)
	}
	t.Logf("OK: the dumped schema applied cleanly on PostgreSQL 17 and cpt_alpha.destino → %s", got)
}

func runRelationScenario(t *testing.T, engineLabel string, open engineOpener, dsn string) []string {
	t.Helper()
	ctx := context.Background()
	st, cleanup := open(t, dsn)
	defer cleanup()

	var lines []string
	record := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	// errWord records ONLY whether an error occurred. Driver message text
	// legitimately differs between the engines and is never compared.
	errWord := func(err error) string {
		if err == nil {
			return "none"
		}
		return "yes"
	}

	if err := store.EnsureSchema(ctx, st); err != nil {
		t.Fatalf("%s: ensure schema: %v", engineLabel, err)
	}
	if err := store.SeedCatalogs(ctx, st); err != nil {
		t.Fatalf("%s: seed: %v", engineLabel, err)
	}

	// 1. The new registry table is created by the ordinary startup path.
	present, err := st.TableExists(ctx, schema.ContentTypeReferencesTable)
	if err != nil {
		t.Fatalf("%s: probe references table: %v", engineLabel, err)
	}
	record("references registry table present=%v", present)

	// 2. The referrer BEFORE its target: refused by name, nothing created.
	err = store.CreateContentType(ctx, st, alphaC27)
	record("create alpha before zeta: err=%s unknown_target=%v", errWord(err), errors.Is(err, store.ErrUnknownReferenceTarget))
	present, _ = st.TableExists(ctx, "cpt_alpha")
	record("cpt_alpha exists after the refusal=%v", present)

	// 3. In the right order.
	err = store.CreateContentType(ctx, st, zetaC27)
	record("create zeta: err=%s", errWord(err))
	err = store.CreateContentType(ctx, st, alphaC27)
	record("create alpha: err=%s", errWord(err))

	// 4. THE FOREIGN KEY IS REAL, read from each engine's OWN catalog through
	//    compat's inspection (never from the schema we composed).
	inspection, err := st.InspectSchema(ctx)
	if err != nil {
		t.Fatalf("%s: inspect: %v", engineLabel, err)
	}
	record("catalog: cpt_alpha.destino → %s", describeFKC27(inspection.Schema, "cpt_alpha", "destino"))

	// 5. Rows: one target row, one referrer row pointing at it, one without.
	userID := seedUserC27(t, engineLabel, st)
	zetaID := "11111111-1111-4111-8111-111111111111"
	execC27(t, engineLabel, st, `INSERT INTO "cpt_zeta" ("id", "author_id", "nombre") VALUES (`+binds(st, 3)+`)`,
		zetaID, userID, "destino")
	err = execErrC27(st, `INSERT INTO "cpt_alpha" ("id", "author_id", "titulo", "destino") VALUES (`+binds(st, 4)+`)`,
		"22222222-2222-4222-8222-222222222222", userID, "con relacion", zetaID)
	record("insert a row WITH the relation: err=%s", errWord(err))
	err = execErrC27(st, `INSERT INTO "cpt_alpha" ("id", "author_id", "titulo", "destino") VALUES (`+binds(st, 4)+`)`,
		"33333333-3333-4333-8333-333333333333", userID, "sin relacion", nil)
	record("insert a row WITHOUT the relation (NULL): err=%s", errWord(err))

	// 6. THE CONSTRAINT IS ENFORCED — the same answer on both engines, which on
	//    SQLite depends on the foreign_keys pragma compat pins on the connection.
	err = execErrC27(st, `INSERT INTO "cpt_alpha" ("id", "author_id", "titulo", "destino") VALUES (`+binds(st, 4)+`)`,
		"44444444-4444-4444-8444-444444444444", userID, "fantasma",
		"99999999-9999-4999-8999-999999999999")
	record("insert a row referencing an id that does not exist: err=%s", errWord(err))

	// 7. Deleting the REFERENCED row is refused (RESTRICT, never CASCADE), and
	//    the row survives.
	err = execErrC27(st, `DELETE FROM "cpt_zeta" WHERE "id" = `+binds(st, 1), zetaID)
	record("delete the referenced row: err=%s", errWord(err))
	record("the referenced row survives=%v", countC27(t, engineLabel, st, `SELECT count(*) FROM "cpt_zeta" WHERE "id" = `+binds(st, 1), zetaID) == 1)
	record("the referring rows survive=%d", countC27(t, engineLabel, st, `SELECT count(*) FROM "cpt_alpha"`))

	// 8. THE T3 GUARD, on both operations, BEFORE anything is touched.
	_, storedFields, err := store.LoadContentTypeFields(ctx, st, "zeta")
	if err != nil {
		t.Fatalf("%s: load zeta fields: %v", engineLabel, err)
	}
	edits := []store.FieldEdit{{ID: storedFields[0].ID, Name: "nombre_largo", Type: schema.FieldText}}
	_, err = store.EditContentType(ctx, st, "zeta", edits, nil)
	var referenced store.ReferencedTypeError
	record("edit the referenced type: err=%s guarded=%v referrers=%s",
		errWord(err), errors.As(err, &referenced), describeReferrersC27(err))
	present, _ = st.TableExists(ctx, "cpt_zeta")
	record("cpt_zeta survives the refused edit=%v", present)

	_, err = store.DeleteContentType(ctx, st, "zeta",
		store.DeleteConfirmation{Name: "zeta", Rows: 1, RowsStated: true})
	record("delete the referenced type: err=%s guarded=%v referrers=%s",
		errWord(err), errors.As(err, &referenced), describeReferrersC27(err))
	present, _ = st.TableExists(ctx, "cpt_zeta")
	record("cpt_zeta survives the refused deletion=%v", present)

	// 9. Free the target and confirm BOTH operations work again.
	_, err = store.DeleteContentType(ctx, st, "alpha",
		store.DeleteConfirmation{Name: "alpha", Rows: 2, RowsStated: true})
	record("delete the referrer: err=%s", errWord(err))
	_, err = store.EditContentType(ctx, st, "zeta", edits, nil)
	record("edit the target once freed: err=%s", errWord(err))
	_, err = store.DeleteContentType(ctx, st, "zeta",
		store.DeleteConfirmation{Name: "zeta", Rows: 1, RowsStated: true})
	record("delete the target once freed: err=%s", errWord(err))
	present, _ = st.TableExists(ctx, "cpt_zeta")
	record("cpt_zeta exists at the end=%v", present)

	// 10. `--dump-schema` ordering: the target must precede its referrer, and the
	//     dump must COMPILE for both engines in that order. Re-created for the
	//     purpose, since step 9 removed both types.
	if err := store.CreateContentType(ctx, st, zetaC27); err != nil {
		t.Fatalf("%s: re-create zeta: %v", engineLabel, err)
	}
	if err := store.CreateContentType(ctx, st, alphaC27); err != nil {
		t.Fatalf("%s: re-create alpha: %v", engineLabel, err)
	}
	defs, err := store.LoadDefinitions(ctx, st)
	if err != nil {
		t.Fatalf("%s: load definitions: %v", engineLabel, err)
	}
	full, err := schema.BuildWith(defs)
	if err != nil {
		t.Fatalf("%s: compose: %v", engineLabel, err)
	}
	record("dump order: cpt_zeta at %d, cpt_alpha at %d, target first=%v",
		tableIndexC27(full, "cpt_zeta"), tableIndexC27(full, "cpt_alpha"),
		tableIndexC27(full, "cpt_zeta") < tableIndexC27(full, "cpt_alpha"))

	// 11. A restart on this very database keeps everything.
	if err := store.EnsureSchema(ctx, st); err != nil {
		t.Fatalf("%s: restart ensure: %v", engineLabel, err)
	}
	defs, err = store.LoadDefinitions(ctx, st)
	if err != nil {
		t.Fatalf("%s: reload definitions: %v", engineLabel, err)
	}
	record("after a restart: %d definitions, alpha references=%s", len(defs), describeReferencesC27(defs, "alpha"))
	inspection, err = st.InspectSchema(ctx)
	if err != nil {
		t.Fatalf("%s: re-inspect: %v", engineLabel, err)
	}
	record("after a restart, catalog: cpt_alpha.destino → %s", describeFKC27(inspection.Schema, "cpt_alpha", "destino"))

	return lines
}

// --- helpers -----------------------------------------------------------------

// binds renders n placeholders for the store's engine.
func binds(st *compat.Store, n int) string {
	parts := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		parts = append(parts, compat.Placeholder(st.Target.Engine, i))
	}
	return strings.Join(parts, ", ")
}

func execC27(t *testing.T, engineLabel string, st *compat.Store, query string, args ...any) {
	t.Helper()
	if _, err := st.DB.Exec(query, args...); err != nil {
		t.Fatalf("%s: %s: %v", engineLabel, query, err)
	}
}

func execErrC27(st *compat.Store, query string, args ...any) error {
	_, err := st.DB.Exec(query, args...)
	return err
}

func countC27(t *testing.T, engineLabel string, st *compat.Store, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := st.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %s: %v", engineLabel, query, err)
	}
	return n
}

// seedUserC27 inserts one user so content rows satisfy author_id's FK.
func seedUserC27(t *testing.T, engineLabel string, st *compat.Store) string {
	t.Helper()
	id := "55555555-5555-4555-8555-555555555555"
	execC27(t, engineLabel, st,
		`INSERT INTO "users" ("id", "email", "password_hash", "status") VALUES (`+binds(st, 4)+`)`,
		id, "c27@example.test", "x", "active")
	return id
}

// describeFKC27 renders the foreign key of a column as an engine-neutral string:
// target table, target column and referential action. It is what makes "the FK
// is real, and it is RESTRICT" an observation of the CATALOG on both engines.
func describeFKC27(s compat.Schema, table, column string) string {
	for _, tbl := range s.Tables {
		if tbl.Name != table {
			continue
		}
		for _, c := range tbl.Constraints {
			if c.Kind != compat.ForeignKey || c.References == nil || len(c.Columns) != 1 || c.Columns[0] != column {
				continue
			}
			action := string(c.References.OnDelete)
			if action == "" {
				action = "(none)"
			}
			return fmt.Sprintf("%s(%s) on_delete=%s", c.References.Table, strings.Join(c.References.Columns, ","), action)
		}
		return "NO FOREIGN KEY"
	}
	return "NO SUCH TABLE"
}

// describeReferrersC27 renders the guard's referrers, or "-" when the error is
// not the guard's.
func describeReferrersC27(err error) string {
	var referenced store.ReferencedTypeError
	if !errors.As(err, &referenced) {
		return "-"
	}
	parts := make([]string, 0, len(referenced.Referrers))
	for _, r := range referenced.Referrers {
		parts = append(parts, r.TypeName+"."+r.ReferenceName)
	}
	return referenced.Operation + ":" + strings.Join(parts, "+")
}

func describeReferencesC27(defs []schema.ContentTypeDefinition, name string) string {
	for _, d := range defs {
		if d.Name != name {
			continue
		}
		parts := make([]string, 0, len(d.References))
		for _, r := range d.References {
			parts = append(parts, r.Name+"→"+r.Target)
		}
		return strings.Join(parts, ",")
	}
	return "(absent)"
}

func tableIndexC27(s compat.Schema, name string) int {
	for i, tbl := range s.Tables {
		if tbl.Name == name {
			return i
		}
	}
	return -1
}
