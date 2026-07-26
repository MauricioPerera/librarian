// Package store owns opening the database librarian runs on — SQLite/libSQL or
// PostgreSQL — and applying the canonical schema idempotently at startup.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// TargetFor returns the canonical compat.Target for an engine.
//
// CONTRACT-21 T2. It is the ONLY place in librarian that maps an engine onto a
// target, and it is deliberately not exported as a pair of package-level
// variables the way schema.SQLiteTarget/PostgresTarget are: those two are the
// DECLARATIONS used to compile DDL for a named engine (`--dump-schema`, the
// exportability tests), while this function is the RUNTIME choice, and the
// runtime choice must never be made anywhere except next to the connection it
// belongs to.
//
// An unknown engine is an error, never a default. Falling back to SQLite here
// is precisely the silent failure the contract forbids: the process would come
// up serving from an empty local file while the operator believed it was
// talking to PostgreSQL.
func TargetFor(engine compat.Engine) (compat.Target, error) {
	switch engine {
	case compat.SQLite:
		return schema.SQLiteTarget, nil
	case compat.Postgres:
		return schema.PostgresTarget, nil
	default:
		return compat.Target{}, fmt.Errorf("unsupported engine %q (expected %q or %q)", engine, compat.SQLite, compat.Postgres)
	}
}

// Open opens the database for the chosen engine and returns a compat Store
// bound to it. dsn is a file path for SQLite and a PostgreSQL connection URL
// for PostgreSQL; callers own Close.
//
// THE TARGET AND THE CONNECTION COME FROM THE SAME PLACE, BY CONSTRUCTION.
// compat.OpenStore takes the target and picks the driver FROM it, so the
// returned Store.Target is necessarily the target this function resolved — a
// Store carrying a SQLite target over a PostgreSQL pool (which compiles, and
// then emits `?` against PostgreSQL) is not expressible through this API. That
// is why nothing in librarian composes a compat.Store literal any more: the old
// store.FromDB and the `&compat.Store{Target: schema.SQLiteTarget, DB: deps.DB}`
// in server.NewMux were exactly that hazard, spelled out by hand in two places.
func Open(engine compat.Engine, dsn string) (*compat.Store, error) {
	target, err := TargetFor(engine)
	if err != nil {
		return nil, err
	}
	return compat.OpenStore(target, dsn)
}

// EnsureSchema applies the canonical librarian schema, creating ONLY the
// tables that do not exist yet. It is idempotent (a restart on a database that
// already has every table is a no-op) AND incremental (a restart after a code
// change that adds a NEW content type creates just that table, leaving every
// existing table and its data untouched).
//
// This matters for a real deployed instance: compat's ApplySchema compiles
// and runs a plain CREATE TABLE per table with no "IF NOT EXISTS" — applying
// the FULL canonical schema against a database that already has some (but not
// all) of its tables fails outright on the first pre-existing table, which
// would crash the service on startup after any schema-adding change (found
// verifying CONTRACT-11, which adds a second content type to an already
// deployed database — a full-schema re-apply would have taken production
// down). Filtering to only the missing tables before calling ApplySchema is
// the fix: it is the exact same DDL for each new table, just scoped to what
// is actually absent.
// CONTRACT-13 changes ONE thing here, and it is the heart of that contract:
// `want` is no longer schema.Build() (the code half) but the COMPOSED canonical
// schema — code PLUS the tables derived from the dynamic content-type
// definitions persisted in the database. Two distinct bugs are fixed by that
// single change, and they compound:
//
//	(a) the incremental apply would consider every dynamic table "not wanted"
//	    and therefore never (re)create one that is genuinely missing;
//	(b) far worse, writeFullSchemaMetadata below would REWRITE __compat_schema
//	    with a schema that omits the dynamic tables — on EVERY restart. Since
//	    compat's InspectSchema prefers that metadata over the live catalog, the
//	    next restart would then believe those tables never existed, and
//	    `--dump-schema`/`compat copy` would export the instance without them or
//	    their data.
//
// CanonicalSchema (below) is careful about ordering: it reads the definitions
// BEFORE anything is applied, and it treats "the registry table does not exist
// yet" (a fresh database, or one created by a pre-CONTRACT-13 binary) as
// "provably zero dynamic types" — which is correct, not a silent omission,
// because a definition can only live in that table. That keeps EnsureSchema a
// SINGLE pass: composing first and applying once means the reduced metadata
// compat writes inside ApplySchema is always overwritten with the full composed
// schema before anything reads it again.
// CONTRACT-23 T1: EnsureSchema is EnsureSchemaFor with every optional capability
// ENABLED — the schema of every installation deployed before that contract, and
// what every existing caller and test means. The startup path passes the
// installation's declared capabilities through EnsureSchemaFor.
func EnsureSchema(ctx context.Context, store *compat.Store) error {
	return EnsureSchemaFor(ctx, store, schema.Capabilities{})
}

