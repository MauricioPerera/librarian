//go:build dualengine

package store_test

// CONTRACT-29 T2 — THE TEST THAT GIVES THE CONTRACT ITS MEANING.
//
// It reproduces, on purpose, the exact situation that cost this project two
// incidents and one false diagnosis: the `public` schema of the server the
// batteries point at is DIRTY — it holds a whole librarian installation,
// `__compat_schema` included, left there by a manual probe or by a run that
// never got to clean up.
//
// THE POLLUTION IS TAKEN AS IT IS FOUND, AND ONLY PLANTED WHEN THERE IS NONE.
// If `public` already carries application-named relations — an operator probe,
// an interrupted run, anything — those ARE the pollution and nothing is planted.
// Only over a `public` with no recognisable residue does this test create its
// own, by running the PRODUCTION startup path (store.EnsureSchema →
// store.SeedCatalogs) straight into `public`, which is how the residue really
// appeared and the only way to get a truthful `__compat_schema`.
//
// That order is not a detail. An earlier version planted unconditionally, and a
// `public` holding a HALF installation made the planting itself fail on
// CONTRACT-23's coherence guard — so the test that proves "the isolation
// survives a dirty public" died of a dirty public, with a message about vector
// capabilities. See ensureDirtyPublic for the rule that came out of it.
//
// Part 2 then measures the OLD isolation (a schema per run reached with
// `search_path=<schema>,public`) against that dirty server, and it is expected
// to FAIL to isolate: compat's InspectSchema — which store.missingTables asks
// what already exists — resolves through the search path, sees the installation
// sitting in `public`, and concludes nothing is missing. EnsureSchema then
// creates NOTHING in the run's own schema, and every later write silently goes
// to `public` instead. That is the incident, reproduced. It is measured rather
// than merely described, because a contract that only asserts its fix leaves
// nobody able to tell whether the fix is still doing anything.
//
// Part 3 measures the NEW isolation (a DATABASE per run, internal/pgtestdb) on
// the very same dirty server, through the very same fixture the CONTRACT-21/26/27
// batteries use, and it must be untouched by the pollution.
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestContract29 -count=1 -v ./internal/store
//
// The pollution is always removed, including when the test fails: leaving it
// behind would be the very hazard this contract exists to close.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/pgtestdb"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// TestContract29IsolationSurvivesADirtyPublic is the acceptance criterion of the
// contract.
func TestContract29IsolationSurvivesADirtyPublic(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this test is meaningless without a real PostgreSQL", pgDSNEnv)
	}
	ctx := context.Background()

	admin, err := compat.OpenPostgres(schema.PostgresVersion, dsn)
	if err != nil {
		t.Fatalf("postgres open (admin): %v", err)
	}
	// Registered FIRST so it runs LAST: t.Cleanup is LIFO, and every cleanup
	// below needs this connection alive.
	t.Cleanup(func() { _ = admin.Close() })

	// --- 1. `public` must be DIRTY — whatever state it is already in ---------
	preexisting := relationsInPublic(ctx, t, admin.DB)
	t.Cleanup(func() { restorePublic(t, admin.DB, preexisting) })

	dirt := ensureDirtyPublic(ctx, t, dsn, preexisting)

	// --- 1b. And that dirt has to be REACHABLE through the `,public` fallback -
	//
	// Without this the test could "pass" over a pollution that was never visible
	// in the first place, which would make part 3 vacuous.
	provePublicLeaks(ctx, t, admin.DB, dsn, dirt)

	// --- 2. The OLD mechanism, reproduced ------------------------------------
	//
	// This half runs inside its OWN throwaway database, whose `public` is dirtied
	// the same way, and not against the server's `public` — because the old
	// mechanism is DESTRUCTIVE through the fallback it is being measured for:
	// applyViews issues an unqualified `DROP VIEW`, which the `,public` entry
	// resolves to somebody else's views. Measuring the bug must not commit it.
	measureLegacyMechanism(t, dsn)

	// --- 3. The NEW mechanism, on the same dirty server ----------------------
	//
	// openPostgresForStore is the very fixture the CONTRACT-21/26/27 batteries
	// use, so what is measured here is what they get.
	isolated, release := openPostgresForStore(t, dsn)
	defer release()

	var strays int
	if err := isolated.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&strays); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if strays != 0 {
		t.Fatalf("the isolated run starts with %d table(s) visible — the pollution reached it", strays)
	}
	// EVERY relation of the dirty public, whatever it happens to be, has to be
	// invisible — not just the handful this test would have planted itself.
	for _, polluting := range dirt {
		present, err := isolated.TableExists(ctx, polluting.name)
		if err != nil {
			t.Fatalf("isolated TableExists(%q): %v", polluting.name, err)
		}
		if present {
			t.Fatalf("the polluting public.%s is VISIBLE from the isolated run — the isolation still depends on public being clean", polluting.name)
		}
	}
	inspection, err := isolated.InspectSchema(ctx)
	if err != nil {
		t.Fatalf("isolated InspectSchema: %v", err)
	}
	if len(inspection.Schema.Tables) != 0 {
		t.Fatalf("InspectSchema — the very call the incident went through — reports %d table(s) in a run that has none",
			len(inspection.Schema.Tables))
	}
	t.Logf("NEW mechanism (database per run): 0 of the %d polluting relations visible, InspectSchema reports 0 tables", len(dirt))

	// --- 4. And the production startup path really builds, and really writes --
	//
	// "Nothing is visible" would also be true of a connection to a server that
	// silently swallowed the DDL. The proof that the run is a WORKING database
	// is that the production path creates the schema, seeds it, and the rows come
	// back — twice, because the metadata class of bug only appears on boot #2.
	if err := store.EnsureSchema(ctx, isolated); err != nil {
		t.Fatalf("boot#1 ensure schema on the isolated database: %v", err)
	}
	if err := store.SeedCatalogs(ctx, isolated); err != nil {
		t.Fatalf("boot#1 seed: %v", err)
	}
	if err := store.EnsureSchema(ctx, isolated); err != nil {
		t.Fatalf("boot#2 ensure schema on the isolated database: %v", err)
	}
	for _, name := range []string{"users", "roles", "articles", schema.ContentTypesTable} {
		present, err := isolated.TableExists(ctx, name)
		if err != nil {
			t.Fatalf("TableExists(%q) after EnsureSchema: %v", name, err)
		}
		if !present {
			t.Fatalf("EnsureSchema did not create %q — the dirty public made it skip the table, which is the incident itself", name)
		}
	}
	var roles int
	if err := isolated.DB.QueryRowContext(ctx, `SELECT count(*) FROM "roles"`).Scan(&roles); err != nil {
		t.Fatalf("count roles: %v", err)
	}
	if roles != len(schema.Roles) {
		t.Fatalf("roles seeded = %d, want %d — the writes did not land in this run's own tables", roles, len(schema.Roles))
	}
	t.Logf("production path on the isolated database: schema built twice, %d roles seeded, reads land in the run's own tables", roles)
}

