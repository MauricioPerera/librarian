//go:build dualengine

package auth_test

// CONTRACT-19 T3 — the battery that gives the contract its meaning: the SAME
// public functions of internal/auth, exercised against a REAL SQLite file and a
// REAL PostgreSQL 17 server, must produce the SAME observable results.
//
// It is excluded from the default suite by the build tag `dualengine` (same
// pattern CONTRACT-04 established with `exportfixture`) because it needs a live
// PostgreSQL. Run it with:
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineAuth -count=1 -v ./internal/auth
//
// Without COMPAT_POSTGRES_DSN the test SKIPS rather than passing vacuously: a
// green run that never touched PostgreSQL would prove nothing.
//
// How equality is proved. Each engine runs the identical scenario, which appends
// one line per OBSERVATION to a transcript: values returned, error sentinels
// hit, and — critically — the ORDER of every listing. The two transcripts must
// be byte-identical. Anything intrinsically per-run (uuids, key secrets,
// timestamps) is deliberately NOT recorded; what is recorded is everything a
// caller of this package can actually observe about behavior. A divergence in
// listing order, in a NULL test, or in which error came back therefore fails
// here, which is exactly the class of bug that survives a test comparing only
// SETS of rows.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// pgDSNEnv is the environment variable carrying the PostgreSQL connection
// string. The password is never printed by this test.
const pgDSNEnv = "COMPAT_POSTGRES_DSN"

// TestDualEngineAuth is the whole battery: it builds the librarian schema on
// both engines, runs the same scenario on each, and requires the transcripts to
// match line for line.
func TestDualEngineAuth(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteStore, closeSQLite := openSQLiteEngine(t)
	defer closeSQLite()
	pgStore, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	sqliteTranscript := runAuthScenario(t, "sqlite", sqliteStore)
	pgTranscript := runAuthScenario(t, "postgres", pgStore)

	t.Logf("transcript (%d lines, identical on both engines):\n%s",
		len(sqliteTranscript), strings.Join(sqliteTranscript, "\n"))

	if len(sqliteTranscript) != len(pgTranscript) {
		t.Fatalf("transcript length differs: sqlite=%d postgres=%d", len(sqliteTranscript), len(pgTranscript))
	}
	diverged := 0
	for i := range sqliteTranscript {
		if sqliteTranscript[i] != pgTranscript[i] {
			diverged++
			t.Errorf("line %d diverges:\n  sqlite  : %s\n  postgres: %s", i+1, sqliteTranscript[i], pgTranscript[i])
		}
	}
	if diverged == 0 {
		t.Logf("OK: %d observations identical on SQLite and PostgreSQL 17", len(sqliteTranscript))
	}
}

// --- engine fixtures ---------------------------------------------------------

// openSQLiteEngine builds the SQLite side through the PRODUCTION path
// (store.Open → store.EnsureSchema → store.SeedCatalogs), so the battery also
// proves that the real startup path creates the CONTRACT-19 views.
func openSQLiteEngine(t *testing.T) (*compat.Store, func()) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "dual.db"))
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	ctx := context.Background()
	if err := store.EnsureSchema(ctx, st); err != nil {
		t.Fatalf("sqlite ensure schema: %v", err)
	}
	if err := store.SeedCatalogs(ctx, st.DB); err != nil {
		t.Fatalf("sqlite seed: %v", err)
	}
	return st, func() { _ = st.Close() }
}