// EnsureSchemaFor is EnsureSchema for a DECLARED set of capabilities
// (CONTRACT-23). Before it composes or applies anything it runs the COHERENCE
// GUARD (T3): the declaration is compared against what the physical tables
// actually have, and a disagreement stops the service instead of booting it onto
// a schema that does not describe its own database.
func EnsureSchemaFor(ctx context.Context, store *compat.Store, caps schema.Capabilities) error {
	// CONTRACT-23 T3 runs FIRST, before requireVectorType and before anything is
	// composed: a mismatch here means every later step would be operating on the
	// wrong schema, and the operator must be told what is wrong rather than
	// shown its downstream symptom.
	if err := requireCoherentCapabilities(ctx, store, caps); err != nil {
		return err
	}
	// CONTRACT-21 T2: the first real obstacle of a clean PostgreSQL install.
	// It is checked BEFORE anything is composed or applied so the message is the
	// first thing the operator sees, not a driver error buried in a CREATE TABLE.
	//
	// CONTRACT-23: it is now conditional on the capability, and that single `if`
	// is what closes hueco 5 — an installation that does not declare the vector
	// capability has no vector(N) column to create, so it has no reason to demand
	// the extension, and it can run on a managed PostgreSQL that does not offer it.
	if caps.Vector() {
		if err := requireVectorType(ctx, store); err != nil {
			return err
		}
	}
	want, err := CanonicalSchemaFor(ctx, store, caps)
	if err != nil {
		return err
	}

	missing, err := missingTables(ctx, store, want)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		if err := store.ApplySchema(ctx, compat.Schema{Tables: missing}); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}
	// CONTRACT-19 adds VIEWS to the canonical schema (the JOINs internal/auth
	// used to hand-write). They are recreated on every start rather than
	// created only when missing, for two reasons: a view is a stateless
	// DEFINITION (dropping and recreating it destroys no data and is what makes
	// a changed definition take effect), and an ALREADY DEPLOYED database has no
	// missing TABLE, so a "create only what is missing" pass gated on the table
	// diff would never create the views at all and every auth read would fail.
	if err := applyViews(ctx, store, want.Views); err != nil {
		return err
	}
	// ApplySchema above records compat's own "canonical_schema" metadata (the
	// __compat_schema table InspectSchema prefers over introspecting the live
	// catalog — see InspectSchema's source) — but it records it for the
	// REDUCED schema passed in (only the tables just created), not the full
	// canonical schema. Left as-is, the NEXT restart would read that narrow
	// metadata back, believe every pre-existing table (users, articles, ...)
	// is missing again, and crash trying to re-CREATE them. Overwrite that row
	// with the full `want` schema so InspectSchema's canonical-metadata view
	// stays accurate after an incremental apply, not just after a from-scratch
	// one. Same table/key/upsert shape compat's own writeSchemaMetadata uses
	// (unexported, so replicated here rather than touching compat).
	if err := writeFullSchemaMetadata(ctx, store.Target.Engine, store.DB, want); err != nil {
		return fmt.Errorf("record full schema metadata: %w", err)
	}
	return nil
}

