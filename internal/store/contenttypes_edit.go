package store

// contenttypes_edit.go implements CONTRACT-18 T1: EDITING the fields of an
// already-applied dynamic content type, closing hueco 2 of docs/PENDIENTES.md.
//
// WHY A REBUILD, AND WHY IT IS THE ONLY HONEST OPTION HERE
//
// sqlite-postgres-compat has no ALTER TABLE and no RENAME (verified in v0.2.0).
// That is deliberate: the package expresses a schema DECLARATIVELY and compiles
// it for both engines, and a per-engine mutation grammar would be a second,
// divergent source of truth. So an edit is composed out of the operations
// compat DOES express — create, copy, drop — inside ONE transaction. DDL is
// transactional on both engines, which is what makes "all of it or none of it"
// a real guarantee rather than a hope.
//
// THE SHAPE OF THE REBUILD (the order matters and is the contract's):
//
//	1. CREATE the staging table with the NEW shape.
//	2. Copy the rows into it, mapping the surviving fields BY IDENTITY
//	   (content_type_fields.id), never by name — see the cross-rename note below.
//	3. DROP the original table.
//	4. CREATE it again, with its NAME UNCHANGED and the NEW shape.
//	5. Copy the rows back.
//	6. DROP the staging table.
//	7. Update the registry rows (content_types / content_type_fields).
//	8. Write the FULL composed __compat_schema metadata.
//
// Steps 3-5 are why the staging table exists at all: without RENAME, the only
// way to end up with the new shape UNDER THE ORIGINAL NAME is to drop the
// original and re-create it, and the rows have to live somewhere while that
// happens. The cost is an O(2n) copy — every row is written twice. That is
// accepted explicitly: an edit is a rare administrative action on an
// admin-managed content type, and the alternative (leaving the rebuilt table
// under a new name) would change the real table name of a type, which
// CONTRACT-17 pinned to schema.DynamicTableName and which the generic CRUD
// layer derives from the public name.
//
// WHY IDENTITY, NOT NAME (the cross-rename): with `a`→`b` and `b`→`a` in the
// SAME edit, any implementation that maps surviving columns by name — or that
// renames one field at a time — either loses data or swaps it wrongly. Here the
// whole mapping is computed as ONE set from content_type_fields.id and applied
// in a single INSERT ... SELECT, so a cross-rename is not a special case: it is
// the same code path as any other mapping.
//
// compat.Store.DropTable is NOT used: it opens its OWN transaction (and compat
// pins the SQLite pool to a single connection, so a nested transaction would
// deadlock). The PURE compiler compat.CompileDropTable is used instead and the
// statement is executed inside this transaction — the same treatment
// CreateContentType gives compat.CompileDDL. The corollary is that keeping
// __compat_schema truthful becomes OUR responsibility (Store.DropTable would
// have done it), which is what step 8 is for: InspectSchema PREFERS that
// metadata over the physical catalog, so a metadata row that lies is the single
// most damaging state reachable in this project.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// PersistedField is one field of a dynamic content type AS STORED, i.e. with
// its database identity (content_type_fields.id).
//
// It is a STORE-level type on purpose. schema.FieldDefinition stays exactly
// {Name, Type}: it is the COMPILATION type, the thing that becomes a
// compat.Column, and a compilation input has no identity. Identity only exists
// once a definition has been persisted, so it lives here, next to the code that
// reads and writes those rows.
type PersistedField struct {
	ID   string           `json:"id"`
	Name string           `json:"name"`
	Type schema.FieldType `json:"type"`
}

// FieldEdit is ONE entry of the desired new field list.
//
// THE API DECISION OF THIS CONTRACT (documented in the report): an edit sends
// the COMPLETE new field list, in the desired order, and each entry either
//
//   - carries the ID of an existing field — "this position IS that field",
//     whatever its name now says: the column's data is carried over and, if the
//     name differs, that is a RENAME; or
//   - carries no ID — "this is a NEW field": the column is created and is NULL
//     for every existing row.
//
// An existing field that no entry references is REMOVED, and its data is lost.
//
// Why the full list rather than a patch ("rename a to b", "drop c"): the field
// list is ordered (content_type_fields.ordinal drives the physical column
// order), the result must be validated as a WHOLE (duplicate names, reserved
// names, the identifier gate), and the rebuild consumes exactly one final
// shape. A patch language would have to be reduced to this list anyway, and it
// would make a cross-rename an ordering puzzle for the caller instead of a
// plain set.
type FieldEdit struct {
	ID   string
	Name string
	Type schema.FieldType
}

