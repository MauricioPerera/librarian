package auth

// CONTRACT-22 T1 — the operation that makes a clean installation ADMINISTRABLE.
//
// THE PROBLEM IT CLOSES. EnsureSchema creates the tables and SeedCatalogs seeds
// the roles and the permissions, but NOTHING connects them: `role_permissions`
// is empty on a fresh database, so a user holding `administrator` holds no
// permission at all. Every write route is gated, including the CONTRACT-16 UI
// that grants permissions to a role (gated by `roles.manage`), so the one tool
// built to repair the situation is itself unreachable. That is a circular
// lock, and it is not hypothetical: production ran for weeks in effective
// read-only mode because of exactly this (docs/PENDIENTES.md hueco 1), unnoticed
// because every verification performed there had been a READ.
//
// WHAT IT DOES, AND WHY ALL OF IT AT ONCE. One transaction that:
//
//	1. claims the installation (the marker row — see schema.BootstrapTable),
//	2. creates the first identity with the `administrator` role,
//	3. grants that ROLE every permission of the catalog.
//
// Steps 2 and 3 are inseparable. Performed as two self-committing calls, a
// failure between them leaves "a user with a role, and a role with no
// permissions" — WHICH IS THE PRODUCTION INCIDENT ITSELF, reproduced by the very
// code written to prevent it. That is why CreateUser and SetRolePermissions were
// split into transaction-scoped bodies (createUserWithIDTx,
// setRolePermissionsTx) instead of being called as they stand: this function
// REUSES them, it does not reimplement them, and it does so inside one
// transaction it owns.
//
// WHICH PERMISSIONS, AND TO WHOM. All of schema.Permissions, to `administrator`
// only. editor/author/contributor are deliberately left with NO permissions:
// nothing in this project defines what each of them should be able to do, and
// inventing that here would fix policy in code. Once the bootstrap has run, the
// administrator assigns them through the CONTRACT-16 UI — which is reachable for
// the first time precisely because this ran.
//
// SEEDING THE ADMINISTRATOR'S GRANTS INSIDE SeedCatalogs WAS REJECTED (and the
// contract fixed that decision): the grants would then be re-applied on every
// boot, silently overwriting any change an administrator made through the UI.
// SeedCatalogs is untouched by this contract.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// BootstrapRole is the role the first identity receives and the role that
// receives the catalog's permissions. It is schema.Roles' highest-privilege
// entry, spelled once.
const BootstrapRole = "administrator"

// maxPasswordBytes is bcrypt's hard input limit. Beyond 72 bytes
// bcrypt.GenerateFromPassword fails, so a longer password is rejected UP FRONT
// with a message that says why, instead of surfacing as a hashing error halfway
// through the transaction.
const maxPasswordBytes = 72

// The sentinels. Each is a distinct, recognizable refusal so the command can
// print the right thing and a test can assert on the reason rather than on
// message text.
var (
	// ErrAlreadyBootstrapped: the marker row exists. This installation has been
	// claimed and CANNOT be claimed again.
	ErrAlreadyBootstrapped = errors.New("this installation has already been bootstrapped")
	// ErrInstallationNotEmpty: no marker, but `users` is not empty — an
	// installation predating this contract, which is the state of production
	// today. See the note on Bootstrap for why this refuses rather than repairs.
	ErrInstallationNotEmpty = errors.New("this installation already has users, so it is not a clean install")
	// ErrInvalidEmail / ErrInvalidPassword: the two argument validations,
	// performed BEFORE any transaction is opened.
	ErrInvalidEmail    = errors.New("invalid administrator email")
	ErrInvalidPassword = errors.New("invalid administrator password")
)

// BootstrapRecord is the marker row: who claimed this installation and when.
type BootstrapRecord struct {
	AdminUserID string
	AdminEmail  string
	CreatedAt   string
}

// BootstrapResult describes what a successful bootstrap actually did, so the
// caller can REPORT it rather than assert it. Permissions is the set granted to
// Role, in the project's engine-independent byte order.
type BootstrapResult struct {
	UserID      string
	Email       string
	Role        string
	Permissions []string
	CreatedAt   string
}

// BootstrapStatus reads the marker row outside any transaction. found=false
// means the installation has never been bootstrapped. An error means the marker
// could not be read at all — including "the table does not exist", i.e. a
// database with no schema; that is reported, never treated as "not bootstrapped"
// (the same fail-loudly rule --dump-schema follows).
func BootstrapStatus(ctx context.Context, store *compat.Store) (BootstrapRecord, bool, error) {
	return bootstrapRecord(ctx, store.DB, store.Target.Engine)
}