// requireVectorType fails, legibly, when the chosen engine is PostgreSQL and the
// `vector` type of the pgvector extension cannot be resolved by this session.
//
// WHY IT EXISTS (CONTRACT-21 T2). articles.embedding is declared vector(1536)
// (CONTRACT-05). On a clean PostgreSQL without pgvector, ApplySchema dies on
// that column with `ERROR: type "vector" does not exist (SQLSTATE 42704)` — a
// raw driver error naming neither the extension, nor the column, nor what to do
// about it, emitted at the very first boot of a fresh install. The whole promise
// of this series is that moving to PostgreSQL is an operational step, so the
// first operational step has to say what it needs.
//
// WHY `to_regtype`, AND NOT `pg_extension`. Asking whether the extension is
// INSTALLED is the wrong question, and the contract's red-team asks it directly:
// pgvector installed into a schema that is not on this session's `search_path`
// leaves `vector` unresolvable for exactly the same CREATE TABLE. to_regtype
// resolves the name the way the parser would, so it answers the question that
// actually matters — "can THIS session name this type?" — and its NULL answer is
// not an error, so a missing extension does not abort a transaction.
//
// It is a per-engine precondition and it lives here on purpose: this is the file
// that owns choosing the engine, so it is the one place where knowing which one
// was chosen is not a leak.
func requireVectorType(ctx context.Context, store *compat.Store) error {
	if store.Target.Engine != compat.Postgres {
		return nil
	}
	var present bool
	if err := store.DB.QueryRowContext(ctx, `SELECT to_regtype('vector') IS NOT NULL`).Scan(&present); err != nil {
		return fmt.Errorf("check for the pgvector extension: %w", err)
	}
	if present {
		return nil
	}
	return fmt.Errorf(
		"the pgvector extension is required on PostgreSQL and its `vector` type is not resolvable by this connection: " +
			"librarian's canonical schema declares articles.embedding as vector(1536) (CONTRACT-05), so the schema cannot be created without it. " +
			"Run `CREATE EXTENSION IF NOT EXISTS vector;` in the target database as a superuser, " +
			"and if it is installed into a schema other than the one this connection uses, make that schema visible on the connection's search_path")
}

// --- CONTRACT-23: the capability of an INSTALLATION, read from the base -------

// articlesTable / embeddingColumn name the one table and the one column the
// vector capability adds. Spelled once so the probe, the guard and the schema
// declaration cannot drift apart.
const (
	articlesTable   = "articles"
	embeddingColumn = "embedding"
)

// InstalledCapabilities reports the capabilities THIS DATABASE WAS CREATED WITH,
// read from the physical catalog. installed=false means the installation has not
// made the choice yet (the articles table does not exist), which is the only
// state in which a declaration is free to decide anything.
//
// IT ASKS THE BASE, NEVER THE METADATA. The obvious shortcut — read the
// canonical schema back out of __compat_schema and look for the column — is
// wrong for the reason this project has repeated since CONTRACT-13: that row is
// a RECORD of what some process believed, written by the very code path whose
// belief is in question, and EnsureSchema rewrites it on every boot. If a
// mis-declared instance ever booted, the metadata would already agree with the
// mistake while the table did not. The physical column is the only fact.
//
// It is the direct answer to the contract's first red-team question — what
// happens to an installation that ALREADY exists with the column, which is every
// installation today: it reports the capability as ENABLED, which matches the
// default declaration, so nothing changes for any of them.
func InstalledCapabilities(ctx context.Context, store *compat.Store) (caps schema.Capabilities, installed bool, err error) {
	present, err := store.TableExists(ctx, articlesTable)
	if err != nil {
		return schema.Capabilities{}, false, fmt.Errorf("probe for the %s table: %w", articlesTable, err)
	}
	if !present {
		return schema.Capabilities{}, false, nil
	}
	hasColumn, err := columnExists(ctx, store, articlesTable, embeddingColumn)
	if err != nil {
		return schema.Capabilities{}, false, err
	}
	return schema.Capabilities{VectorDisabled: !hasColumn}, true, nil
}

