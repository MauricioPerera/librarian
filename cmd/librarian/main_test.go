package main

// CONTRACT-13 T2 acceptance tests for the --dump-schema mode, which stopped
// being offline in this contract.
//
// The non-negotiable property under test: the command NEVER emits a silently
// incomplete schema. Either it reads the database and emits the composed
// (code + dynamic) schema, or it fails with a non-zero exit and an explanatory
// message. There is no third outcome.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// TestDumpSchemaFailsLoudlyWhenDatabaseMissing is the design decision made
// executable: pointed at a path that does not exist, --dump-schema must FAIL,
// not fall back to the code-only schema and not let SQLite create an empty
// database (which would yield a plausible-looking dump from a mistyped path).
func TestDumpSchemaFailsLoudlyWhenDatabaseMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.db")
	out := filepath.Join(t.TempDir(), "schema.json")

	err := dumpSchema([]string{"--dump-schema", out, "--db", missing}, out)
	if err == nil {
		t.Fatal("SILENT INCOMPLETE DUMP: --dump-schema succeeded against a nonexistent database")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("--dump-schema wrote an output file despite failing")
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Fatal("--dump-schema created the database file it was pointed at")
	}
	t.Logf("FAILS LOUDLY: %v", err)
}

// TestDumpSchemaIncludesDynamicTypes is the end-to-end T2 criterion at the CLI
// boundary: build a real database, define a dynamic type, run the real
// --dump-schema code path, and confirm the emitted artifact — the schema_ref
// `compat copy` consumes — contains that table.
func TestDumpSchemaIncludesDynamicTypes(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "librarian.db")
	out := filepath.Join(dir, "schema.json")
	ctx := context.Background()

	db, err := store.Open(compat.SQLite, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.CreateContentType(ctx, db, schema.ContentTypeDefinition{
		Name: "reviews",
		Fields: []schema.FieldDefinition{
			{Name: "headline", Type: schema.FieldText},
			{Name: "score", Type: schema.FieldInteger},
		},
	}); err != nil {
		t.Fatalf("create content type: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := dumpSchema([]string{"--dump-schema", out, "--db", dbPath}, out); err != nil {
		t.Fatalf("dumpSchema: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	var dumped compat.Schema
	if err := json.Unmarshal(data, &dumped); err != nil {
		t.Fatalf("unmarshal dump: %v", err)
	}
	var names []string
	for _, tbl := range dumped.Tables {
		names = append(names, tbl.Name)
	}
	joined := strings.Join(names, ",")
	// CONTRACT-17: the exported table is the PREFIXED one — that is what
	// `compat copy` will create on PostgreSQL — and the bare name must not be
	// present at all.
	if !strings.Contains(joined, schema.DynamicTableName("reviews")) {
		t.Fatalf("EXPORT WOULD OMIT DATA: dumped schema has no %q table; tables=%v", schema.DynamicTableName("reviews"), names)
	}
	for _, tbl := range dumped.Tables {
		if tbl.Name == "reviews" {
			t.Fatalf("the dump contains the UNPREFIXED table 'reviews'; tables=%v", names)
		}
	}
	for _, code := range []string{"users", "articles", "products", "terms", schema.ContentTypesTable} {
		if !strings.Contains(joined, code) {
			t.Fatalf("dumped schema lost code table %q; tables=%v", code, names)
		}
	}
	if err := dumped.Validate(); err != nil {
		t.Fatalf("dumped schema does not validate: %v", err)
	}
	if _, err := compat.CompileDDL(schema.PostgresTarget, dumped); err != nil {
		t.Fatalf("dumped schema does not compile for the PostgreSQL export destination: %v", err)
	}
	t.Logf("DUMP OK: %d tables, dynamic 'reviews' included, compiles for PostgreSQL. tables=%v", len(dumped.Tables), names)
}

// TestDumpSchemaOnPreContract13Database confirms the honest bootstrap case: a
// database with no registry table dumps the code-only schema — which IS
// complete for that database, because a definition can only live in that table.
func TestDumpSchemaOnPreContract13Database(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy.db")
	out := filepath.Join(dir, "schema.json")
	ctx := context.Background()

	db, err := store.Open(compat.SQLite, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Apply the code schema MINUS the registry tables (a pre-CONTRACT-13 DB).
	var old compat.Schema
	for _, tbl := range schema.Build().Tables {
		if tbl.Name == schema.ContentTypesTable || tbl.Name == schema.ContentTypeFieldsTable {
			continue
		}
		old.Tables = append(old.Tables, tbl)
	}
	if err := db.ApplySchema(ctx, old); err != nil {
		t.Fatalf("apply legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := dumpSchema([]string{"--dump-schema", out, "--db", dbPath}, out); err != nil {
		t.Fatalf("dumpSchema on a pre-CONTRACT-13 database: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	var dumped compat.Schema
	if err := json.Unmarshal(data, &dumped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dumped.Tables) != len(schema.Build().Tables) {
		t.Fatalf("dumped %d tables, want %d (code schema)", len(dumped.Tables), len(schema.Build().Tables))
	}
	t.Logf("LEGACY DB OK: dumped the complete code schema (%d tables), no dynamic types to include", len(dumped.Tables))
}

// TestDbPathFlag covers the flag parsing, including the case that matters:
// `--dump-schema --db x.db` must NOT read "--db" as the output path.
func TestDbPathFlag(t *testing.T) {
	cases := []struct {
		args     []string
		wantDB   string
		wantOut  string
		wantDump bool
	}{
		{[]string{"--dump-schema"}, "", "", true},
		{[]string{"--dump-schema", "out.json"}, "", "out.json", true},
		{[]string{"--dump-schema=out.json"}, "", "out.json", true},
		{[]string{"--dump-schema", "--db", "x.db"}, "x.db", "", true},
		{[]string{"--dump-schema", "out.json", "--db", "x.db"}, "x.db", "out.json", true},
		{[]string{"--dump-schema=out.json", "--db=x.db"}, "x.db", "out.json", true},
		{[]string{}, "", "", false},
	}
	for _, tc := range cases {
		out, ok := dumpSchemaFlag(tc.args)
		db := dbPathFlag(tc.args)
		if ok != tc.wantDump || out != tc.wantOut || db != tc.wantDB {
			t.Fatalf("args=%v -> dump=%v out=%q db=%q; want dump=%v out=%q db=%q",
				tc.args, ok, out, db, tc.wantDump, tc.wantOut, tc.wantDB)
		}
		t.Logf("args=%-46v -> dump=%v out=%-11q db=%q", tc.args, ok, out, db)
	}
}
