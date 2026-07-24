package schema_test

// CONTRACT-13 T1/T2 acceptance tests at the schema layer: the identifier
// validator (the security piece of the contract) and the composition of the
// canonical schema as code + dynamic.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// --- T1: the hostile-name battery --------------------------------------------

// TestValidateIdentifierHostileBattery is the T1 acceptance criterion: every
// hostile or malformed name the contract lists (plus a few more) is rejected,
// one by one, with the real reason logged for each.
//
// Note what is NOT being tested: escaping. There is no sanitising path — a name
// either matches [a-z][a-z0-9_]* and is not reserved, or it never becomes an
// identifier at all.
func TestValidateIdentifierHostileBattery(t *testing.T) {
	cases := []struct {
		label string
		name  string
	}{
		{"empty", ""},
		{"double quotes", `re"views`},
		{"quote-escape injection", `x" ; DROP TABLE "users`},
		{"semicolon", "reviews; DROP TABLE users"},
		{"spaces", "my reviews"},
		{"leading/trailing space", " reviews "},
		{"uppercase", "MiTipo"},
		{"single uppercase letter", "Reviews"},
		{"unicode", "reseñas"},
		{"unicode homoglyph", "revіews"}, // Cyrillic і
		{"only digits", "123"},
		{"starts with digit", "1abc"},
		{"starts with underscore", "_reviews"},
		{"hyphen", "my-reviews"},
		{"dot-qualified", "public.reviews"},
		{"newline", "reviews\nDROP TABLE users"},
		{"null byte", "reviews\x00"},
		{"too long (33)", strings.Repeat("a", 33)},
		{"way too long (200)", strings.Repeat("a", 200)},
		{"reserved code table: users", "users"},
		{"reserved code table: articles", "articles"},
		{"reserved code table: terms", "terms"},
		{"reserved registry table", schema.ContentTypesTable},
		{"reserved injected column: id", "id"},
		{"reserved injected column: author_id", "author_id"},
		{"reserved injected column: metadata", "metadata"},
		{"reserved injected column: created_at", "created_at"},
		{"compat internal", "__compat_schema"},
		{"compat internal", "__compat_applied_changes"},
	}
	for _, tc := range cases {
		err := schema.ValidateIdentifier(tc.name)
		if err == nil {
			t.Fatalf("REJECT FAILED [%s] %q was ACCEPTED — this is a security hole", tc.label, tc.name)
		}
		t.Logf("REJECTED [%-28s] %-40q -> %v", tc.label, tc.name, err)
	}
}

// TestValidateIdentifierAcceptsLegitimateNames confirms the validator is not
// merely "reject everything": ordinary names pass, including the boundary
// length.
func TestValidateIdentifierAcceptsLegitimateNames(t *testing.T) {
	for _, name := range []string{
		"reviews",
		"a",
		"book_reviews",
		"x1",
		"my_type_2026",
		strings.Repeat("a", schema.MaxIdentifierLength),
	} {
		if err := schema.ValidateIdentifier(name); err != nil {
			t.Fatalf("ACCEPT FAILED %q was rejected: %v", name, err)
		}
		t.Logf("ACCEPTED %q", name)
	}
	// One over the limit must fail — the boundary is exact, not approximate.
	over := strings.Repeat("a", schema.MaxIdentifierLength+1)
	if err := schema.ValidateIdentifier(over); err == nil {
		t.Fatalf("name of length %d was accepted; MaxIdentifierLength=%d", len(over), schema.MaxIdentifierLength)
	}
}

