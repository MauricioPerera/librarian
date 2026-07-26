//go:build dualengine

package store_test

// CONTRACT-26 T4 — the deletion battery on BOTH engines.
//
// It follows the CONTRACT-21 pattern exactly and reuses its fixtures
// (openSQLiteForStore / openPostgresForStore) and helpers. It is excluded from
// the default suite by the same build tag, because it needs a live PostgreSQL 17
// with pgvector:
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineDeleteContentType -count=1 -v ./internal/store
//
// Without COMPAT_POSTGRES_DSN it SKIPS rather than passing vacuously.
//
// WHY IT HAS TO RUN ON BOTH. This operation mixes DDL compiled by compat, a
// DELETE of application rows, a COUNT and the metadata rewrite inside ONE
// transaction, and its failure mode is a database left half-changed. The two
// engines do NOT behave the same after an error inside a transaction —
// PostgreSQL poisons the whole transaction (25P02), SQLite does not — so
// "the rollback undoes the DROP TABLE too" is a claim that must be MEASURED on
// both. Driver error TEXT is never recorded (it legitimately differs), only
// whether an error occurred and, above all, THE STATE OF THE DATABASE AFTER IT.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// eventosC26 is the type these scenarios create, fill, destroy and re-create.
var eventosC26 = schema.ContentTypeDefinition{
	Name: "eventos",
	Fields: []schema.FieldDefinition{
		{Name: "titulo", Type: schema.FieldText},
		{Name: "lugar", Type: schema.FieldText},
		{Name: "asistentes", Type: schema.FieldInteger},
	},
}

