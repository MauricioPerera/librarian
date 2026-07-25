// Package auth implements librarian's dual authentication: password-based user
// credentials with bcrypt (T2), HS256 JWT issue/verify (T3), and SHA-256 API
// keys (T4).
//
// CONTRACT-19: every function that touches the database takes a *compat.Store
// instead of a bare *sql.DB. The store carries BOTH the connection and the
// ENGINE, which is what the three portable doors need — QueryRoutine and
// CallRoutine are methods on it, and compat.Placeholder needs the engine to emit
// `?` or `$n`. A bare *sql.DB cannot answer "which engine is this?", and asking
// the driver would be exactly the engine-branching this layer exists to remove.
// *compat.Store was chosen over passing the engine as an extra argument because
// the two values must never be paired wrongly, and because it is the receiver
// the routine calls already require.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidCredentials is the single, generic error returned for every
// credential failure — unknown email, wrong password, AND a non-active account
// — so the cases are indistinguishable to callers (anti-enumeration). It
// carries no detail.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrUserNotFound is returned by the CONTRACT-08 data functions (UpdateUserStatus,
// SetUserRoles) when the target user id does not exist, so the HTML handlers can
// map it to a 404 rather than a 500.
var ErrUserNotFound = errors.New("user not found")

// ErrUnknownRole is returned when a supplied role name is not in the fixed role
// catalog (schema.Roles). It is a sentinel so callers can map an unknown role to
// a 400 (client error) rather than a 500. roleIDsForNames wraps it with the
// offending name.
var ErrUnknownRole = errors.New("unknown role")

// ErrInvalidStatus is returned by UpdateUserStatus when the requested status is
// not one of UserStatuses (the values the schema CHECK allows).
var ErrInvalidStatus = errors.New("invalid status")

// UserStatuses is the fixed set of values the users.status column accepts,
// mirroring the CHECK constraint declared in internal/schema/schema.go. It is
// the single source both the status-update validation and the UI status selector
// read from, so the two never drift.
var UserStatuses = []string{"active", "suspended", "invited"}

// statusActive is the only status that may authenticate. A user must be active
// to pass VerifyCredentials; suspended and invited are both rejected with the
// generic ErrInvalidCredentials.
const statusActive = "active"

// User is the application-level view of a row in users plus its assigned role
// names (resolved through user_roles → roles).
type User struct {
	ID    string
	Email string
	Roles []string
}

// UserRecord is the admin-facing view of a user row for the CONTRACT-08 UI:
// email + current status + assigned role names. Distinct from User (the auth
// subject carried in a JWT) because the admin listing/detail needs the status,
// which the auth subject does not.
type UserRecord struct {
	ID     string
	Email  string
	Status string
	Roles  []string
}

// dummyHash is a precomputed bcrypt hash used only to keep the verify path's
// CPU cost roughly constant whether or not the email exists. Without it, a
// missing-email response returns immediately while a wrong-password response
// runs bcrypt, leaking existence via timing. Comparing against a fixed hash
// equalizes both branches. The hash itself encodes no secret — it is the
// bcrypt digest of a random throwaway password.
var dummyHash = func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("dummy-timing-equalizer"), bcrypt.DefaultCost)
	return h
}()

