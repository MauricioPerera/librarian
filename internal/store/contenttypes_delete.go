package store

// contenttypes_delete.go implements CONTRACT-26: DELETING a dynamic content
// type — its definition, its fields and its real table with every row in it.
//
// It is the most destructive operation of the product, and the guard is the
// contract, not decoration. Everything below follows the shape
// contenttypes_edit.go established (CONTRACT-18), for the same reasons, and the
// two comments that matter most are repeated here because they are the two
// traps of this file:
//
// 1. compat.Store.DropTable is NOT used. It opens its OWN transaction (and
//    compat pins the SQLite pool to a single connection, so a nested
//    transaction would deadlock), which would put the DROP outside the
//    transaction that removes the registry rows — precisely the split this
//    contract exists to make impossible. The PURE compiler
//    compat.CompileDropTableIfExists is used instead and its statement is
//    executed inside OUR transaction.
//
// 2. The corollary of (1) is that keeping `__compat_schema` truthful becomes
//    OUR responsibility: Store.DropTable would have rewritten it. InspectSchema
//    PREFERS that metadata over the physical catalog, so a metadata row that
//    still lists a dropped table is the single most damaging state reachable in
//    this project — every later EnsureSchema would believe the table exists and
//    every `--dump-schema` would export a table that is not there. The last
//    statement of the transaction writes the FULL composed schema WITHOUT this
//    type, exactly as CreateContentType and EditContentType write it WITH it.
//
// WHY `IF EXISTS`, i.e. the decision the contract asks for explicitly (a
// definition whose physical table is NOT in the catalog):
//
//   - EditContentType refuses that state (ErrMissingTable) and is right to: a
//     rebuild has nothing to copy FROM, and creating the table on the way would
//     invent a state the registry only claimed.
//   - A deletion needs NOTHING from the table. Refusing it would leave the
//     admin with a type that cannot be used (the generic CRUD layer queries a
//     missing table), cannot be edited (ErrMissingTable), and now could not be
//     deleted either: a registry row with no way out of the product, in the one
//     operation whose entire purpose is to remove it. Closing the lifecycle
//     means the cleanup path must work on the broken state too.
//   - The documented cost of IF EXISTS — "a typo in the table name becomes a
//     silent no-op" — cannot apply here: the name is never typed by anyone. It
//     is derived by schema.DynamicTableName from the PERSISTED definition, the
//     same function that produced the CREATE TABLE. The only way the table is
//     absent is that it genuinely is absent.
//   - It is not silent either: the operation PROBES the physical catalog first
//     and reports TableMissing in its result, so the caller (and the report)
//     sees that it cleaned up an inconsistent state rather than a normal type.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// ContentTypeDeletion is the fully resolved, READ-ONLY plan of a deletion: what
// exists right now and what would be destroyed. It is computed before anything
// is written so it can be SHOWN to whoever has to confirm — the contract's
// "quien confirma tiene que ver cuántas filas va a destruir ANTES de confirmar"
// — and returned again after the fact as the receipt of what went.
type ContentTypeDeletion struct {
	// TypeName is the public name; TableName is the real table
	// (schema.DynamicTableName), which is what actually gets dropped.
	TypeName  string
	TypeID    string
	TableName string
	// Fields are the field names that go with the definition, in ordinal order.
	Fields []string
	// Rows is how many rows the real table holds. It is THE number the
	// confirmation has to state, so a type with zero rows and a type with ten
	// thousand cannot be confirmed by the same request.
	Rows int64
	// TableMissing is true when the definition exists but its real table does
	// not — the inconsistent state discussed at the top of this file.
	TableMissing bool
}

// DeleteConfirmation is what a caller must state to be allowed to proceed.
//
// THE SHAPE OF THE GUARD, AND WHY IT IS NOT A BOOLEAN. CONTRACT-18 refused
// `confirm: true` for a PARTIAL loss (removing fields) and demanded the list of
// names being destroyed. This is the TOTAL loss, so the criterion is extended,
// not relaxed: the caller must state
//
//   - the exact name of the type, so a request aimed at the wrong type cannot
//     be accepted by a client that had a different one on screen, and
//   - the exact number of rows that will be destroyed, which is a value nobody
//     can produce without first asking the server for it.
//
// The row count is the part a booleanised confirmation can never express: it is
// the difference between deleting an empty draft type and deleting ten thousand
// rows of production content, and it CHANGES, so a confirmation that carries it
// is also a freshness token — a count read before someone else loaded content
// no longer matches, and the deletion is refused instead of silently taking
// rows the confirming admin never saw.
//
// RowsStated distinguishes "I confirm zero rows" from "I sent no count at all".
// Without it, an empty body would delete every type that happens to be empty —
// exactly the accident this guard exists to prevent.
type DeleteConfirmation struct {
	Name       string
	Rows       int64
	RowsStated bool
}