// applicationRelationNames are the names an incident-shaped residue carries:
// librarian's own tables plus compat's metadata table. They are what makes a
// leftover DANGEROUS rather than merely present — a stray table called `foo`
// could never make EnsureSchema skip anything.
var applicationRelationNames = map[string]struct{}{
	"__compat_schema":         {},
	"users":                   {},
	"roles":                   {},
	"permissions":             {},
	"role_permissions":        {},
	"user_roles":              {},
	"api_keys":                {},
	"articles":                {},
	"products":                {},
	"article_terms":           {},
	"product_terms":           {},
	"terms":                   {},
	"taxonomies":              {},
	"bootstrap":               {},
	"content_type_fields":     {},
	"content_type_references": {},
	schema.ContentTypesTable:  {},
}

// librarianResidue returns, in catalog order, the relations of `public` that
// carry an application name.
func librarianResidue(relations []relation) []string {
	var out []string
	for _, r := range relations {
		if _, ok := applicationRelationNames[r.name]; ok {
			out = append(out, r.name)
		}
	}
	return out
}

// ensureDirtyPublic guarantees that `public` carries incident-shaped pollution,
// and returns what it ended up holding.
//
// IT NEVER REQUIRES `public` TO BE IN ANY PARTICULAR STATE FIRST, and that is
// not a nicety — an earlier version of this test planted its own installation
// unconditionally, and against a `public` that already held a HALF installation
// (an `articles` without the vector column) the planting was refused by
// CONTRACT-23's coherence guard. The test then failed with a message about
// capabilities: a bewildering failure caused by a dirty `public`, which is the
// exact thing this contract exists to abolish, reproduced inside the test that
// proves it abolished. The rule that came out of that:
//
//	a test asserting "the isolation survives a dirty public" may never
//	require a clean public in order to start.
//
// So there are three outcomes, and all three are legible:
//
//   - `public` ALREADY carries application-named relations → use them. Somebody
//     else's residue is pollution just as good, and nothing is planted, so no
//     precondition on its shape exists at all. This is the common case on a
//     server an operator has been probing.
//   - `public` carries nothing recognisable → plant a real installation through
//     the PRODUCTION startup path, which is how the residue really appeared and
//     the only way to get a truthful `__compat_schema`.
//   - planting is refused → SKIP naming the refusal, rather than fail. What
//     cannot be measured is the POLLUTION; the isolation itself is not in doubt
//     and reporting a red test would point at the wrong thing.
func ensureDirtyPublic(ctx context.Context, t *testing.T, dsn string, preexisting []relation) []relation {
	t.Helper()

	if residue := librarianResidue(preexisting); len(residue) > 0 {
		t.Logf("public was ALREADY dirty when this test started: %d relations, %d of them application-named (%s). Using THAT as the pollution — nothing is planted, so this test imposes no precondition on the shape of the mess.",
			len(preexisting), len(residue), strings.Join(residue, ", "))
		return preexisting
	}

	dirty, err := store.Open(compat.Postgres, withParam(dsn, "search_path", "public"))
	if err != nil {
		t.Fatalf("open a connection pinned to public: %v", err)
	}
	defer func() { _ = dirty.Close() }()

	if err := store.EnsureSchema(ctx, dirty); err != nil {
		t.Skipf("public holds %d relation(s) that carry no application name, and planting an installation on top of them was refused by the production startup path: %v — so this run has no safe way to PRODUCE the pollution. The isolation itself is not what failed here. Clean public, or leave it with application-named residue, and rerun.",
			len(preexisting), err)
	}
	if err := store.SeedCatalogs(ctx, dirty); err != nil {
		t.Skipf("an installation was planted in public but seeding it was refused: %v — this run has no safe way to produce the pollution", err)
	}

	planted := relationsInPublic(ctx, t, dirty.DB)
	residue := librarianResidue(planted)
	if len(residue) == 0 {
		t.Fatalf("an installation was planted in public and yet nothing application-named is there (%d relations) — this test cannot reproduce the incident", len(planted))
	}
	t.Logf("public was clean, so this test planted the pollution itself through the production startup path: %d relations, %d application-named, __compat_schema among them: %v",
		len(planted), len(residue), contains(planted, "__compat_schema"))
	return planted
}

