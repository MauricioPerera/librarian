package store

// relations.go implements the persistence half of CONTRACT-27: reading the
// declared relations back out of the registry, and THE GUARD (T3) that turns
// this contract's accepted price into a legible refusal.
//
// THE PRICE, RESTATED WHERE IT IS PAID. compat never emits CASCADE, and editing
// the fields of a type works by REBUILDING its table (create staging, copy, DROP
// the original, recreate — contenttypes_edit.go). Both operations therefore die
// on a table that something else references, because dropping it is exactly what
// a foreign key exists to forbid. That limit is the deal, not a defect to hide:
// a type that is the TARGET of a reference cannot be edited nor deleted while
// the reference exists.
//
// What this file guarantees is that the limit is met THE RIGHT WAY. Without it
// the admin gets `SQLSTATE 23503` (or `FOREIGN KEY constraint failed`) from the
// middle of a transaction — an error that names an engine-chosen constraint, not
// a content type, and that arrives after the operation has already begun. With
// it, the operation is refused BEFORE anything is touched, naming which type
// holds the reference, through which column, and what to do to free it.

import (
	"context"
	"fmt"
	"strings"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// Referrer is ONE relation pointing AT a content type: which type declares it
// and through which column.
type Referrer struct {
	// TypeName is the public name of the type that DECLARES the reference.
	TypeName string
	// ReferenceName is the column that carries the foreign key.
	ReferenceName string
}

// ReferencedTypeError is the REFUSAL of T3: an operation that would have to drop
// the type's real table, on a type something else points at.
//
// It carries the referrers rather than a formatted string so the HTTP and HTML
// layers can render the same fact their own way, and so a caller can act on it
// (the panel lists the referring types as links). Operation is the verb of the
// refused action ("edit", "delete"), because the two are refused for the same
// reason but are fixed by the admin in the same single way, and a message that
// says "cannot be deleted" for an edit would be worse than useless.
type ReferencedTypeError struct {
	TypeName  string
	Operation string
	Referrers []Referrer
}

func (e ReferencedTypeError) Error() string {
	parts := make([]string, 0, len(e.Referrers))
	names := make([]string, 0, len(e.Referrers))
	for _, r := range e.Referrers {
		parts = append(parts, fmt.Sprintf("%q (through its reference %q)", r.TypeName, r.ReferenceName))
		names = append(names, r.TypeName)
	}
	return fmt.Sprintf(
		"the content type %q cannot be %s because %s references it: %s. "+
			"Nothing was done. %s rebuilds or drops the real table %q, and a foreign key exists precisely to forbid that while a referring table lives — "+
			"this project never emits ON DELETE CASCADE, so the reference is not removed silently. "+
			"A reference is declared when a content type is created and cannot be detached afterwards, so to free %q you must DELETE the content type(s) %s and then retry",
		e.TypeName, e.pastTense(), e.subject(), strings.Join(parts, ", "),
		e.verbPhrase(), schema.DynamicTableName(e.TypeName),
		e.TypeName, strings.Join(quoteAll(names), ", "))
}

// pastTense / subject / verbPhrase keep the single message above readable for
// both operations without duplicating it.
func (e ReferencedTypeError) pastTense() string {
	if e.Operation == "delete" {
		return "deleted"
	}
	return "edited"
}

func (e ReferencedTypeError) subject() string {
	if len(e.Referrers) == 1 {
		return "another content type"
	}
	return fmt.Sprintf("%d other content types", len(e.Referrers))
}

func (e ReferencedTypeError) verbPhrase() string {
	if e.Operation == "delete" {
		return "Deleting it"
	}
	return "Editing its fields"
}

func quoteAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, fmt.Sprintf("%q", n))
	}
	return out
}

// referencesTablePresent reports whether the CONTRACT-27 registry table
// physically exists.
//
// IT IS NOT OPTIONAL POLITENESS, IT IS THE UPGRADE PATH. LoadDefinitions runs
// INSIDE EnsureSchema (through CanonicalSchemaFor) — that is, BEFORE the missing
// tables have been created. On the very first boot of this binary against an
// already-deployed database, content_type_references does not exist yet, so a
// query against it would fail and the service would refuse to start on every
// installation in the world. The probe answers "provably zero relations" for
// exactly the same reason registryPresent answers "provably zero dynamic types":
// a relation can only be persisted in that table.
//
// It asks the ENGINE'S catalog through compat.Store.TableExists, never
// __compat_schema, for the reason that primitive documents and this project has
// repeated since CONTRACT-13: the metadata is a record of what some process
// believed, and EnsureSchema is about to rewrite it.
func referencesTablePresent(ctx context.Context, store *compat.Store) (bool, error) {
	present, err := store.TableExists(ctx, schema.ContentTypeReferencesTable)
	if err != nil {
		return false, fmt.Errorf("look up %s in the engine catalog: %w", schema.ContentTypeReferencesTable, err)
	}
	return present, nil
}

