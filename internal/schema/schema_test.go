package schema_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
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

	// CONTRACT-13: schema.JSON() became schema.JSONWith(defs) — the dump must be
	// handed the dynamic definitions so it can never emit an incomplete schema
	// by accident. With nil definitions the dump is exactly the code schema, so
	// this CONTRACT-04 round-trip test keeps testing precisely what it tested
	// before. The composed (code + dynamic) round trip is covered separately by
	// TestDynamicSchemaRoundTripJSON in dynamic_test.go.
	data, err := schema.JSONWith(nil)
	if err != nil {
		t.Fatalf("schema.JSONWith(nil): %v", err)
	}
	if len(data) == 0 {
		t.Fatal("schema.JSONWith(nil) produced empty output")
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
		"users", "roles", "permissions", "role_permissions", "user_roles", "api_keys", "articles", "products",
	} {
		if !got[want] {
			t.Errorf("missing expected table %q", want)
		}
	}
}

// TestProductsTable covers CONTRACT-11 T1: the products content type carries the
// own columns beyond title/body (a decimal price and a sku), a table-level
// UNIQUE(sku), the schema Validate()s, and CompileDDL renders the price column
// for BOTH engines — NUMERIC on Postgres, TEXT on SQLite (the canonical carrier).
// This is the no-DB acceptance test for the decimalColumn helper + unique(sku).
func TestProductsTable(t *testing.T) {
	s := schema.Build()
	var tbl *compat.Table
	for i := range s.Tables {
		if s.Tables[i].Name == "products" {
			tbl = &s.Tables[i]
			break
		}
	}
	if tbl == nil {
		t.Fatal("products table not in schema")
	}

	// Own columns present, plus the ContentType-injected trailer.
	cols := make(map[string]compat.Column, len(tbl.Columns))
	for _, c := range tbl.Columns {
		cols[c.Name] = c
	}
	for _, want := range []string{"id", "author_id", "title", "body", "price", "sku", "created_at", "updated_at", "metadata"} {
		if _, ok := cols[want]; !ok {
			t.Errorf("products missing column %q", want)
		}
	}

	// price is a decimal column, NOT NULL, with no fixed precision/scale.
	price := cols["price"]
	if price.Type.Family != compat.DecimalType {
		t.Errorf("price family = %q, want decimal", price.Type.Family)
	}
	if len(price.Type.Arguments) != 0 {
		t.Errorf("price arguments = %v, want none (unbounded NUMERIC)", price.Type.Arguments)
	}
	if price.Nullable {
		t.Error("price must be NOT NULL")
	}

	// products has NO published_at (it is not articles — simple CRUD, no publish).
	if _, ok := cols["published_at"]; ok {
		t.Error("products must NOT have a published_at column (no publish workflow)")
	}

	// Table-level UNIQUE(sku) must be present (schema-level dup guarantee).
	var hasSKUUnique bool
	for _, c := range tbl.Constraints {
		if c.Kind == compat.UniqueKey && len(c.Columns) == 1 && c.Columns[0] == "sku" {
			hasSKUUnique = true
		}
	}
	if !hasSKUUnique {
		t.Error("products has no UNIQUE(sku)")
	}

	// Schema.Validate must pass with the products table.
	if err := s.Validate(); err != nil {
		t.Fatalf("schema with products does not validate: %v", err)
	}

	// CompileDDL for both engines must succeed and render price per engine.
	sqlite, err := compat.CompileDDL(schema.SQLiteTarget, s)
	if err != nil {
		t.Fatalf("CompileDDL(sqlite) with products: %v", err)
	}
	postgres, err := compat.CompileDDL(schema.PostgresTarget, s)
	if err != nil {
		t.Fatalf("CompileDDL(postgres) with products: %v", err)
	}
	var sqliteStmt, pgStmt string
	for _, stmt := range sqlite {
		if strings.Contains(stmt, `CREATE TABLE "products"`) {
			sqliteStmt = stmt
		}
	}
	for _, stmt := range postgres {
		if strings.Contains(stmt, `CREATE TABLE "products"`) {
			pgStmt = stmt
		}
	}
	if sqliteStmt == "" || !strings.Contains(sqliteStmt, `"price" TEXT`) {
		t.Fatalf("sqlite DDL did not compile price as TEXT: %v", sqlite)
	}
	if pgStmt == "" || !strings.Contains(pgStmt, `"price" NUMERIC`) {
		t.Fatalf("postgres DDL did not compile price as NUMERIC: %v", postgres)
	}
	if !strings.Contains(sqliteStmt, `UNIQUE ("sku")`) {
		t.Errorf("sqlite products DDL missing UNIQUE(sku): %s", sqliteStmt)
	}
	if !strings.Contains(pgStmt, `UNIQUE ("sku")`) {
		t.Errorf("postgres products DDL missing UNIQUE(sku): %s", pgStmt)
	}
	t.Logf("SQLite products DDL: %s", sqliteStmt)
	t.Logf("Postgres products DDL: %s", pgStmt)
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

// TestArticlesEmbeddingVectorColumn covers CONTRACT-05 T1: the articles table
// carries a nullable vector(N) embedding column, the schema Validate()s, and
// CompileDDL renders it for BOTH engines without error — TEXT on SQLite (the
// interoperable carrier) and native vector(N) on Postgres. This is the no-PG
// acceptance test for the vector column.
func TestArticlesEmbeddingVectorColumn(t *testing.T) {
	s := schema.Build()
	var tbl *compat.Table
	for i := range s.Tables {
		if s.Tables[i].Name == "articles" {
			tbl = &s.Tables[i]
			break
		}
	}
	if tbl == nil {
		t.Fatal("articles table not in schema")
	}
	var emb *compat.Column
	for i := range tbl.Columns {
		if tbl.Columns[i].Name == "embedding" {
			emb = &tbl.Columns[i]
			break
		}
	}
	if emb == nil {
		t.Fatal("articles has no embedding column")
	}
	if emb.Type.Family != compat.VectorType {
		t.Fatalf("embedding family = %q, want vector", emb.Type.Family)
	}
	if !emb.Nullable {
		t.Error("embedding must be nullable (an article with no embedding is valid)")
	}
	if !reflect.DeepEqual(emb.Type.Arguments, []int{schema.EmbeddingDimension}) {
		t.Fatalf("embedding dimension = %v, want [%d]", emb.Type.Arguments, schema.EmbeddingDimension)
	}

	// Schema.Validate must pass with the vector column.
	if err := s.Validate(); err != nil {
		t.Fatalf("schema with vector column does not validate: %v", err)
	}

	// CompileDDL for both engines must succeed; on Postgres the column renders
	// as native vector(N).
	sqlite, err := compat.CompileDDL(schema.SQLiteTarget, s)
	if err != nil {
		t.Fatalf("CompileDDL(sqlite) with vector: %v", err)
	}
	postgres, err := compat.CompileDDL(schema.PostgresTarget, s)
	if err != nil {
		t.Fatalf("CompileDDL(postgres) with vector: %v", err)
	}
	var sqliteStmt, pgStmt string
	for _, stmt := range sqlite {
		if strings.Contains(stmt, `"embedding" TEXT`) {
			sqliteStmt = stmt
		}
	}
	for _, stmt := range postgres {
		if strings.Contains(stmt, fmt.Sprintf(`"embedding" vector(%d)`, schema.EmbeddingDimension)) {
			pgStmt = stmt
		}
	}
	if sqliteStmt == "" {
		t.Fatalf("sqlite DDL did not compile embedding as TEXT: %v", sqlite)
	}
	if pgStmt == "" {
		t.Fatalf("postgres DDL did not compile embedding as vector(%d): %v", schema.EmbeddingDimension, postgres)
	}
	t.Logf("SQLite articles DDL: %s", sqliteStmt)
	t.Logf("Postgres articles DDL: %s", pgStmt)
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