// CarriedField is one surviving field of a rebuild: which stored field it is,
// the column name it had, and the column name it will have.
type CarriedField struct {
	ID      string
	OldName string
	NewName string
}

// ContentTypeEdit is the fully resolved plan of an edit. It is computed BEFORE
// anything touches the database, so it can be shown to the caller (the UI shows
// the removals it implies before asking for confirmation) and asserted in
// tests.
type ContentTypeEdit struct {
	TypeName string
	TypeID   string
	// New is the definition the type will have. It has already passed the same
	// schema.ContentTypeDefinition.Validate() gate a creation passes.
	New schema.ContentTypeDefinition
	// Carried are the fields that survive, in the NEW order.
	Carried []CarriedField
	// Added are the names of the fields that did not exist before.
	Added []string
	// Removed are the names of the fields that disappear — the ones whose data
	// is destroyed.
	Removed []string
	// Renamed lists {old, new} for the carried fields whose name changes.
	Renamed [][2]string
	// NoOp is true when the requested list is identical to the stored one (same
	// fields, same names, same types, same order). Nothing is rebuilt then.
	NoOp bool
}

// ErrUnknownField is returned when an edit entry references a field id that
// does not belong to this content type (a stale editor, or a hand-crafted
// request) → 400, never a 500.
var ErrUnknownField = errors.New("unknown field")

// ErrFieldTypeChange is returned when an edit keeps a field's identity but
// changes its declared type.
//
// OUT OF SCOPE BY DECISION, not by omission: casting between families diverges
// hard between the two engines ('abc' to an integer is 0 on SQLite and an error
// on PostgreSQL), and this project exists so that no such divergence can be
// introduced. The error says so explicitly and points at the supported route
// (remove the field and add a new one, accepting the loss).
var ErrFieldTypeChange = errors.New("changing the type of an existing field is not supported")

// ErrMissingTable is returned when the registry has a definition whose real
// table is not in the catalog. A rebuild cannot be attempted then: there is
// nothing to copy from, and creating the table silently would hide a database
// that is not in the state the registry claims.
var ErrMissingTable = errors.New("the real table of this content type does not exist")

// ErrStagingTableExists is returned when the transient rebuild table is already
// present. It can only survive a crash mid-DDL (both engines roll back DDL) or
// be squatted by an operator; either way, rebuilding on top of it would be
// unsafe, so this fails loudly instead.
var ErrStagingTableExists = errors.New("the rebuild staging table already exists")

// DataLossError is the REFUSAL to destroy data without an explicit, itemised
// confirmation. It names every field whose data would be lost so the caller
// (API client or UI) can state exactly what it is agreeing to — the contract's
// "say what will be lost BEFORE doing it".
type DataLossError struct {
	Fields []string
}

func (e DataLossError) Error() string {
	return fmt.Sprintf("removing the field(s) %s destroys their stored data irreversibly; confirm each one explicitly to proceed",
		strings.Join(e.Fields, ", "))
}

// UnexpectedConfirmationError is the mirror refusal: the caller confirmed the
// removal of a field that is NOT being removed. That means the caller's view of
// the type is stale, so applying the edit would be applying it to a type the
// caller did not actually see.
type UnexpectedConfirmationError struct {
	Fields []string
}

func (e UnexpectedConfirmationError) Error() string {
	return fmt.Sprintf("confirmed the removal of the field(s) %s, which are not being removed; reload the content type and try again",
		strings.Join(e.Fields, ", "))
}

