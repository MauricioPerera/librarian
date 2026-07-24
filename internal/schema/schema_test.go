package schema_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// TestSchemaValidates covers the acceptance criterion: the full T1+T2 schema
// passes Schema.Validate() with no error.
func TestSchemaValidates(t *testing.T) {
	if err := schema.Build().Validate(); err != nil {
		t.Fatalf("schema does not validate: %v", err)
	}
}

// TestSchemaRoundTripJSON covers CONTRACT-04 T1: the JSON dump of the canonical
// schema deserializes back to a compat.Schema that Validate()s and that
// CompileDDL renders to the EXACT same statements as the original schema.Build()
// — for BOTH engines. If a field lost its json tag or an Expression got a bad
// omitempty, the round-trip DDL would diverge and fail here, not silently later
// in the real CLI. This is the no-PG acceptance test for the dump mechanism.
func TestSchemaRoundTripJSON(t *testing.T) {
	orig := schema.Build()

	origSQLite, err := compat.CompileDDL(schema.SQLiteTarget, orig)
	if err != nil {
		t.Fatalf("CompileDDL(sqlite) original: %v", err)
	}
	origPostgres, err := compat.CompileDDL(schema.PostgresTarget, orig)
	if err != nil {
		t.Fatalf("CompileDDL(postgres) original: %v", err)
	}

	data, err := schema.JSON()
	if err != nil {
		t.Fatalf("schema.JSON: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("schema.JSON produced empty output")
	}

	var round compat.Schema
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshal dumped schema: %v", err)
	}
	if err := round.Validate(); err != nil {
		t.Fatalf("round-tripped schema does not validate: %v", err)
	}

	rtSQLite, err := compat.CompileDDL(schema.SQLiteTarget, round)
	if err != nil {
		t.Fatalf("CompileDDL(sqlite) round-trip: %v", err)
	}
	rtPostgres, err := compat.CompileDDL(schema.PostgresTarget, round)
	if err != nil {
		t.Fatalf("CompileDDL(postgres) round-trip: %v", err)
	}

	if !reflect.DeepEqual(origSQLite, rtSQLite) {
		t.Fatalf("sqlite DDL diverged across JSON round-trip:\norig=%#v\nrt  =%#v", origSQLite, rtSQLite)
	}
	if !reflect.DeepEqual(origPostgres, rtPostgres) {
		t.Fatalf("postgres DDL diverged across JSON round-trip:\norig=%#v\nrt  =%#v", origPostgres, rtPostgres)
	}
	t.Logf("ROUND_TRIP OK: sqlite statements=%d, postgres statements=%d, DIFF=none (both engines)",
		len(origSQLite), len(origPostgres))
}

// TestCompileDDLBothEngines covers the exportability invariant: the full schema
// compiles to DDL for SQLite AND for Postgres without error.
func TestCompileDDLBothEngines(t *testing.T) {
	s := schema.Build()
	for _, tc := range []struct {
		name   string
		target compat.Target
	}{
		{"sqlite", schema.SQLiteTarget},
		{"postgres", schema.PostgresTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := compat.CompileDDL(tc.target, s)
			if err != nil {
				t.Fatalf("CompileDDL(%s) failed: %v", tc.name, err)
			}
			if len(stmts) == 0 {
				t.Fatalf("CompileDDL(%s) produced no statements", tc.name)
			}
		})
	}
}

// TestExpectedTables guards the model shape: the canonical tables are all
// present.
func TestExpectedTables(t *testing.T) {
	s := schema.Build()
	got := make(map[string]bool, len(s.Tables))
	for _, tbl := range s.Tables {
		got[tbl.Name] = true
	}
	for _, want := range []string{
		"users", "roles", "permissions", "role_permissions", "user_roles", "api_keys", "articles",
	} {
		if !got[want] {
			t.Errorf("missing expected table %q", want)
		}
	}
}

// TestAPIKeysTable guards the CONTRACT-02 T4 model shape: api_keys carries the
// required columns and its key_hash uniqueness + role_id FK. This complements
// the validate/compile invariant above (api_keys must not break exportability).
func TestAPIKeysTable(t *testing.T) {
	s := schema.Build()
	var tbl *compat.Table
	for i := range s.Tables {
		if s.Tables[i].Name == "api_keys" {
			tbl = &s.Tables[i]
			break
		}
	}
	if tbl == nil {
		t.Fatalf("api_keys table not in schema")
	}
	cols := make(map[string]bool, len(tbl.Columns))
	for _, c := range tbl.Columns {
		cols[c.Name] = true
	}
	for _, want := range []string{"id", "label", "key_hash", "role_id", "created_at", "revoked_at"} {
		if !cols[want] {
			t.Errorf("api_keys missing column %q", want)
		}
	}
	var hasKeyHashUnique, hasRoleFK bool
	for _, c := range tbl.Constraints {
		if c.Kind == compat.UniqueKey && len(c.Columns) == 1 && c.Columns[0] == "key_hash" {
			hasKeyHashUnique = true
		}
		if c.Kind == compat.ForeignKey && c.References != nil && c.References.Table == "roles" {
			hasRoleFK = true
		}
	}
	if !hasKeyHashUnique {
		t.Error("api_keys has no UNIQUE(key_hash)")
	}
	if !hasRoleFK {
		t.Error("api_keys has no role_id FK to roles")
	}
}

// TestContentTypeHelper checks the reusable helper injects the shared columns
// (id, author_id, created_at, updated_at, metadata) plus the caller's own ones.
func TestContentTypeHelper(t *testing.T) {
	tbl := schema.ContentType("widgets", []compat.Column{
		{Name: "label", Type: compat.Type{Family: compat.TextType}},
	})
	if tbl.Name != "widgets" {
		t.Fatalf("name = %q, want widgets", tbl.Name)
	}
	cols := make(map[string]bool, len(tbl.Columns))
	for _, c := range tbl.Columns {
		cols[c.Name] = true
	}
	for _, want := range []string{"id", "author_id", "label", "created_at", "updated_at", "metadata"} {
		if !cols[want] {
			t.Errorf("content type missing column %q", want)
		}
	}
	// It must carry a PK and an author FK.
	var hasPK, hasFK bool
	for _, c := range tbl.Constraints {
		switch c.Kind {
		case compat.PrimaryKey:
			hasPK = true
		case compat.ForeignKey:
			if c.References != nil && c.References.Table == "users" {
				hasFK = true
			}
		}
	}
	if !hasPK {
		t.Error("content type has no primary key")
	}
	if !hasFK {
		t.Error("content type has no author_id FK to users")
	}
}