// loadReferencesByType returns every declared relation, grouped by the PUBLIC
// name of the type that declares it, in ordinal order.
//
// The two joins back onto content_types are what turn the stored uuids into the
// public names the rest of the project speaks. ORDER BY ordinal is an INTEGER
// key, so it needs no Go-side reordering: the collation divergence CONTRACT-20
// measured is a property of TEXT comparison only. The order of the TYPES is
// irrelevant here — the result is a map — and the order of the composed schema
// is imposed by schema.sortDefinitionsByDependency.
func loadReferencesByType(ctx context.Context, store *compat.Store) (map[string][]schema.ReferenceDefinition, error) {
	present, err := referencesTablePresent(ctx, store)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	rows, err := store.DB.QueryContext(ctx,
		`SELECT src.name, r.name, tgt.name
		   FROM `+schema.ContentTypeReferencesTable+` r
		   JOIN `+schema.ContentTypesTable+` src ON src.id = r.content_type_id
		   JOIN `+schema.ContentTypesTable+` tgt ON tgt.id = r.target_type_id
		  ORDER BY r.ordinal`)
	if err != nil {
		return nil, fmt.Errorf("query content type references: %w", err)
	}
	defer rows.Close()
	out := map[string][]schema.ReferenceDefinition{}
	for rows.Next() {
		var owner, name, target string
		if err := rows.Scan(&owner, &name, &target); err != nil {
			return nil, fmt.Errorf("scan content type reference: %w", err)
		}
		out[owner] = append(out[owner], schema.ReferenceDefinition{Name: name, Target: target})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate content type references: %w", err)
	}
	return out, nil
}

