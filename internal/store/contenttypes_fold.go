package store

// contenttypes_fold.go implements the persistence half of CONTRACT-34: WHICH
// dynamic content types carry the folded search column schema.SearchFoldColumn.
//
// WHY THE ANSWER IS PERSISTED IN A REGISTRY TABLE AND NOT READ FROM THE ENGINE
// CATALOG — this was the real design choice of the contract, and both options
// were on the table:
//
//   - Asking the catalog ("does cpt_x have a column called _search_fold?") is
//     the ground truth and cannot drift. But compat exposes TableExists and
//     nothing for a COLUMN, so librarian would have to hand-write the probe —
//     and the two engines have NO common spelling for it (`pragma table_info`
//     versus information_schema.columns). That is precisely the per-engine SQL
//     this project exists to not have. Worse, the probe would sit on the hot
//     path: FetchContentType runs on every content request, so every read would
//     pay a catalog round trip per type.
//   - A registry row costs one extra query where two are already being made
//     (fields, references), speaks the same vocabulary as the rest of the
//     registry, and — decisively — CANNOT drift, because the row and the column
//     are written by the SAME transaction. DDL is transactional on both engines,
//     which is the property CreateContentType and the CONTRACT-18 rebuild
//     already rest on.
//
// The registry table is CODE (schema.Build()), so EnsureSchema creates it on an
// already-deployed database as a MISSING TABLE — the one schema change it is
// allowed to make. An installation upgrading to this binary therefore gets an
// empty fold register, which reads as "no existing type is folded", which is
// exactly true of its tables. Nothing is rewritten, nothing is rebuilt, and a
// restart stays non-destructive.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// foldsTablePresent reports whether the CONTRACT-34 registry table physically
// exists.
//
// IT IS THE UPGRADE PATH, exactly like referencesTablePresent (read its comment:
// the argument is identical and was already paid for once). LoadDefinitions runs
// INSIDE EnsureSchema, i.e. BEFORE the missing tables are created, so on the
// first boot of this binary against any deployed database this table does not
// exist yet. Answering "provably zero folded types" there is not a fallback: a
// fold marker can only live in that table, so a database without the table
// provably has none — and, correspondingly, no table with the column.
//
// It asks the ENGINE'S catalog through compat.Store.TableExists and never
// __compat_schema, for the reason this project has repeated since CONTRACT-13:
// the metadata records what some process believed, and EnsureSchema is about to
// rewrite it.
func foldsTablePresent(ctx context.Context, store *compat.Store) (bool, error) {
	present, err := store.TableExists(ctx, schema.ContentTypeFoldsTable)
	if err != nil {
		return false, fmt.Errorf("look up %s in the engine catalog: %w", schema.ContentTypeFoldsTable, err)
	}
	return present, nil
}