// bootstrapRecord reads the marker through any querier — the pool or a
// transaction — so the in-transaction check and the out-of-transaction one are
// literally the same statement.
func bootstrapRecord(ctx context.Context, q dual.TxQuerier, engine compat.Engine) (BootstrapRecord, bool, error) {
	statement := `SELECT "admin_user_id", "admin_email", "created_at" FROM "` + schema.BootstrapTable + `" WHERE "id" = ` +
		dual.Bind(engine, 1)
	var record BootstrapRecord
	err := q.QueryRowContext(ctx, statement, schema.BootstrapMarkerID).
		Scan(&record.AdminUserID, &record.AdminEmail, &record.CreatedAt)
	switch {
	case err == nil:
		return record, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return BootstrapRecord{}, false, nil
	default:
		return BootstrapRecord{}, false, err
	}
}

// Bootstrap performs the whole operation atomically and returns what it did.
//
// ATOMICITY. Everything below happens in ONE transaction; any failure rolls the
// whole thing back, so the database is either untouched or fully administrable.
// In particular the forbidden intermediate — a user carrying `administrator`
// while `administrator` has no permissions — is not reachable, and the battery
// forces two distinct mid-transaction failures to prove it rather than claim it.
//
// USABLE EXACTLY ONCE, AND THE GUARANTEE IS THE DATABASE'S. The marker row is
// claimed with a plain INSERT into a table whose primary key has exactly ONE
// representable value (schema.BootstrapMarkerID, pinned by a CHECK). A second
// run — or a second PROCESS running concurrently, which no amount of check
// ordering could exclude — violates that primary key and is rolled back whole.
// The two readable checks that precede the claim (marker present? users
// present?) exist only to produce a legible refusal; delete them and the
// guarantee still holds.
//
// WHY AN INSTALLATION THAT ALREADY HAS USERS IS REFUSED RATHER THAN REPAIRED.
// This is the state of production today (users exist, role_permissions empty),
// and the temptation is to let the bootstrap fix it. It must not: a path that
// mints an administrator on a system that already has identities is an
// authentication backdoor, not a repair tool — anyone who can run the binary
// could grant themselves an account. Production was repaired by hand and is
// recorded as such; from this contract on, an installation created through the
// bootstrap can never reach that state in the first place.
func Bootstrap(ctx context.Context, store *compat.Store, email, password string) (BootstrapResult, error) {
	email = strings.TrimSpace(email)
	if err := validateBootstrapEmail(email); err != nil {
		return BootstrapResult{}, err
	}
	if err := validateBootstrapPassword(password); err != nil {
		return BootstrapResult{}, err
	}

	engine := store.Target.Engine
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("begin bootstrap tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Legibility checks. Neither is the guarantee; both make the refusal say
	//    something useful instead of surfacing a constraint violation.
	if record, found, err := bootstrapRecord(ctx, tx, engine); err != nil {
		return BootstrapResult{}, fmt.Errorf("read the bootstrap marker (is the schema applied?): %w", err)
	} else if found {
		return BootstrapResult{}, alreadyBootstrapped(record, "")
	}
	var existingUsers int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM "users"`).Scan(&existingUsers); err != nil {
		return BootstrapResult{}, fmt.Errorf("count existing users: %w", err)
	}
	if existingUsers > 0 {
		return BootstrapResult{}, fmt.Errorf(
			"%w (%d user(s) already exist): the bootstrap creates the FIRST identity and refuses to mint an administrator on a system that already has accounts; grant permissions to a role from the admin UI instead (POST /admin/roles/{name}/permissions, CONTRACT-16)",
			ErrInstallationNotEmpty, existingUsers)
	}

	// 2. THE CLAIM — the database-level guarantee. Done before the user exists so
	//    a racing process is stopped as early as possible; the id is generated
	//    here so the marker can name its owner.
	userID, err := dual.NewUUID()
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("generate administrator id: %w", err)
	}
	timestamp := now()
	claim := `INSERT INTO "` + schema.BootstrapTable + `" ("id", "admin_user_id", "admin_email", "created_at") VALUES (` +
		dual.Bind(engine, 1) + `, ` + dual.Bind(engine, 2) + `, ` + dual.Bind(engine, 3) + `, ` + dual.Bind(engine, 4) + `)`
	if _, err := tx.ExecContext(ctx, claim, schema.BootstrapMarkerID, userID, email, timestamp); err != nil {
		// The insert can only fail on the primary key (the key is a constant that
		// satisfies the CHECK) or on a broken connection. Which one is decided by
		// LOOKING, not by matching driver message text — the two engines word it
		// differently, and CONTRACT-21 already paid for that lesson. The
		// transaction has to be rolled back first: PostgreSQL poisons an aborted
		// transaction (25P02), so the re-read must happen on the pool.
		_ = tx.Rollback()
		if record, found, readErr := BootstrapStatus(ctx, store); readErr == nil && found {
			return BootstrapResult{}, alreadyBootstrapped(record, "a concurrent bootstrap claimed it first")
		}
		return BootstrapResult{}, fmt.Errorf("claim the bootstrap marker: %w", err)
	}

	// 3. The first identity, with the administrator role.
	user, err := createUserWithIDTx(ctx, tx, engine, userID, email, password, []string{BootstrapRole})
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("create the administrator identity: %w", err)
	}

	// 4. The grants THE ROLE never had. Same call CONTRACT-16's UI makes, on this
	//    transaction.
	permissions := append([]string(nil), schema.Permissions...)
	if err := setRolePermissionsTx(ctx, tx, engine, BootstrapRole, permissions); err != nil {
		return BootstrapResult{}, fmt.Errorf("grant the catalog permissions to role %q: %w", BootstrapRole, err)
	}

	if err := tx.Commit(); err != nil {
		return BootstrapResult{}, fmt.Errorf("commit bootstrap: %w", err)
	}

	dual.SortStrings(permissions)
	return BootstrapResult{
		UserID:      user.ID,
		Email:       user.Email,
		Role:        BootstrapRole,
		Permissions: permissions,
		CreatedAt:   timestamp,
	}, nil
}

// alreadyBootstrapped renders the refusal that MUST NOT read like a generic
// error: it names the identity that holds the installation and when it was
// claimed, so the operator knows immediately that nothing is broken.
func alreadyBootstrapped(record BootstrapRecord, note string) error {
	suffix := ""
	if note != "" {
		suffix = " (" + note + ")"
	}
	return fmt.Errorf(
		"%w%s: it was claimed on %s by %q (user id %s), and a bootstrap can never run twice — to add another administrator, sign in as that user and use the admin UI",
		ErrAlreadyBootstrapped, suffix, record.CreatedAt, record.AdminEmail, record.AdminUserID)
}

// validateBootstrapEmail applies a SHAPE test, not an RFC 5322 implementation:
// non-empty, no whitespace, exactly one "@" with both sides non-empty, and
// within the 254-byte maximum of an addressable mailbox. It exists so a typo
// (or an empty --email) is refused BEFORE a transaction is opened rather than
// creating an identity nobody can sign in as. Anything stricter would start
// rejecting addresses that are legal and deliverable.
func validateBootstrapEmail(email string) error {
	switch {
	case email == "":
		return fmt.Errorf("%w: it is empty", ErrInvalidEmail)
	case len(email) > 254:
		return fmt.Errorf("%w: it is %d bytes long (maximum 254)", ErrInvalidEmail, len(email))
	case strings.ContainsAny(email, " \t\r\n"):
		return fmt.Errorf("%w %q: it contains whitespace", ErrInvalidEmail, email)
	}
	local, domain, found := strings.Cut(email, "@")
	if !found || local == "" || domain == "" || strings.Contains(domain, "@") {
		return fmt.Errorf("%w %q: expected exactly one \"@\" with a non-empty local part and domain", ErrInvalidEmail, email)
	}
	return nil
}

// validateBootstrapPassword rejects the two inputs that cannot produce a usable
// administrator, and nothing else. NO minimum length, complexity rule or
// character policy is invented here: this contract has no mandate to define a
// password policy, and a policy fixed in code is exactly the kind of decision
// the contract refused to make for the other roles' permissions.
//
//   - EMPTY is refused because the single account that administers the whole
//     installation would be openable by pressing Enter.
//   - OVER 72 BYTES is refused because bcrypt cannot hash it; surfacing that as
//     a validation error is strictly better than as a hashing failure inside the
//     transaction.
func validateBootstrapPassword(password string) error {
	switch {
	case password == "":
		return fmt.Errorf("%w: it is empty", ErrInvalidPassword)
	case len(password) > maxPasswordBytes:
		return fmt.Errorf("%w: it is %d bytes long and bcrypt hashes at most %d", ErrInvalidPassword, len(password), maxPasswordBytes)
	}
	return nil
}
