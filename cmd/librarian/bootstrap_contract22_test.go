package main

// CONTRACT-22 T2 acceptance tests for the `--bootstrap` mode.
//
// The property that gets the most attention here is the one the contract fixed
// as a requirement: THE PASSWORD NEVER TRAVELS THROUGH THE COMMAND LINE. That is
// tested from both sides — the argument parser refuses a --password flag with an
// explanatory message, and the only channel that works is standard input.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// runBootstrap drives the real command with an explicit stdin and captures its
// output, exactly as main would.
func runBootstrap(t *testing.T, args []string, stdin string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := bootstrapCommand(args, strings.NewReader(stdin), &out)
	return out.String(), err
}

// openDB22 reopens the database the command wrote to, for independent
// verification (never through the command's own report).
func openDB22(t *testing.T, path string) *compat.Store {
	t.Helper()
	st, err := store.Open(compat.SQLite, path)
	if err != nil {
		t.Fatalf("reopen %q: %v", path, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func count22(t *testing.T, st *compat.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM "`+table+`"`).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestBootstrapCommandCreatesTheSchemaAndAdministers answers the red-team
// question "a database with no schema: does it create it or fail?" — it creates
// it, with the same two calls the server makes on startup — and then checks the
// whole outcome independently.
func TestBootstrapCommandCreatesTheSchemaAndAdministers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	t.Setenv("LIBRARIAN_DB", dbPath)
	t.Setenv("LIBRARIAN_ENGINE", "")
	// No LIBRARIAN_JWT_SECRET: the bootstrap does not serve traffic, so it must
	// not require one (the same rule --dump-schema follows).
	t.Setenv("LIBRARIAN_JWT_SECRET", "")

	out, err := runBootstrap(t, []string{"--bootstrap", "--email", "admin@example.com"}, "una-clave-larga\n")
	if err != nil {
		t.Fatalf("bootstrapCommand: %v", err)
	}

	st := openDB22(t, dbPath)
	ctx := context.Background()
	if got := count22(t, st, "users"); got != 1 {
		t.Fatalf("users = %d, want 1", got)
	}
	if got := count22(t, st, "role_permissions"); got != len(schema.Permissions) {
		t.Fatalf("role_permissions = %d, want %d", got, len(schema.Permissions))
	}
	user, err := auth.VerifyCredentials(ctx, st, "admin@example.com", "una-clave-larga")
	if err != nil {
		t.Fatalf("the created identity cannot authenticate: %v", err)
	}
	perms, _, err := auth.RolePermissions(ctx, st, auth.BootstrapRole)
	if err != nil {
		t.Fatalf("RolePermissions: %v", err)
	}
	if len(perms) != len(schema.Permissions) {
		t.Fatalf("administrator holds %v", perms)
	}

	// The report has to state BOTH halves of what was done — the identity and
	// the grants — because the missing half is what caused the incident.
	for _, fragment := range []string{"bootstrap complete", "admin@example.com", user.ID, "administrator", "roles.manage"} {
		if !strings.Contains(out, fragment) {
			t.Fatalf("the report does not mention %q:\n%s", fragment, out)
		}
	}
	if strings.Contains(out, "una-clave-larga") {
		t.Fatalf("THE PASSWORD WAS PRINTED:\n%s", out)
	}
	t.Logf("OUTPUT:\n%s", out)
}

// TestBootstrapCommandSecondRunIsRefused covers the once-only guarantee at the
// command boundary: the message says what happened, and nothing changed.
func TestBootstrapCommandSecondRunIsRefused(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "twice.db")
	t.Setenv("LIBRARIAN_DB", dbPath)
	t.Setenv("LIBRARIAN_ENGINE", "")

	if _, err := runBootstrap(t, []string{"--bootstrap", "--email", "admin@example.com"}, "clave-uno\n"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	st := openDB22(t, dbPath)
	usersBefore := count22(t, st, "users")
	grantsBefore := count22(t, st, "role_permissions")

	out, err := runBootstrap(t, []string{"--bootstrap", "--email", "otro@example.com"}, "clave-dos\n")
	if err == nil {
		t.Fatal("SECURITY: the second run succeeded")
	}
	if !errors.Is(err, auth.ErrAlreadyBootstrapped) {
		t.Fatalf("error = %v, want ErrAlreadyBootstrapped", err)
	}
	if out != "" {
		t.Fatalf("the refused run printed a report:\n%s", out)
	}
	if got := count22(t, st, "users"); got != usersBefore {
		t.Fatalf("users changed: %d -> %d", usersBefore, got)
	}
	if got := count22(t, st, "role_permissions"); got != grantsBefore {
		t.Fatalf("grants changed: %d -> %d", grantsBefore, got)
	}
	if got := count22(t, st, schema.BootstrapTable); got != 1 {
		t.Fatalf("marker rows = %d, want 1", got)
	}
	t.Logf("REFUSED: %v", err)
}

// TestBootstrapCommandRefusesAPasswordFlag is the T2 requirement made
// executable. A password on the command line is written to the shell history and
// is visible in the process list; the command must refuse it EXPLAINING that,
// not ignore it silently.
func TestBootstrapCommandRefusesAPasswordFlag(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "flag.db")
	t.Setenv("LIBRARIAN_DB", dbPath)
	t.Setenv("LIBRARIAN_ENGINE", "")

	for _, args := range [][]string{
		{"--bootstrap", "--email", "admin@example.com", "--password", "hunter2"},
		{"--bootstrap", "--email", "admin@example.com", "--password=hunter2"},
		{"--bootstrap", "--email", "admin@example.com", "--pass=hunter2"},
		{"--bootstrap", "--email", "admin@example.com", "--secret", "hunter2"},
	} {
		_, err := runBootstrap(t, args, "otra-clave\n")
		if err == nil {
			t.Fatalf("args %v were accepted", args)
		}
		for _, fragment := range []string{"shell history", "process list", "STANDARD INPUT"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("args %v: the refusal does not explain %q: %v", args, fragment, err)
			}
		}
	}
	// And nothing was created by any of those attempts.
	if _, err := store.Open(compat.SQLite, dbPath); err != nil {
		t.Fatalf("open: %v", err)
	}
	st := openDB22(t, dbPath)
	if _, _, err := auth.BootstrapStatus(context.Background(), st); err == nil {
		t.Fatal("a refused run applied the schema")
	}
	t.Log("the password can only arrive through standard input")
}

// TestBootstrapCommandRejectsBadInvocations covers the remaining argument
// failures: no --email, and an empty standard input (the mistake of forgetting
// the redirection, which must not create an account with an empty password).
func TestBootstrapCommandRejectsBadInvocations(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bad.db")
	t.Setenv("LIBRARIAN_DB", dbPath)
	t.Setenv("LIBRARIAN_ENGINE", "")

	if _, err := runBootstrap(t, []string{"--bootstrap"}, "clave\n"); err == nil {
		t.Fatal("--bootstrap without --email was accepted")
	} else if !strings.Contains(err.Error(), "--email") {
		t.Fatalf("message does not name --email: %v", err)
	}
	for _, stdin := range []string{"", "\n", "\r\n"} {
		if _, err := runBootstrap(t, []string{"--bootstrap", "--email", "a@b.com"}, stdin); err == nil {
			t.Fatalf("an empty standard input (%q) was accepted", stdin)
		}
	}
	if _, err := runBootstrap(t, []string{"--bootstrap", "--email", "not-an-email"}, "clave\n"); !errors.Is(err, auth.ErrInvalidEmail) {
		t.Fatalf("invalid email error = %v", err)
	}
	t.Log("no --email, empty stdin and a malformed address are all refused before anything is written")
}

// TestReadPassword pins the standard-input contract: exactly one trailing
// end-of-line is stripped, inner spaces survive, and nothing else is touched.
func TestReadPassword(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"clave\n", "clave"},
		{"clave\r\n", "clave"},
		{"clave", "clave"},
		{"con espacios y $ raros\n", "con espacios y $ raros"},
		{"  espacios al inicio\n", "  espacios al inicio"},
	}
	for _, tc := range cases {
		got, err := readPassword(strings.NewReader(tc.in))
		if err != nil {
			t.Fatalf("readPassword(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("readPassword(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, empty := range []string{"", "\n", "\r\n", "\n\n"} {
		if _, err := readPassword(strings.NewReader(empty)); err == nil {
			t.Fatalf("readPassword(%q) accepted an empty password", empty)
		}
	}
}

// TestBootstrapFlagParsing covers the argument helpers, including the case that
// matters: `--bootstrap --email x` must not read "--bootstrap" as anything else,
// and --dump-schema must keep working untouched.
func TestBootstrapFlagParsing(t *testing.T) {
	cases := []struct {
		args      []string
		bootstrap bool
		email     string
		db        string
		password  bool
	}{
		{[]string{"--bootstrap", "--email", "a@b.com"}, true, "a@b.com", "", false},
		{[]string{"--bootstrap", "--email=a@b.com"}, true, "a@b.com", "", false},
		{[]string{"--bootstrap", "--email", "a@b.com", "--db", "x.db"}, true, "a@b.com", "x.db", false},
		{[]string{"--bootstrap", "--email", "--db"}, true, "", "", false},
		{[]string{"--bootstrap"}, true, "", "", false},
		{[]string{"--bootstrap", "--email", "a@b.com", "--password", "p"}, true, "a@b.com", "", true},
		{[]string{"--dump-schema", "out.json"}, false, "", "", false},
		{[]string{}, false, "", "", false},
	}
	for _, tc := range cases {
		gotBootstrap := hasFlag(tc.args, "--bootstrap")
		gotEmail := valueFlag(tc.args, "--email")
		gotDB := dbPathFlag(tc.args)
		_, gotPassword := passwordFlag(tc.args)
		if gotBootstrap != tc.bootstrap || gotEmail != tc.email || gotDB != tc.db || gotPassword != tc.password {
			t.Fatalf("args=%v -> bootstrap=%v email=%q db=%q password=%v; want %v %q %q %v",
				tc.args, gotBootstrap, gotEmail, gotDB, gotPassword,
				tc.bootstrap, tc.email, tc.db, tc.password)
		}
		// --bootstrap must never be mistaken for the dump mode.
		if _, isDump := dumpSchemaFlag(tc.args); isDump && tc.bootstrap {
			t.Fatalf("args=%v matched BOTH modes", tc.args)
		}
		t.Logf("args=%-56v -> bootstrap=%v email=%-12q db=%-6q password-flag=%v",
			tc.args, gotBootstrap, gotEmail, gotDB, gotPassword)
	}
}

// TestBootstrapCommandRefusesAnInstallationWithUsers is the production shape at
// the command boundary: the operator runs the command on a database that already
// has identities (which is what production is today) and gets a refusal that
// tells them what to do instead.
func TestBootstrapCommandRefusesAnInstallationWithUsers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	t.Setenv("LIBRARIAN_DB", dbPath)
	t.Setenv("LIBRARIAN_ENGINE", "")

	ctx := context.Background()
	st := openDB22(t, dbPath)
	if err := store.EnsureSchema(ctx, st); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.SeedCatalogs(ctx, st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := auth.CreateUser(ctx, st, "viejo@example.com", "clave-vieja", []string{auth.BootstrapRole}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	_, err := runBootstrap(t, []string{"--bootstrap", "--email", "nuevo@example.com"}, "clave-nueva\n")
	if !errors.Is(err, auth.ErrInstallationNotEmpty) {
		t.Fatalf("error = %v, want ErrInstallationNotEmpty", err)
	}
	if got := count22(t, st, "users"); got != 1 {
		t.Fatalf("users = %d, want the pre-existing 1", got)
	}
	if got := count22(t, st, "role_permissions"); got != 0 {
		t.Fatalf("the refused run granted %d permission(s)", got)
	}
	t.Logf("REFUSED: %v", err)
}
