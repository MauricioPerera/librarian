package auth_test

// CONTRACT-22 T3 — the battery for the bootstrap operation, on SQLite.
//
// It is deliberately organised around the two properties the contract says are
// not details: ATOMICITY (there is no reachable state where a user holds
// `administrator` while `administrator` holds nothing) and USABLE-ONCE (the
// guarantee is the database's primary key, not the order of the checks). Both
// are FORCED, never asserted from reading the code: the atomicity tests induce a
// failure in the middle of the transaction, and the once-only tests include two
// bootstraps running concurrently in separate connections.
//
// The dual-engine half — the same operation plus the HTTP proof against a real
// PostgreSQL 17 — is internal/server/dualengine_contract22_test.go.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// freshInstall returns a brand new SQLite database with the schema applied and
// the catalogs seeded: exactly what a first boot leaves behind, and exactly the
// state the contract measured as INADMINISTRABLE (roles and permissions seeded,
// role_permissions empty).
func freshInstall(t *testing.T) *compat.Store {
	t.Helper()
	return installAt(t, filepath.Join(t.TempDir(), "bootstrap22.db"))
}

func installAt(t *testing.T, path string) *compat.Store {
	t.Helper()
	st, err := store.Open(compat.SQLite, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := store.EnsureSchema(ctx, st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if err := store.SeedCatalogs(ctx, st); err != nil {
		t.Fatalf("seed catalogs: %v", err)
	}
	return st
}

func countRows22(t *testing.T, st *compat.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM "`+table+`"`).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func exec22(t *testing.T, st *compat.Store, statement string, args ...any) {
	t.Helper()
	if _, err := st.DB.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

// TestCleanInstallIsInadministrableBeforeBootstrap is the measurement the
// contract opens with, re-taken here so the rest of the battery is not proving
// something against a fixture that was never broken. Without it, every later
// assertion could pass on a database that was already fine.
func TestCleanInstallIsInadministrableBeforeBootstrap(t *testing.T) {
	st := freshInstall(t)
	ctx := context.Background()

	if got := countRows22(t, st, "roles"); got != len(schema.Roles) {
		t.Fatalf("roles seeded = %d, want %d", got, len(schema.Roles))
	}
	if got := countRows22(t, st, "permissions"); got != len(schema.Permissions) {
		t.Fatalf("permissions seeded = %d, want %d", got, len(schema.Permissions))
	}
	if got := countRows22(t, st, "role_permissions"); got != 0 {
		t.Fatalf("role_permissions = %d on a clean install, want 0", got)
	}
	perms, found, err := auth.RolePermissions(ctx, st, auth.BootstrapRole)
	if err != nil || !found {
		t.Fatalf("RolePermissions(%q): found=%v err=%v", auth.BootstrapRole, found, err)
	}
	if len(perms) != 0 {
		t.Fatalf("administrator already holds %v on a clean install", perms)
	}
	t.Logf("MEASURED: clean install has roles=%d permissions=%d role_permissions=0, administrator holds %v",
		countRows22(t, st, "roles"), countRows22(t, st, "permissions"), perms)
}

// TestBootstrapMakesTheInstallationAdministrable is the happy path: ONE call
// leaves an identity that exists, holds the role, and whose role holds every
// permission of the catalog.
func TestBootstrapMakesTheInstallationAdministrable(t *testing.T) {
	st := freshInstall(t)
	ctx := context.Background()

	result, err := auth.Bootstrap(ctx, st, "admin@example.com", "una-clave-larga")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if result.UserID == "" || result.Email != "admin@example.com" || result.Role != auth.BootstrapRole {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Permissions) != len(schema.Permissions) {
		t.Fatalf("granted %d permissions, want %d (%v)", len(result.Permissions), len(schema.Permissions), result.Permissions)
	}

	// The identity authenticates, with the role.
	user, err := auth.VerifyCredentials(ctx, st, "admin@example.com", "una-clave-larga")
	if err != nil {
		t.Fatalf("VerifyCredentials after bootstrap: %v", err)
	}
	if len(user.Roles) != 1 || user.Roles[0] != auth.BootstrapRole {
		t.Fatalf("roles = %v, want [%s]", user.Roles, auth.BootstrapRole)
	}
	if user.ID != result.UserID {
		t.Fatalf("user id = %q, want the reported %q", user.ID, result.UserID)
	}

	// And the ROLE holds the grants, which is the half that was missing in
	// production for weeks.
	perms, found, err := auth.RolePermissions(ctx, st, auth.BootstrapRole)
	if err != nil || !found {
		t.Fatalf("RolePermissions: found=%v err=%v", found, err)
	}
	want := strings.Join(result.Permissions, ",")
	if strings.Join(perms, ",") != want {
		t.Fatalf("role holds %v, want %v", perms, result.Permissions)
	}
	for _, p := range schema.Permissions {
		if !strings.Contains(","+want+",", ","+p+",") {
			t.Fatalf("catalog permission %q was not granted; got %v", p, perms)
		}
	}

	// The OTHER roles were left alone, on purpose.
	for _, role := range []string{"editor", "author", "contributor"} {
		got, found, err := auth.RolePermissions(ctx, st, role)
		if err != nil || !found {
			t.Fatalf("RolePermissions(%q): found=%v err=%v", role, found, err)
		}
		if len(got) != 0 {
			t.Fatalf("role %q was granted %v; the contract forbids granting it anything", role, got)
		}
	}
	t.Logf("ADMINISTRABLE: %s (id %s) holds %q, which holds %v; editor/author/contributor hold nothing",
		result.Email, result.UserID, result.Role, perms)
}

// TestSecondBootstrapIsRefusedAndChangesNothing covers "usable una sola vez"
// from the operator's side: the refusal is the specific sentinel (not a generic
// error), and the database is byte-for-byte what it was.
func TestSecondBootstrapIsRefusedAndChangesNothing(t *testing.T) {
	st := freshInstall(t)
	ctx := context.Background()

	first, err := auth.Bootstrap(ctx, st, "admin@example.com", "clave-uno")
	if err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	before := snapshot22(t, st)

	_, err = auth.Bootstrap(ctx, st, "otro@example.com", "clave-dos")
	if err == nil {
		t.Fatal("SECURITY: a second bootstrap succeeded")
	}
	if !errors.Is(err, auth.ErrAlreadyBootstrapped) {
		t.Fatalf("second bootstrap error = %v, want ErrAlreadyBootstrapped", err)
	}
	// The message has to say WHAT happened, not "error".
	for _, fragment := range []string{"already been bootstrapped", first.Email, first.UserID} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("refusal message does not mention %q: %v", fragment, err)
		}
	}
	if after := snapshot22(t, st); after != before {
		t.Fatalf("the refused bootstrap CHANGED the database:\n before=%v\n after =%v", before, after)
	}
	// And the second email was never created.
	if _, err := auth.VerifyCredentials(ctx, st, "otro@example.com", "clave-dos"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("the refused identity can authenticate: %v", err)
	}
	t.Logf("REFUSED, NOTHING CHANGED: %v", err)
}

// snapshot22 is the comparable state of everything the bootstrap can touch. Used
// to assert "nothing changed" without listing tables at each call site.
func snapshot22(t *testing.T, st *compat.Store) string {
	t.Helper()
	ctx := context.Background()
	perms, _, err := auth.RolePermissions(ctx, st, auth.BootstrapRole)
	if err != nil {
		t.Fatalf("snapshot RolePermissions: %v", err)
	}
	users, err := auth.ListUsers(ctx, st)
	if err != nil {
		t.Fatalf("snapshot ListUsers: %v", err)
	}
	var emails []string
	for _, u := range users {
		emails = append(emails, u.ID+"|"+u.Email+"|"+u.Status+"|"+strings.Join(u.Roles, "+"))
	}
	record, found, err := auth.BootstrapStatus(ctx, st)
	if err != nil {
		t.Fatalf("snapshot BootstrapStatus: %v", err)
	}
	return strings.Join([]string{
		"users=" + strings.Join(emails, ";"),
		"grants=" + strings.Join(perms, ","),
		"role_permissions=" + itoa22(countRows22(t, st, "role_permissions")),
		"marker=" + itoa22(boolInt22(found)) + "|" + record.AdminEmail + "|" + record.AdminUserID,
	}, "  ")
}

func boolInt22(b bool) int {
	if b {
		return 1
	}
	return 0
}

func itoa22(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestBootstrapRefusesAnInstallationThatAlreadyHasUsers is RED-TEAM QUESTION #1,
// and it is the state of PRODUCTION TODAY: users exist, role_permissions is
// empty, no marker. The bootstrap must refuse — a path that mints an
// administrator on a system that already has identities is a backdoor — and it
// must say so distinctly enough for an operator to know what to do instead.
func TestBootstrapRefusesAnInstallationThatAlreadyHasUsers(t *testing.T) {
	st := freshInstall(t)
	ctx := context.Background()

	// The production shape: a user with the administrator role, and NOTHING in
	// role_permissions.
	existing, err := auth.CreateUser(ctx, st, "viejo@example.com", "clave-vieja", []string{auth.BootstrapRole})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if got := countRows22(t, st, "role_permissions"); got != 0 {
		t.Fatalf("fixture is not the production state: role_permissions = %d", got)
	}
	before := snapshot22(t, st)

	_, err = auth.Bootstrap(ctx, st, "nuevo@example.com", "clave-nueva")
	if err == nil {
		t.Fatal("SECURITY: the bootstrap minted an administrator on an installation that already had users")
	}
	if !errors.Is(err, auth.ErrInstallationNotEmpty) {
		t.Fatalf("error = %v, want ErrInstallationNotEmpty", err)
	}
	if after := snapshot22(t, st); after != before {
		t.Fatalf("the refused bootstrap changed the database:\n before=%v\n after =%v", before, after)
	}
	if _, _, err := auth.BootstrapStatus(ctx, st); err != nil {
		t.Fatalf("BootstrapStatus: %v", err)
	}
	if _, found, _ := auth.BootstrapStatus(ctx, st); found {
		t.Fatal("the refused bootstrap left a marker row behind")
	}
	t.Logf("REFUSED (production shape: user %s with role, zero grants): %v", existing.ID, err)
}

// TestBootstrapIsAtomicWhenTheRoleIsMissing forces a failure AFTER the user row
// has been inserted: `administrator` is deleted from the catalog, so role
// resolution fails at a point where the marker and the user row already exist
// inside the transaction. This is also RED-TEAM QUESTION #2 (the role exists in
// code but was deleted from the catalog).
//
// The state that MUST NOT survive is precisely the historical incident: a user
// carrying a role whose grants are empty.
func TestBootstrapIsAtomicWhenTheRoleIsMissing(t *testing.T) {
	st := freshInstall(t)
	ctx := context.Background()

	exec22(t, st, `DELETE FROM "roles" WHERE "name" = ?`, auth.BootstrapRole)
	before := snapshot22(t, st)

	_, err := auth.Bootstrap(ctx, st, "admin@example.com", "clave")
	if err == nil {
		t.Fatal("Bootstrap succeeded with no administrator role in the catalog")
	}
	if !errors.Is(err, auth.ErrUnknownRole) {
		t.Fatalf("error = %v, want it to wrap ErrUnknownRole", err)
	}
	assertNothingLeftBehind22(t, st, before)
	t.Logf("ATOMIC (failure after the user insert): %v", err)
}

// TestBootstrapIsAtomicWhenAPermissionIsMissing forces a failure one step LATER
// still: the user has been inserted, the role assigned, and the grant step then
// fails because a permission of the catalog is not in the table. If the
// operation were not atomic this is EXACTLY the state that broke production —
// user with a role, role without permissions — so it is the most important of
// the two.
func TestBootstrapIsAtomicWhenAPermissionIsMissing(t *testing.T) {
	st := freshInstall(t)
	ctx := context.Background()

	exec22(t, st, `DELETE FROM "permissions" WHERE "name" = ?`, "roles.manage")
	before := snapshot22(t, st)

	_, bootstrapErr := auth.Bootstrap(ctx, st, "admin@example.com", "clave")
	if bootstrapErr == nil {
		t.Fatal("Bootstrap succeeded with a catalog permission missing")
	}
	if !errors.Is(bootstrapErr, auth.ErrUnknownPermission) {
		t.Fatalf("error = %v, want it to wrap ErrUnknownPermission", bootstrapErr)
	}
	assertNothingLeftBehind22(t, st, before)

	// The forbidden state, checked by name rather than by inference.
	users, err := auth.ListUsers(ctx, st)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	for _, u := range users {
		for _, role := range u.Roles {
			perms, _, err := auth.RolePermissions(ctx, st, role)
			if err != nil {
				t.Fatalf("RolePermissions(%q): %v", role, err)
			}
			if len(perms) == 0 {
				t.Fatalf("THE HISTORICAL INCIDENT IS REACHABLE: user %q holds role %q, which holds no permission", u.Email, role)
			}
		}
	}
	t.Logf("ATOMIC (failure inside the grant step): %v", bootstrapErr)
}

// TestTheMarkerTableMakesASecondClaimUnrepresentable is the contract's
// "la garantía tiene que ser de la base, no del orden de las comprobaciones",
// tested WITHOUT going through Bootstrap at all — because the point is precisely
// that the guarantee does not depend on that code.
//
// Two raw INSERTs are attempted after a successful bootstrap:
//
//	the same key      → PRIMARY KEY violation
//	a different key   → CHECK violation (there is only one representable key)
//
// Together they say: no second marker row can exist, whatever the caller does.
// This matters on SQLite specifically, where the concurrency test above is won
// by the file lock (SQLITE_BUSY) and so does not by itself exercise the key.
func TestTheMarkerTableMakesASecondClaimUnrepresentable(t *testing.T) {
	st := freshInstall(t)
	ctx := context.Background()
	if _, err := auth.Bootstrap(ctx, st, "admin@example.com", "clave"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	insert := `INSERT INTO "` + schema.BootstrapTable + `" ("id", "admin_user_id", "admin_email", "created_at") VALUES (?, ?, ?, ?)`
	stamp := "2026-01-01T00:00:00.000000000Z"

	_, samePK := st.DB.ExecContext(ctx, insert, schema.BootstrapMarkerID, "00000000-0000-4000-8000-000000000000", "intruso@example.com", stamp)
	if samePK == nil {
		t.Fatal("SECURITY: a second marker row with the same key was accepted")
	}
	_, otherKey := st.DB.ExecContext(ctx, insert, "otro", "00000000-0000-4000-8000-000000000000", "intruso@example.com", stamp)
	if otherKey == nil {
		t.Fatal("SECURITY: a marker row with a different key was accepted; the CHECK does not pin the key")
	}
	if got := countRows22(t, st, schema.BootstrapTable); got != 1 {
		t.Fatalf("marker rows = %d, want 1", got)
	}
	t.Logf("UNREPRESENTABLE: same key -> %v ; other key -> %v", samePK, otherKey)
}

// assertNothingLeftBehind22 is the rollback oracle: no user, no grant, no marker
// — and the installation is still bootstrappable, which is what makes the
// rollback USEFUL rather than merely clean.
func assertNothingLeftBehind22(t *testing.T, st *compat.Store, before string) {
	t.Helper()
	ctx := context.Background()
	if got := countRows22(t, st, "users"); got != 0 {
		t.Fatalf("HALF-DONE: %d user(s) survived a failed bootstrap", got)
	}
	if got := countRows22(t, st, "role_permissions"); got != 0 {
		t.Fatalf("HALF-DONE: %d grant(s) survived a failed bootstrap", got)
	}
	if got := countRows22(t, st, schema.BootstrapTable); got != 0 {
		t.Fatalf("HALF-DONE: the marker survived a failed bootstrap (%d row(s)) — the installation would be permanently unbootstrappable", got)
	}
	if _, found, err := auth.BootstrapStatus(ctx, st); err != nil || found {
		t.Fatalf("BootstrapStatus after rollback: found=%v err=%v", found, err)
	}
	if after := snapshot22(t, st); after != before {
		t.Fatalf("the failed bootstrap left the database changed:\n before=%v\n after =%v", before, after)
	}
}

// TestBootstrapAfterARolledBackAttemptStillWorks is the complement of the two
// atomicity tests: a failed attempt must not consume the single use. Repair the
// catalog, run again, and the installation becomes administrable.
func TestBootstrapAfterARolledBackAttemptStillWorks(t *testing.T) {
	st := freshInstall(t)
	ctx := context.Background()

	exec22(t, st, `DELETE FROM "permissions" WHERE "name" = ?`, "roles.manage")
	if _, err := auth.Bootstrap(ctx, st, "admin@example.com", "clave"); err == nil {
		t.Fatal("expected the seeded-catalog failure")
	}
	if err := store.SeedCatalogs(ctx, st); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	result, err := auth.Bootstrap(ctx, st, "admin@example.com", "clave")
	if err != nil {
		t.Fatalf("Bootstrap after a rolled-back attempt: %v", err)
	}
	if len(result.Permissions) != len(schema.Permissions) {
		t.Fatalf("granted %v", result.Permissions)
	}
	t.Logf("a rolled-back attempt did NOT consume the single use: %s now administers the install", result.Email)
}

// TestConcurrentBootstrapsExactlyOneWins is the RED-TEAM question that the
// contract says cannot be answered by check ordering: two bootstraps running at
// the same time, on SEPARATE connections to the same database, with DIFFERENT
// emails. Exactly one may pass, and the loser must leave nothing behind.
func TestConcurrentBootstrapsExactlyOneWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race22.db")
	installAt(t, path) // apply schema + catalogs once, then race two fresh handles

	const racers = 4
	stores := make([]*compat.Store, racers)
	for i := range stores {
		st, err := store.Open(compat.SQLite, path)
		if err != nil {
			t.Fatalf("open racer %d: %v", i, err)
		}
		defer st.Close()
		stores[i] = st
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		losses  []error
		start   = make(chan struct{})
	)
	for i, st := range stores {
		wg.Add(1)
		go func(i int, st *compat.Store) {
			defer wg.Done()
			<-start
			result, err := auth.Bootstrap(context.Background(), st, emailFor22(i), "clave-de-carrera")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				losses = append(losses, err)
				return
			}
			winners = append(winners, result.Email)
		}(i, st)
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("SECURITY: %d of %d concurrent bootstraps succeeded (%v); exactly one must", len(winners), racers, winners)
	}
	if len(losses) != racers-1 {
		t.Fatalf("losses = %d, want %d", len(losses), racers-1)
	}

	// The database agrees with the count: one user, one marker, one fully granted
	// role.
	verify := stores[0]
	if got := countRows22(t, verify, "users"); got != 1 {
		t.Fatalf("users = %d after the race, want 1", got)
	}
	if got := countRows22(t, verify, schema.BootstrapTable); got != 1 {
		t.Fatalf("marker rows = %d after the race, want 1", got)
	}
	perms, _, err := auth.RolePermissions(context.Background(), verify, auth.BootstrapRole)
	if err != nil {
		t.Fatalf("RolePermissions: %v", err)
	}
	if len(perms) != len(schema.Permissions) {
		t.Fatalf("the winner left %d grants, want %d", len(perms), len(schema.Permissions))
	}
	for _, err := range losses {
		t.Logf("loser: %v", err)
	}
	t.Logf("EXACTLY ONE WON: %v; users=1 marker=1 grants=%d", winners, len(perms))
}

func emailFor22(i int) string {
	return "racer" + itoa22(i) + "@example.com"
}

// TestBootstrapRejectsBadArguments covers the remaining red-team inputs. Each
// must be refused BEFORE anything is written, so the installation stays
// bootstrappable.
func TestBootstrapRejectsBadArguments(t *testing.T) {
	cases := []struct {
		name     string
		email    string
		password string
		want     error
	}{
		{"empty email", "", "clave", auth.ErrInvalidEmail},
		{"whitespace-only email", "   ", "clave", auth.ErrInvalidEmail},
		{"no at sign", "admin.example.com", "clave", auth.ErrInvalidEmail},
		{"two at signs", "a@b@c", "clave", auth.ErrInvalidEmail},
		{"empty local part", "@example.com", "clave", auth.ErrInvalidEmail},
		{"empty domain", "admin@", "clave", auth.ErrInvalidEmail},
		{"inner whitespace", "ad min@example.com", "clave", auth.ErrInvalidEmail},
		{"empty password", "admin@example.com", "", auth.ErrInvalidPassword},
		{"password over bcrypt's limit", "admin@example.com", strings.Repeat("x", 73), auth.ErrInvalidPassword},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := freshInstall(t)
			ctx := context.Background()
			_, err := auth.Bootstrap(ctx, st, tc.email, tc.password)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got := countRows22(t, st, "users"); got != 0 {
				t.Fatalf("%d user(s) created despite the refusal", got)
			}
			if got := countRows22(t, st, schema.BootstrapTable); got != 0 {
				t.Fatalf("the marker was claimed despite the refusal")
			}
			t.Logf("refused: %v", err)
		})
	}
}

// TestBootstrapOnADatabaseWithoutSchemaFailsLoudly is the last red-team
// question. auth.Bootstrap does NOT apply the schema — it reports the missing
// table instead of silently doing nothing. Creating the schema is the job of the
// COMMAND (cmd/librarian: EnsureSchema + SeedCatalogs, the same two calls the
// server makes), which is covered in that package.
func TestBootstrapOnADatabaseWithoutSchemaFailsLoudly(t *testing.T) {
	st, err := store.Open(compat.SQLite, filepath.Join(t.TempDir(), "empty22.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if _, err := auth.Bootstrap(context.Background(), st, "admin@example.com", "clave"); err == nil {
		t.Fatal("Bootstrap succeeded against a database with no schema")
	} else {
		t.Logf("FAILS LOUDLY: %v", err)
	}
	if _, _, err := auth.BootstrapStatus(context.Background(), st); err == nil {
		t.Fatal("BootstrapStatus reported a definite answer on a database with no marker table")
	}
}

// TestSetRolePermissionsStillBehaves guards the CONTRACT-16 function whose body
// this contract moved into a transaction-scoped helper. Same replacement
// semantics, same sentinels, nothing changed for its existing caller.
func TestSetRolePermissionsStillBehaves(t *testing.T) {
	st := freshInstall(t)
	ctx := context.Background()

	if err := auth.SetRolePermissions(ctx, st, "editor", []string{"content.create", "content.update", "content.create"}); err != nil {
		t.Fatalf("SetRolePermissions: %v", err)
	}
	perms, _, err := auth.RolePermissions(ctx, st, "editor")
	if err != nil {
		t.Fatalf("RolePermissions: %v", err)
	}
	if strings.Join(perms, ",") != "content.create,content.update" {
		t.Fatalf("perms = %v", perms)
	}
	if err := auth.SetRolePermissions(ctx, st, "editor", nil); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if perms, _, _ := auth.RolePermissions(ctx, st, "editor"); len(perms) != 0 {
		t.Fatalf("revoke-all left %v", perms)
	}
	if err := auth.SetRolePermissions(ctx, st, "nope", nil); !errors.Is(err, auth.ErrRoleNotFound) {
		t.Fatalf("unknown role error = %v", err)
	}
	if err := auth.SetRolePermissions(ctx, st, "editor", []string{"not.a.permission"}); !errors.Is(err, auth.ErrUnknownPermission) {
		t.Fatalf("unknown permission error = %v", err)
	}
	if got := countRows22(t, st, "role_permissions"); got != 0 {
		t.Fatalf("the rejected call left %d grant(s)", got)
	}
}
