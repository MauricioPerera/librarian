package store_test

// CONTRACT-26 T1/T2/T4 acceptance tests — DELETING a dynamic content type.
//
// As in the CONTRACT-18 battery, the evidence is never the return value of the
// function under test. After every deletion (and after every REFUSED deletion)
// the assertions go to SQLite's OWN catalog (sqlite_master / PRAGMA
// table_info), to the registry rows, and to the __compat_schema metadata row —
// the three things that can come apart, and the three the contract demands be
// proven rather than asserted in prose.
//
// The helpers (openWithType, columnsOf, metadataColumnsOf, stagingTables,
// insertEvento, storedFields, tableExists, metadataSchema, hasTable) are the
// ones contenttypes_edit_test.go / contenttypes_test.go already own; nothing is
// duplicated.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// countFieldRows returns how many content_type_fields rows exist in the whole
// database. Deleting a type must take ITS rows and nobody else's, and the
// cascade is the mechanism, so the number is checked rather than assumed.
func countFieldRows(t *testing.T, db *compat.Store) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM ` + schema.ContentTypeFieldsTable).Scan(&n); err != nil {
		t.Fatalf("count field rows: %v", err)
	}
	return n
}

func countTypeRows(t *testing.T, db *compat.Store) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM ` + schema.ContentTypesTable).Scan(&n); err != nil {
		t.Fatalf("count type rows: %v", err)
	}
	return n
}

// exactConfirmation builds the confirmation the plan demands. Tests that want to
// prove the GUARD build a wrong one by hand instead.
func exactConfirmation(plan store.ContentTypeDeletion) store.DeleteConfirmation {
	return store.DeleteConfirmation{Name: plan.TypeName, Rows: plan.Rows, RowsStated: true}
}

// TestDeleteContentTypeRoundTripWithRealData is THE test of the contract: a type
// with real rows disappears completely — definition, fields, table and metadata
// — in one operation, and nothing else in the database moves.
func TestDeleteContentTypeRoundTripWithRealData(t *testing.T) {
	db, author := openWithType(t, "delete-roundtrip.db")
	ctx := context.Background()

	insertEvento(t, db.DB, author, "Feria", "Montevideo", 120, false)
	insertEvento(t, db.DB, author, "Charla Go", "Rosario", 41, true)
	insertEvento(t, db.DB, author, "Taller", "Bahía", 7, true)

	// A SECOND type, which must survive untouched: the deletion is scoped, and
	// the cascade must not reach anybody else's field rows.
	otro := schema.ContentTypeDefinition{
		Name:   "notas",
		Fields: []schema.FieldDefinition{{Name: "cuerpo", Type: schema.FieldText}},
	}
	if err := store.CreateContentType(ctx, db, otro); err != nil {
		t.Fatalf("create second type: %v", err)
	}

	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Rows != 3 {
		t.Fatalf("plan reports %d rows, want 3 — the count is the whole guard", plan.Rows)
	}
	if plan.TableName != "cpt_eventos" || plan.TableMissing {
		t.Fatalf("plan describes the wrong table: %+v", plan)
	}
	if strings.Join(plan.Fields, ",") != "titulo,lugar,asistentes,gratis" {
		t.Fatalf("plan fields %v", plan.Fields)
	}

	deleted, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted.Rows != 3 {
		t.Fatalf("receipt says %d rows destroyed, want 3", deleted.Rows)
	}

	// 1. The physical table is gone from the ENGINE'S OWN catalog.
	if tableExists(t, db.DB, "cpt_eventos") {
		t.Fatal("the real table survived the deletion")
	}
	// 2. The definition and its fields are gone; the OTHER type is intact.
	if _, err := store.FetchContentType(ctx, db, "eventos"); !errors.Is(err, store.ErrContentTypeNotFound) {
		t.Fatalf("the definition survived: %v", err)
	}
	if got := countTypeRows(t, db); got != 1 {
		t.Fatalf("content_types has %d rows, want 1 (only `notas`)", got)
	}
	if got := countFieldRows(t, db); got != 1 {
		t.Fatalf("content_type_fields has %d rows, want 1 — the CASCADE took the wrong rows", got)
	}
	if got := storedFields(t, db, "notas"); strings.Join(got, ",") != "cuerpo:text" {
		t.Fatalf("the surviving type lost fields: %v", got)
	}
	if !tableExists(t, db.DB, "cpt_notas") {
		t.Fatal("the surviving type lost its table")
	}
	// 3. THE METADATA AGREES WITH THE CATALOG — the responsibility inherited
	//    from using the PURE compiler instead of compat.Store.DropTable.
	meta := metadataSchema(t, db.DB)
	if hasTable(meta, "cpt_eventos") {
		t.Fatal("__compat_schema still lists the dropped table — InspectSchema prefers it over the catalog")
	}
	if !hasTable(meta, "cpt_notas") || !hasTable(meta, "articles") {
		t.Fatal("__compat_schema lost tables that were not deleted")
	}
	// 4. And the composed canonical schema — what --dump-schema emits — agrees.
	canonical, err := store.CanonicalSchema(ctx, db)
	if err != nil {
		t.Fatalf("canonical schema: %v", err)
	}
	if hasTable(canonical, "cpt_eventos") {
		t.Fatal("the canonical schema still contains the deleted type")
	}
	if leftovers := stagingTables(t, db.DB); len(leftovers) != 0 {
		t.Fatalf("staging leftovers: %v", leftovers)
	}
	t.Logf("DELETE OK: cpt_eventos and its 3 rows gone from the catalog, registry types=%d fields=%d, metadata and canonical schema agree, `notas` untouched",
		countTypeRows(t, db), countFieldRows(t, db))
}