// DeleteConfirmationError is the REFUSAL to destroy a type without a
// confirmation that proves it was read. It carries the live facts (name, row
// count, whether the table is there), so the refusal itself is the channel
// through which a caller learns what it has to confirm — the same role
// DataLossError's Fields list plays for an edit.
type DeleteConfirmationError struct {
	// TypeName and Rows are what is ACTUALLY there right now.
	TypeName     string
	TableName    string
	Rows         int64
	TableMissing bool
	// Reason says which part of the confirmation was wrong, so the message is
	// actionable instead of merely negative.
	Reason string
}

func (e DeleteConfirmationError) Error() string {
	missing := ""
	if e.TableMissing {
		missing = " (its real table is already absent from the catalog, so only the definition and its fields would be removed)"
	}
	return fmt.Sprintf(
		"refusing to delete the content type %q: %s. Deleting it drops the table %q and destroys its %d stored row(s) irreversibly%s; to proceed, send confirm_name=%q and confirm_rows=%d exactly",
		e.TypeName, e.Reason, e.TableName, e.Rows, missing, e.TypeName, e.Rows)
}

// PlanContentTypeDeletion answers "what would deleting this type destroy?"
// WITHOUT destroying anything. It is the read half of the guard: the row count
// it reports is the number the confirmation must carry, and it is the number
// the UI and the 400 response show before anyone can agree to anything.
//
// An unknown name is ErrContentTypeNotFound (→ 404). The name is bound as a
// parameter by LoadContentTypeFields, never interpolated, so a syntactically
// impossible name simply finds nothing.
func PlanContentTypeDeletion(ctx context.Context, store *compat.Store, typeName string) (ContentTypeDeletion, error) {
	typeID, fields, err := LoadContentTypeFields(ctx, store, typeName)
	if err != nil {
		return ContentTypeDeletion{}, err
	}
	plan := ContentTypeDeletion{
		TypeName:  typeName,
		TypeID:    typeID,
		TableName: schema.DynamicTableName(typeName),
	}
	for _, f := range fields {
		plan.Fields = append(plan.Fields, f.Name)
	}

	// The probe goes to the ENGINE'S OWN catalog, never to __compat_schema: the
	// metadata is precisely what this operation rewrites, so it cannot also be
	// the oracle. Same argument, same primitive, as EditContentType's
	// preconditions.
	present, err := physicalTableExists(ctx, store, plan.TableName)
	if err != nil {
		return ContentTypeDeletion{}, err
	}
	if !present {
		plan.TableMissing = true
		return plan, nil
	}
	rows, err := countTableRows(ctx, store.DB, plan.TableName)
	if err != nil {
		// THE WINDOW BETWEEN THE PROBE AND THE COUNT. These are two statements on
		// the pool, not one transaction, so a deletion that commits in between
		// leaves the count querying a table that is no longer there. That is not
		// a failure of anything — it is the answer "there is nothing left to
		// count" arriving in the shape of an error — so it is CLASSIFIED by
		// re-asking the catalog rather than by matching the driver's message
		// (which differs per engine and is exactly the class of bug CONTRACT-21
		// removed from this project). If the table really is still there, the
		// error was real and is propagated untouched.
		present, probeErr := physicalTableExists(ctx, store, plan.TableName)
		if probeErr != nil {
			return ContentTypeDeletion{}, probeErr
		}
		if present {
			return ContentTypeDeletion{}, err
		}
		plan.TableMissing = true
		return plan, nil
	}
	plan.Rows = rows
	return plan, nil
}