// TestReservedNamesDerivedFromBuild confirms the reserved set is DERIVED, not
// hardcoded: every table Build() declares and every column ContentType()
// injects is in it. If a future contract adds a code table, this stays true
// with no list to update.
func TestReservedNamesDerivedFromBuild(t *testing.T) {
	reserved := schema.ReservedNames()
	for _, table := range schema.Build().Tables {
		if _, ok := reserved[table.Name]; !ok {
			t.Fatalf("code table %q is not reserved", table.Name)
		}
	}
	for _, column := range schema.ContentType("probe", nil).Columns {
		if _, ok := reserved[column.Name]; !ok {
			t.Fatalf("ContentType-injected column %q is not reserved", column.Name)
		}
	}
	t.Logf("reserved names (%d): %v", len(reserved), sortedKeys(reserved))
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestDefinitionValidateRejectsBadFields covers the definition-level checks a
// per-name validator cannot make.
func TestDefinitionValidateRejectsBadFields(t *testing.T) {
	cases := []struct {
		label string
		def   schema.ContentTypeDefinition
	}{
		{"hostile type name", schema.ContentTypeDefinition{Name: `x"; --`}},
		{"reserved type name", schema.ContentTypeDefinition{Name: "products"}},
		{"hostile field name", schema.ContentTypeDefinition{Name: "reviews", Fields: []schema.FieldDefinition{{Name: "sc ore", Type: schema.FieldInteger}}}},
		{"reserved field name", schema.ContentTypeDefinition{Name: "reviews", Fields: []schema.FieldDefinition{{Name: "metadata", Type: schema.FieldText}}}},
		{"unknown field type", schema.ContentTypeDefinition{Name: "reviews", Fields: []schema.FieldDefinition{{Name: "score", Type: "bigint"}}}},
		{"empty field type", schema.ContentTypeDefinition{Name: "reviews", Fields: []schema.FieldDefinition{{Name: "score", Type: ""}}}},
		{"json field type (excluded in v1)", schema.ContentTypeDefinition{Name: "reviews", Fields: []schema.FieldDefinition{{Name: "extra", Type: "json"}}}},
		{"vector field type (excluded in v1)", schema.ContentTypeDefinition{Name: "reviews", Fields: []schema.FieldDefinition{{Name: "emb", Type: "vector"}}}},
		{"duplicate field name", schema.ContentTypeDefinition{Name: "reviews", Fields: []schema.FieldDefinition{
			{Name: "score", Type: schema.FieldInteger}, {Name: "score", Type: schema.FieldText},
		}}},
	}
	for _, tc := range cases {
		err := tc.def.Validate()
		if err == nil {
			t.Fatalf("REJECT FAILED [%s] definition %+v was ACCEPTED", tc.label, tc.def)
		}
		t.Logf("REJECTED [%-32s] -> %v", tc.label, err)
	}
}

// --- T2: composition ----------------------------------------------------------

// sampleDef is a dynamic type exercising ALL five allowed field types.
var sampleDef = schema.ContentTypeDefinition{
	Name: "reviews",
	Fields: []schema.FieldDefinition{
		{Name: "headline", Type: schema.FieldText},
		{Name: "score", Type: schema.FieldInteger},
		{Name: "price_paid", Type: schema.FieldDecimal},
		{Name: "verified", Type: schema.FieldBoolean},
		{Name: "read_on", Type: schema.FieldDate},
	},
}

// TestDynamicTableShapeMatchesCodeContentType confirms a dynamic type produces
// EXACTLY the same shape as a code-defined one: the ContentType() helper's
// injected columns in the same positions, the own columns in declared order,
// and the same PK + author FK constraints.
func TestDynamicTableShapeMatchesCodeContentType(t *testing.T) {
	table, err := schema.DynamicTable(sampleDef)
	if err != nil {
		t.Fatalf("DynamicTable: %v", err)
	}
	var got []string
	for _, c := range table.Columns {
		got = append(got, c.Name)
	}
	want := []string{"id", "author_id", "headline", "score", "price_paid", "verified", "read_on", "created_at", "updated_at", "metadata"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dynamic table columns = %v, want %v", got, want)
	}
	// Same constraint set a code content type gets (PK on id + author FK).
	reference := schema.ContentType("reviews", nil)
	if !reflect.DeepEqual(table.Constraints, reference.Constraints) {
		t.Fatalf("dynamic constraints = %+v, want the same as ContentType(): %+v", table.Constraints, reference.Constraints)
	}
	// Every own column is nullable (documented v1 decision: no per-field
	// "required" concept, and no ALTER TABLE to add one later).
	for _, c := range table.Columns[2:7] {
		if !c.Nullable {
			t.Fatalf("dynamic own column %q should be nullable", c.Name)
		}
	}
	t.Logf("dynamic table %q columns: %v", table.Name, got)
}

// TestBuildWithComposesCodePlusDynamic is the core T2 unit check: BuildWith
// returns every code table AND the dynamic ones, the composed schema validates,
// and it compiles to DDL for BOTH engines (the exportability invariant — a
// dynamic type that only compiles on SQLite would silently break the export).
func TestBuildWithComposesCodePlusDynamic(t *testing.T) {
	code := schema.Build()
	full, err := schema.BuildWith([]schema.ContentTypeDefinition{sampleDef})
	if err != nil {
		t.Fatalf("BuildWith: %v", err)
	}
	if len(full.Tables) != len(code.Tables)+1 {
		t.Fatalf("composed schema has %d tables, want %d (code) + 1", len(full.Tables), len(code.Tables))
	}
	names := map[string]bool{}
	for _, tbl := range full.Tables {
		names[tbl.Name] = true
	}
	for _, tbl := range code.Tables {
		if !names[tbl.Name] {
			t.Fatalf("composed schema lost code table %q", tbl.Name)
		}
	}
	if !names["reviews"] {
		t.Fatal("composed schema does not contain the dynamic table 'reviews'")
	}
	if err := full.Validate(); err != nil {
		t.Fatalf("composed schema does not validate: %v", err)
	}
	sqliteDDL, err := compat.CompileDDL(schema.SQLiteTarget, full)
	if err != nil {
		t.Fatalf("CompileDDL(sqlite) composed: %v", err)
	}
	pgDDL, err := compat.CompileDDL(schema.PostgresTarget, full)
	if err != nil {
		t.Fatalf("CompileDDL(postgres) composed: %v", err)
	}
	t.Logf("COMPOSED OK: %d tables (%d code + 1 dynamic); sqlite stmts=%d, postgres stmts=%d",
		len(full.Tables), len(code.Tables), len(sqliteDDL), len(pgDDL))
}

// TestBuildWithIsDeterministic confirms the composed schema does not depend on
// the order definitions came back from the database — otherwise the dumped
// schema.json would churn and the __compat_schema metadata would differ between
// restarts for no reason.
func TestBuildWithIsDeterministic(t *testing.T) {
	a := schema.ContentTypeDefinition{Name: "alpha", Fields: []schema.FieldDefinition{{Name: "x", Type: schema.FieldText}}}
	b := schema.ContentTypeDefinition{Name: "beta", Fields: []schema.FieldDefinition{{Name: "y", Type: schema.FieldInteger}}}

	one, err := schema.BuildWith([]schema.ContentTypeDefinition{a, b})
	if err != nil {
		t.Fatalf("BuildWith(a,b): %v", err)
	}
	two, err := schema.BuildWith([]schema.ContentTypeDefinition{b, a})
	if err != nil {
		t.Fatalf("BuildWith(b,a): %v", err)
	}
	if !reflect.DeepEqual(one, two) {
		t.Fatal("BuildWith is order-dependent; the composed schema must be deterministic")
	}
}

// TestDynamicSchemaRoundTripJSON is the CONTRACT-04 round-trip guarantee
// extended to the COMPOSED schema (CONTRACT-13): the dump that `compat copy`
// consumes deserializes back to a schema that validates and compiles to the
// byte-identical DDL on both engines, dynamic tables included.
func TestDynamicSchemaRoundTripJSON(t *testing.T) {
	orig, err := schema.BuildWith([]schema.ContentTypeDefinition{sampleDef})
	if err != nil {
		t.Fatalf("BuildWith: %v", err)
	}
	origSQLite, err := compat.CompileDDL(schema.SQLiteTarget, orig)
	if err != nil {
		t.Fatalf("CompileDDL(sqlite) original: %v", err)
	}
	origPostgres, err := compat.CompileDDL(schema.PostgresTarget, orig)
	if err != nil {
		t.Fatalf("CompileDDL(postgres) original: %v", err)
	}

	data, err := schema.JSONWith([]schema.ContentTypeDefinition{sampleDef})
	if err != nil {
		t.Fatalf("JSONWith: %v", err)
	}
	if !strings.Contains(string(data), `"reviews"`) {
		t.Fatal("JSONWith output does not mention the dynamic table 'reviews'")
	}
	var round compat.Schema
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshal dumped schema: %v", err)
	}
	if err := round.Validate(); err != nil {
		t.Fatalf("round-tripped composed schema does not validate: %v", err)
	}
	rtSQLite, err := compat.CompileDDL(schema.SQLiteTarget, round)
	if err != nil {
		t.Fatalf("CompileDDL(sqlite) round-trip: %v", err)
	}
	rtPostgres, err := compat.CompileDDL(schema.PostgresTarget, round)
	if err != nil {
		t.Fatalf("CompileDDL(postgres) round-trip: %v", err)
	}
	if !reflect.DeepEqual(origSQLite, rtSQLite) || !reflect.DeepEqual(origPostgres, rtPostgres) {
		t.Fatal("composed DDL diverged across the JSON round-trip")
	}
	t.Logf("COMPOSED ROUND_TRIP OK: sqlite stmts=%d, postgres stmts=%d, DIFF=none", len(origSQLite), len(origPostgres))
}

