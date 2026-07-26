package schema

// relations.go implements CONTRACT-27 T1: a RELATION (a real foreign key)
// between two DYNAMIC content types.
//
// WHY A REFERENCE IS NOT A FIELD TYPE, AND WHY IT LIVES IN ITS OWN TABLE.
// The obvious shape — one more value of FieldType, plus a `target` column on
// content_type_fields — is unreachable on an installation that already exists,
// and it fails in the worst possible place:
//
//   - content_type_fields.field_type carries a schema-level CHECK pinned to
//     exactly FieldTypeNames() (see contentTypeFieldsTable). Adding a value to
//     the vocabulary means ALTERING that CHECK on a table that already exists,
//     and EnsureSchema only ever creates MISSING tables — deliberately, because
//     that restriction is what makes a restart safe. The old CHECK would survive
//     the upgrade and reject the new value AT INSERT TIME: a runtime 500 on the
//     first admin who declares a relation, not a failure at boot where it would
//     be found.
//   - Adding a `target_type_id` COLUMN to that table hits the identical wall for
//     the identical reason.
//
// A NEW table is purely ADDITIVE: EnsureSchema creates it on a fresh database
// and on a deployed one alike, with no migration and no altered constraint. So a
// reference is a SIBLING concept to a scalar field, not a kind of field — the
// composed table takes its scalar columns from the fields and its FK columns
// from the references.
//
// THE PRICE, ACCEPTED UP FRONT (see the contract, "El precio, decidido de
// antemano"): compat never emits CASCADE and a field edit works by REBUILDING
// the table (create staging, copy, DROP the original, recreate). A type that is
// the TARGET of a reference therefore cannot be edited or deleted while that
// reference exists. This package's job is not to dodge that; store/relations.go
// is where it is turned into a checked, legible refusal instead of a raw
// SQLSTATE 23503 from the middle of a transaction.