// TestDeleteContentTypeGuard is T2: the confirmation cannot be sent without
// having been read, and EVERY refusal leaves the database exactly as it was.
//
// Note what is NOT accepted: an empty body, `{"confirm_name":"eventos"}` alone,
// a bare row count alone, the name of ANOTHER type, and a stale/invented count.
// The only accepted request is the one that states both the name and the real
// number of rows.
func TestDeleteContentTypeGuard(t *testing.T) {
	db, author := openWithType(t, "delete-guard.db")
	ctx := context.Background()
	insertEvento(t, db.DB, author, "Feria", "Montevideo", 120, false)
	insertEvento(t, db.DB, author, "Charla Go", "Rosario", 41, true)

	beforeCols := columnsOf(t, db.DB, "cpt_eventos")
	beforeFields := storedFields(t, db, "eventos")
	three := int64(3)

	cases := []struct {
		label   string
		confirm store.DeleteConfirmation
	}{
		{"no confirmation at all (an empty body)", store.DeleteConfirmation{}},
		{"a boolean-style confirmation: the name only", store.DeleteConfirmation{Name: "eventos"}},
		{"the row count only, no name", store.DeleteConfirmation{Rows: 2, RowsStated: true}},
		{"the name of a DIFFERENT type", store.DeleteConfirmation{Name: "notas", Rows: 2, RowsStated: true}},
		{"a stale/invented row count", store.DeleteConfirmation{Name: "eventos", Rows: three, RowsStated: true}},
		{"zero rows on a type that has two", store.DeleteConfirmation{Name: "eventos", Rows: 0, RowsStated: true}},
	}
	for _, c := range cases {
		_, err := store.DeleteContentType(ctx, db, "eventos", c.confirm)
		var refusal store.DeleteConfirmationError
		if !errors.As(err, &refusal) {
			t.Fatalf("%s: got %v, want a DeleteConfirmationError", c.label, err)
		}
		// The refusal is the channel through which the count is learned.
		if refusal.Rows != 2 {
			t.Fatalf("%s: the refusal reports %d rows, want the live 2", c.label, refusal.Rows)
		}
		if !strings.Contains(refusal.Error(), "confirm_rows=2") || !strings.Contains(refusal.Error(), `confirm_name="eventos"`) {
			t.Fatalf("%s: the refusal does not say what to send: %v", c.label, refusal)
		}
		t.Logf("%-45s rejected: %v", c.label, err)
	}

	// NOTHING happened, six refusals later.
	if !tableExists(t, db.DB, "cpt_eventos") {
		t.Fatal("the table was destroyed by a refused deletion")
	}
	if got := columnsOf(t, db.DB, "cpt_eventos"); strings.Join(got, ",") != strings.Join(beforeCols, ",") {
		t.Fatalf("table shape changed: %v", got)
	}
	if got := storedFields(t, db, "eventos"); strings.Join(got, ",") != strings.Join(beforeFields, ",") {
		t.Fatalf("registry changed: %v", got)
	}
	var rows int
	if err := db.DB.QueryRow(`SELECT count(*) FROM "cpt_eventos"`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("rows changed: %d (err=%v)", rows, err)
	}

	// And the exact confirmation, built from the plan, DOES work.
	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan)); err != nil {
		t.Fatalf("the exact confirmation was refused: %v", err)
	}
	t.Logf("GUARD OK: 6 wrong confirmations refused with the live count visible and nothing touched; the exact one (name=%q rows=%d) applied",
		plan.TypeName, plan.Rows)
}