// countTableRows counts the rows of a dynamic table.
//
// The table name cannot be a bound parameter, so it goes through
// schema.QuoteIdentifier — the SAME gate compileRebuild uses for the very same
// name — and is therefore re-validated at the moment of interpolation. A name
// that would need escaping cannot reach this statement: the identifier gate
// accepts only [a-z][a-z0-9_]*, so quoting is a formality rather than a
// sanitiser.
func countTableRows(ctx context.Context, q queryer, table string) (int64, error) {
	quoted, err := schema.QuoteIdentifier(table)
	if err != nil {
		return 0, err
	}
	var rows int64
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM `+quoted).Scan(&rows); err != nil {
		return 0, fmt.Errorf("count the rows of %q: %w", table, err)
	}
	return rows, nil
}

// queryer is the tiny shared surface of *sql.DB and *sql.Tx that countTableRows
// needs, so the SAME count is used for the read-only plan (on the pool) and for
// the re-check inside the deletion's transaction. Mirrors execer above.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DeleteContentType removes a dynamic content type ATOMICALLY: its definition
// row, its field rows and its real table, all in ONE transaction, and returns
// the receipt of what was destroyed.
//
// THE ORDER INSIDE THE TRANSACTION, and why each step is where it is:
//
//  1. DELETE the content_types row. content_type_fields carries
//     foreignKeyCascade("content_type_id"), so the field rows go with it — no
//     second statement, and no way for the two to come apart. It goes FIRST
//     because its RowsAffected is what resolves a race between two deletions of
//     the same type.
//  2. Re-count the rows and re-check the confirmation, immediately before the
//     drop. The plan the caller confirmed was read OUTSIDE any transaction; this
//     is what makes the count in the confirmation a fact about the database
//     being modified rather than about a snapshot taken some seconds ago.
//  3. DROP the real table (the statement compiled BEFORE the transaction
//     opened, so a compilation failure costs nothing).
//  4. Write the FULL composed __compat_schema metadata WITHOUT this type.
//
// ATOMICITY IS REAL ON BOTH ENGINES for the reason CreateContentType writes
// down: both execute DDL transactionally, and this function RETURNS at the
// first error so the `defer tx.Rollback()` is always the next thing that runs.
// No statement is ever issued after a failure, which is what makes PostgreSQL's
// poisoned-transaction behaviour (25P02) and SQLite's non-poisoning behaviour
// reach the same end state.
//
// Concurrency: two deletions of the SAME type are decided by step 1. The loser's
// DELETE affects zero rows and it gets ErrContentTypeNotFound (→ 404) with its
// whole transaction rolled back — the drop included. That is a database
// guarantee, not an application check, so it holds across processes. The HTTP
// layer additionally serializes schema-mutating requests with h.schemaMu.
func DeleteContentType(ctx context.Context, store *compat.Store, typeName string, confirm DeleteConfirmation) (ContentTypeDeletion, error) {
	plan, err := PlanContentTypeDeletion(ctx, store, typeName)
	if err != nil {
		return ContentTypeDeletion{}, err
	}
	// The half of the guard that CANNOT race: the name, and the fact that a
	// count was stated at all. Both are properties of the request itself, so
	// checking them here costs nothing and cannot leave anything half-done.
	// The COUNT COMPARISON deliberately does NOT happen here — see step 2.
	if err := checkDeleteConfirmationShape(plan, confirm); err != nil {
		return ContentTypeDeletion{}, err
	}

	// The composed schema this database WILL have: every persisted definition
	// EXCEPT this one. compat validates the whole thing before anything runs.
	existing, err := LoadDefinitions(ctx, store)
	if err != nil {
		return ContentTypeDeletion{}, fmt.Errorf("load existing content type definitions: %w", err)
	}
	want := make([]schema.ContentTypeDefinition, 0, len(existing))
	for _, d := range existing {
		if d.Name == typeName {
			continue
		}
		want = append(want, d)
	}
	// CONTRACT-23: composed for the capabilities this installation was CREATED
	// with (read from the physical table), for the same reason CreateContentType
	// and EditContentType are — this operation rewrites __compat_schema, and a
	// composition built from the default would record a column the articles
	// table does not have.
	caps, err := installedCapabilitiesOrDefault(ctx, store)
	if err != nil {
		return ContentTypeDeletion{}, err
	}
	full, err := schema.BuildWithFor(caps, want)
	if err != nil {
		return ContentTypeDeletion{}, err
	}

	// Compiled BEFORE the transaction opens, exactly as CreateContentType and
	// EditContentType do. See the header for why IF EXISTS.
	dropTable, err := compat.CompileDropTableIfExists(store.Target, plan.TableName)
	if err != nil {
		return ContentTypeDeletion{}, err
	}

	engine := store.Target.Engine
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return ContentTypeDeletion{}, err
	}
	defer tx.Rollback()

	// 1. The definition. Its fields follow by ON DELETE CASCADE.
	//
	//    IT GOES FIRST, and that ORDER IS THE RACE RESOLUTION. RowsAffected on
	//    this statement is what decides a contest between two deletions of the
	//    same type: the loser sees 0 and returns ErrContentTypeNotFound with its
	//    whole transaction rolled back. Counting first instead would make the
	//    loser fail on "no such table" — a 500-shaped internal error for what is
	//    simply a type that is already gone.
	result, err := tx.ExecContext(ctx,
		`DELETE FROM `+schema.ContentTypesTable+` WHERE id = `+dual.Bind(engine, 1), plan.TypeID)
	if err != nil {
		return ContentTypeDeletion{}, fmt.Errorf("delete content type %q: %w", typeName, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ContentTypeDeletion{}, fmt.Errorf("delete content type %q: %w", typeName, err)
	}
	if affected == 0 {
		// Someone else deleted it between the plan and this statement. Nothing
		// this transaction did survives the rollback.
		return ContentTypeDeletion{}, ErrContentTypeNotFound
	}

	// 2. THE COUNT COMPARISON, and it happens HERE — inside the transaction, on
	//    a count read immediately before the drop — rather than next to the
	//    other half of the guard above. Two reasons, both learned from a real
	//    failure:
	//
	//    a) It is the only way the confirmed number is a fact about the database
	//       being modified rather than about a snapshot taken some seconds ago.
	//       A client that confirms 3 while someone else loads a fourth row is
	//       refused instead of silently destroying a row it never saw.
	//    b) It must come AFTER the registry DELETE above, so that a LOSER of a
	//       concurrent deletion gets ErrContentTypeNotFound (its type is simply
	//       gone) and not a confusing complaint about a row count. Comparing
	//       before the DELETE made two racers refuse each other with
	//       "you confirmed 1, there are 0 rows" — technically true, useless.
	//
	//    The query is skipped when the table is already absent, and the live
	//    count is then 0 by construction: on PostgreSQL a statement against a
	//    missing relation would ABORT the whole transaction (25P02) and take the
	//    orphan cleanup down with it, which is the very state that branch exists
	//    to repair. The comparison itself still runs, so an orphan definition
	//    cannot be deleted by confirming a made-up number either.
	live := int64(0)
	if !plan.TableMissing {
		live, err = countTableRows(ctx, tx, plan.TableName)
		if err != nil {
			return ContentTypeDeletion{}, err
		}
	}
	if confirm.Rows != live {
		return ContentTypeDeletion{}, DeleteConfirmationError{
			TypeName:     plan.TypeName,
			TableName:    plan.TableName,
			Rows:         live,
			TableMissing: plan.TableMissing,
			Reason: fmt.Sprintf("the confirmed row count %d does not match the %d row(s) this content type holds",
				confirm.Rows, live),
		}
	}
	plan.Rows = live

	// 3. The table with every row in it.
	if _, err := tx.ExecContext(ctx, dropTable); err != nil {
		return ContentTypeDeletion{}, fmt.Errorf("drop table for content type %q: %w", typeName, err)
	}

	// 4. The metadata, WITHOUT this type — the responsibility inherited from not
	//    using compat.Store.DropTable.
	if err := writeFullSchemaMetadata(ctx, engine, tx, full); err != nil {
		return ContentTypeDeletion{}, fmt.Errorf("record full schema metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ContentTypeDeletion{}, err
	}
	return plan, nil
}

// checkDeleteConfirmationShape is the race-free half of the guard: the parts
// that are properties of the REQUEST (a name was sent, it is this type's name, a
// row count was stated at all) rather than of the database. It is PURE, so every
// rejection it produces costs nothing and provably touched nothing.
//
// The count COMPARISON is deliberately not here: it belongs inside the
// transaction, where the number is live. See DeleteContentType step 2.
//
// Every rejection returns the SAME error type, carrying the row count the plan
// saw, so a caller that gets it back always has everything it needs to build the
// correct confirmation — and a caller that has NOT read the count cannot
// construct one.
func checkDeleteConfirmationShape(plan ContentTypeDeletion, confirm DeleteConfirmation) error {
	refuse := func(reason string) error {
		return DeleteConfirmationError{
			TypeName:     plan.TypeName,
			TableName:    plan.TableName,
			Rows:         plan.Rows,
			TableMissing: plan.TableMissing,
			Reason:       reason,
		}
	}
	switch {
	case confirm.Name == "" && !confirm.RowsStated:
		return refuse("no confirmation was sent")
	case confirm.Name == "":
		return refuse("the name of the content type was not confirmed")
	case confirm.Name != plan.TypeName:
		return refuse(fmt.Sprintf("the confirmed name %q is not the name of this content type", confirm.Name))
	case !confirm.RowsStated:
		return refuse("the number of rows to be destroyed was not confirmed")
	}
	return nil
}
