package store_test

// CONTRACT-23 T1/T3 on SQLite — the capability at the level of a real database:
// what a clean installation gets, what an EXISTING installation reports, and the
// coherence guard that makes the irreversible choice impossible to change by
// accident.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

var (
	vectorOn  = schema.Capabilities{}
	vectorOff = schema.Capabilities{VectorDisabled: true}
)

// openFresh opens a brand new SQLite database and applies the schema for caps.
func openFresh(t *testing.T, name string, caps schema.Capabilities) (*compat.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := store.Open(compat.SQLite, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.EnsureSchemaFor(context.Background(), db, caps); err != nil {
		t.Fatalf("ensure schema (%s): %v", caps.Describe(), err)
	}
	return db, path
}

func reopen(t *testing.T, path string) *compat.Store {
	t.Helper()
	db, err := store.Open(compat.SQLite, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func articleColumns(t *testing.T, db *compat.Store) []string {
	t.Helper()
	rows, err := db.DB.QueryContext(context.Background(), `SELECT name FROM pragma_table_info('articles')`)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

// TestCleanInstallHonoursTheChoice: a fresh database gets exactly the physical
// table its declaration asks for, and a restart with the SAME declaration is the
// no-op it always was. Two boots, not one — the metadata bug of CONTRACT-11 only
// shows up on the second.
func TestCleanInstallHonoursTheChoice(t *testing.T) {
	for _, tc := range []struct {
		name        string
		caps        schema.Capabilities
		wantColumn  bool
		wantVectorC bool
	}{
		{name: "enabled", caps: vectorOn, wantColumn: true, wantVectorC: true},
		{name: "disabled", caps: vectorOff, wantColumn: false, wantVectorC: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, path := openFresh(t, "clean.db", tc.caps)
			columns := articleColumns(t, db)
			has := false
			for _, c := range columns {
				if c == "embedding" {
					has = true
				}
			}
			if has != tc.wantColumn {
				t.Fatalf("articles columns %v: embedding present=%v, want %v", columns, has, tc.wantColumn)
			}
			t.Logf("boot#1 %s: articles=%v", tc.name, columns)

			// Boot #2 on the same file with the same declaration: no-op.
			if err := store.EnsureSchemaFor(context.Background(), db, tc.caps); err != nil {
				t.Fatalf("boot#2: %v", err)
			}
			// Boot #3 through a fresh connection, the way a restart really works.
			again := reopen(t, path)
			if err := store.EnsureSchemaFor(context.Background(), again, tc.caps); err != nil {
				t.Fatalf("boot#3: %v", err)
			}
			installed, exists, err := store.InstalledCapabilities(context.Background(), again)
			if err != nil {
				t.Fatal(err)
			}
			if !exists || installed.Vector() != tc.wantVectorC {
				t.Fatalf("InstalledCapabilities = (%v, exists=%v), want vector=%v", installed, exists, tc.wantVectorC)
			}
			t.Logf("boot#3 %s: InstalledCapabilities reports vector %s", tc.name, installed.Describe())
		})
	}
}

// TestInstalledCapabilitiesOnAnEmptyDatabase: a database that has not been
// created yet has made NO choice, so the declaration is still free. This is the
// only state in which the guard stays quiet no matter what is declared.
func TestInstalledCapabilitiesOnAnEmptyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	db, err := store.Open(compat.SQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	caps, exists, err := store.InstalledCapabilities(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("an empty database reported an installed choice: %v", caps)
	}
	t.Log("empty database: installed=false — the declaration decides")
}

// TestTheChoiceIsIrreversible is T3, in BOTH directions. It is the heart of the
// contract: the guard must fire, must say WHY, and must not have written
// anything.
func TestTheChoiceIsIrreversible(t *testing.T) {
	for _, tc := range []struct {
		name        string
		created     schema.Capabilities
		nowDeclared schema.Capabilities
		wantPhrases []string
	}{
		{
			name: "created WITH, now declared without", created: vectorOn, nowDeclared: vectorOff,
			wantPhrases: []string{"created WITH the vector capability", "IRREVERSIBLE", "LIBRARIAN_VECTOR=enabled", "compat copy"},
		},
		{
			name: "created WITHOUT, now declared with", created: vectorOff, nowDeclared: vectorOn,
			wantPhrases: []string{"created WITHOUT the vector capability", "IRREVERSIBLE", "LIBRARIAN_VECTOR=disabled", "compat copy"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, path := openFresh(t, "flip.db", tc.created)
			before := articleColumns(t, reopen(t, path))

			db := reopen(t, path)
			err := store.EnsureSchemaFor(context.Background(), db, tc.nowDeclared)
			if err == nil {
				t.Fatal("the service started with a changed, irreversible declaration")
			}
			for _, phrase := range tc.wantPhrases {
				if !strings.Contains(err.Error(), phrase) {
					t.Fatalf("the refusal does not mention %q:\n%v", phrase, err)
				}
			}
			after := articleColumns(t, reopen(t, path))
			if strings.Join(before, ",") != strings.Join(after, ",") {
				t.Fatalf("the refused boot changed the table: %v → %v", before, after)
			}
			t.Logf("REFUSED as required: %v", err)
			t.Logf("table untouched: %v", after)
		})
	}
}

// TestTheGuardReadsTheBaseNotTheMetadata answers the contract's instruction to
// detect the mismatch against the BASE. It is proven by CORRUPTING the metadata:
// __compat_schema is rewritten to describe the opposite installation, and the
// probe still answers from the physical column.
//
// This is not a hypothetical. That row is written by the very code path whose
// belief is in question, and compat's InspectSchema PREFERS it over the live
// catalog — so a probe that trusted it would agree with any mistake that ever
// booted, which is precisely when the guard needs to disagree.
func TestTheGuardReadsTheBaseNotTheMetadata(t *testing.T) {
	db, path := openFresh(t, "metadata.db", vectorOff)

	// Overwrite compat's canonical-schema metadata with the schema of an
	// installation that DOES have the capability.
	lying, err := schema.JSONWithFor(vectorOn, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(context.Background(),
		`INSERT INTO "__compat_schema" ("key", "value") VALUES ('canonical_schema', ?)
		 ON CONFLICT("key") DO UPDATE SET "value" = excluded."value"`, string(lying)); err != nil {
		t.Fatalf("poison metadata: %v", err)
	}

	installed, exists, err := store.InstalledCapabilities(context.Background(), reopen(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if !exists || installed.Vector() {
		t.Fatalf("the probe believed the metadata: installed=%v exists=%v", installed, exists)
	}
	t.Log("metadata says vector enabled, the physical table says disabled — the probe answers disabled")

	// And the guard still fires for the declaration the metadata agrees with.
	if err := store.EnsureSchemaFor(context.Background(), reopen(t, path), vectorOn); err == nil {
		t.Fatal("the guard was fooled by the poisoned metadata")
	} else {
		t.Logf("guard still REFUSES: %v", err)
	}
}

// TestContentTypesComposeForTheInstalledChoice is the mirror hazard: creating or
// editing a dynamic content type rewrites __compat_schema, and it must do so for
// the installation it is running on — not for the default.
func TestContentTypesComposeForTheInstalledChoice(t *testing.T) {
	db, _ := openFresh(t, "cpt.db", vectorOff)
	ctx := context.Background()

	def := schema.ContentTypeDefinition{
		Name:   "eventos",
		Fields: []schema.FieldDefinition{{Name: "titulo", Type: schema.FieldText}},
	}
	if err := store.CreateContentType(ctx, db, def); err != nil {
		t.Fatalf("create content type: %v", err)
	}

	var recorded string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT "value" FROM "__compat_schema" WHERE "key" = 'canonical_schema'`).Scan(&recorded); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if strings.Contains(recorded, `"embedding"`) {
		t.Fatal("creating a content type recorded a schema declaring the embedding column on an installation that has none")
	}
	if !strings.Contains(recorded, "cpt_eventos") {
		t.Fatal("the recorded schema is missing the dynamic table")
	}
	t.Logf("metadata after CreateContentType: %d bytes, embedding declared: no, cpt_eventos declared: yes", len(recorded))

	// And a restart on top of it is still clean.
	if err := store.EnsureSchemaFor(ctx, db, vectorOff); err != nil {
		t.Fatalf("restart after creating a type: %v", err)
	}
	t.Log("restart after CreateContentType: clean")
}