// installedCapabilitiesOrDefault is InstalledCapabilities for the callers that
// only ever run against an already-created installation (the content-type
// writers): a database with no articles table has made no choice, so the default
// applies and the composition is the one every pre-CONTRACT-23 caller produced.
func installedCapabilitiesOrDefault(ctx context.Context, store *compat.Store) (schema.Capabilities, error) {
	caps, installed, err := InstalledCapabilities(ctx, store)
	if err != nil {
		return schema.Capabilities{}, err
	}
	if !installed {
		return schema.Capabilities{}, nil
	}
	return caps, nil
}

// columnExists reports whether a physical table has a physical column, asking
// each engine's own catalog.
//
// This is the one place in librarian that branches on the engine to READ, and it
// does so for the same reason store.go owns requireVectorType: a catalog is not
// portable (SQLite has no information_schema; PostgreSQL has no PRAGMA), compat
// exposes TableExists but no column-level equivalent, and this contract does not
// touch compat. On PostgreSQL the lookup is restricted to current_schema(),
// which is exactly where this code's unqualified CREATE TABLE lands — the same
// scoping compat.TableExists uses, so the two probes answer about one table.
func columnExists(ctx context.Context, store *compat.Store, table, column string) (bool, error) {
	switch store.Target.Engine {
	case compat.Postgres:
		var present bool
		err := store.DB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			   WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2)`,
			table, column).Scan(&present)
		if err != nil {
			return false, fmt.Errorf("inspect the columns of %s: %w", table, err)
		}
		return present, nil
	case compat.SQLite:
		// PRAGMA table_info does not accept a bound parameter for its argument,
		// so the name is quoted with the same rule compat uses for identifiers.
		// Both names are compile-time constants of this package, never input.
		rows, err := store.DB.QueryContext(ctx, `SELECT name FROM pragma_table_info(`+quoteLiteral(table)+`)`)
		if err != nil {
			return false, fmt.Errorf("inspect the columns of %s: %w", table, err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return false, fmt.Errorf("inspect the columns of %s: %w", table, err)
			}
			if name == column {
				return true, rows.Err()
			}
		}
		return false, rows.Err()
	default:
		return false, fmt.Errorf("unsupported engine %q", store.Target.Engine)
	}
}

// quoteLiteral renders a single-quoted SQL string literal (embedded quote
// doubled), for the one argument that cannot be bound: the table name of
// pragma_table_info. Its inputs are compile-time constants of this package.
func quoteLiteral(text string) string {
	return `'` + strings.ReplaceAll(text, `'`, `''`) + `'`
}

// requireCoherentCapabilities implements CONTRACT-23 T3: the service does not
// start when the declared capabilities and the ones this installation was
// CREATED with disagree.
//
// WHY IT CANNOT JUST APPLY THE NEW DECLARATION. EnsureSchema creates only
// MISSING tables and never alters an existing one — a deliberate decision that
// is what makes a restart safe. So on an installation whose `articles` table
// already exists, changing this declaration changes NOTHING physical:
//
//   - enabling it later does not add the column, because `articles` is not
//     missing;
//   - disabling it later does not drop it, for the same reason.
//
// What WOULD change is the canonical schema the process believes it has. That
// belief is not decorative: it is what compat compiles the read routines from,
// what EnsureSchema writes into __compat_schema, and what `--dump-schema` hands
// `compat copy` as the schema_ref of an export. A process running on a schema
// that disagrees with its own tables breaks reads, writes and the export — not
// at boot, where it would be found, but later and quietly. Refusing to start is
// the only outcome that keeps the failure where it can be understood.
func requireCoherentCapabilities(ctx context.Context, store *compat.Store, declared schema.Capabilities) error {
	installed, exists, err := InstalledCapabilities(ctx, store)
	if err != nil {
		return err
	}
	if !exists || installed.Vector() == declared.Vector() {
		return nil
	}
	if declared.Vector() {
		return fmt.Errorf(
			"this installation was created WITHOUT the vector capability (its %s table has no %q column) but the configuration now declares it ENABLED (%s=enabled): "+
				"refusing to start. The choice is made at the first boot and is IRREVERSIBLE — the schema is applied by creating MISSING tables, never by altering an existing one, "+
				"so enabling it now would NOT add the column; the service would run believing in a schema its own tables do not have, which breaks the export and the writes silently. "+
				"Set %s=disabled to keep serving this installation, or create a NEW installation with the capability enabled and move the data into it with `compat copy`",
			articlesTable, embeddingColumn, VectorVarName, VectorVarName)
	}
	return fmt.Errorf(
		"this installation was created WITH the vector capability (its %s table has the %q column) but the configuration now declares it DISABLED (%s=disabled): "+
			"refusing to start. The choice is made at the first boot and is IRREVERSIBLE — the schema is applied by creating MISSING tables, never by altering an existing one, "+
			"so disabling it now would NOT drop the column; the service would run believing in a schema its own tables do not match, which breaks the export and the writes silently. "+
			"Set %s=enabled to keep serving this installation, or create a NEW installation with the capability disabled and move the data into it with `compat copy`",
		articlesTable, embeddingColumn, VectorVarName, VectorVarName)
}

