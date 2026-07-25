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

// TestReservedNamesDerivedFromContentType confirms the reserved set is still
// DERIVED, not hardcoded: every column ContentType() injects is in it, so a
// FIELD can never collide with id/author_id/created_at/updated_at/metadata.
//
// It ALSO pins the CONTRACT-17 T2 decision in the other direction: code table
// names are NO LONGER reserved. The prefix makes the collision they used to
// prevent structurally impossible (see TestTypeNamedLikeCodeTableDoesNotCollide),
// so the reservation was removed rather than kept as a rule with no cause. If a
// future contract puts it back, this test fails and forces the decision to be
// re-argued instead of silently reverted.
func TestReservedNamesDerivedFromContentType(t *testing.T) {
	reserved := schema.ReservedNames()
	for _, column := range schema.ContentType("probe", nil).Columns {
		if _, ok := reserved[column.Name]; !ok {
			t.Fatalf("ContentType-injected column %q is not reserved", column.Name)
		}
	}
	for _, table := range schema.Build().Tables {
		if _, ok := reserved[table.Name]; ok {
			// Only legitimate if the table name IS an injected column name, which
			// no code table is.
			t.Fatalf("code table %q is reserved again — CONTRACT-17 removed that reservation on purpose", table.Name)
		}
	}
	t.Logf("reserved names (%d): %v", len(reserved), sortedKeys(reserved))
}

// --- CONTRACT-17: the structural prefix ---------------------------------------

// TestNoCodeTableUsesDynamicPrefix IS the enforcement of CONTRACT-17. Code
// tables are Go literals in Build(): they never pass through
// ValidateIdentifier, so nothing at runtime can stop one from being named
// "cpt_something". This test is therefore the ONLY real guarantee that the two
// namespaces stay disjoint — without it, the prefix is a convention.
func TestNoCodeTableUsesDynamicPrefix(t *testing.T) {
	for _, table := range schema.Build().Tables {
		if strings.HasPrefix(table.Name, schema.DynamicTablePrefix) {
			t.Fatalf(`code table %q starts with %q, which is RESERVED for dynamic content types.

CONSEQUENCE: an admin can create a dynamic type whose real table is exactly this
name. The composed schema (schema.BuildWith) would then contain the same table
twice, Schema.Validate() would fail, and THE SERVICE WOULD NOT START — the exact
failure CONTRACT-17 exists to make impossible.

FIX: rename the code table so it does not start with %q. Never "solve" this by
changing the prefix: the existing databases already carry it.`,
				table.Name, schema.DynamicTablePrefix, schema.DynamicTablePrefix)
		}
	}
	t.Logf("ENFORCEMENT OK: none of the %d code tables uses the %q prefix", len(schema.Build().Tables), schema.DynamicTablePrefix)
}

// TestDynamicTableIsPrefixed is the T1 acceptance criterion at the unit level:
// the compat.Table a definition produces carries the prefixed name, and the
// derivation goes through the single DynamicTableName helper.
func TestDynamicTableIsPrefixed(t *testing.T) {
	table, err := schema.DynamicTable(schema.ContentTypeDefinition{Name: "eventos"})
	if err != nil {
		t.Fatalf("DynamicTable: %v", err)
	}
	if table.Name != "cpt_eventos" {
		t.Fatalf("dynamic table name = %q, want %q", table.Name, "cpt_eventos")
	}
	if got := schema.DynamicTableName("eventos"); got != table.Name {
		t.Fatalf("DynamicTableName(%q) = %q but DynamicTable produced %q — two derivations", "eventos", got, table.Name)
	}
	if got := (schema.ContentTypeDefinition{Name: "eventos"}).TableName(); got != table.Name {
		t.Fatalf("TableName() = %q, want %q", got, table.Name)
	}
	t.Logf("PREFIX OK: public name %q -> real table %q", "eventos", table.Name)
}

// TestTypeNamedLikeCodeTableDoesNotCollide is the inverse of the enforcement:
// a type named exactly like a code table is now ACCEPTED, produces a distinct
// prefixed table, and the composed schema still validates and compiles. This is
// what makes hueco 3 unreachable rather than merely detectable.
func TestTypeNamedLikeCodeTableDoesNotCollide(t *testing.T) {
	for _, name := range []string{"users", "articles", "products", schema.ContentTypesTable} {
		def := schema.ContentTypeDefinition{Name: name, Fields: []schema.FieldDefinition{{Name: "nota", Type: schema.FieldText}}}
		if err := def.Validate(); err != nil {
			t.Fatalf("type named %q was rejected: %v", name, err)
		}
		full, err := schema.BuildWith([]schema.ContentTypeDefinition{def})
		if err != nil {
			t.Fatalf("BuildWith(type %q): %v", name, err)
		}
		if len(full.Tables) != len(schema.Build().Tables)+1 {
			t.Fatalf("type %q: composed schema has %d tables, want %d+1", name, len(full.Tables), len(schema.Build().Tables))
		}
		var sawCode, sawDynamic bool
		for _, tbl := range full.Tables {
			if tbl.Name == name {
				sawCode = true
			}
			if tbl.Name == schema.DynamicTableName(name) {
				sawDynamic = true
			}
		}
		if !sawCode || !sawDynamic {
			t.Fatalf("type %q: code table present=%v, dynamic table %q present=%v", name, sawCode, schema.DynamicTableName(name), sawDynamic)
		}
		if _, err := compat.CompileDDL(schema.PostgresTarget, full); err != nil {
			t.Fatalf("type %q: CompileDDL(postgres): %v", name, err)
		}
		t.Logf("NO COLLISION: code table %q and dynamic table %q coexist", name, schema.DynamicTableName(name))
	}
}