// provePublicLeaks checks that the pollution really IS reachable through the
// `,public` fallback the old mechanism used — otherwise part 3 would be proving
// that something invisible stayed invisible.
//
// It is strictly READ-ONLY over `public`: a uniquely named EMPTY schema, a
// handful of `to_regclass` lookups, and the schema dropped again. No DDL touches
// the pollution, which matters because the old mechanism damages it (see
// measureLegacyMechanism).
func provePublicLeaks(ctx context.Context, t *testing.T, db *sql.DB, dsn string, dirt []relation) {
	t.Helper()
	residue := librarianResidue(dirt)
	if len(residue) == 0 {
		t.Fatalf("the pollution carries no application-named relation — nothing here could ever have caused the incident")
	}

	probeSchema := fmt.Sprintf("contract29_probe_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+probeSchema+`"`); err != nil {
		t.Fatalf("create probe schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+probeSchema+`" CASCADE`); err != nil {
			t.Errorf("cleanup: drop probe schema %s: %v", probeSchema, err)
		}
	})

	probe, err := store.Open(compat.Postgres, withParam(dsn, "search_path", probeSchema+",public"))
	if err != nil {
		t.Fatalf("open probe connection: %v", err)
	}
	defer func() { _ = probe.Close() }()

	visible := 0
	for _, name := range residue {
		var resolved sql.NullString
		if err := probe.DB.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, name).Scan(&resolved); err != nil {
			t.Fatalf("to_regclass(%q) from the probe: %v", name, err)
		}
		if resolved.Valid {
			visible++
		}
	}
	if visible != len(residue) {
		t.Fatalf("only %d of %d polluting relations resolve from a `search_path=%s,public` session — the pollution is not the kind that caused the incident, so this test would prove nothing",
			visible, len(residue), probeSchema)
	}
	t.Logf("the pollution IS reachable the old way: %d/%d application-named relations resolve from a `search_path=%s,public` session",
		visible, len(residue), probeSchema)
}

