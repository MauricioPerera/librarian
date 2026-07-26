//go:build dualengine

package pgtestdb

// CONTRACT-29 — the isolation the dual-engine batteries run inside.
//
// THE PROBLEM THIS REPLACES. Every battery used to create a uniquely named
// SCHEMA and connect with `search_path=<schema>,public`. The `,public` was not
// an oversight: pgvector installs its `vector` TYPE in `public`, and a type is
// resolved through the search path, so dropping it makes the canonical
// `articles.embedding vector(1536)` column unbuildable:
//
//	SET search_path=probe_iso;         CREATE TABLE t (v vector(3));  -> ERROR: type "vector" does not exist
//	SET search_path=probe_iso,public;  CREATE TABLE t (v vector(3));  -> CREATE TABLE
//
// But that same fallback made the isolation depend on `public` being clean. A
// table left in `public` — by a manual probe, by an interrupted run, by anything
// — is VISIBLE from the battery, so store.EnsureSchema sees it through the
// fallback, concludes nothing is missing, and never creates the isolated table.
// The writes then land somewhere else and the battery fails with an error that
// has no visible relation to the cause. That happened twice in this project, and
// the second time cost a false diagnosis.
//
// THE MECHANISM. A DATABASE per run instead of a schema per run. `CREATE
// DATABASE` copies template1, so the new database's own `public` is born empty;
// pgvector is installed INTO that database, so `vector` resolves from `public`
// with no fallback to anyone else's namespace. The battery then runs with
// `search_path=public`, where `public` IS the run's private namespace. The
// property the contract asks for follows by construction: whatever is in the
// `public` of the DSN's database is in a DIFFERENT DATABASE and cannot be seen
// at all. Provision asserts it anyway (`publicIsEmpty`), because an isolation
// nobody checks is an isolation that quietly stops holding.
//
// CONCURRENCY. `go test ./...` runs packages concurrently, so internal/auth,
// internal/server and internal/store provision at the same time. Every database
// name carries a nanosecond clock, the OS pid and 4 crypto-random bytes, so two
// simultaneous provisions cannot collide.
//
// LEFTOVERS. The cleanup returned by Provision drops the database with
// `WITH (FORCE)`, and the batteries defer it, so it also runs when a test calls
// t.Fatal. What it cannot cover is a Ctrl-C or a panic — so every provision
// first SWEEPS databases of this same naming scheme whose embedded timestamp is
// older than staleAfter. The age guard is what keeps the sweep from deleting a
// sibling package's live database.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

const (
	// namePrefix marks every database this package creates, so the sweep can
	// recognise its own leftovers and nothing else's.
	namePrefix = "lbtest"

	// staleAfter is how old a leftover database must be before the sweep is
	// allowed to drop it. It has to comfortably exceed the longest battery, or
	// the sweep would delete a database another package is still using.
	staleAfter = 30 * time.Minute
)

// Provision creates a BRAND NEW PostgreSQL database on the server addressed by
// dsn, installs pgvector into it when the server offers the extension, and
// returns a store connected to it plus the cleanup that drops it.
//
// The returned store carries NO schema: applying one (store.EnsureSchema,
// compat.ApplySchema, or nothing at all) is the caller's business, because for
// some batteries that step is the thing under test.
//
// label is a short human tag that ends up in the database name so a leftover is
// traceable to the battery that created it; it is sanitised, not trusted.
//
// pgvector is installed only if `pg_available_extensions` offers it. That is
// deliberate: CONTRACT-23's battery points at a PostgreSQL that must NOT be able
// to resolve the `vector` type, and it has to keep provisioning normally there.
func Provision(t *testing.T, dsn, label string) (*compat.Store, func()) {
	t.Helper()
	ctx := context.Background()

	admin, err := compat.OpenPostgres(schema.PostgresVersion, dsn)
	if err != nil {
		t.Fatalf("postgres open (admin): %v", err)
	}
	// One connection is all the admin pool ever needs (CREATE DATABASE, the
	// sweep, DROP DATABASE), and several batteries hold one of these at a time.
	admin.DB.SetMaxOpenConns(1)

	sweepStale(ctx, t, admin.DB)

	name := uniqueName(label)
	scopedDSN, err := WithDatabase(dsn, name)
	if err != nil {
		_ = admin.Close()
		t.Fatalf("derive scoped dsn: %v", err)
	}
	// `public` is the run's OWN namespace inside its own database, and it is
	// where pgvector is about to be installed, so pinning to it is the whole
	// isolation — there is no second entry to fall back to.
	scopedDSN = withParameter(scopedDSN, "search_path", "public")
	if _, err := admin.DB.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		_ = admin.Close()
		t.Fatalf("create database %s: %v", name, err)
	}

	drop := func() {
		if _, err := admin.DB.ExecContext(context.Background(),
			`DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
		_ = admin.Close()
	}

	scoped, err := store.Open(compat.Postgres, scopedDSN)
	if err != nil {
		drop()
		t.Fatalf("postgres open (scoped): %v", err)
	}
	fail := func(format string, args ...any) {
		_ = scoped.Close()
		drop()
		t.Fatalf(format, args...)
	}

	// The isolation, asserted rather than assumed.
	var currentDatabase, currentSchema string
	if err := scoped.DB.QueryRowContext(ctx,
		`SELECT current_database(), current_schema()`).Scan(&currentDatabase, &currentSchema); err != nil {
		fail("current_database/current_schema: %v", err)
	}
	if currentDatabase != name {
		fail("current_database() = %q, want %q — the scoped connection is not isolated", currentDatabase, name)
	}
	if currentSchema != "public" {
		fail("current_schema() = %q, want \"public\" — the scoped connection is not where the schema will be built", currentSchema)
	}
	var strays int
	if err := scoped.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&strays); err != nil {
		fail("count public tables: %v", err)
	}
	if strays != 0 {
		fail("the fresh database %s already has %d table(s) in public — template1 is polluted, so this run would not be isolated", name, strays)
	}

	if err := installVector(ctx, scoped.DB); err != nil {
		fail("install pgvector in %s: %v", name, err)
	}

	return scoped, func() {
		_ = scoped.Close()
		drop()
	}
}

// installVector creates the pgvector extension in the CURRENT database when the
// server has it available. A server that does not offer it is not an error here:
// CONTRACT-23's battery needs exactly that case to keep working.
func installVector(ctx context.Context, db *sql.DB) error {
	var available bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')`).Scan(&available); err != nil {
		return fmt.Errorf("probe pg_available_extensions: %w", err)
	}
	if !available {
		return nil
	}
	if _, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return err
	}
	return nil
}