// LoadContentTypeReferences returns the relations declared by ONE content type,
// identified by its registry id, in ordinal order.
//
// It is the read half the EDIT path needs: a rebuild must re-emit the FK columns
// the type already has, and an edit plan must validate the new field names
// against them (a new field called like an existing reference would otherwise
// become a duplicate column at apply time).
func LoadContentTypeReferences(ctx context.Context, store *compat.Store, typeID string) ([]schema.ReferenceDefinition, error) {
	present, err := referencesTablePresent(ctx, store)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	rows, err := store.DB.QueryContext(ctx,
		`SELECT r.name, tgt.name
		   FROM `+schema.ContentTypeReferencesTable+` r
		   JOIN `+schema.ContentTypesTable+` tgt ON tgt.id = r.target_type_id
		  WHERE r.content_type_id = `+dual.Bind(store.Target.Engine, 1)+`
		  ORDER BY r.ordinal`, typeID)
	if err != nil {
		return nil, fmt.Errorf("query references of content type %q: %w", typeID, err)
	}
	defer rows.Close()
	var out []schema.ReferenceDefinition
	for rows.Next() {
		var ref schema.ReferenceDefinition
		if err := rows.Scan(&ref.Name, &ref.Target); err != nil {
			return nil, fmt.Errorf("scan reference of content type %q: %w", typeID, err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate references of content type %q: %w", typeID, err)
	}
	return out, nil
}

// ReferencesTo returns every relation pointing AT a content type, i.e. exactly
// the reasons its table cannot be dropped. An empty result means the type is
// free to be edited and deleted.
//
// The name is bound as a parameter, never interpolated. The result is ordered
// byte-wise in Go by (type name, reference name) through dual.SortByKeys, for
// the CONTRACT-20 reason: PostgreSQL orders TEXT by the database collation and
// SQLite by bytes, and these names go into an ERROR MESSAGE and into a panel
// page, which the dual-engine tests compare.
func ReferencesTo(ctx context.Context, store *compat.Store, typeName string) ([]Referrer, error) {
	present, err := referencesTablePresent(ctx, store)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	rows, err := store.DB.QueryContext(ctx,
		`SELECT src.name, r.name
		   FROM `+schema.ContentTypeReferencesTable+` r
		   JOIN `+schema.ContentTypesTable+` src ON src.id = r.content_type_id
		   JOIN `+schema.ContentTypesTable+` tgt ON tgt.id = r.target_type_id
		  WHERE tgt.name = `+dual.Bind(store.Target.Engine, 1), typeName)
	if err != nil {
		return nil, fmt.Errorf("query the references to content type %q: %w", typeName, err)
	}
	defer rows.Close()
	var out []Referrer
	for rows.Next() {
		var r Referrer
		if err := rows.Scan(&r.TypeName, &r.ReferenceName); err != nil {
			return nil, fmt.Errorf("scan a reference to content type %q: %w", typeName, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate the references to content type %q: %w", typeName, err)
	}
	dual.SortByKeys(out, func(r Referrer) []dual.Key {
		return dual.Ascending(r.TypeName, r.ReferenceName)
	})
	return out, nil
}

// contentTypeIDsByName resolves the registry ids of the TARGETS of a set of
// declared references. It returns an error for a target that is not in the
// registry — the same refusal CreateContentType's step 2b produces, repeated
// here as a fail-closed guard so no caller can reach the INSERT with a target it
// never verified.
//
// It resolves only the names it needs, one bound lookup each, rather than
// loading the whole registry: a definition declares a handful of references at
// most, and binding the name keeps it a value rather than SQL text.
func contentTypeIDsByName(ctx context.Context, store *compat.Store, references []schema.ReferenceDefinition) (map[string]string, error) {
	if len(references) == 0 {
		return nil, nil
	}
	engine := store.Target.Engine
	out := make(map[string]string, len(references))
	for _, r := range references {
		if _, done := out[r.Target]; done {
			continue
		}
		var id string
		err := store.DB.QueryRowContext(ctx,
			`SELECT id FROM `+schema.ContentTypesTable+` WHERE name = `+dual.Bind(engine, 1), r.Target).Scan(&id)
		if err != nil {
			return nil, fmt.Errorf("%w: could not resolve the target %q of the reference %q: %w", ErrUnknownReferenceTarget, r.Target, r.Name, err)
		}
		out[r.Target] = id
	}
	return out, nil
}

// insertReferenceRows writes the relation half of a definition INSIDE the
// caller's transaction. ordinal is the declaration position, so the stored order
// and the physical column order of the composed table agree — the same contract
// content_type_fields.ordinal has.
//
// The ids are generated here rather than read back with RETURNING, the
// resolution CONTRACT-19/20/21 gave every insert in this project: compat
// deliberately does not implement RETURNING, and nothing needs the id back.
func insertReferenceRows(ctx context.Context, engine compat.Engine, tx execer, typeID string, def schema.ContentTypeDefinition, targetIDs map[string]string) error {
	statement := `INSERT INTO ` + schema.ContentTypeReferencesTable +
		` (id, content_type_id, target_type_id, name, ordinal) VALUES (` +
		dual.Bind(engine, 1) + `, ` + dual.Bind(engine, 2) + `, ` + dual.Bind(engine, 3) + `, ` +
		dual.Bind(engine, 4) + `, ` + dual.Bind(engine, 5) + `)`
	for i, r := range def.References {
		targetID, ok := targetIDs[r.Target]
		if !ok {
			return fmt.Errorf("%w: %q (declared by the reference %q of content type %q)", ErrUnknownReferenceTarget, r.Target, r.Name, def.Name)
		}
		refID, err := dual.NewUUID()
		if err != nil {
			return fmt.Errorf("generate id for reference %q of content type %q: %w", r.Name, def.Name, err)
		}
		if _, err := tx.ExecContext(ctx, statement, refID, typeID, targetID, r.Name, i); err != nil {
			return fmt.Errorf("insert reference %q of content type %q: %w", r.Name, def.Name, err)
		}
	}
	return nil
}

// guardNotReferenced is THE T3 GUARD: it refuses an operation that would rebuild
// or drop a type's real table while something references it.
//
// IT RUNS BEFORE ANYTHING IS TOUCHED, and that placement is the contract. Both
// callers invoke it outside their transaction, on a pure read, so the refusal
// provably wrote nothing — the same "validation errors must not touch the
// database" property PlanContentTypeEdit has by being pure.
//
// THE RACE IT DOES NOT WIN, stated rather than papered over: a reference could
// be declared between this check and the operation's transaction. The database
// still wins that one — the DROP fails on the real foreign key and the whole
// transaction rolls back — so the outcome is a rejected operation either way;
// only the QUALITY of the message degrades in that window. In practice the HTTP
// layer serializes every schema-mutating request through h.schemaMu, so the
// window does not exist within one process.
func guardNotReferenced(ctx context.Context, store *compat.Store, typeName, operation string) error {
	referrers, err := ReferencesTo(ctx, store, typeName)
	if err != nil {
		return err
	}
	if len(referrers) == 0 {
		return nil
	}
	return ReferencedTypeError{TypeName: typeName, Operation: operation, Referrers: referrers}
}