// TestDeleteContentTypeRowsChangedWhileConfirming is the concurrency question of
// the contract in its realistic form: someone LOADS CONTENT into the type
// between the moment the count was shown and the moment it is confirmed.
//
// The count is re-read INSIDE the transaction that is about to destroy the
// table, so a confirmation carrying the older number is refused — the admin is
// never allowed to destroy rows they were not shown.
func TestDeleteContentTypeRowsChangedWhileConfirming(t *testing.T) {
	db, author := openWithType(t, "delete-race.db")
	ctx := context.Background()
	insertEvento(t, db.DB, author, "Feria", "Montevideo", 120, false)

	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	stale := exactConfirmation(plan) // rows = 1

	// Another client writes content of this type.
	insertEvento(t, db.DB, author, "Charla Go", "Rosario", 41, true)

	_, err = store.DeleteContentType(ctx, db, "eventos", stale)
	var refusal store.DeleteConfirmationError
	if !errors.As(err, &refusal) {
		t.Fatalf("a stale count was accepted: %v", err)
	}
	if refusal.Rows != 2 {
		t.Fatalf("the refusal reports %d rows, want the new 2", refusal.Rows)
	}
	if !tableExists(t, db.DB, "cpt_eventos") {
		t.Fatal("the table was destroyed despite the refusal")
	}
	var rows int
	if err := db.DB.QueryRow(`SELECT count(*) FROM "cpt_eventos"`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("rows changed: %d (err=%v)", rows, err)
	}
	t.Logf("STALE COUNT OK: rejected with %v; the freshly counted 2 rows are intact", err)
}

// TestDeleteContentTypeRollbackMidwayFK forces a failure AFTER the registry rows
// were already deleted: another table holds a foreign key into cpt_eventos, so
// the engine refuses the DROP. Everything must come back.
func TestDeleteContentTypeRollbackMidwayFK(t *testing.T) {
	db, author := openWithType(t, "delete-rollbackfk.db")
	ctx := context.Background()

	id := insertEvento(t, db.DB, author, "Charla", "Montevideo", 40, true)
	if _, err := db.DB.Exec(`CREATE TABLE "zz_refs" (id INTEGER PRIMARY KEY, evento_id TEXT REFERENCES "cpt_eventos"("id"))`); err != nil {
		t.Fatalf("create referencing table: %v", err)
	}
	if _, err := db.DB.Exec(`INSERT INTO "zz_refs" (id, evento_id) VALUES (1, ?)`, id); err != nil {
		t.Fatalf("insert referencing row: %v", err)
	}
	beforeCols := columnsOf(t, db.DB, "cpt_eventos")
	beforeFields := storedFields(t, db, "eventos")
	beforeMeta := metadataColumnsOf(t, db.DB, "cpt_eventos")

	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan)); err == nil {
		t.Fatal("the deletion succeeded although DROP TABLE could not have")
	} else {
		t.Logf("deletion failed as expected: %v", err)
	}

	// THE ASSERTIONS: neither half went. No type without its table, no orphan
	// table without its definition.
	if !tableExists(t, db.DB, "cpt_eventos") {
		t.Fatal("the table went despite the failure")
	}
	if got := columnsOf(t, db.DB, "cpt_eventos"); strings.Join(got, ",") != strings.Join(beforeCols, ",") {
		t.Fatalf("table shape changed: %v", got)
	}
	if got := storedFields(t, db, "eventos"); strings.Join(got, ",") != strings.Join(beforeFields, ",") {
		t.Fatalf("the definition or its fields went despite the failure: %v", got)
	}
	var titulo string
	if err := db.DB.QueryRow(`SELECT titulo FROM "cpt_eventos" WHERE id = ?`, id).Scan(&titulo); err != nil || titulo != "Charla" {
		t.Fatalf("row lost or damaged: %q (err=%v)", titulo, err)
	}
	if got := metadataColumnsOf(t, db.DB, "cpt_eventos"); strings.Join(got, ",") != strings.Join(beforeMeta, ",") {
		t.Fatalf("metadata changed: %v, want %v", got, beforeMeta)
	}
	t.Logf("ROLLBACK OK: definition %v, table %v, row and metadata intact", beforeFields, beforeCols)
}