// loadFoldedTypeNames returns the PUBLIC names of every content type whose table
// carries the folded column. A map, because the only question ever asked of it
// is membership.
func loadFoldedTypeNames(ctx context.Context, store *compat.Store) (map[string]struct{}, error) {
	present, err := foldsTablePresent(ctx, store)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	rows, err := store.DB.QueryContext(ctx,
		`SELECT t.name
		   FROM `+schema.ContentTypeFoldsTable+` f
		   JOIN `+schema.ContentTypesTable+` t ON t.id = f.content_type_id`)
	if err != nil {
		return nil, fmt.Errorf("query folded content types: %w", err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan folded content type: %w", err)
		}
		out[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate folded content types: %w", err)
	}
	return out, nil
}

// insertFoldRow marks one content type as folded, inside the caller's
// transaction. It is only ever called by the same transaction that creates the
// table WITH the column, which is what makes the marker and the physical column
// impossible to separate.
func insertFoldRow(ctx context.Context, engine compat.Engine, tx *sql.Tx, typeID string) error {
	id, err := dual.NewUUID()
	if err != nil {
		return fmt.Errorf("generate id for the fold marker: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+schema.ContentTypeFoldsTable+` (id, content_type_id) VALUES (`+
			dual.Bind(engine, 1)+`, `+dual.Bind(engine, 2)+`)`, id, typeID); err != nil {
		return fmt.Errorf("record the fold marker: %w", err)
	}
	return nil
}

// refoldRows recomputes schema.SearchFoldColumn for EVERY row of a folded type,
// inside the caller's transaction. It is the CONTRACT-18 rebuild's obligation to
// CONTRACT-34 T3: the rebuild reshapes the table, and the fold has to end up
// describing the shape the table now has.
//
// THE FOLD IS COMPUTED IN GO, ONE ROW AT A TIME, AND THAT IS THE POINT. The
// obvious statement — UPDATE t SET _search_fold = lower(campo) — is exactly what
// this contract forbids: lower() is the engine's own case folding, which is what
// diverges between SQLite and PostgreSQL for anything outside ASCII, and a fold
// written by the engine could no longer be compared against a needle folded by
// Go. So the values are read out, folded by schema.FoldSearchText, and written
// back as ordinary bound parameters. The database never folds anything.
//
// The rows are collected BEFORE the first UPDATE is issued rather than updated
// while the cursor is open. That is required, not tidy: compat pins the SQLite
// pool to a single connection, so an Exec on the same transaction while a
// Query's rows are still open would deadlock.
//
// A type with nothing searchable (no field, or a first field that is not text —
// see schema.DynamicSearchField) has its fold column blanked wholesale. That is
// a real case an edit can produce, by removing the searched field or by putting
// an integer first, and leaving the previous folds behind would make the search
// of a type that HAS no search find rows anyway.
func refoldRows(ctx context.Context, engine compat.Engine, tx *sql.Tx, def schema.ContentTypeDefinition) error {
	table, err := schema.QuoteIdentifier(def.TableName())
	if err != nil {
		return err
	}
	// SearchFoldColumn and `id` are names this project owns, not admin-supplied
	// ones, so they go through the INTERNAL gate — the same distinction
	// compileRebuild makes for the columns ContentType() injects.
	foldColumn, err := schema.QuoteInternalIdentifier(schema.SearchFoldColumn)
	if err != nil {
		return err
	}
	idColumn, err := schema.QuoteInternalIdentifier("id")
	if err != nil {
		return err
	}
	field, searchable := schema.DynamicSearchField(def)
	if !searchable {
		_, err := tx.ExecContext(ctx, `UPDATE `+table+` SET `+foldColumn+` = NULL`)
		return err
	}
	source, err := schema.QuoteIdentifier(field.Name)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+idColumn+`, `+source+` FROM `+table)
	if err != nil {
		return err
	}
	type folded struct {
		id    string
		value any
	}
	var pending []folded
	for rows.Next() {
		var id string
		var text sql.NullString
		if err := rows.Scan(&id, &text); err != nil {
			rows.Close()
			return err
		}
		entry := folded{id: id}
		if text.Valid {
			// A NULL field stays a NULL fold: `contains` over NULL is not a match on
			// either engine, which is the correct answer for a row with no text.
			entry.value = schema.FoldSearchText(text.String)
		}
		pending = append(pending, entry)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	update := `UPDATE ` + table + ` SET ` + foldColumn + ` = ` + dual.Bind(engine, 1) +
		` WHERE ` + idColumn + ` = ` + dual.Bind(engine, 2)
	for _, entry := range pending {
		if _, err := tx.ExecContext(ctx, update, entry.value, entry.id); err != nil {
			return err
		}
	}
	return nil
}

// SearchFoldValue is THE value that belongs in schema.SearchFoldColumn for a row
// of this type, given the row's own-column values in declaration order (the
// order ownColumnNames imposes, which is fields then references).
//
// It lives in the store rather than in the server because BOTH writers need it
// and they are in different packages: internal/server writes it on every INSERT
// and UPDATE of the generic CRUD layer, and the CONTRACT-18 rebuild recomputes
// it here after reshaping the table. One function, so a row written by the CRUD
// layer and a row rewritten by an edit can never carry differently-computed
// folds — which would make the search find one and miss the other.
//
// It returns nil (SQL NULL) for a type with nothing to fold: no searchable first
// field, or a stored NULL. NULL is the honest value — `contains` over NULL is
// NULL, i.e. not a match — and it is what an added column starts as, so a row
// written before its type had anything searchable and a row written after agree.
func SearchFoldValue(def schema.ContentTypeDefinition, values []any) any {
	field, ok := schema.DynamicSearchField(def)
	if !ok {
		return nil
	}
	// The index is LOOKED UP rather than assumed to be 0. DynamicSearchField
	// happens to return the first declared field today, and the first declared
	// field happens to be the first own column; deriving the position from the
	// field's NAME means this stays correct if either of those ever stops being
	// true, instead of writing the fold of the wrong column.
	index := -1
	for i, f := range def.Fields {
		if f.Name == field.Name {
			index = i
			break
		}
	}
	if index < 0 || index >= len(values) {
		return nil
	}
	text, ok := values[index].(string)
	if !ok {
		// A NULL (nil) or anything that is not text. The gate that makes the
		// second case unreachable is DynamicSearchField itself, which only accepts
		// a `text` field; this is the fail-closed half of that gate.
		return nil
	}
	return schema.FoldSearchText(text)
}