// LoadContentTypeFields returns a content type's id and its fields WITH their
// identities, in ordinal order. It is the read half of the edit API: a caller
// must know the ids before it can express a rename.
//
// The name is bound as a parameter, never interpolated; an unknown name is
// ErrContentTypeNotFound (→ 404), never an error about SQL.
// CONTRACT-21 T1: both statements bind through compat's marker for the engine.
// The ORDER BY key is `ordinal`, an INTEGER, so it needs no Go-side reordering:
// the collation divergence of CONTRACT-20 is a property of TEXT comparison only,
// and both engines order integers identically.
func LoadContentTypeFields(ctx context.Context, store *compat.Store, typeName string) (string, []PersistedField, error) {
	engine := store.Target.Engine
	var typeID string
	err := store.DB.QueryRowContext(ctx,
		`SELECT id FROM `+schema.ContentTypesTable+` WHERE name = `+dual.Bind(engine, 1), typeName).Scan(&typeID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrContentTypeNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("look up content type %q: %w", typeName, err)
	}
	rows, err := store.DB.QueryContext(ctx,
		`SELECT id, name, field_type FROM `+schema.ContentTypeFieldsTable+`
		  WHERE content_type_id = `+dual.Bind(engine, 1)+` ORDER BY ordinal`, typeID)
	if err != nil {
		return "", nil, fmt.Errorf("query fields of content type %q: %w", typeName, err)
	}
	defer rows.Close()
	var out []PersistedField
	for rows.Next() {
		var f PersistedField
		var fieldType string
		if err := rows.Scan(&f.ID, &f.Name, &fieldType); err != nil {
			return "", nil, fmt.Errorf("scan field of content type %q: %w", typeName, err)
		}
		f.Type = schema.FieldType(fieldType)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("iterate fields of content type %q: %w", typeName, err)
	}
	return typeID, out, nil
}

// PlanContentTypeEdit resolves a desired field list against the stored one. It
// is PURE (no database access) so every rejection it produces costs nothing and
// can never leave anything half-done — the contract's "validation errors must
// not touch the database" is true by construction, not by rollback.
//
// CONTRACT-27 adds `references`: the relations the type ALREADY has, which an
// edit never changes (a relation is declared at creation — see the report) but
// which the plan must carry for two reasons. First, the rebuild re-creates the
// table from plan.New, so a definition without them would silently DROP the
// foreign-key columns and their data. Second, they participate in validation: a
// new field named like an existing reference would otherwise become a duplicate
// column, caught by compat at apply time as an opaque error instead of here as a
// clean 400.
func PlanContentTypeEdit(typeName, typeID string, stored []PersistedField, references []schema.ReferenceDefinition, edits []FieldEdit) (ContentTypeEdit, error) {
	plan := ContentTypeEdit{TypeName: typeName, TypeID: typeID}

	// 1. The SAME gate a creation passes — the identifier rules are not relaxed
	//    because this is an edit. It also rejects duplicate field names and
	//    unknown field types inside the requested list.
	def := schema.ContentTypeDefinition{Name: typeName, References: references}
	for _, e := range edits {
		def.Fields = append(def.Fields, schema.FieldDefinition{Name: e.Name, Type: e.Type})
	}
	if err := def.Validate(); err != nil {
		return ContentTypeEdit{}, err
	}
	plan.New = def

	byID := make(map[string]PersistedField, len(stored))
	for _, f := range stored {
		byID[f.ID] = f
	}

	// 2. Resolve every entry against the stored fields.
	used := make(map[string]struct{}, len(edits))
	for _, e := range edits {
		if e.ID == "" {
			plan.Added = append(plan.Added, e.Name)
			continue
		}
		current, ok := byID[e.ID]
		if !ok {
			return ContentTypeEdit{}, fmt.Errorf("%w: no field with id %q belongs to content type %q", ErrUnknownField, e.ID, typeName)
		}
		if _, dup := used[e.ID]; dup {
			return ContentTypeEdit{}, fmt.Errorf("%w: field %q is referenced twice in the same edit", ErrUnknownField, current.Name)
		}
		used[e.ID] = struct{}{}
		if current.Type != e.Type {
			return ContentTypeEdit{}, fmt.Errorf("%w: field %q is %q and cannot become %q. Casting between type families diverges between SQLite and PostgreSQL, which this project exists to prevent; remove the field and add a new one instead, accepting that its data is lost",
				ErrFieldTypeChange, current.Name, string(current.Type), string(e.Type))
		}
		plan.Carried = append(plan.Carried, CarriedField{ID: e.ID, OldName: current.Name, NewName: e.Name})
		if current.Name != e.Name {
			plan.Renamed = append(plan.Renamed, [2]string{current.Name, e.Name})
		}
	}

	// 3. Everything not referenced disappears, with its data.
	for _, f := range stored {
		if _, ok := used[f.ID]; !ok {
			plan.Removed = append(plan.Removed, f.Name)
		}
	}

	// 4. A no-op edit (identical list, identical order) rebuilds NOTHING. An
	//    O(2n) copy of every row to obtain the table that already exists would be
	//    pure risk for zero benefit.
	plan.NoOp = len(plan.Added) == 0 && len(plan.Removed) == 0 && len(plan.Renamed) == 0 &&
		sameOrder(stored, plan.Carried)
	return plan, nil
}