// --- naming ------------------------------------------------------------------

// uniqueName builds a database name that no concurrent provision can repeat and
// that the sweep can date. Shape:
//
//	lbtest_<unixnano>_<pid>_<8 hex chars>_<label>
//
// The nanosecond field is what sweepStale parses; the pid and the random suffix
// are what make two provisions in the same nanosecond (different processes, or
// a clock with coarse resolution) still distinct.
func uniqueName(label string) string {
	var entropy [4]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		// crypto/rand failing is not a condition a test fixture can repair, and
		// the nanosecond+pid pair is already close to unique on its own.
		entropy = [4]byte{}
	}
	return fmt.Sprintf("%s_%d_%d_%x_%s", namePrefix, time.Now().UnixNano(), os.Getpid(), entropy, sanitizeLabel(label))
}

// sanitizeLabel reduces a caller's tag to lower-case [a-z0-9_], capped short
// enough that the whole name stays inside PostgreSQL's 63-byte identifier limit.
// The name is interpolated into DDL (a database name cannot be a placeholder),
// so this is also what makes that interpolation safe.
func sanitizeLabel(label string) string {
	const maxLabel = 16
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		if b.Len() == maxLabel {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "battery"
	}
	return b.String()
}

// WithDatabase rewrites a PostgreSQL DSN to point at another database on the
// same server, leaving every other connection parameter alone. It is exported
// because a test that wants a SECOND connection into a provisioned database —
// with a different search_path, say, to reproduce the old mechanism — cannot
// derive it from the *compat.Store it was handed.
func WithDatabase(dsn, name string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", fmt.Errorf("dsn scheme %q is not a postgres URL", parsed.Scheme)
	}
	parsed.Path = "/" + name
	return parsed.String(), nil
}

// withParameter sets a pgx runtime parameter on a DSN.
func withParameter(dsn, key, value string) string {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	query := parsed.Query()
	query.Set(key, value)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// --- leftovers ---------------------------------------------------------------

// sweepStale drops databases of this package's naming scheme that are older than
// staleAfter — the residue of a run killed before its cleanup could run. Errors
// are logged and swallowed: a concurrent sweep that got there first is a normal
// outcome, not a reason to fail the battery.
func sweepStale(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT datname FROM pg_database WHERE datname LIKE $1`, namePrefix+`\_%`)
	if err != nil {
		t.Logf("sweep leftovers: list databases: %v", err)
		return
	}
	var stale []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Logf("sweep leftovers: scan: %v", err)
			break
		}
		if createdAt, ok := creationTime(name); ok && time.Since(createdAt) > staleAfter {
			stale = append(stale, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Logf("sweep leftovers: iterate: %v", err)
	}
	_ = rows.Close()

	for _, name := range stale {
		if _, err := db.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			t.Logf("sweep leftovers: drop %s: %v", name, err)
			continue
		}
		t.Logf("sweep leftovers: dropped %s, left behind by an interrupted run", name)
	}
}

// creationTime recovers the moment a database of this scheme was provisioned
// from its own name. It reports false for anything that does not parse, so a
// database that merely starts with the prefix is never dropped on a guess.
func creationTime(name string) (time.Time, bool) {
	parts := strings.Split(name, "_")
	if len(parts) < 5 || parts[0] != namePrefix {
		return time.Time{}, false
	}
	nanos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || nanos <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, nanos), true
}