// VectorVarName is the name of the environment variable that declares the vector
// capability, quoted in the guard's message: an error about an irreversible
// choice has to name the knob the operator actually turns. It is a literal
// rather than config.VectorVar because internal/config is the ENVIRONMENT layer
// and sits above this one; importing it downward to read a string constant would
// invert the layering for no benefit. config_test asserts the two agree.
const VectorVarName = "LIBRARIAN_VECTOR"

// applyViews recreates every canonical view: DROP VIEW IF EXISTS followed by the
// CREATE VIEW compat compiles for this engine, all in one transaction.
//
// compat has no CompileDropView (CompileDDL only ever creates, and DROP TABLE is
// its own entry point), so the drop is the one statement written by hand here.
// It is safe to do so: `DROP VIEW IF EXISTS "name"` is byte-identical on SQLite
// and PostgreSQL, the name is a compile-time constant from schema.Build() and it
// is quoted with the same rule compat uses, so it introduces no divergence and
// no injection surface.
//
// store.ApplySchema is deliberately NOT used for this: it would overwrite the
// __compat_schema metadata with a views-only schema. EnsureSchema writes the
// FULL canonical schema right after this call, which is what keeps that metadata
// truthful.
func applyViews(ctx context.Context, store *compat.Store, views []compat.View) error {
	if len(views) == 0 {
		return nil
	}
	statements, err := compat.CompileDDL(store.Target, compat.Schema{Views: views})
	if err != nil {
		return fmt.Errorf("compile views: %w", err)
	}
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin view tx: %w", err)
	}
	defer tx.Rollback()
	for _, view := range views {
		if _, err := tx.ExecContext(ctx, `DROP VIEW IF EXISTS `+quoteIdentifier(view.Name)); err != nil {
			return fmt.Errorf("drop view %s: %w", view.Name, err)
		}
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create view: %w", err)
		}
	}
	return tx.Commit()
}