// sameOrder reports whether the carried fields are exactly the stored fields,
// in the stored order. Reordering the columns IS a change (the physical column
// order is part of the schema and of the export), so it does not count as a
// no-op.
func sameOrder(stored []PersistedField, carried []CarriedField) bool {
	if len(stored) != len(carried) {
		return false
	}
	for i := range stored {
		if stored[i].ID != carried[i].ID {
			return false
		}
	}
	return true
}

// EditContentType applies a field edit to an existing dynamic content type,
// ATOMICALLY, and returns the plan that was applied.
//
// confirmedRemovals must name EXACTLY the fields the edit removes (order and
// duplicates are irrelevant). Anything else is refused with DataLossError or
// UnexpectedConfirmationError before a single statement runs.
func EditContentType(ctx context.Context, store *compat.Store, typeName string, edits []FieldEdit, confirmedRemovals []string) (ContentTypeEdit, error) {
	typeID, stored, err := LoadContentTypeFields(ctx, store, typeName)
	if err != nil {
		return ContentTypeEdit{}, err
	}
	references, err := LoadContentTypeReferences(ctx, store, typeID)
	if err != nil {
		return ContentTypeEdit{}, err
	}
	plan, err := PlanContentTypeEdit(typeName, typeID, stored, references, edits)
	if err != nil {
		return ContentTypeEdit{}, err
	}
	// CONTRACT-34 — THE FOLD STATE IS CARRIED, NEVER DECIDED, BY AN EDIT. Folded
	// says whether the physical table has the system column; an edit reshapes the
	// FIELDS and has no business adding or removing a system column, so plan.New
	// must describe the table as it is. Getting this wrong is not cosmetic: the
	// rebuild re-creates the table from plan.New, so a false here would silently
	// DROP the folded column of a type that has one (and every search over it
	// would then ask for a column the routine declares and the table lacks).
	//
	// It is resolved HERE and not in PlanContentTypeEdit because the plan is PURE
	// — that purity is what makes "a rejected edit provably wrote nothing" true by
	// construction — and this answer only exists in the database.
	folded, err := loadFoldedTypeNames(ctx, store)
	if err != nil {
		return ContentTypeEdit{}, err
	}
	_, plan.New.Folded = folded[typeName]
	if err := checkConfirmation(plan.Removed, confirmedRemovals); err != nil {
		return ContentTypeEdit{}, err
	}
	if plan.NoOp {
		// Nothing changed: succeed without touching the database at all.
		return plan, nil
	}

	// CONTRACT-27 T3 — THE GUARD, and it is HERE: after the no-op shortcut (an
	// edit that rebuilds nothing cannot fail on a foreign key) and before
	// anything is composed, compiled or written.
	//
	// The rebuild DROPS the real table and re-creates it under the same name
	// (there is no RENAME in compat, see the file header). A foreign key pointing
	// at that table forbids exactly that, and this project never emits CASCADE,
	// so the DROP would fail inside the transaction with an engine error naming a
	// constraint rather than a content type. That limit is accepted; being told
	// about it in those terms is not.
	if err := guardNotReferenced(ctx, store, typeName, "edit"); err != nil {
		return ContentTypeEdit{}, err
	}

	// The composed schema this database WILL have: every persisted definition,
	// with THIS one replaced by the new shape. compat validates the whole thing
	// (duplicate columns, families, reserved names) before anything is executed.
	existing, err := LoadDefinitions(ctx, store)
	if err != nil {
		return ContentTypeEdit{}, fmt.Errorf("load existing content type definitions: %w", err)
	}
	want := make([]schema.ContentTypeDefinition, 0, len(existing))
	for _, d := range existing {
		if d.Name == typeName {
			d = plan.New
		}
		want = append(want, d)
	}
	// CONTRACT-23: composed for the capabilities this installation was CREATED
	// with (read from the physical table), for the same reason CreateContentType
	// is — this operation rewrites __compat_schema, and a composition built from
	// the default would put a column the articles table does not have into it.
	caps, err := installedCapabilitiesOrDefault(ctx, store)
	if err != nil {
		return ContentTypeEdit{}, err
	}
	full, err := schema.BuildWithFor(caps, want)
	if err != nil {
		return ContentTypeEdit{}, err
	}

	// Compile every statement BEFORE opening the transaction, exactly as
	// CreateContentType does: a compilation failure then costs nothing.
	statements, err := compileRebuild(store.Target, plan)
	if err != nil {
		return ContentTypeEdit{}, err
	}

	// The two catalog preconditions. Both are consulted on the ENGINE'S OWN
	// catalog rather than compat's metadata: the metadata is precisely what this
	// operation rewrites, so it cannot also be the oracle. They also run BEFORE
	// the transaction opens — required on SQLite, where compat pins the pool to a
	// single connection, and correct on both engines.
	present, err := physicalTableExists(ctx, store, plan.New.TableName())
	if err != nil {
		return ContentTypeEdit{}, err
	}
	if !present {
		return ContentTypeEdit{}, fmt.Errorf("%w: %q", ErrMissingTable, plan.New.TableName())
	}
	staging, err := physicalTableExists(ctx, store, schema.StagingTableName)
	if err != nil {
		return ContentTypeEdit{}, err
	}
	if staging {
		return ContentTypeEdit{}, fmt.Errorf("%w: %q must be dropped by hand before retrying", ErrStagingTableExists, schema.StagingTableName)
	}

	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return ContentTypeEdit{}, err
	}
	defer tx.Rollback()

	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return ContentTypeEdit{}, fmt.Errorf("rebuild table for content type %q: %w", typeName, err)
		}
	}
	// CONTRACT-34 T3 — THE FOLD IS RECOMPUTED, NOT COPIED, AND UNCONDITIONALLY.
	//
	// compileRebuild deliberately does not list SearchFoldColumn in either copy,
	// so after the statements above the column exists and is NULL for every row.
	// That is not an oversight to be patched by adding it to the copy lists: an
	// edit can RENAME the searched field, REORDER the fields so a different one
	// becomes first, REMOVE it, or ADD a new first field that is NULL everywhere —
	// in three of those four cases a copied fold would be a fold of text that is
	// no longer what the panel searches, and the search would find rows by their
	// OLD value. A stale fold is strictly worse than no fold, because it is wrong
	// instead of merely case-sensitive.
	//
	// Recomputing ALWAYS rather than only when the source changed is a deliberate
	// trade of work for the absence of a "when is it stale?" rule: a rebuild is a
	// rare administrative action that already accepts an O(2n) copy of every row
	// (see the file header), and this adds an O(n) pass to it. The alternative —
	// folding in the SQL of the copy with lower() — is forbidden by the contract
	// and would defeat the entire point: lower() is exactly what diverges between
	// the two engines, and the fold has to happen in Go or not at all.
	if plan.New.Folded {
		if err := refoldRows(ctx, store.Target.Engine, tx, plan.New); err != nil {
			return ContentTypeEdit{}, fmt.Errorf("recompute the folded search column of content type %q: %w", typeName, err)
		}
	}
	if err := applyRegistryEdit(ctx, store.Target.Engine, tx, plan); err != nil {
		return ContentTypeEdit{}, err
	}
	// The FULL composed schema — the responsibility inherited from not using
	// compat.Store.DropTable, and the thing InspectSchema prefers over the
	// physical catalog.
	if err := writeFullSchemaMetadata(ctx, store.Target.Engine, tx, full); err != nil {
		return ContentTypeEdit{}, fmt.Errorf("record full schema metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ContentTypeEdit{}, err
	}
	return plan, nil
}