// measureLegacyMechanism reproduces the OLD isolation — a schema per run reached
// with `search_path=<schema>,public` — over a `public` that carries a librarian
// installation, and measures how much of the canonical schema ends up in the
// run's OWN namespace. The answer is "almost none", and that is the incident.
//
// It runs inside a throwaway database provisioned by the very mechanism under
// test, so the reproduction cannot damage anything outside itself.
func measureLegacyMechanism(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()

	scratch, release := openPostgresForStore(t, dsn)
	defer release()

	var scratchName string
	if err := scratch.DB.QueryRowContext(ctx, `SELECT current_database()`).Scan(&scratchName); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	scratchDSN, err := pgtestdb.WithDatabase(dsn, scratchName)
	if err != nil {
		t.Fatalf("derive scratch dsn: %v", err)
	}

	// The same pollution, in the scratch database's own `public`.
	if err := store.EnsureSchema(ctx, scratch); err != nil {
		t.Fatalf("plant an installation in the scratch public: %v", err)
	}
	if err := store.SeedCatalogs(ctx, scratch); err != nil {
		t.Fatalf("seed the scratch installation: %v", err)
	}

	const legacySchema = "contract29_legacy"
	if _, err := scratch.DB.ExecContext(ctx, `CREATE SCHEMA "`+legacySchema+`"`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	legacy, err := store.Open(compat.Postgres, withParam(scratchDSN, "search_path", legacySchema+",public"))
	if err != nil {
		t.Fatalf("open legacy-scoped connection: %v", err)
	}
	want, err := store.CanonicalSchema(ctx, legacy)
	if err != nil {
		_ = legacy.Close()
		t.Fatalf("canonical schema: %v", err)
	}
	legacyErr := store.EnsureSchema(ctx, legacy)
	_ = legacy.Close()

	built := tablesInSchema(ctx, t, scratch.DB, legacySchema)
	t.Logf("OLD mechanism (search_path=%s,public over a dirty public): EnsureSchema err=%v, tables created in the run's OWN schema: %d of %d wanted",
		legacySchema, legacyErr, len(built), len(want.Tables))
	if len(built) >= len(want.Tables) {
		t.Fatalf("the OLD mechanism built the whole schema in its own namespace — it is no longer the mechanism that caused the incident, so this test proves nothing")
	}
	t.Log("OLD mechanism: the dirty public was seen through the `,public` fallback, so EnsureSchema created (almost) nothing and every later write would land in public — this IS the incident")
}

// --- helpers -----------------------------------------------------------------

// withParam appends a pgx runtime parameter to a DSN.
func withParam(dsn, key, value string) string {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + key + "=" + value
}

// relation is a table or a view of a schema, with the kind needed to drop it.
type relation struct {
	name   string
	isView bool
}

// relationsInPublic lists the tables and views of `public` on the server the DSN
// points at. Views are included because applyViews puts them there too.
func relationsInPublic(ctx context.Context, t *testing.T, db *sql.DB) []relation {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT c.relname, c.relkind = 'v' FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v') ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list relations in public: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []relation
	for rows.Next() {
		var r relation
		if err := rows.Scan(&r.name, &r.isView); err != nil {
			t.Fatalf("scan relation: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate relations: %v", err)
	}
	return out
}

// tablesInSchema lists the TABLES physically created in one schema — the
// question part 2 turns on, and one the scoped connection cannot answer about
// itself once the `,public` fallback is in play.
func tablesInSchema(ctx context.Context, t *testing.T, db *sql.DB, schemaName string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = $1 ORDER BY tablename`, schemaName)
	if err != nil {
		t.Fatalf("list tables in %s: %v", schemaName, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	return out
}

// restorePublic removes every relation this test added to `public`, leaving
// exactly what was there before. It runs even when the test fails — an
// interrupted cleanup is how the pollution got there in the first place.
func restorePublic(t *testing.T, db *sql.DB, keep []relation) {
	t.Helper()
	ctx := context.Background()
	survivors := map[string]struct{}{}
	for _, r := range keep {
		survivors[r.name] = struct{}{}
	}
	var removed []string
	for _, r := range relationsInPublic(ctx, t, db) {
		if _, ok := survivors[r.name]; ok {
			continue
		}
		statement := `DROP TABLE IF EXISTS public."` + r.name + `" CASCADE`
		if r.isView {
			statement = `DROP VIEW IF EXISTS public."` + r.name + `" CASCADE`
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Errorf("cleanup: %s: %v", statement, err)
			continue
		}
		removed = append(removed, r.name)
	}
	sort.Strings(removed)
	t.Logf("cleanup: removed %d planted relations from public, leaving the %d that were there before", len(removed), len(keep))
	if left := relationsInPublic(ctx, t, db); len(left) != len(keep) {
		t.Errorf("cleanup: public still holds %d relations, want the %d it started with", len(left), len(keep))
	}
}

func contains(relations []relation, name string) bool {
	for _, r := range relations {
		if r.name == name {
			return true
		}
	}
	return false
}