import (
	"fmt"
	"sort"

	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// ContentTypeReferencesTable holds one row per RELATION declared by a dynamic
// content type. It is the third registry table (after content_types and
// content_type_fields) and the only one CONTRACT-27 adds — see the file header
// for why it is a new table rather than a column on an existing one.
const ContentTypeReferencesTable = "content_type_references"

// ReferenceDefinition is ONE declared relation of a dynamic content type: the
// name of the column it produces, and the PUBLIC name of the dynamic content
// type it points at.
//
// Target is the public name (`autores`), never the real table (`cpt_autores`):
// every surface of this project — routes, payloads, the panel — speaks public
// names, and DynamicTableName is the single place the two are bridged.
type ReferenceDefinition struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

// validateReferences applies the T1 gate to the relation half of a definition.
// It is called from ContentTypeDefinition.Validate with the set of names already
// taken by the fields, so a reference cannot silently shadow a field (which
// would surface as an opaque duplicate-column error from compat's
// Schema.Validate at apply time instead of a clean 400 at request time).
//
// What it CANNOT check, by construction: that the target type EXISTS. Validate
// is pure and sees one definition; existence is a fact about the registry, so it
// is checked by store.CreateContentType (a friendly, explicit refusal) and again
// structurally by BuildWithFor, which cannot order a table before a target it
// does not have.
func validateReferences(typeName string, references []ReferenceDefinition, taken map[string]struct{}) error {
	for _, r := range references {
		if err := ValidateIdentifier(r.Name); err != nil {
			return fmt.Errorf("reference %w", err)
		}
		if _, dup := taken[r.Name]; dup {
			return fmt.Errorf("reference %q collides with another field or reference of the same content type", r.Name)
		}
		taken[r.Name] = struct{}{}
		if err := ValidateTypeName(r.Target); err != nil {
			return fmt.Errorf("reference %q target %w", r.Name, err)
		}
		// DECIDED BY THE CONTRACT, and it is a LIMITATION rather than a mistake by
		// whoever asked for it: a type that points at itself would need its own
		// table to exist before its own foreign key can be created, and this
		// project creates a dynamic table in ONE statement inside ONE transaction
		// (there is no ALTER TABLE in compat to add the constraint afterwards). The
		// message says so explicitly instead of blaming the request.
		if r.Target == typeName {
			return fmt.Errorf("reference %q points at %q itself: a content type cannot reference itself in v1. This is a known limitation, not a mistake in your request: the foreign key would have to be created before its own table exists, and the schema layer creates a dynamic table in a single statement (compat has no ALTER TABLE to add the constraint afterwards)", r.Name, typeName)
		}
	}
	return nil
}

// ReadSchemaFor composes the schema used to EXECUTE the two read routines of ONE
// dynamic content type (internal/server's generic CRUD layer).
//
// WHY IT IS NOT JUST BuildWithFor(caps, []{def}). A composed schema must contain
// the target of every foreign key it declares — sortDefinitionsByDependency
// refuses a dangling one, and it is right to: an unappliable schema is exactly
// the silent export break this contract set out to prevent. But a READ of
// `libros` needs `libros` declared and nothing else; loading every definition in
// the instance on every list and every detail request would put a registry query
// on the hot path of the whole content API.
//
// So the targets are added as PLACEHOLDER definitions: the target's name with no
// fields. That is enough to satisfy the foreign key (the FK names a TABLE, not a
// column set) and it is honest about what it is — this schema is never applied,
// never written to __compat_schema and never dumped; it is the argument of
// QueryRoutine, whose only job is to resolve the routine being executed and the
// relation it reads. Every path that DOES apply or export a schema
// (store.CanonicalSchemaFor, store's three writers, `--dump-schema`) composes
// from the FULL set of persisted definitions and is unaffected by this function.
func ReadSchemaFor(caps Capabilities, def ContentTypeDefinition) (compat.Schema, error) {
	defs := []ContentTypeDefinition{def}
	added := map[string]struct{}{def.Name: {}}
	for _, r := range def.References {
		if _, done := added[r.Target]; done {
			continue
		}
		added[r.Target] = struct{}{}
		defs = append(defs, ContentTypeDefinition{Name: r.Target})
	}
	return BuildWithFor(caps, defs)
}

// sortDefinitionsByDependency returns the definitions in an order in which every
// definition comes AFTER every definition it references.
//
// THIS IS THE RED-TEAM ITEM THAT BREAKS THE EXPORT IN SILENCE, and it is the
// reason this function exists at all. BuildWithFor used to sort the dynamic
// definitions by NAME, which was a total, deterministic order and therefore
// correct while the only foreign key a dynamic table carried was author_id →
// users (a CODE table, always emitted first by BuildFor). With a relation
// between two DYNAMIC types that is no longer enough: `a` referencing `z` sorts
// BEFORE its own target, so
//
//   - `--dump-schema` emits the tables in that order, and applying the exported
//     schema on the destination fails on `a` — the FK names a relation that does
//     not exist yet. The export compiles, validates and looks right; it just
//     cannot be applied. That is exactly the silent break the contract points at.
//   - the same order drives missingTables → ApplySchema, so a from-scratch
//     re-creation of an instance would fail in the same place.
//
// The order is a topological sort with an ALPHABETICAL tie-break: the input is
// name-sorted first and, at every step, every definition whose targets are
// already emitted is emitted in that name order. Two consequences matter:
//
//   - With NO references (every installation before this contract) the result is
//     byte-identical to the old name sort, so no existing dump changes.
//   - The result is deterministic and independent of the order the rows came
//     back from the database — the property CONTRACT-21 established for the
//     canonical schema and which makes two instances comparable.
//
// A target that is not among the definitions is a HARD ERROR, never a skipped
// table: compat's Schema.Validate does NOT check that an FK target exists
// (verified reading its source), so a dangling reference would compile happily
// and die at CREATE TABLE. A CYCLE is likewise a hard error. Neither is
// reachable through the API — a reference can only be declared at creation, and
// the target must already exist, so the graph is acyclic by construction — but
// the registry is a table, and a hand-written row must fail loudly rather than
// hang this function or emit an unappliable schema.
func sortDefinitionsByDependency(defs []ContentTypeDefinition) ([]ContentTypeDefinition, error) {
	sorted := make([]ContentTypeDefinition, len(defs))
	copy(sorted, defs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	known := make(map[string]struct{}, len(sorted))
	for _, d := range sorted {
		known[d.Name] = struct{}{}
	}
	out := make([]ContentTypeDefinition, 0, len(sorted))
	emitted := make(map[string]struct{}, len(sorted))
	for len(out) < len(sorted) {
		progress := false
		for _, d := range sorted {
			if _, done := emitted[d.Name]; done {
				continue
			}
			ready := true
			for _, r := range d.References {
				if _, ok := known[r.Target]; !ok {
					return nil, fmt.Errorf("content type %q declares a reference %q to the content type %q, which is not among the definitions of this schema; a foreign key to a table that is never created cannot be applied", d.Name, r.Name, r.Target)
				}
				if _, ok := emitted[r.Target]; !ok {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			out = append(out, d)
			emitted[d.Name] = struct{}{}
			progress = true
		}
		if !progress {
			var stuck []string
			for _, d := range sorted {
				if _, done := emitted[d.Name]; !done {
					stuck = append(stuck, d.Name)
				}
			}
			return nil, fmt.Errorf("the references between the content types %v form a cycle, so no order creates every foreign key target before its referrer; a cycle cannot be produced through the API (a reference is declared at creation and its target must already exist), so these registry rows were written outside it", stuck)
		}
	}
	return out, nil
}