// checkConfirmation compares the removals the plan implies with the ones the
// caller confirmed. Both directions are errors: an unconfirmed removal would
// destroy data the caller never agreed to lose, and a confirmation for a field
// that is not being removed means the caller was looking at a different version
// of the type.
func checkConfirmation(removed, confirmed []string) error {
	want := make(map[string]struct{}, len(removed))
	for _, n := range removed {
		want[n] = struct{}{}
	}
	got := make(map[string]struct{}, len(confirmed))
	for _, n := range confirmed {
		got[n] = struct{}{}
	}
	var missing, extra []string
	for _, n := range removed {
		if _, ok := got[n]; !ok {
			missing = append(missing, n)
		}
	}
	for _, n := range confirmed {
		if _, ok := want[n]; !ok {
			extra = append(extra, n)
		}
	}
	if len(missing) > 0 {
		return DataLossError{Fields: missing}
	}
	if len(extra) > 0 {
		return UnexpectedConfirmationError{Fields: extra}
	}
	return nil
}

// physicalTableExists asks the ENGINE'S own catalog whether a table exists.
//
// CONTRACT-21 T1 — THE SECOND OF THE TWO CATALOG PROBES, and the same story as
// registryPresent: a hand-written `sqlite_master` lookup, replaced by
// compat.Store.TableExists, which is the primitive that exists precisely for
// this question. Everything the old comment claimed is preserved and now
// enforced by the primitive rather than by prose: it asks the physical catalog
// (never __compat_schema, which this operation rewrites), it matches the name
// byte for byte on both engines, and on PostgreSQL it restricts the lookup to
// current_schema() — which is exactly where an unqualified CREATE TABLE from
// this same code lands, so the probe and the DDL agree about "here".
func physicalTableExists(ctx context.Context, store *compat.Store, name string) (bool, error) {
	present, err := store.TableExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("look up %q in the engine catalog: %w", name, err)
	}
	return present, nil
}