// CreateUser inserts a new user with the given email and password (hashed with
// bcrypt, never stored or logged in plaintext), sets status to "active", and
// assigns the supplied role names via user_roles. Role names that do not exist
// in the roles catalog are an error. The plaintext password is hashed and
// discarded within this function; only the hash crosses the DB boundary.
//
// CONTRACT-19: this is one of the three operations that CANNOT go through a
// routine. It writes a user row and then N junction rows, where N comes from the
// caller, and all of it must commit together; a routine's action list is static,
// and CallRoutine would open a transaction per call. So it composes raw SQL with
// compat.Placeholder inside the transaction it owns. The id, previously read
// back with `RETURNING id`, is generated here and bound as one more value.
func CreateUser(ctx context.Context, store *compat.Store, email, password string, roleNames []string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	id, err := dual.NewUUID()
	if err != nil {
		return nil, fmt.Errorf("generate user id: %w", err)
	}
	engine := store.Target.Engine
	timestamp := now()

	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	insertUser := `INSERT INTO "users" ("id", "email", "password_hash", "status", "created_at", "updated_at")` +
		` VALUES (` + dual.Bind(engine, 1) + `, ` + dual.Bind(engine, 2) + `, ` + dual.Bind(engine, 3) +
		`, ` + dual.Bind(engine, 4) + `, ` + dual.Bind(engine, 5) + `, ` + dual.Bind(engine, 6) + `)`
	if _, err := tx.ExecContext(ctx, insertUser, id, email, string(hash), statusActive, timestamp, timestamp); err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	roleIDs, err := roleIDsForNames(ctx, tx, engine, roleNames)
	if err != nil {
		return nil, err
	}
	if err := insertUserRoles(ctx, tx, engine, id, roleIDs); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit user: %w", err)
	}
	return &User{ID: id, Email: email, Roles: roleNames}, nil
}

// VerifyCredentials checks email + password against the stored bcrypt hash and
// returns the user with its roles on success. On any failure — unknown email,
// wrong password, or an account whose status is not "active" (suspended or
// invited) — it returns ErrInvalidCredentials (the same error, same message),
// so a caller cannot distinguish the cases. A dummy bcrypt compare is run on
// the missing-email branch to equalize timing.
//
// The status check is deliberately placed AFTER the bcrypt compare, not before:
// running the (expensive, constant-cost) compare on every existing-email path
// keeps a suspended account's timing indistinguishable from a wrong-password
// one. Putting the status test before the compare would create a faster branch
// for suspended users and leak account state by timing — the same enumeration
// leak this function already avoids for missing emails.
func VerifyCredentials(ctx context.Context, store *compat.Store, email, password string) (*User, error) {
	rows, err := store.QueryRoutine(ctx, authSchema, schema.RoutineUserCredentialsByEmail, map[string]compat.Value{
		"email": dual.TextValue(email),
	})
	if err != nil {
		return nil, fmt.Errorf("query user: %w", err)
	}
	if len(rows) == 0 {
		// Equalize timing with the wrong-password branch: run a real bcrypt
		// compare against a fixed hash. Result is discarded; the error is
		// the generic one regardless.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrInvalidCredentials
	}
	id := dual.RowText(rows[0], "id")
	hash := dual.RowText(rows[0], "password_hash")
	status := dual.RowText(rows[0], "status")

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}

	// Only an active account may authenticate. A suspended or invited user with
	// a correct password is rejected with the SAME generic error — no separate
	// "account suspended" message (anti-enumeration). Checked after the compare
	// above so the branch cost is identical to the wrong-password branch.
	if status != statusActive {
		return nil, ErrInvalidCredentials
	}

	roles, err := rolesForUser(ctx, store, id)
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Email: email, Roles: roles}, nil
}