// TestDeleteContentTypeRollbackAtMetadataStep forces the failure at the LAST
// step — after the registry rows are gone AND the table is dropped — by removing
// the metadata table the final write targets. It is the complement of the FK
// test: the rollback must undo a DROP TABLE, which is exactly the claim
// "atomicity is real because both engines execute DDL transactionally".
func TestDeleteContentTypeRollbackAtMetadataStep(t *testing.T) {
	db, author := openWithType(t, "delete-rollbackmeta.db")
	ctx := context.Background()

	id := insertEvento(t, db.DB, author, "Charla", "Montevideo", 40, true)
	beforeCols := columnsOf(t, db.DB, "cpt_eventos")
	beforeFields := storedFields(t, db, "eventos")

	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := db.DB.Exec(`DROP TABLE "__compat_schema"`); err != nil {
		t.Fatalf("drop metadata table: %v", err)
	}
	if _, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan)); err == nil {
		t.Fatal("the deletion succeeded although the metadata write could not have")
	} else {
		t.Logf("deletion failed as expected at the metadata step: %v", err)
	}

	if !tableExists(t, db.DB, "cpt_eventos") {
		t.Fatal("the DROP TABLE was not rolled back")
	}
	if got := columnsOf(t, db.DB, "cpt_eventos"); strings.Join(got, ",") != strings.Join(beforeCols, ",") {
		t.Fatalf("table shape changed: %v", got)
	}
	if got := storedFields(t, db, "eventos"); strings.Join(got, ",") != strings.Join(beforeFields, ",") {
		t.Fatalf("registry changed: %v", got)
	}
	var titulo string
	if err := db.DB.QueryRow(`SELECT titulo FROM "cpt_eventos" WHERE id = ?`, id).Scan(&titulo); err != nil || titulo != "Charla" {
		t.Fatalf("row lost or damaged: %q (err=%v)", titulo, err)
	}
	t.Logf("ROLLBACK-AT-LAST-STEP OK: the dropped table is back (%v), registry %v, row intact", beforeCols, beforeFields)
}