// compileRebuild produces, in order, every statement of the rebuild. It is pure
// (target + plan in, SQL out), so the whole sequence is unit-testable without a
// database and a failure cannot leave a half-built table behind.
//
// Every identifier is emitted through schema.QuoteIdentifier — the SAME gate
// internal/server/content.go uses — so no name reaches a statement without
// being re-validated at the moment of interpolation. The staging table's name
// is a constant of this project and goes through QuoteInternalIdentifier, which
// says at the call site that it is not a user-supplied name.
func compileRebuild(target compat.Target, plan ContentTypeEdit) ([]string, error) {
	realName := plan.New.TableName()
	realTable, err := schema.QuoteIdentifier(realName)
	if err != nil {
		return nil, err
	}
	stagingTable, err := schema.QuoteInternalIdentifier(schema.StagingTableName)
	if err != nil {
		return nil, err
	}

	// The new shape, twice: once under the staging name, once under the real
	// one. Both are produced by schema.DynamicTable/ContentType, so the staging
	// table is structurally identical to the definitive one — the copy back
	// cannot silently lose a column.
	definitive, err := schema.DynamicTable(plan.New)
	if err != nil {
		return nil, err
	}
	stagingDef := definitive
	stagingDef.Name = schema.StagingTableName

	createStaging, err := compat.CompileDDL(target, compat.Schema{Tables: []compat.Table{stagingDef}})
	if err != nil {
		return nil, fmt.Errorf("compile staging table DDL: %w", err)
	}
	createDefinitive, err := compat.CompileDDL(target, compat.Schema{Tables: []compat.Table{definitive}})
	if err != nil {
		return nil, fmt.Errorf("compile rebuilt table DDL: %w", err)
	}
	dropOriginal, err := compat.CompileDropTable(target, realName)
	if err != nil {
		return nil, err
	}
	dropStaging, err := compat.CompileDropTable(target, schema.StagingTableName)
	if err != nil {
		return nil, err
	}

	// The injected columns are carried verbatim, INCLUDING id: an edit of the
	// fields must never change the identity of a piece of content, so a URL to a
	// concrete row keeps working. created_at/updated_at are copied too — a
	// schema edit is not a content edit and must not look like one.
	//
	// They go through QuoteInternalIdentifier, not QuoteIdentifier: these are
	// names this project injects (they are in schema.ReservedNames() precisely
	// so that no dynamic FIELD can be called that), so the DYNAMIC-name gate
	// would — correctly — reject them. internal/server/content.go makes the same
	// distinction by interpolating them from its own constants.
	common := []string{"id", "author_id", "created_at", "updated_at", "metadata"}
	quotedCommon := make([]string, 0, len(common))
	for _, c := range common {
		q, err := schema.QuoteInternalIdentifier(c)
		if err != nil {
			return nil, err
		}
		quotedCommon = append(quotedCommon, q)
	}

	// CONTRACT-27: the RELATION columns are carried VERBATIM, like the injected
	// ones — an edit changes the field list and never the relations, so their
	// name is the same on both sides of every copy. They go through
	// QuoteIdentifier (not QuoteInternalIdentifier): a reference name IS an
	// admin-supplied dynamic identifier, subject to the same gate as a field.
	//
	// Omitting them here would be the quietest way to lose data in this file: the
	// rebuilt table WOULD have the columns (schema.DynamicTable emits them from
	// plan.New.References) and the copies would simply leave them NULL.
	quotedReferences := make([]string, 0, len(plan.New.References))
	for _, r := range plan.New.References {
		q, err := schema.QuoteIdentifier(r.Name)
		if err != nil {
			return nil, err
		}
		quotedReferences = append(quotedReferences, q)
	}
	quotedCommon = append(quotedCommon, quotedReferences...)

	// Forward copy: only the SURVIVING fields, mapped new-name ← old-name by
	// identity. A field being added is simply absent from both lists, so it
	// takes its column default (NULL) for every existing row.
	intoStaging := append([]string{}, quotedCommon...)
	fromOriginal := append([]string{}, quotedCommon...)
	for _, c := range plan.Carried {
		newCol, err := schema.QuoteIdentifier(c.NewName)
		if err != nil {
			return nil, err
		}
		oldCol, err := schema.QuoteIdentifier(c.OldName)
		if err != nil {
			return nil, err
		}
		intoStaging = append(intoStaging, newCol)
		fromOriginal = append(fromOriginal, oldCol)
	}

	// Copy back: the staging table already HAS the new shape, so both sides are
	// the full new column list, name for name.
	allNew := append([]string{}, quotedCommon...)
	for _, f := range plan.New.Fields {
		q, err := schema.QuoteIdentifier(f.Name)
		if err != nil {
			return nil, err
		}
		allNew = append(allNew, q)
	}

	statements := make([]string, 0, len(createStaging)+len(createDefinitive)+4)
	statements = append(statements, createStaging...)
	statements = append(statements,
		`INSERT INTO `+stagingTable+` (`+strings.Join(intoStaging, ", ")+`) SELECT `+strings.Join(fromOriginal, ", ")+` FROM `+realTable,
		dropOriginal,
	)
	statements = append(statements, createDefinitive...)
	statements = append(statements,
		`INSERT INTO `+realTable+` (`+strings.Join(allNew, ", ")+`) SELECT `+strings.Join(allNew, ", ")+` FROM `+stagingTable,
		dropStaging,
	)
	return statements, nil
}

