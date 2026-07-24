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
	"sort"

	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// Registry table names. They are code tables in Build(), so ReservedNames()
// picks them up automatically — a dynamic type can never be called
// "content_types".
const (
	// ContentTypesTable holds one row per dynamic content type.
	ContentTypesTable = "content_types"
	// ContentTypeFieldsTable holds one row per field of a dynamic content type.
	ContentTypeFieldsTable = "content_type_fields"
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
type ContentTypeDefinition struct {
	Name   string            `json:"name"`
	Fields []FieldDefinition `json:"fields"`
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
	if err := ValidateIdentifier(d.Name); err != nil {
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
	return nil
}

// DynamicTable turns a validated definition into the real compat.Table, through
// the SAME ContentType() helper articles/products use. It re-validates first:
// this function is the ONLY way a dynamic name reaches a compat.Table, so the
// gate cannot be bypassed by a caller that forgot to validate.
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
	return ContentType(d.Name, own), nil
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
func BuildWith(defs []ContentTypeDefinition) (compat.Schema, error) {
	full := Build()
	sorted := make([]ContentTypeDefinition, len(defs))
	copy(sorted, defs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, d := range sorted {
		table, err := DynamicTable(d)
		if err != nil {
			return compat.Schema{}, fmt.Errorf("dynamic content type %q: %w", d.Name, err)
		}
		full.Tables = append(full.Tables, table)
	}
	if err := full.Validate(); err != nil {
		return compat.Schema{}, fmt.Errorf("composed schema (code + dynamic) does not validate: %w", err)
	}
	return full, nil
}