// openPostgresEngine builds the PostgreSQL side in a FRESH, uniquely named
// schema so a run can never collide with leftovers or with another consumer of
// the same database, and drops it afterwards.
//
// The schema is applied with compat's own ApplySchema over schema.Build() — the
// canonical schema, tables AND views. store.EnsureSchema is not reused here
// because its metadata upsert (and store.SeedCatalogs) are still SQLite-only
// statements; migrating internal/store is CONTRACT-21 and is deliberately not
// anticipated. The catalogs are seeded here with compat.Placeholder, the same
// primitive internal/auth uses.
func openPostgresEngine(t *testing.T, dsn string) (*compat.Store, func()) {
	t.Helper()
	ctx := context.Background()
	schemaName := fmt.Sprintf("librarian_c19_%d", time.Now().UnixNano())

	admin, err := compat.OpenPostgres(schema.PostgresVersion, dsn)
	if err != nil {
		t.Fatalf("postgres open (admin): %v", err)
	}
	if _, err := admin.DB.ExecContext(ctx, `CREATE SCHEMA "`+schemaName+`"`); err != nil {
		_ = admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	scoped, err := compat.OpenPostgres(schema.PostgresVersion, withSearchPath(dsn, schemaName))
	if err != nil {
		t.Fatalf("postgres open (scoped): %v", err)
	}
	var current string
	if err := scoped.DB.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	if current != schemaName {
		t.Fatalf("current_schema() = %q, want %q — the scoped connection is not isolated", current, schemaName)
	}

	if err := scoped.ApplySchema(ctx, schema.Build()); err != nil {
		t.Fatalf("postgres apply schema: %v", err)
	}
	seedPostgresCatalogs(t, scoped)

	return scoped, func() {
		_ = scoped.Close()
		if _, err := admin.DB.ExecContext(context.Background(), `DROP SCHEMA "`+schemaName+`" CASCADE`); err != nil {
			t.Logf("drop schema %s: %v", schemaName, err)
		}
		_ = admin.Close()
	}
}

// withSearchPath appends the pgx runtime parameter that pins every connection of
// the pool to one schema, so unqualified CREATE TABLE / SELECT land there.
//
// `public` is kept as a SECOND entry, and only as a second entry: the pgvector
// extension (needed by the articles.embedding vector(1536) column of the
// canonical schema) installs its `vector` TYPE in public, and a type is resolved
// through the search path. The run's own schema stays first, so every
// unqualified CREATE TABLE and every unqualified read still lands in it —
// current_schema() is asserted right after connecting.
func withSearchPath(dsn, schemaName string) string {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "search_path=" + schemaName + ",public"
}

// seedPostgresCatalogs inserts the fixed role and permission catalogs on the
// PostgreSQL side. The id is left to the column DEFAULT gen_random_uuid(), which
// compat compiles natively for PostgreSQL.
func seedPostgresCatalogs(t *testing.T, st *compat.Store) {
	t.Helper()
	ctx := context.Background()
	insert := func(table string, names []string) {
		statement := `INSERT INTO "` + table + `" ("name") VALUES (` + compat.Placeholder(compat.Postgres, 1) + `)`
		for _, name := range names {
			if _, err := st.DB.ExecContext(ctx, statement, name); err != nil {
				t.Fatalf("seed %s %q: %v", table, name, err)
			}
		}
	}
	insert("roles", schema.Roles)
	insert("permissions", schema.Permissions)
	insert("taxonomies", schema.Taxonomies)
}

// --- the scenario ------------------------------------------------------------

// transcript accumulates one line per observation.
type transcript struct {
	t      *testing.T
	engine string
	lines  []string
}

func (tr *transcript) add(format string, args ...any) {
	tr.lines = append(tr.lines, fmt.Sprintf(format, args...))
}

// errName reduces an error to a stable, engine-independent name: the sentinel it
// wraps, or "<none>" / "<other>". The raw message is deliberately not recorded —
// a driver-specific message would differ between engines for reasons that are
// not behavior.
func errName(err error) string {
	switch {
	case err == nil:
		return "<none>"
	case errors.Is(err, auth.ErrInvalidCredentials):
		return "ErrInvalidCredentials"
	case errors.Is(err, auth.ErrUserNotFound):
		return "ErrUserNotFound"
	case errors.Is(err, auth.ErrUnknownRole):
		return "ErrUnknownRole"
	case errors.Is(err, auth.ErrInvalidStatus):
		return "ErrInvalidStatus"
	case errors.Is(err, auth.ErrRoleNotFound):
		return "ErrRoleNotFound"
	case errors.Is(err, auth.ErrUnknownPermission):
		return "ErrUnknownPermission"
	case errors.Is(err, auth.ErrAPIKeyRejected):
		return "ErrAPIKeyRejected"
	default:
		return "<other>"
	}
}

// runAuthScenario exercises every public function of internal/auth in a fixed
// order and returns the transcript of what was observed.
func runAuthScenario(t *testing.T, engine string, st *compat.Store) []string {
	t.Helper()
	ctx := context.Background()
	tr := &transcript{t: t, engine: engine}

	roleID := func(name string) string {
		statement := `SELECT "id" FROM "roles" WHERE "name" = ` + compat.Placeholder(st.Target.Engine, 1)
		var id string
		if err := st.DB.QueryRowContext(ctx, statement, name).Scan(&id); err != nil {
			t.Fatalf("[%s] lookup role %q: %v", engine, name, err)
		}
		return id
	}

	// --- 1. create a user and verify its credentials -------------------------
	zeta, err := auth.CreateUser(ctx, st, "zeta@example.com", "correct-horse", []string{"author"})
	if err != nil {
		t.Fatalf("[%s] create zeta: %v", engine, err)
	}
	tr.add("createUser zeta email=%s roles=%v", zeta.Email, zeta.Roles)

	alpha, err := auth.CreateUser(ctx, st, "alpha@example.com", "s3cret", []string{"editor", "administrator"})
	if err != nil {
		t.Fatalf("[%s] create alpha: %v", engine, err)
	}
	tr.add("createUser alpha email=%s roles=%v", alpha.Email, alpha.Roles)

	got, err := auth.VerifyCredentials(ctx, st, "alpha@example.com", "s3cret")
	tr.add("verify alpha correct err=%s email=%s roles=%v", errName(err), userEmail(got), userRoles(got))

	_, err = auth.VerifyCredentials(ctx, st, "alpha@example.com", "WRONG")
	tr.add("verify alpha wrong err=%s", errName(err))

	_, errMissing := auth.VerifyCredentials(ctx, st, "ghost@example.com", "WRONG")
	tr.add("verify ghost err=%s", errName(errMissing))

	_, errWrong := auth.VerifyCredentials(ctx, st, "alpha@example.com", "WRONG")
	tr.add("verify messages-identical=%t", errWrong.Error() == errMissing.Error())

	// --- 2. listing, where the ORDER matters --------------------------------
	users, err := auth.ListUsers(ctx, st)
	if err != nil {
		t.Fatalf("[%s] list users: %v", engine, err)
	}
	tr.add("listUsers err=%s order=%v", errName(err), userEmails(users))
	for _, u := range users {
		tr.add("listUsers row email=%s status=%s roles=%v", u.Email, u.Status, u.Roles)
	}

	record, found, err := auth.GetUser(ctx, st, alpha.ID)
	tr.add("getUser alpha found=%t err=%s email=%s status=%s roles=%v",
		found, errName(err), record.Email, record.Status, record.Roles)

	_, found, err = auth.GetUser(ctx, st, "11111111-1111-1111-1111-111111111111")
	tr.add("getUser missing-uuid found=%t err=%s", found, errName(err))
	_, found, err = auth.GetUser(ctx, st, "not-a-uuid")
	tr.add("getUser malformed found=%t err=%s", found, errName(err))

	// --- 3. status transitions and the CHECK constraint ----------------------
	tr.add("updateStatus suspended err=%s", errName(auth.UpdateUserStatus(ctx, st, alpha.ID, "suspended")))
	_, err = auth.VerifyCredentials(ctx, st, "alpha@example.com", "s3cret")
	tr.add("verify suspended-with-correct-password err=%s", errName(err))
	tr.add("updateStatus banned err=%s", errName(auth.UpdateUserStatus(ctx, st, alpha.ID, "banned")))
	tr.add("updateStatus missing-id err=%s",
		errName(auth.UpdateUserStatus(ctx, st, "11111111-1111-1111-1111-111111111111", "active")))
	record, _, _ = auth.GetUser(ctx, st, alpha.ID)
	tr.add("status after rejected values=%s", record.Status)
	tr.add("updateStatus active err=%s", errName(auth.UpdateUserStatus(ctx, st, alpha.ID, "active")))
	_, err = auth.VerifyCredentials(ctx, st, "alpha@example.com", "s3cret")
	tr.add("verify reactivated err=%s", errName(err))

	// The CHECK on users.status must reject the same value on both engines when
	// the write bypasses auth's own validation.
	rawStatus := `UPDATE "users" SET "status" = ` + compat.Placeholder(st.Target.Engine, 1) +
		` WHERE "id" = ` + compat.Placeholder(st.Target.Engine, 2)
	_, checkErr := st.DB.ExecContext(ctx, rawStatus, "banned", alpha.ID)
	tr.add("schema CHECK rejects raw banned status=%t", checkErr != nil)

	// --- 4. role replacement (must REPLACE, not accumulate) ------------------
	tr.add("setUserRoles replace err=%s", errName(auth.SetUserRoles(ctx, st, alpha.ID, []string{"author", "contributor"})))
	record, _, _ = auth.GetUser(ctx, st, alpha.ID)
	tr.add("roles after replace=%v", record.Roles)

	tr.add("setUserRoles unknown err=%s", errName(auth.SetUserRoles(ctx, st, alpha.ID, []string{"author", "superadmin"})))
	record, _, _ = auth.GetUser(ctx, st, alpha.ID)
	tr.add("roles after unknown-rejected=%v", record.Roles)

	tr.add("setUserRoles duplicates err=%s",
		errName(auth.SetUserRoles(ctx, st, alpha.ID, []string{"author", "author", "author"})))
	record, _, _ = auth.GetUser(ctx, st, alpha.ID)
	tr.add("roles after duplicates=%v", record.Roles)

	tr.add("setUserRoles empty err=%s", errName(auth.SetUserRoles(ctx, st, alpha.ID, nil)))
	record, _, _ = auth.GetUser(ctx, st, alpha.ID)
	tr.add("roles after empty=%v len=%d", record.Roles, len(record.Roles))

	tr.add("setUserRoles missing-user err=%s",
		errName(auth.SetUserRoles(ctx, st, "11111111-1111-1111-1111-111111111111", []string{"editor"})))

	// --- 5. role permissions: grant, replace, atomicity ----------------------
	tr.add("setRolePermissions grant err=%s",
		errName(auth.SetRolePermissions(ctx, st, "editor", []string{"content.create", "content.update"})))
	perms, found, err := auth.RolePermissions(ctx, st, "editor")
	tr.add("rolePermissions editor found=%t err=%s order=%v", found, errName(err), perms)

	tr.add("setRolePermissions replace err=%s",
		errName(auth.SetRolePermissions(ctx, st, "editor", []string{"terms.manage", "content.publish"})))
	perms, _, _ = auth.RolePermissions(ctx, st, "editor")
	tr.add("rolePermissions after replace order=%v", perms)

	// ATOMICITY: one valid name plus one that does not exist. The whole call
	// must abort with the previous set intact — neither the DELETE nor the
	// partial INSERT may survive.
	atomicErr := auth.SetRolePermissions(ctx, st, "editor", []string{"users.manage", "articles.nuke"})
	perms, _, _ = auth.RolePermissions(ctx, st, "editor")
	tr.add("setRolePermissions atomic-abort err=%s set-unchanged=%v", errName(atomicErr), perms)

	tr.add("setRolePermissions unknown-role err=%s",
		errName(auth.SetRolePermissions(ctx, st, "superadmin", []string{"content.create"})))

	tr.add("setRolePermissions duplicates err=%s",
		errName(auth.SetRolePermissions(ctx, st, "contributor",
			[]string{"content.create", "content.create", "content.create"})))
	perms, _, _ = auth.RolePermissions(ctx, st, "contributor")
	tr.add("rolePermissions contributor after duplicates=%v", perms)

	tr.add("setRolePermissions empty err=%s", errName(auth.SetRolePermissions(ctx, st, "editor", nil)))
	perms, _, _ = auth.RolePermissions(ctx, st, "editor")
	tr.add("rolePermissions editor after empty=%v len=%d", perms, len(perms))

	_, found, err = auth.RolePermissions(ctx, st, "ghost-role")
	tr.add("rolePermissions ghost found=%t err=%s", found, errName(err))

	// --- 6. API keys: mint, verify, list (ORDER), revoke ---------------------
	// The labels are chosen so alphabetical order is the REVERSE of creation
	// order: a listing that silently fell back to natural order, or that sorted
	// by a timestamp rendered differently per engine, could not produce the same
	// sequence on both. The sleep guarantees three distinct created_at values,
	// so the primary sort key alone decides the order.
	editorRole := roleID("editor")
	adminRole := roleID("administrator")
	secretC, err := auth.MintAPIKey(ctx, st, "c-first-minted", editorRole)
	if err != nil {
		t.Fatalf("[%s] mint c: %v", engine, err)
	}
	time.Sleep(5 * time.Millisecond)
	secretB, err := auth.MintAPIKey(ctx, st, "b-second-minted", adminRole)
	if err != nil {
		t.Fatalf("[%s] mint b: %v", engine, err)
	}
	time.Sleep(5 * time.Millisecond)
	secretA, err := auth.MintAPIKey(ctx, st, "a-third-minted", editorRole)
	if err != nil {
		t.Fatalf("[%s] mint a: %v", engine, err)
	}

	key, err := auth.VerifyAPIKey(ctx, st, secretC)
	tr.add("verifyAPIKey fresh err=%s label=%s role-matches=%t", errName(err), keyLabel(key), keyRoleID(key) == editorRole)
	_, err = auth.VerifyAPIKey(ctx, st, "lbk_notarealkey")
	tr.add("verifyAPIKey bogus err=%s", errName(err))
	_, err = auth.VerifyAPIKey(ctx, st, "")
	tr.add("verifyAPIKey empty err=%s", errName(err))

	keys, err := auth.ListAPIKeys(ctx, st)
	if err != nil {
		t.Fatalf("[%s] list api keys: %v", engine, err)
	}
	tr.add("listAPIKeys order=%v", keyLabels(keys))
	for _, k := range keys {
		tr.add("listAPIKeys row label=%s role=%s revoked=%t", k.Label, k.RoleName, k.Revoked)
	}

	// Revoke by plaintext secret, then by id — both paths, both idempotent.
	tr.add("revokeAPIKey by-secret err=%s", errName(auth.RevokeAPIKey(ctx, st, secretC)))
	_, err = auth.VerifyAPIKey(ctx, st, secretC)
	tr.add("verifyAPIKey after-revoke err=%s", errName(err))
	tr.add("revokeAPIKey by-secret again err=%s", errName(auth.RevokeAPIKey(ctx, st, secretC)))
	_, err = auth.VerifyAPIKey(ctx, st, secretC)
	tr.add("verifyAPIKey after-double-revoke err=%s", errName(err))

	byLabel := map[string]auth.APIKeyRecord{}
	keys, _ = auth.ListAPIKeys(ctx, st)
	for _, k := range keys {
		byLabel[k.Label] = k
	}
	revokedRecord := byLabel["c-first-minted"]
	tr.add("revoked key still listed=%t revoked=%t revoked-at-present=%t",
		revokedRecord.ID != "", revokedRecord.Revoked, revokedRecord.RevokedAt != "")

	targetID := byLabel["b-second-minted"].ID
	tr.add("revokeAPIKeyByID err=%s", errName(auth.RevokeAPIKeyByID(ctx, st, targetID)))
	_, err = auth.VerifyAPIKey(ctx, st, secretB)
	tr.add("verifyAPIKey b after-revoke-by-id err=%s", errName(err))
	tr.add("revokeAPIKeyByID again err=%s", errName(auth.RevokeAPIKeyByID(ctx, st, targetID)))
	tr.add("revokeAPIKeyByID unknown err=%s",
		errName(auth.RevokeAPIKeyByID(ctx, st, "22222222-2222-2222-2222-222222222222")))

	// The still-active key must be unaffected by the two revocations —
	// `revoked_at IS NULL` has to mean the same thing on both engines.
	_, err = auth.VerifyAPIKey(ctx, st, secretA)
	tr.add("verifyAPIKey untouched-key err=%s", errName(err))

	single, found, err := auth.GetAPIKey(ctx, st, targetID)
	tr.add("getAPIKey found=%t err=%s label=%s role=%s revoked=%t revoked-at-present=%t",
		found, errName(err), single.Label, single.RoleName, single.Revoked, single.RevokedAt != "")
	_, found, err = auth.GetAPIKey(ctx, st, "33333333-3333-3333-3333-333333333333")
	tr.add("getAPIKey missing found=%t err=%s", found, errName(err))

	keys, _ = auth.ListAPIKeys(ctx, st)
	tr.add("listAPIKeys final order=%v", keyLabels(keys))
	for _, k := range keys {
		tr.add("listAPIKeys final row label=%s role=%s revoked=%t", k.Label, k.RoleName, k.Revoked)
	}

	// The canonical timestamp form is the same on both engines: a value read
	// back is parseable as RFC3339Nano regardless of the engine that stored it.
	_, parseErr := time.Parse(time.RFC3339Nano, keys[0].CreatedAt)
	tr.add("created_at canonical-rfc3339nano=%t", parseErr == nil)

	// --- 7. the user created first is still intact ---------------------------
	got, err = auth.VerifyCredentials(ctx, st, "zeta@example.com", "correct-horse")
	tr.add("verify zeta err=%s roles=%v", errName(err), userRoles(got))

	return tr.lines
}

// --- small accessors that tolerate a nil result ------------------------------

func userEmail(u *auth.User) string {
	if u == nil {
		return "<nil>"
	}
	return u.Email
}

func userRoles(u *auth.User) []string {
	if u == nil {
		return nil
	}
	return u.Roles
}

func userEmails(users []auth.UserRecord) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Email)
	}
	return out
}

func keyLabel(k *auth.APIKey) string {
	if k == nil {
		return "<nil>"
	}
	return k.Label
}

func keyRoleID(k *auth.APIKey) string {
	if k == nil {
		return "<nil>"
	}
	return k.RoleID
}

func keyLabels(keys []auth.APIKeyRecord) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.Label)
	}
	return out
}
