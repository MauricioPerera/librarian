package schema

// dynamic.go implements CONTRACT-13 T1/T2: the Go model of a RUNTIME-defined
// content type (a "dynamic CPT"), and the composition of the canonical schema
// as CODE + DYNAMIC.
//
// A dynamic content type is DATA (rows in content_types / content_type_fields),
// but the table it produces is built through the very same ContentType() helper
// the code-defined types use — its signature is NOT touched. A dynamic type is
// therefore structurally indistinguishable from articles/products: the same
// id / author_id FK / own columns / created_at / updated_at / metadata shape.

import (
	"fmt"
	"strings"

	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// Registry table names. They are code tables in Build(); since CONTRACT-17 a
// dynamic type named "content_types" is harmless (it produces the table
// "cpt_content_types"), because every dynamic table carries
// DynamicTablePrefix and no code table may.
const (
	// ContentTypesTable holds one row per dynamic content type.
	ContentTypesTable = "content_types"
	// ContentTypeFieldsTable holds one row per field of a dynamic content type.
	ContentTypeFieldsTable = "content_type_fields"
	// ContentTypeFoldsTable holds one row per dynamic content type whose real
	// table carries SearchFoldColumn (CONTRACT-34). A type with no row there does
	// not have the column, and its search stays case-sensitive.
	ContentTypeFoldsTable = "content_type_folds"
)

// FieldType is the closed set of scalar field types a dynamic content type may
// use in v1 (DEFINITION-CPT-DINAMICOS.md). Deliberately NOT included: json,
// vector(N), domains, generated columns and relations (FK) — those stay
// reserved for code-defined content types, where a developer understands the
// implications (e.g. vector(N) requires pgvector on the export destination).
type FieldType string

// The five allowed field types. The string values are the public API vocabulary
// AND the values stored in content_type_fields.field_type AND the values of the
// schema-level CHECK constraint on that column — one vocabulary, no mapping
// table to drift.
const (
	FieldText    FieldType = "text"
	FieldInteger FieldType = "integer"
	FieldDecimal FieldType = "decimal"
	FieldBoolean FieldType = "boolean"
	FieldDate    FieldType = "date"
)

// FieldTypes is the ordered catalog of allowed field types. It drives both the
// CHECK constraint in Build() and the API validation, so the two can never
// disagree.
var FieldTypes = []FieldType{FieldText, FieldInteger, FieldDecimal, FieldBoolean, FieldDate}

// family maps a FieldType to its compat type family. ok=false for anything
// outside the closed set (including the zero value).
func (f FieldType) family() (compat.TypeFamily, bool) {
	switch f {
	case FieldText:
		return compat.TextType, true
	case FieldInteger:
		return compat.IntegerType, true
	case FieldDecimal:
		return compat.DecimalType, true
	case FieldBoolean:
		return compat.BooleanType, true
	case FieldDate:
		return compat.DateType, true
	default:
		return "", false
	}
}

// FieldTypeNames returns the allowed field types as plain strings (for error
// messages and for the CHECK constraint).
func FieldTypeNames() []string {
	out := make([]string, 0, len(FieldTypes))
	for _, f := range FieldTypes {
		out = append(out, string(f))
	}
	return out
}

// FieldDefinition is one field of a dynamic content type. Order matters (it is
// persisted as content_type_fields.ordinal) so the produced table has a stable,
// reproducible column order — required for a byte-exact schema round-trip.
type FieldDefinition struct {
	Name string    `json:"name"`
	Type FieldType `json:"type"`
}

// ContentTypeDefinition is a complete runtime content-type definition: its
// table name plus its ordered own columns. Everything else (id, author_id,
// timestamps, metadata) is supplied by ContentType().
//
// CONTRACT-27 adds References, the SIBLING of Fields: the scalar columns come
// from Fields and the foreign-key columns come from References. It is
// `omitempty` so every JSON artifact produced before this contract — the
// `--dump-schema` output of an installation with no relations, the API
// responses — is byte-identical to what it was.
//
// CONTRACT-34 adds Folded, which is NOT a declaration an admin writes: it says
// whether this type's real table CARRIES the system column SearchFoldColumn, and
// its only source of truth is the registry table ContentTypeFoldsTable, written
// in the SAME transaction that creates the table. It is part of the definition
// because the definition is what composes the table AND its routines, and those
// two must agree with the physical column set exactly — a definition claiming a
// column the table does not have makes every read of that type fail.
//
// It is `omitempty` for the same reason References is: every artifact produced
// before this contract — the `--dump-schema` output, the API responses of an
// installation whose types predate it — stays byte-identical.
type ContentTypeDefinition struct {
	Name       string                `json:"name"`
	Fields     []FieldDefinition     `json:"fields"`
	References []ReferenceDefinition `json:"references,omitempty"`
	Folded     bool                  `json:"folded,omitempty"`
}

// FoldSearchText is THE fold, and there is exactly one of it on purpose: the
// value written into SearchFoldColumn and the needle bound into the search WHERE
// both go through this function, so the two can never disagree about what
// "folded" means. A second spelling anywhere would make a search silently miss
// rows whose fold was computed by the other one.
//
// strings.ToLower and nothing else. It folds the whole Unicode range (`Ñ`→`ñ`,
// `Ú`→`ú`), which is exactly what `like` used to do on only one engine, and it
// does it IN GO — identical on SQLite and PostgreSQL by construction, because
// neither engine is involved. It deliberately does NOT strip accents: `nandu`
// still does not find `ñandú`. Accent folding is a different, larger decision
// about the whole panel and is not this contract's to take.
func FoldSearchText(s string) string {
	return strings.ToLower(s)
}

// Validate applies the T1 identifier gate to the type name and to every field
// name, checks each field type is in the closed set, and rejects duplicate
// field names WITHIN the definition (which would otherwise surface as an opaque
// duplicate-column error from compat's Schema.Validate at apply time). A type
// with zero fields is allowed: it still produces a usable table (id, author,
// timestamps, metadata) and there is no principled reason to forbid it.
//
// Uniqueness of the type name ACROSS definitions is NOT checked here — that is
// a schema-level UNIQUE(name) constraint on content_types (see Build()), which
// is the only guarantee that cannot lose a concurrent race.
func (d ContentTypeDefinition) Validate() error {
	if err := ValidateTypeName(d.Name); err != nil {
		return fmt.Errorf("content type %w", err)
	}
	seen := make(map[string]struct{}, len(d.Fields))
	for _, f := range d.Fields {
		if err := ValidateIdentifier(f.Name); err != nil {
			return fmt.Errorf("field %w", err)
		}
		if _, ok := f.Type.family(); !ok {
			return fmt.Errorf("field %q has unknown type %q (allowed: %v)", f.Name, string(f.Type), FieldTypeNames())
		}
		if _, dup := seen[f.Name]; dup {
			return fmt.Errorf("field %q is declared more than once", f.Name)
		}
		seen[f.Name] = struct{}{}
	}
	// CONTRACT-27: the relation half, gated with the SAME identifier rules and
	// against the SAME name set, so a reference can neither shadow a field nor an
	// injected column of ContentType(). See relations.go.
	return validateReferences(d.Name, d.References, seen)
}

// TableName is the REAL name of the table backing this definition: the public
// name with DynamicTablePrefix applied (CONTRACT-17). Every layer that needs a
// table name — the generic CRUD layer, the store's diff step — calls this
// instead of using d.Name, and it delegates to the single DynamicTableName
// helper, so the derivation lives in exactly one place.
//
// d.Name stays the PUBLIC name: routes, API payloads and the UI never see this.
func (d ContentTypeDefinition) TableName() string {
	return DynamicTableName(d.Name)
}

// DynamicTable turns a validated definition into the real compat.Table, through
// the SAME ContentType() helper articles/products use (its signature is
// untouched: the prefixed name is simply what is passed in). It re-validates
// first: this function is the ONLY way a dynamic name reaches a compat.Table,
// so the gate cannot be bypassed by a caller that forgot to validate.
func DynamicTable(d ContentTypeDefinition) (compat.Table, error) {
	if err := d.Validate(); err != nil {
		return compat.Table{}, err
	}
	own := make([]compat.Column, 0, len(d.Fields))
	for _, f := range d.Fields {
		family, ok := f.Type.family()
		if !ok { // unreachable after Validate; kept as a hard fail-closed guard.
			return compat.Table{}, fmt.Errorf("field %q has unknown type %q", f.Name, string(f.Type))
		}
		// Every dynamic column is NULLABLE. A dynamic type is created AFTER the
		// fact by an admin and its rows are written by the generic CRUD layer;
		// there is no per-field "required" concept in v1 (DEFINITION-CPT-
		// DINAMICOS.md lists only name + type), and a NOT NULL column with no
		// default would make every insert that omits it fail with an opaque
		// constraint error. Nullable is the honest default for a v1 that cannot
		// ALTER TABLE later.
		own = append(own, compat.Column{
			Name:     f.Name,
			Type:     compat.Type{Family: family},
			Nullable: true,
		})
	}
	// CONTRACT-27: one nullable uuid column per declared relation, AFTER the
	// scalar fields so the physical column order is (fields, then references) —
	// stable, reproducible, and the order every copy statement of a rebuild uses.
	//
	// NULLABLE, and that is the contract's decision: a relation is OPTIONAL, like
	// every other dynamic column. A NOT NULL foreign key with no default would
	// make every insert that omits it fail with an opaque constraint error, and
	// the definition model has no notion of "required" to express the choice.
	for _, r := range d.References {
		own = append(own, compat.Column{
			Name:     r.Name,
			Type:     compat.Type{Family: compat.UUIDType},
			Nullable: true,
		})
	}
	// CONTRACT-34: the folded search column, LAST of the own columns, and only
	// for a type whose registry says it has one (see ContentTypeFoldsTable). It is
	// appended after the references so that adding it changes nothing about the
	// physical position of any column an admin declared — a type created before
	// this contract and one created after it differ by an appended column and
	// nothing else.
	//
	// It is TEXT and NULLABLE unconditionally, INCLUDING for a type whose first
	// field is not text and which therefore has nothing to fold (see
	// DynamicSearchField). Making its presence depend on searchability would tie
	// the physical shape of a table to a property an EDIT can flip — renaming or
	// reordering fields can make a type searchable or stop it being so — and every
	// such flip would then require a rebuild. Presence depends only on Folded; what
	// the column CONTAINS (a folded value, or NULL) is what depends on the fields.
	if d.Folded {
		own = append(own, compat.Column{
			Name:     SearchFoldColumn,
			Type:     compat.Type{Family: compat.TextType},
			Nullable: true,
		})
	}
	table := ContentType(d.TableName(), own)
	// The FKs are appended after ContentType returns, exactly the way
	// productsTable appends its UNIQUE(sku): ContentType's signature takes a name
	// and own columns and is NOT touched. The target is the REAL table name of the
	// target type (DynamicTableName), never its public name.
	for _, r := range d.References {
		table.Constraints = append(table.Constraints, foreignKeyRestrict(r.Name, DynamicTableName(r.Target), "id"))
	}
	return table, nil
}

// BuildWith returns THE canonical schema of a running librarian instance:
// Build() (the code-defined tables) PLUS one table per persisted dynamic
// definition. This is the function that must be used everywhere the "complete
// schema" is meant — store.EnsureSchema, the --dump-schema artifact consumed by
// `compat copy`, and the compat.InferFeatures audit contract. Build() alone is
// NO LONGER the canonical schema of a live instance; it is only the code half.
//
// Dynamic tables are appended AFTER every code table (their author_id FK
// references users, which Build() declares first — required for FK creation on
// both engines) and sorted by name, so the composed schema is deterministic
// regardless of the order rows came back from the database.
//
// An invalid definition (which can only happen if the registry rows were
// tampered with outside the API) is a hard error: composing a schema that
// silently drops a persisted type is the one outcome this contract forbids.
// CONTRACT-23 T1: BuildWith is BuildWithFor(Capabilities{}, defs) — every
// optional capability enabled, the schema of every installation deployed before
// this contract. Only the callers that KNOW this installation's declaration (the
// startup path, the --dump-schema path and the content-type writers, which read
// it back from the physical table) use BuildWithFor.
func BuildWith(defs []ContentTypeDefinition) (compat.Schema, error) {
	return BuildWithFor(Capabilities{}, defs)
}

// BuildWithFor is BuildWith for a declared set of capabilities. The dynamic half
// is unaffected by caps: a dynamic content type has no vector field (the field
// types are text/integer/decimal/boolean/date — see FieldType), so the choice
// only ever changes the code half.
func BuildWithFor(caps Capabilities, defs []ContentTypeDefinition) (compat.Schema, error) {
	full := BuildFor(caps)
	// CONTRACT-27: name order is no longer sufficient — a dynamic table may now
	// carry an FK to ANOTHER dynamic table, which has to be created first. With no
	// references this returns exactly the old name order, so nothing changes for an
	// installation that declares none. See sortDefinitionsByDependency.
	sorted, err := sortDefinitionsByDependency(defs)
	if err != nil {
		return compat.Schema{}, err
	}
	for _, d := range sorted {
		table, err := DynamicTable(d)
		if err != nil {
			return compat.Schema{}, fmt.Errorf("dynamic content type %q: %w", d.Name, err)
		}
		full.Tables = append(full.Tables, table)
		// CONTRACT-20 T1: the two READ routines of this type, generated here —
		// the same place its table is generated — because a routine's output
		// columns must be DECLARED and a dynamic type's column set is only known
		// at runtime. Writes stay raw SQL (they need RowsAffected to answer
		// 404 vs 200), so no write routine is generated.
		routines, err := dynamicContentRoutines(d)
		if err != nil {
			return compat.Schema{}, fmt.Errorf("dynamic content type %q: %w", d.Name, err)
		}
		full.Routines = append(full.Routines, routines...)
	}
	if err := full.Validate(); err != nil {
		return compat.Schema{}, fmt.Errorf("composed schema (code + dynamic) does not validate: %w", err)
	}
	return full, nil
}