// TestTypeNameBudgetAccountsForThePrefix pins the documented length decision:
// MaxIdentifierLength applies to the REAL (prefixed) table, so the type-name
// budget is smaller by exactly len(prefix). A type name at the old limit (32)
// must now be rejected — otherwise it would produce a 36-byte table and break
// the invariant the limit exists to protect.
func TestTypeNameBudgetAccountsForThePrefix(t *testing.T) {
	if schema.MaxTypeNameLength != schema.MaxIdentifierLength-len(schema.DynamicTablePrefix) {
		t.Fatalf("MaxTypeNameLength=%d does not account for the prefix", schema.MaxTypeNameLength)
	}
	atLimit := strings.Repeat("a", schema.MaxTypeNameLength)
	if err := schema.ValidateTypeName(atLimit); err != nil {
		t.Fatalf("type name of length %d rejected: %v", len(atLimit), err)
	}
	if got := len(schema.DynamicTableName(atLimit)); got != schema.MaxIdentifierLength {
		t.Fatalf("table for the longest legal type name is %d bytes, want %d", got, schema.MaxIdentifierLength)
	}
	over := strings.Repeat("a", schema.MaxTypeNameLength+1)
	if err := schema.ValidateTypeName(over); err == nil {
		t.Fatalf("type name of length %d was accepted; its table would be %d bytes", len(over), len(schema.DynamicTableName(over)))
	} else {
		t.Logf("REJECTED type name of length %d -> %v", len(over), err)
	}
	// A FIELD is not prefixed, so it keeps the full budget.
	if err := schema.ValidateIdentifier(strings.Repeat("f", schema.MaxIdentifierLength)); err != nil {
		t.Fatalf("field name at MaxIdentifierLength rejected: %v", err)
	}
}

// TestTypeNameStartingWithThePrefixIsAllowed covers the red-team case: a type
// literally called "cpt_algo". name -> prefix+name is injective, so it simply
// yields "cpt_cpt_algo" and cannot collide with anything, including the type
// "algo". Forbidding it would be a rule with no failure to prevent.
func TestTypeNameStartingWithThePrefixIsAllowed(t *testing.T) {
	a := schema.ContentTypeDefinition{Name: "cpt_algo"}
	b := schema.ContentTypeDefinition{Name: "algo"}
	full, err := schema.BuildWith([]schema.ContentTypeDefinition{a, b})
	if err != nil {
		t.Fatalf("BuildWith: %v", err)
	}
	if err := full.Validate(); err != nil {
		t.Fatalf("composed schema does not validate: %v", err)
	}
	names := map[string]bool{}
	for _, tbl := range full.Tables {
		names[tbl.Name] = true
	}
	if !names["cpt_cpt_algo"] || !names["cpt_algo"] {
		t.Fatalf("want tables cpt_cpt_algo and cpt_algo, got %v", sortedKeys(setOf(names)))
	}
	t.Logf("INJECTIVE OK: types %q and %q -> tables %q and %q", a.Name, b.Name, a.TableName(), b.TableName())
}

func setOf(m map[string]bool) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
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
		{"type name reserved as an injected column", schema.ContentTypeDefinition{Name: "metadata"}},
		{"type name one over the type budget", schema.ContentTypeDefinition{Name: strings.Repeat("a", schema.MaxTypeNameLength+1)}},
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
	if !names[schema.DynamicTableName("reviews")] {
		t.Fatalf("composed schema does not contain the dynamic table %q", schema.DynamicTableName("reviews"))
	}
	// CONTRACT-17: the BARE name must NOT be a table. There is one namespace for
	// code tables and another for dynamic ones, and they do not intersect.
	if names["reviews"] {
		t.Fatal("composed schema contains the UNPREFIXED table 'reviews'")
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
	if !strings.Contains(string(data), `"`+schema.DynamicTableName("reviews")+`"`) {
		t.Fatalf("JSONWith output does not mention the dynamic table %q", schema.DynamicTableName("reviews"))
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