// applyRegistryEdit brings content_type_fields in line with the new shape,
// INSIDE the rebuild transaction.
//
// The carried rows are UPDATED, not deleted and re-inserted, so a field keeps
// its identity across edits (a client that fetched the ids can edit twice in a
// row without re-reading them).
//
// THE TWO-PHASE PARK is not decoration: content_type_fields carries
// UNIQUE(content_type_id, name) and UNIQUE(content_type_id, ordinal), which
// SQLite enforces per STATEMENT. A cross-rename (`a`→`b` and `b`→`a`), or any
// reordering, would violate one of them halfway through a naive row-by-row
// update. So every carried row is first parked on a value that provably cannot
// collide (a name derived from its own uuid; a negative ordinal, which no real
// field ever has) and only then set to its final value.
//
// CONTRACT-21 T1 — THE PARK IS NOW LOAD-BEARING ON BOTH ENGINES, AND MORE SO ON
// PostgreSQL. PostgreSQL also enforces a UNIQUE index per statement (its
// constraints are not DEFERRABLE unless declared so, and the canonical schema
// does not declare them that way), so the same halfway violation happens there.
// The difference that matters is the consequence: on SQLite the offending
// statement fails and the transaction survives, while on PostgreSQL the
// transaction is poisoned and the rebuild — already past its DDL — could only be
// abandoned. Parking makes the violation unreachable on both, which is why it is
// kept exactly as it was rather than traded for a deferred-constraint trick that
// only one engine can express.
func applyRegistryEdit(ctx context.Context, engine compat.Engine, tx *sql.Tx, plan ContentTypeEdit) error {
	keep := make(map[string]struct{}, len(plan.Carried))
	for _, c := range plan.Carried {
		keep[c.ID] = struct{}{}
	}
	// Removals first: their names and ordinals become free for the survivors.
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM `+schema.ContentTypeFieldsTable+` WHERE content_type_id = `+dual.Bind(engine, 1), plan.TypeID)
	if err != nil {
		return fmt.Errorf("read field ids of content type %q: %w", plan.TypeName, err)
	}
	var obsolete []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan field id: %w", err)
		}
		if _, ok := keep[id]; !ok {
			obsolete = append(obsolete, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	deleteField := `DELETE FROM ` + schema.ContentTypeFieldsTable + ` WHERE id = ` + dual.Bind(engine, 1)
	for _, id := range obsolete {
		if _, err := tx.ExecContext(ctx, deleteField, id); err != nil {
			return fmt.Errorf("delete removed field of content type %q: %w", plan.TypeName, err)
		}
	}
	setNameOrdinal := `UPDATE ` + schema.ContentTypeFieldsTable +
		` SET name = ` + dual.Bind(engine, 1) + `, ordinal = ` + dual.Bind(engine, 2) +
		` WHERE id = ` + dual.Bind(engine, 3)
	// Phase 1: park every survivor on a collision-free name and ordinal.
	for i, c := range plan.Carried {
		if _, err := tx.ExecContext(ctx, setNameOrdinal,
			"__parked_"+c.ID, -1-i, c.ID); err != nil {
			return fmt.Errorf("park field %q of content type %q: %w", c.OldName, plan.TypeName, err)
		}
	}
	// Phase 2: the final names and ordinals. Positions are the positions of the
	// NEW definition, so the physical column order and the registry agree.
	ordinalOf := make(map[string]int, len(plan.New.Fields))
	for i, f := range plan.New.Fields {
		ordinalOf[f.Name] = i
	}
	for _, c := range plan.Carried {
		if _, err := tx.ExecContext(ctx, setNameOrdinal,
			c.NewName, ordinalOf[c.NewName], c.ID); err != nil {
			return fmt.Errorf("rename field %q of content type %q: %w", c.OldName, plan.TypeName, err)
		}
	}
	// The new fields last: their ordinals are exactly the positions no survivor
	// occupies, so neither unique constraint can be violated.
	carriedNames := make(map[string]struct{}, len(plan.Carried))
	for _, c := range plan.Carried {
		carriedNames[c.NewName] = struct{}{}
	}
	insertField := `INSERT INTO ` + schema.ContentTypeFieldsTable +
		` (id, content_type_id, name, field_type, ordinal) VALUES (` +
		dual.Bind(engine, 1) + `, ` + dual.Bind(engine, 2) + `, ` + dual.Bind(engine, 3) + `, ` +
		dual.Bind(engine, 4) + `, ` + dual.Bind(engine, 5) + `)`
	for i, f := range plan.New.Fields {
		if _, ok := carriedNames[f.Name]; ok {
			continue
		}
		fieldID, err := dual.NewUUID()
		if err != nil {
			return fmt.Errorf("generate id for new field %q of content type %q: %w", f.Name, plan.TypeName, err)
		}
		if _, err := tx.ExecContext(ctx, insertField,
			fieldID, plan.TypeID, f.Name, string(f.Type), i); err != nil {
			return fmt.Errorf("insert new field %q of content type %q: %w", f.Name, plan.TypeName, err)
		}
	}
	return nil
}