// TestDeleteContentTypeWithoutItsTable is the decision T1 asks for, proven: a
// definition whose real table is NOT in the catalog is CLEANED UP, not refused.
// See the header of contenttypes_delete.go for the argument; the test also pins
// that the state is REPORTED (TableMissing) rather than silently normalised.
func TestDeleteContentTypeWithoutItsTable(t *testing.T) {
	db, _ := openWithType(t, "delete-notable.db")
	ctx := context.Background()

	// The inconsistent state: someone dropped the table by hand.
	if _, err := db.DB.Exec(`DROP TABLE "cpt_eventos"`); err != nil {
		t.Fatalf("drop the real table: %v", err)
	}
	// It is genuinely unusable and un-editable in that state — which is why the
	// deletion has to work.
	if _, err := store.EditContentType(ctx, db, "eventos", []store.FieldEdit{{Name: "x", Type: schema.FieldText}}, []string{"titulo", "lugar", "asistentes", "gratis"}); !errors.Is(err, store.ErrMissingTable) {
		t.Fatalf("expected the edit to refuse the broken state, got %v", err)
	}

	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.TableMissing || plan.Rows != 0 {
		t.Fatalf("the plan hides the inconsistency: %+v", plan)
	}
	// The guard still applies: zero rows must still be STATED.
	if _, err := store.DeleteContentType(ctx, db, "eventos", store.DeleteConfirmation{Name: "eventos"}); err == nil {
		t.Fatal("an unconfirmed row count was accepted on a type with no table")
	}
	deleted, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan))
	if err != nil {
		t.Fatalf("delete of an orphan definition: %v", err)
	}
	if !deleted.TableMissing {
		t.Fatal("the receipt does not report that the table was already absent")
	}
	if _, err := store.FetchContentType(ctx, db, "eventos"); !errors.Is(err, store.ErrContentTypeNotFound) {
		t.Fatalf("the orphan definition survived: %v", err)
	}
	if got := countFieldRows(t, db); got != 0 {
		t.Fatalf("orphan field rows survived: %d", got)
	}
	if hasTable(metadataSchema(t, db.DB), "cpt_eventos") {
		t.Fatal("the metadata still lists the table")
	}
	t.Logf("MISSING TABLE OK: the orphan definition and its %d fields were cleaned up, reported as table_missing=%v", len(plan.Fields), deleted.TableMissing)
}

// TestDeleteContentTypeTwice: the second deletion is a 404, not a 500 and not a
// silent success.
func TestDeleteContentTypeTwice(t *testing.T) {
	db, _ := openWithType(t, "delete-twice.db")
	ctx := context.Background()

	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan)); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if _, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan)); !errors.Is(err, store.ErrContentTypeNotFound) {
		t.Fatalf("second delete: got %v, want ErrContentTypeNotFound", err)
	}
	if _, err := store.PlanContentTypeDeletion(ctx, db, "eventos"); !errors.Is(err, store.ErrContentTypeNotFound) {
		t.Fatalf("plan after delete: got %v, want ErrContentTypeNotFound", err)
	}
	t.Log("DOUBLE DELETE OK: the second attempt is ErrContentTypeNotFound (→ 404)")
}

// TestDeleteContentTypeConcurrent is the red-team question the contract flags as
// the one that usually breaks: N deletions of the SAME type at once.
//
// Exactly one may win. The decision is NOT an application check: it is
// RowsAffected on the DELETE of the content_types row inside each transaction,
// so a loser's whole transaction — the DROP included — rolls back.
func TestDeleteContentTypeConcurrent(t *testing.T) {
	db, author := openWithType(t, "delete-concurrent.db")
	ctx := context.Background()
	insertEvento(t, db.DB, author, "Feria", "Montevideo", 120, false)

	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	const racers = 6
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		wins     int
		notFound int
		others   []error
		start    = make(chan struct{})
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, store.ErrContentTypeNotFound):
				notFound++
			default:
				others = append(others, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d racers succeeded, want exactly 1 (notFound=%d others=%v)", wins, notFound, others)
	}
	if len(others) != 0 {
		t.Fatalf("unexpected errors from the losers: %v", others)
	}
	if tableExists(t, db.DB, "cpt_eventos") {
		t.Fatal("the table survived the winning deletion")
	}
	if got := countTypeRows(t, db); got != 0 {
		t.Fatalf("content_types has %d rows after the race", got)
	}
	if got := countFieldRows(t, db); got != 0 {
		t.Fatalf("content_type_fields has %d rows after the race", got)
	}
	if hasTable(metadataSchema(t, db.DB), "cpt_eventos") {
		t.Fatal("the metadata still lists the dropped table after the race")
	}
	t.Logf("CONCURRENCY OK: %d racers → 1 success, %d ErrContentTypeNotFound, catalog and metadata consistent", racers, notFound)
}