// ListUsers returns every user with its status and assigned role names, ordered
// by email for a stable admin listing. Roles are resolved per user via
// rolesForUser (the same junction query the rest of the package uses); the
// catalog is small and this runs on an admin page, so the per-user query is
// acceptable and avoids duplicating an aggregation query.
//
// The routine's declared ORDER BY (email, which is UNIQUE, so it is TOTAL)
// provides a stable base, and CONTRACT-20B imposes the FINAL order here, in Go,
// by BYTE comparison.
//
// Declaring the ORDER BY was not enough, and that is the defect this contract
// fixes: PostgreSQL orders TEXT by the database COLLATION while SQLite orders it
// by BYTES, so the same declared `ORDER BY email` returns the same rows in a
// DIFFERENT SEQUENCE on the two engines whenever two emails differ in
// punctuation or in case — `soporte.web@…` before `soporte_admin@…` on SQLite
// and after it on PostgreSQL; `Redaccion@…` before `prensa@…` on SQLite and
// after it on PostgreSQL. Both are addresses an admin can register today.
// Sorting here is engine-independent by construction, and BYTE comparison
// specifically because that is what SQLite already does — the order production
// observes does not change. This listing is not paginated, so re-ordering after
// the read chooses the whole set rather than reshuffling a page.
func ListUsers(ctx context.Context, store *compat.Store) ([]UserRecord, error) {
	rows, err := store.QueryRoutine(ctx, authSchema, schema.RoutineListUsers, nil)
	if err != nil {
		return nil, fmt.Errorf("query users: %w", err)
	}
	var out []UserRecord
	for _, row := range rows {
		out = append(out, UserRecord{
			ID:     dual.RowText(row, "id"),
			Email:  dual.RowText(row, "email"),
			Status: dual.RowText(row, "status"),
		})
	}
	dual.SortByKeys(out, func(u UserRecord) []dual.Key { return dual.Ascending(u.Email) })
	for i := range out {
		roles, err := rolesForUser(ctx, store, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Roles = roles
	}
	return out, nil
}

// GetUser loads one user (id, email, status, roles) by id. found is false when
// no row matches (a missing or malformed id) — never a raw SQL error — so the
// caller maps it to a 404; err is non-nil only on a real DB failure.
func GetUser(ctx context.Context, store *compat.Store, id string) (UserRecord, bool, error) {
	rows, err := store.QueryRoutine(ctx, authSchema, schema.RoutineUserByID, map[string]compat.Value{
		"id": dual.UUIDValue(id),
	})
	if err != nil {
		return UserRecord{}, false, fmt.Errorf("query user: %w", err)
	}
	if len(rows) == 0 {
		return UserRecord{}, false, nil
	}
	u := UserRecord{
		ID:     dual.RowText(rows[0], "id"),
		Email:  dual.RowText(rows[0], "email"),
		Status: dual.RowText(rows[0], "status"),
	}
	roles, err := rolesForUser(ctx, store, u.ID)
	if err != nil {
		return UserRecord{}, false, err
	}
	u.Roles = roles
	return u, true, nil
}

// UpdateUserStatus sets a user's status to one of UserStatuses. An unrecognized
// status is rejected with ErrInvalidStatus BEFORE any SQL (defence in depth over
// the schema CHECK, and a clean 400 for the handler). A status not matching any
// row returns ErrUserNotFound (→ 404). The status string is bound as a
// parameter, never interpolated.
//
// CONTRACT-19: the write goes through CallRoutine, which returns only an error —
// there is no RowsAffected to test. The not-found case is therefore decided by
// reading the row first, which produces the same observable result (the previous
// implementation reported ErrUserNotFound exactly when no row matched the id).
// The UPDATE stays a single statement in its own transaction, so nothing that
// was atomic before has been split.
func UpdateUserStatus(ctx context.Context, store *compat.Store, id, status string) error {
	if !validStatus(status) {
		return fmt.Errorf("%w %q", ErrInvalidStatus, status)
	}
	rows, err := store.QueryRoutine(ctx, authSchema, schema.RoutineUserByID, map[string]compat.Value{
		"id": dual.UUIDValue(id),
	})
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	if len(rows) == 0 {
		return ErrUserNotFound
	}
	if err := store.CallRoutine(ctx, authSchema, schema.RoutineUpdateUserStatus, map[string]compat.Value{
		"id":         dual.UUIDValue(id),
		"status":     dual.TextValue(status),
		"updated_at": timestampValue(now()),
	}); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

// SetUserRoles replaces a user's entire role assignment with roleNames (the UI's
// "reemplazar la asignación de roles"). It runs in a single transaction: verify
// the user exists (→ ErrUserNotFound / 404), resolve every name against the
// fixed catalog (an unknown name → ErrUnknownRole / 400, and nothing is
// changed), delete the current user_roles rows, then insert the new set. An
// empty roleNames is valid — it removes all roles. All ids are bound as
// parameters; roleIDsForNames rejects any name not in the catalog, so a crafted
// request cannot assign a non-catalog role.
//
// CONTRACT-19: raw SQL with compat.Placeholder, for the same structural reason
// as CreateUser — one DELETE plus N INSERTs that must commit together, with N
// coming from the caller.
func SetUserRoles(ctx context.Context, store *compat.Store, userID string, roleNames []string) error {
	engine := store.Target.Engine
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var exists string
	lookupUser := `SELECT "id" FROM "users" WHERE "id" = ` + dual.Bind(engine, 1)
	if err := tx.QueryRowContext(ctx, lookupUser, userID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return fmt.Errorf("lookup user: %w", err)
	}

	// Resolve BEFORE mutating so an unknown role aborts with the table untouched.
	roleIDs, err := roleIDsForNames(ctx, tx, engine, roleNames)
	if err != nil {
		return err
	}

	clear := `DELETE FROM "user_roles" WHERE "user_id" = ` + dual.Bind(engine, 1)
	if _, err := tx.ExecContext(ctx, clear, userID); err != nil {
		return fmt.Errorf("clear roles: %w", err)
	}
	if err := insertUserRoles(ctx, tx, engine, userID, roleIDs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit roles: %w", err)
	}
	return nil
}

// insertUserRoles writes the junction rows for one user. The ids are deduped, so
// a role name repeated by the caller inserts one row instead of colliding with
// the composite primary key — the behavior `ON CONFLICT DO NOTHING` used to give,
// obtained here by making the conflict impossible instead of tolerating it.
func insertUserRoles(ctx context.Context, tx dual.TxQuerier, engine compat.Engine, userID string, roleIDs []string) error {
	assign := `INSERT INTO "user_roles" ("user_id", "role_id") VALUES (` +
		dual.Bind(engine, 1) + `, ` + dual.Bind(engine, 2) + `)`
	for _, rid := range dual.Dedupe(roleIDs) {
		if _, err := tx.ExecContext(ctx, assign, userID, rid); err != nil {
			return fmt.Errorf("assign role: %w", err)
		}
	}
	return nil
}

// validStatus reports whether s is one of the allowed UserStatuses.
func validStatus(s string) bool {
	for _, v := range UserStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// roleIDsForNames resolves role names to their catalog ids inside tx. Returns
// an error if any name is absent from the roles table.
//
// It stays a raw statement rather than the auth_role_id_by_name routine on
// purpose: it runs INSIDE the caller's transaction, and QueryRoutine would open
// its own — the resolve-before-mutate guarantee would then read a catalog
// outside the transaction that is about to delete and re-insert.
func roleIDsForNames(ctx context.Context, tx dual.TxQuerier, engine compat.Engine, roleNames []string) ([]string, error) {
	statement := `SELECT "id" FROM "roles" WHERE "name" = ` + dual.Bind(engine, 1)
	ids := make([]string, 0, len(roleNames))
	for _, name := range roleNames {
		var rid string
		if err := tx.QueryRowContext(ctx, statement, name).Scan(&rid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w %q", ErrUnknownRole, name)
			}
			return nil, fmt.Errorf("query role %q: %w", name, err)
		}
		ids = append(ids, rid)
	}
	return ids, nil
}

// rolesForUser returns the names of the roles assigned to a user, via the
// user_role_names view (the user_roles → roles JOIN, declared in the canonical
// schema so compat compiles it for both engines).
//
// CONTRACT-20B: the routine's declared ORDER BY is a stable base and the final
// order is imposed here in Go, by BYTE comparison, for the same reason as
// ListUsers. Measured honestly, this particular listing CANNOT diverge today —
// schema.Roles is a fixed catalog of four names made of lower-case letters only,
// where a byte comparison and a collation comparison provably agree — so this
// call changes nothing observable and is not what fixed the defect. It is here so
// the guarantee holds by construction rather than by a property of the current
// catalog: role names are the kind of value a later contract could make editable.
func rolesForUser(ctx context.Context, store *compat.Store, userID string) ([]string, error) {
	rows, err := store.QueryRoutine(ctx, authSchema, schema.RoutineUserRoleNames, map[string]compat.Value{
		"user_id": dual.UUIDValue(userID),
	})
	if err != nil {
		return nil, fmt.Errorf("query user roles: %w", err)
	}
	var names []string
	for _, row := range rows {
		names = append(names, dual.RowText(row, "role_name"))
	}
	dual.SortStrings(names)
	return names, nil
}