func TestDualEngineDeleteContentType(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteTranscript := runDeleteScenario(t, "sqlite", openSQLiteForStore, "")
	pgTranscript := runDeleteScenario(t, "postgres", openPostgresForStore, dsn)

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

func runDeleteScenario(t *testing.T, engineLabel string, open engineOpener, dsn string) []string {
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
	refusalWord := func(err error) string {
		var refusal store.DeleteConfirmationError
		if err != nil && asDeleteRefusalC26(err, &refusal) {
			return fmt.Sprintf("refused rows=%d", refusal.Rows)
		}
		return errWord(err)
	}

	if err := store.EnsureSchema(ctx, st); err != nil {
		t.Fatalf("[%s] ensure: %v", engineLabel, err)
	}
	if err := store.SeedCatalogs(ctx, st); err != nil {
		t.Fatalf("[%s] seed: %v", engineLabel, err)
	}
	author := seedAuthorC21(t, st)
	engine := st.Target.Engine

	insertRow := func(titulo string, asistentes int) string {
		id := mustUUIDC26(t)
		stmt := `INSERT INTO "cpt_eventos" ("id", "author_id", "titulo", "lugar", "asistentes") VALUES (` +
			compat.Placeholder(engine, 1) + `, ` + compat.Placeholder(engine, 2) + `, ` +
			compat.Placeholder(engine, 3) + `, ` + compat.Placeholder(engine, 4) + `, ` +
			compat.Placeholder(engine, 5) + `)`
		if _, err := st.DB.ExecContext(ctx, stmt, id, author, titulo, "Montevideo", asistentes); err != nil {
			t.Fatalf("[%s] insert row: %v", engineLabel, err)
		}
		return id
	}

	// --- 1. A type with real rows -------------------------------------------
	record("create eventos err=%s", errWord(store.CreateContentType(ctx, st, eventosC26)))
	referenced := insertRow("Feria", 120)
	insertRow("Charla Go", 41)
	insertRow("Taller", 7)
	record("before: table=%v rows=%d types=%d fields=%d metadata-has-table=%v",
		tableExistsC21(t, st, "cpt_eventos"), countRowsC21(t, st, "cpt_eventos"),
		countRowsC21(t, st, schema.ContentTypesTable), countRowsC21(t, st, schema.ContentTypeFieldsTable),
		metadataHasTableC21(t, st, "cpt_eventos"))

	plan, err := store.PlanContentTypeDeletion(ctx, st, "eventos")
	if err != nil {
		t.Fatalf("[%s] plan: %v", engineLabel, err)
	}
	record("plan rows=%d table=%s table-missing=%v fields=%v", plan.Rows, plan.TableName, plan.TableMissing, plan.Fields)

	// --- 2. THE GUARD --------------------------------------------------------
	_, err = store.DeleteContentType(ctx, st, "eventos", store.DeleteConfirmation{})
	record("delete unconfirmed        %s", refusalWord(err))
	_, err = store.DeleteContentType(ctx, st, "eventos", store.DeleteConfirmation{Name: "eventos"})
	record("delete name only          %s", refusalWord(err))
	_, err = store.DeleteContentType(ctx, st, "eventos", store.DeleteConfirmation{Name: "otro", Rows: 3, RowsStated: true})
	record("delete wrong name         %s", refusalWord(err))
	_, err = store.DeleteContentType(ctx, st, "eventos", store.DeleteConfirmation{Name: "eventos", Rows: 0, RowsStated: true})
	record("delete wrong count        %s", refusalWord(err))
	record("after 4 refusals: table=%v rows=%d types=%d fields=%d",
		tableExistsC21(t, st, "cpt_eventos"), countRowsC21(t, st, "cpt_eventos"),
		countRowsC21(t, st, schema.ContentTypesTable), countRowsC21(t, st, schema.ContentTypeFieldsTable))

	// --- 3. ATOMICITY: forced failure at the DROP ----------------------------
	//
	// Another table holds a foreign key into cpt_eventos, so the engine refuses
	// the DROP — AFTER the registry rows were already deleted in this
	// transaction. Everything must come back on BOTH engines, by different
	// internal routes (PostgreSQL poisons the transaction, SQLite does not).
	//
	// The referencing ROW is not decoration: PostgreSQL refuses to drop a table
	// another table depends on whether or not there are rows, while SQLite only
	// refuses when the implicit delete would actually violate the constraint. A
	// real referencing row is what makes the two engines fail for the same
	// reason, which is the premise of comparing the transcripts at all.
	execC21(t, st, `CREATE TABLE "zz_refs" ("id" TEXT PRIMARY KEY, "evento_id" TEXT REFERENCES "cpt_eventos"("id"))`)
	execArgsC21(t, st, `INSERT INTO "zz_refs" ("id", "evento_id") VALUES (`+
		compat.Placeholder(engine, 1)+`, `+compat.Placeholder(engine, 2)+`)`, mustUUIDC26(t), referenced)
	_, err = store.DeleteContentType(ctx, st, "eventos",
		store.DeleteConfirmation{Name: "eventos", Rows: plan.Rows, RowsStated: true})
	record("delete with a dependent FK err=%s", errWord(err))
	record("ATOMIC: table=%v rows=%d types=%d fields=%d metadata-has-table=%v",
		tableExistsC21(t, st, "cpt_eventos"), countRowsC21(t, st, "cpt_eventos"),
		countRowsC21(t, st, schema.ContentTypesTable), countRowsC21(t, st, schema.ContentTypeFieldsTable),
		metadataHasTableC21(t, st, "cpt_eventos"))
	execC21(t, st, `DROP TABLE "zz_refs"`)

	// --- 4. The exact confirmation -------------------------------------------
	deleted, err := store.DeleteContentType(ctx, st, "eventos",
		store.DeleteConfirmation{Name: "eventos", Rows: plan.Rows, RowsStated: true})
	record("delete confirmed err=%s rows-deleted=%d table-was-missing=%v", errWord(err), deleted.Rows, deleted.TableMissing)
	record("after: table=%v types=%d fields=%d metadata-has-table=%v",
		tableExistsC21(t, st, "cpt_eventos"),
		countRowsC21(t, st, schema.ContentTypesTable), countRowsC21(t, st, schema.ContentTypeFieldsTable),
		metadataHasTableC21(t, st, "cpt_eventos"))
	canonical, err := store.CanonicalSchema(ctx, st)
	if err != nil {
		t.Fatalf("[%s] canonical: %v", engineLabel, err)
	}
	record("canonical schema has the deleted table=%v dynamic=%v", hasTableC26(canonical, "cpt_eventos"), dynamicTableNamesC21(canonical))

	// --- 5. Deleting twice ---------------------------------------------------
	_, err = store.DeleteContentType(ctx, st, "eventos", store.DeleteConfirmation{Name: "eventos", RowsStated: true})
	record("delete again not-found=%v", err != nil && strings.Contains(err.Error(), store.ErrContentTypeNotFound.Error()))

	// --- 6. Restart: nothing is re-created -----------------------------------
	record("ensureSchema after delete #1 err=%s recreated=%v", errWord(store.EnsureSchema(ctx, st)), tableExistsC21(t, st, "cpt_eventos"))
	record("ensureSchema after delete #2 err=%s recreated=%v", errWord(store.EnsureSchema(ctx, st)), tableExistsC21(t, st, "cpt_eventos"))

	// --- 7. THE NAME IS REUSABLE, and the new table is EMPTY -----------------
	again := schema.ContentTypeDefinition{
		Name: "eventos",
		Fields: []schema.FieldDefinition{
			{Name: "encabezado", Type: schema.FieldText},
			{Name: "cupos", Type: schema.FieldInteger},
		},
	}
	record("re-create with the same name err=%s", errWord(store.CreateContentType(ctx, st, again)))
	record("re-created: table=%v rows=%d columns=%v",
		tableExistsC21(t, st, "cpt_eventos"), countRowsC21(t, st, "cpt_eventos"),
		metadataColumnsC21(t, st, "cpt_eventos"))

	// --- 8. THE ORPHAN DEFINITION (the "table absent" decision) --------------
	execC21(t, st, `DROP TABLE "cpt_eventos"`)
	orphan, err := store.PlanContentTypeDeletion(ctx, st, "eventos")
	record("orphan plan err=%s table-missing=%v rows=%d", errWord(err), orphan.TableMissing, orphan.Rows)
	_, err = store.DeleteContentType(ctx, st, "eventos", store.DeleteConfirmation{Name: "eventos"})
	record("orphan delete unconfirmed %s", refusalWord(err))
	cleaned, err := store.DeleteContentType(ctx, st, "eventos",
		store.DeleteConfirmation{Name: "eventos", Rows: 0, RowsStated: true})
	record("orphan delete confirmed err=%s table-was-missing=%v types=%d fields=%d",
		errWord(err), cleaned.TableMissing,
		countRowsC21(t, st, schema.ContentTypesTable), countRowsC21(t, st, schema.ContentTypeFieldsTable))
	record("orphan cleanup: metadata-has-table=%v", metadataHasTableC21(t, st, "cpt_eventos"))

	return lines
}

// asDeleteRefusalC26 is errors.As without importing errors into the record
// helpers' closure signature.
func asDeleteRefusalC26(err error, target *store.DeleteConfirmationError) bool {
	refusal, ok := err.(store.DeleteConfirmationError)
	if ok {
		*target = refusal
	}
	return ok
}

func hasTableC26(s compat.Schema, name string) bool {
	for _, table := range s.Tables {
		if table.Name == name {
			return true
		}
	}
	return false
}

func mustUUIDC26(t *testing.T) string {
	t.Helper()
	id, err := dual.NewUUID()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id
}