// TestDeleteContentTypeNameIsReusable is the criterion of T4 that catches a
// deletion which only LOOKS complete: the name must be free again, and the new
// table must be EMPTY — not carrying the old rows.
func TestDeleteContentTypeNameIsReusable(t *testing.T) {
	db, author := openWithType(t, "delete-reuse.db")
	ctx := context.Background()
	insertEvento(t, db.DB, author, "Feria", "Montevideo", 120, false)
	insertEvento(t, db.DB, author, "Charla Go", "Rosario", 41, true)

	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan)); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The same NAME, a DIFFERENT shape — so the assertion below also proves the
	// table was really re-created and not merely re-registered.
	again := schema.ContentTypeDefinition{
		Name: "eventos",
		Fields: []schema.FieldDefinition{
			{Name: "encabezado", Type: schema.FieldText},
			{Name: "cupos", Type: schema.FieldInteger},
		},
	}
	if err := store.CreateContentType(ctx, db, again); err != nil {
		t.Fatalf("re-create with the same name: %v", err)
	}
	cols := columnsOf(t, db.DB, "cpt_eventos")
	if strings.Join(cols, ",") != "id,author_id,encabezado,cupos,_search_fold,created_at,updated_at,metadata" {
		t.Fatalf("the re-created table has the wrong shape: %v", cols)
	}
	var rows int
	if err := db.DB.QueryRow(`SELECT count(*) FROM "cpt_eventos"`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("the re-created table carries %d rows of the old type", rows)
	}
	_ = author
	t.Logf("REUSE OK: `eventos` re-created with the new shape %v and 0 rows", cols)
}