// quoteIdentifier applies the same identifier quoting compat emits (double
// quotes, embedded quote doubled), so an embedded quote or semicolon stays part
// of the identifier and can never introduce a second statement. It exists only
// for the DROP VIEW above, the single DDL form compat does not compile.
func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// execer is the tiny shared surface of *sql.DB and *sql.Tx that
// writeFullSchemaMetadata needs, so the SAME metadata write is reused both by
// EnsureSchema (on the pool) and by CreateContentType (inside its single atomic
// transaction).
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// writeFullSchemaMetadata upserts the full canonical schema into compat's own
// __compat_schema metadata table, replacing whatever a prior (possibly
// reduced) ApplySchema call left there.
//
// CONTRACT-21 T1: the bind markers come from compat.Placeholder (via dual.Bind),
// so the same statement is emitted with `?` on SQLite and `$1, $2` on
// PostgreSQL. The `ON CONFLICT (target) DO UPDATE SET col = excluded.col` form
// is kept rather than replaced: it is byte-identical on both engines (SQLite
// 3.24+ and PostgreSQL 9.5+ accept exactly this spelling, and it is the shape
// compat's own writeSchemaMetadata uses), and unlike the set-replacement inserts
// of CONTRACT-19/20 the conflict here is not avoidable by construction — the row
// genuinely exists on every boot after the first.
func writeFullSchemaMetadata(ctx context.Context, engine compat.Engine, db execer, want compat.Schema) error {
	payload, err := json.Marshal(want)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO "__compat_schema" ("key", "value") VALUES (`+dual.Bind(engine, 1)+`, `+dual.Bind(engine, 2)+`)
		 ON CONFLICT("key") DO UPDATE SET "value" = excluded."value"`,
		"canonical_schema", string(payload),
	)
	return err
}

// missingTables returns the tables of want that are not yet present in the
// store's live catalog, in want's declared order (so a table's FK targets that
// are themselves also missing are still created before it, same as a
// from-scratch apply).
func missingTables(ctx context.Context, store *compat.Store, want compat.Schema) ([]compat.Table, error) {
	inspection, err := store.InspectSchema(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect schema: %w", err)
	}
	present := make(map[string]struct{}, len(inspection.Schema.Tables))
	for _, table := range inspection.Schema.Tables {
		present[table.Name] = struct{}{}
	}
	var missing []compat.Table
	for _, table := range want.Tables {
		if _, ok := present[table.Name]; !ok {
			missing = append(missing, table)
		}
	}
	return missing, nil
}

// SeedCatalogs inserts the fixed role and permission catalogs (schema.Roles,
// schema.Permissions) into the live database if a row for a given name does not
// already exist. It is idempotent: running it twice on the same file neither
// duplicates rows nor fails. The id column is left to its DEFAULT
// gen_random_uuid() — UUIDs are not generated in Go.
//
// All SQL is parameterized; the only interpolated value is the table name,
// which is a fixed internal constant, not user input.
//
// CONTRACT-21 T1: it takes a *compat.Store rather than a bare *sql.DB, for the
// same reason every other signature in this project did: the bind marker depends
// on the engine, and a *sql.DB cannot answer which engine it is.
func SeedCatalogs(ctx context.Context, store *compat.Store) error {
	if err := seedNames(ctx, store, "roles", schema.Roles); err != nil {
		return fmt.Errorf("seed roles: %w", err)
	}
	if err := seedNames(ctx, store, "permissions", schema.Permissions); err != nil {
		return fmt.Errorf("seed permissions: %w", err)
	}
	// CONTRACT-12: taxonomies is a third code-fixed catalog, seeded exactly like
	// roles/permissions via the same idempotent seedNames helper (INSERT ...
	// ON CONFLICT(name) DO NOTHING). This is the ONLY change to this file for
	// CONTRACT-12 — EnsureSchema and its incremental-apply machinery are left
	// entirely untouched.
	if err := seedNames(ctx, store, "taxonomies", schema.Taxonomies); err != nil {
		return fmt.Errorf("seed taxonomies: %w", err)
	}
	return nil
}

// seedNames inserts each name with INSERT ... ON CONFLICT(name) DO NOTHING,
// supported by both SQLite (3.24+) and PostgreSQL in exactly this spelling.
// table is a fixed internal constant (never user input), so it is interpolated
// into the statement text; name is always bound as a parameter, through
// compat's bind marker for the engine.
//
// The id column is deliberately left to its DEFAULT gen_random_uuid() here,
// unlike every application-level insert of CONTRACT-19/20/21: these are the
// three FIXED catalogs, seeded once at boot, and nothing reads the id back at
// insert time. Both engines provide the function (PostgreSQL 13+ has it
// built in; compat supplies it on SQLite), so it is dual-engine as it stands.
func seedNames(ctx context.Context, store *compat.Store, table string, names []string) error {
	stmt := `INSERT INTO ` + table + ` (name) VALUES (` + dual.Bind(store.Target.Engine, 1) + `) ON CONFLICT(name) DO NOTHING`
	for _, name := range names {
		if _, err := store.DB.ExecContext(ctx, stmt, name); err != nil {
			return fmt.Errorf("insert %q: %w", name, err)
		}
	}
	return nil
}