// TestBuildWithRejectsHostilePersistedDefinition confirms composition FAILS
// LOUDLY on a definition that could only exist if the registry were written
// outside the API. Silently skipping it would produce an incomplete canonical
// schema — the failure mode this whole contract removes.
func TestBuildWithRejectsHostilePersistedDefinition(t *testing.T) {
	_, err := schema.BuildWith([]schema.ContentTypeDefinition{{Name: `evil"; DROP TABLE users; --`}})
	if err == nil {
		t.Fatal("BuildWith accepted a hostile persisted definition")
	}
	t.Logf("BuildWith rejected hostile persisted definition: %v", err)
}

// TestContentTypesRegistryInBuild confirms T1's two registry tables are part of
// the code schema, with the constraints the design depends on: UNIQUE(name) on
// content_types (the race-proof uniqueness guarantee) and the CHECK pinning
// field_type to exactly schema.FieldTypes.
func TestContentTypesRegistryInBuild(t *testing.T) {
	var types, fields *compat.Table
	for i, tbl := range schema.Build().Tables {
		switch tbl.Name {
		case schema.ContentTypesTable:
			types = &schema.Build().Tables[i]
		case schema.ContentTypeFieldsTable:
			fields = &schema.Build().Tables[i]
		}
	}
	if types == nil || fields == nil {
		t.Fatalf("registry tables missing from Build(): types=%v fields=%v", types != nil, fields != nil)
	}
	var sawUniqueName bool
	for _, c := range types.Constraints {
		if c.Kind == compat.UniqueKey && reflect.DeepEqual(c.Columns, []string{"name"}) {
			sawUniqueName = true
		}
	}
	if !sawUniqueName {
		t.Fatal("content_types lacks UNIQUE(name) — type-name uniqueness would be a losable application check")
	}
	var sawCheck bool
	for _, c := range fields.Constraints {
		if c.Kind == compat.Check && c.Expression != nil && c.Expression.Kind == "in" {
			sawCheck = true
			if got := len(c.Expression.Args) - 1; got != len(schema.FieldTypes) {
				t.Fatalf("field_type CHECK lists %d values, want %d (schema.FieldTypes)", got, len(schema.FieldTypes))
			}
		}
	}
	if !sawCheck {
		t.Fatal("content_type_fields lacks the field_type CHECK constraint")
	}
	t.Logf("registry OK: %s(UNIQUE name), %s(CHECK field_type IN %v)",
		schema.ContentTypesTable, schema.ContentTypeFieldsTable, schema.FieldTypeNames())
}

// TestContentTypesManagePermissionInCatalog confirms the ONE new permission is
// in the fixed catalog (the idempotent, data-driven seed picks it up from here).
func TestContentTypesManagePermissionInCatalog(t *testing.T) {
	var found bool
	for _, p := range schema.Permissions {
		if p == "content_types.manage" {
			found = true
		}
	}
	if !found {
		t.Fatalf("content_types.manage missing from schema.Permissions: %v", schema.Permissions)
	}
	t.Logf("permission catalog: %v", schema.Permissions)
}
