package schema_test

import (
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
		"users", "roles", "permissions", "role_permissions", "user_roles", "articles",
	} {
		if !got[want] {
			t.Errorf("missing expected table %q", want)
		}
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