// TestDeleteContentTypeSurvivesTwoRestarts is the CONTRACT-11 lesson applied to
// a deletion: a metadata row that still claims the dropped table would only
// misbehave on the SECOND boot, once it has been read back. EnsureSchema must
// neither fail nor RE-CREATE the table it no longer knows about.
func TestDeleteContentTypeSurvivesTwoRestarts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "delete-restart.db")
	ctx := context.Background()

	first, err := store.Open(compat.SQLite, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.EnsureSchema(ctx, first); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.SeedCatalogs(ctx, first); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.CreateContentType(ctx, first, eventosDef); err != nil {
		t.Fatalf("create: %v", err)
	}
	plan, err := store.PlanContentTypeDeletion(ctx, first, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := store.DeleteContentType(ctx, first, "eventos", exactConfirmation(plan)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for boot := 1; boot <= 2; boot++ {
		db, err := store.Open(compat.SQLite, dbPath)
		if err != nil {
			t.Fatalf("boot #%d open: %v", boot, err)
		}
		if err := store.EnsureSchema(ctx, db); err != nil {
			t.Fatalf("boot #%d ensure: %v", boot, err)
		}
		if tableExists(t, db.DB, "cpt_eventos") {
			t.Fatalf("boot #%d RE-CREATED the deleted table", boot)
		}
		defs, err := store.LoadDefinitions(ctx, db)
		if err != nil {
			t.Fatalf("boot #%d load: %v", boot, err)
		}
		if len(defs) != 0 {
			t.Fatalf("boot #%d sees %v", boot, defs)
		}
		if hasTable(metadataSchema(t, db.DB), "cpt_eventos") {
			t.Fatalf("boot #%d metadata still lists the dropped table", boot)
		}
		canonical, err := store.CanonicalSchema(ctx, db)
		if err != nil {
			t.Fatalf("boot #%d canonical: %v", boot, err)
		}
		if hasTable(canonical, "cpt_eventos") {
			t.Fatalf("boot #%d canonical schema still contains the deleted type", boot)
		}
		t.Logf("boot #%d OK: nothing re-created, %d dynamic types, metadata and canonical schema clean", boot, len(defs))
		if err := db.Close(); err != nil {
			t.Fatalf("boot #%d close: %v", boot, err)
		}
	}
}

// TestDeleteContentTypeEmptyType is the other half of the "zero rows vs ten
// thousand rows" distinction: an empty type deletes fine — but only after the
// zero is explicitly stated.
func TestDeleteContentTypeEmptyType(t *testing.T) {
	db, _ := openWithType(t, "delete-empty.db")
	ctx := context.Background()

	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Rows != 0 {
		t.Fatalf("a fresh type reports %d rows", plan.Rows)
	}
	if _, err := store.DeleteContentType(ctx, db, "eventos", store.DeleteConfirmation{}); err == nil {
		t.Fatal("an empty confirmation deleted an empty type — the accident this guard exists to prevent")
	}
	deleted, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted.Rows != 0 || tableExists(t, db.DB, "cpt_eventos") {
		t.Fatalf("unexpected result: %+v", deleted)
	}
	t.Log("EMPTY TYPE OK: rows=0 still has to be stated; the empty body is refused, `confirm_rows: 0` works")
}

// TestDeleteContentTypeUnknownName covers the red-team "a name that needs
// quoting": the identifier gate accepts only [a-z][a-z0-9_]*, so a hostile name
// cannot even name a stored type — the lookup binds it as a parameter and simply
// finds nothing. There is no escaping path to get wrong.
func TestDeleteContentTypeUnknownName(t *testing.T) {
	db, _ := openWithType(t, "delete-unknown.db")
	ctx := context.Background()
	hostile := []string{
		"no_such_type",
		`eventos"; DROP TABLE users; --`,
		"cpt_eventos", // the REAL table name is not a type name
		"",
		"Eventos",
	}
	for _, name := range hostile {
		if _, err := store.PlanContentTypeDeletion(ctx, db, name); !errors.Is(err, store.ErrContentTypeNotFound) {
			t.Fatalf("plan(%q): got %v, want ErrContentTypeNotFound", name, err)
		}
		if _, err := store.DeleteContentType(ctx, db, name, store.DeleteConfirmation{Name: name, RowsStated: true}); !errors.Is(err, store.ErrContentTypeNotFound) {
			t.Fatalf("delete(%q): got %v, want ErrContentTypeNotFound", name, err)
		}
	}
	// Everything is still there.
	if !tableExists(t, db.DB, "cpt_eventos") || !tableExists(t, db.DB, "users") {
		t.Fatal("a hostile name reached the database")
	}
	t.Logf("HOSTILE NAMES OK: %d names (including one with quotes and a semicolon) → ErrContentTypeNotFound, nothing touched", len(hostile))
}

// TestDeleteContentTypeManyRows is the "ten thousand rows" side of the
// contract's central distinction, at a size that still runs fast: the count is
// exact, and it is what the confirmation has to carry.
func TestDeleteContentTypeManyRows(t *testing.T) {
	db, author := openWithType(t, "delete-manyrows.db")
	ctx := context.Background()
	const n = 2000
	tx, err := db.DB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(`INSERT INTO "cpt_eventos" (author_id, titulo, lugar, asistentes, gratis) VALUES (?, ?, ?, ?, ?)`,
			author, fmt.Sprintf("evento-%d", i), "aqui", i, i%2 == 0); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	plan, err := store.PlanContentTypeDeletion(ctx, db, "eventos")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Rows != n {
		t.Fatalf("plan reports %d rows, want %d", plan.Rows, n)
	}
	// The count of a type with 2000 rows is NOT interchangeable with the count
	// of an empty one — which is the point of putting it in the confirmation.
	if _, err := store.DeleteContentType(ctx, db, "eventos", store.DeleteConfirmation{Name: "eventos", Rows: 0, RowsStated: true}); err == nil {
		t.Fatal("a zero-row confirmation deleted a type with 2000 rows")
	}
	deleted, err := store.DeleteContentType(ctx, db, "eventos", exactConfirmation(plan))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted.Rows != n || tableExists(t, db.DB, "cpt_eventos") {
		t.Fatalf("unexpected result: %+v", deleted)
	}
	t.Logf("MANY ROWS OK: %d rows counted exactly, a zero-row confirmation refused, the exact one destroyed the table", n)
}
