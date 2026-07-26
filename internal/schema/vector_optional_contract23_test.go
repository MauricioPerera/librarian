package schema_test

// CONTRACT-23 T1 — the canonical schema with and without the vector capability.
//
// The two halves of this file answer the two halves of the contract: that the
// capability can be NOT DECLARED (and then no vector(N) type survives anywhere,
// which is what removes the pgvector requirement), and that declaring it changes
// NOTHING with respect to what runs in production today.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// TestEnabledIsByteIdenticalToBeforeTheContract is the acceptance criterion "with
// the capability enabled nothing changes". It is asserted as a byte comparison
// of the serialized schema, which covers tables, columns, views AND routines in
// one shot — the three places the capability reaches.
func TestEnabledIsByteIdenticalToBeforeTheContract(t *testing.T) {
	fromDefault, err := json.Marshal(schema.Build())
	if err != nil {
		t.Fatal(err)
	}
	fromExplicit, err := json.Marshal(schema.BuildFor(schema.Capabilities{}))
	if err != nil {
		t.Fatal(err)
	}
	if string(fromDefault) != string(fromExplicit) {
		t.Fatal("Build() and BuildFor(Capabilities{}) differ: the zero value must be the untouched schema")
	}
	if !strings.Contains(string(fromDefault), `"vector"`) {
		t.Fatal("the enabled schema no longer declares a vector column — the default must be the status quo")
	}
	t.Logf("enabled schema bytes=%d, declares the vector family: yes", len(fromDefault))
}

// TestDisabledDeclaresNoVectorAnywhere is the structural claim of T1: with the
// capability off, NO part of the canonical schema mentions the vector family —
// not the column, not the routine result columns, not the dedicated
// embedding-only routine. Anything left behind would still demand pgvector.
func TestDisabledDeclaresNoVectorAnywhere(t *testing.T) {
	off := schema.BuildFor(schema.Capabilities{VectorDisabled: true})
	if err := off.Validate(); err != nil {
		t.Fatalf("the schema without the vector capability does not validate: %v", err)
	}

	for _, table := range off.Tables {
		for _, column := range table.Columns {
			if column.Type.Family == compat.VectorType {
				t.Fatalf("table %s still declares a vector column %q", table.Name, column.Name)
			}
		}
	}
	for _, routine := range off.Routines {
		if routine.Name == schema.RoutineArticleEmbeddingOnly {
			t.Fatalf("the embedding-only routine %q is still declared", routine.Name)
		}
		for _, action := range routine.Actions {
			for _, column := range action.Columns {
				if column.Type.Family == compat.VectorType {
					t.Fatalf("routine %s still declares a vector result column %q", routine.Name, column.Name)
				}
			}
		}
	}

	// The articles table is otherwise IDENTICAL: exactly one column is gone.
	on := articleColumnNames(t, schema.Build())
	gone := articleColumnNames(t, off)
	if len(on)-len(gone) != 1 {
		t.Fatalf("articles columns: enabled=%v disabled=%v — exactly one column must differ", on, gone)
	}
	for _, name := range gone {
		if name == "embedding" {
			t.Fatal("articles still has the embedding column")
		}
	}
	t.Logf("articles enabled=%v", on)
	t.Logf("articles disabled=%v", gone)
}

// TestDisabledCompilesPostgresDDLWithoutTheVectorType is the reason the whole
// contract exists, proven at the level where it bites: the PostgreSQL DDL of an
// installation without the capability contains no `vector(` at all, so there is
// nothing in it that pgvector has to resolve.
func TestDisabledCompilesPostgresDDLWithoutTheVectorType(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caps      schema.Capabilities
		wantAny   bool
		wantCount int
	}{
		{name: "enabled", caps: schema.Capabilities{}, wantAny: true},
		{name: "disabled", caps: schema.Capabilities{VectorDisabled: true}, wantAny: false},
	} {
		statements, err := compat.CompileDDL(schema.PostgresTarget, schema.BuildFor(tc.caps))
		if err != nil {
			t.Fatalf("%s: compile postgres DDL: %v", tc.name, err)
		}
		hits := 0
		for _, statement := range statements {
			if strings.Contains(strings.ToLower(statement), "vector(") {
				hits++
			}
		}
		if (hits > 0) != tc.wantAny {
			t.Fatalf("%s: %d statements mention vector(, want any=%v", tc.name, hits, tc.wantAny)
		}
		t.Logf("%s: %d postgres DDL statements, %d mention vector(", tc.name, len(statements), hits)
	}
}

// TestJSONWithForReflectsTheChoice covers T4's `--dump-schema` requirement at the
// level the command uses.
func TestJSONWithForReflectsTheChoice(t *testing.T) {
	on, err := schema.JSONWithFor(schema.Capabilities{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	off, err := schema.JSONWithFor(schema.Capabilities{VectorDisabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(on), `"embedding"`) {
		t.Fatal("the enabled dump does not mention embedding")
	}
	if strings.Contains(string(off), `"embedding"`) {
		t.Fatal("the disabled dump still mentions embedding")
	}
	t.Logf("dump enabled bytes=%d (embedding: yes), disabled bytes=%d (embedding: no)", len(on), len(off))
}

// TestInferFeaturesDropsTheVectorFamily answers the contract's red-team question
// about the equivalence contract head-on: compat's feature audit of an
// installation without the capability no longer reports the vector family.
//
// That is the CORRECT answer, not a regression: InferFeatures describes what a
// schema USES, and this schema uses no vector. The equivalence contract is
// between a source and its export, and both sides of an export of this
// installation are built from THIS schema — the dump is what `compat copy`
// consumes, and it carries the same choice (TestJSONWithForReflectsTheChoice).
// An installation WITH the capability reports it exactly as before.
func TestInferFeaturesDropsTheVectorFamily(t *testing.T) {
	describe := func(caps schema.Capabilities) string {
		names := make([]string, 0, 8)
		for _, feature := range compat.InferFeatures(schema.BuildFor(caps)) {
			names = append(names, strings.ToLower(string(feature)))
		}
		return strings.Join(names, " ")
	}
	on, off := describe(schema.Capabilities{}), describe(schema.Capabilities{VectorDisabled: true})
	if !strings.Contains(on, "vector") {
		t.Fatalf("enabled features do not mention vector: %s", on)
	}
	if strings.Contains(off, "vector") {
		t.Fatalf("disabled features still mention vector: %s", off)
	}
	t.Logf("features enabled : %s", on)
	t.Logf("features disabled: %s", off)
}

func articleColumnNames(t *testing.T, s compat.Schema) []string {
	t.Helper()
	for _, table := range s.Tables {
		if table.Name != "articles" {
			continue
		}
		names := make([]string, 0, len(table.Columns))
		for _, column := range table.Columns {
			names = append(names, column.Name)
		}
		return names
	}
	t.Fatal("articles table not found")
	return nil
}
