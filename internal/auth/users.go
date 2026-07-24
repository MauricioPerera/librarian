// Package auth implements librarian's dual authentication: password-based user
// credentials with bcrypt (T2), HS256 JWT issue/verify (T3), and SHA-256 API
// keys (T4). All functions operate directly against the shared *sql.DB handle
// exposed by compat.Store.DB using parameterized database/sql — no separate
// connection, no string-concatenated SQL.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
func CreateUser(ctx context.Context, db *sql.DB, email, password string, roleNames []string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO users (email, password_hash, status) VALUES (?, ?, 'active') RETURNING id`,
		email, string(hash),
	).Scan(&id); err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	roleIDs, err := roleIDsForNames(ctx, tx, roleNames)
	if err != nil {
		return nil, err
	}
	for _, rid := range roleIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_roles (user_id, role_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			id, rid,
		); err != nil {
			return nil, fmt.Errorf("assign role: %w", err)
		}
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
func VerifyCredentials(ctx context.Context, db *sql.DB, email, password string) (*User, error) {
	var (
		id     string
		hash   string
		status string
	)
	err := db.QueryRowContext(ctx,
		`SELECT id, password_hash, status FROM users WHERE email = ?`,
		email,
	).Scan(&id, &hash, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Equalize timing with the wrong-password branch: run a real bcrypt
		// compare against a fixed hash. Result is discarded; the error is
		// the generic one regardless.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrInvalidCredentials
	case err != nil:
		return nil, fmt.Errorf("query user: %w", err)
	}

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

	roles, err := rolesForUser(ctx, db, id)
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
func ListUsers(ctx context.Context, db *sql.DB) ([]UserRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, email, status FROM users ORDER BY email`,
	)
	if err != nil {
		return nil, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()
	var out []UserRecord
	for rows.Next() {
		var u UserRecord
		if err := rows.Scan(&u.ID, &u.Email, &u.Status); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	for i := range out {
		roles, err := rolesForUser(ctx, db, out[i].ID)
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
func GetUser(ctx context.Context, db *sql.DB, id string) (UserRecord, bool, error) {
	var u UserRecord
	err := db.QueryRowContext(ctx,
		`SELECT id, email, status FROM users WHERE id = ?`,
		id,
	).Scan(&u.ID, &u.Email, &u.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return UserRecord{}, false, nil
	}
	if err != nil {
		return UserRecord{}, false, fmt.Errorf("query user: %w", err)
	}
	roles, err := rolesForUser(ctx, db, u.ID)
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
func UpdateUserStatus(ctx context.Context, db *sql.DB, id, status string) error {
	if !validStatus(status) {
		return fmt.Errorf("%w %q", ErrInvalidStatus, status)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE users SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		status, id,
	)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrUserNotFound
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
func SetUserRoles(ctx context.Context, db *sql.DB, userID string, roleNames []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var exists string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = ?`, userID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return fmt.Errorf("lookup user: %w", err)
	}

	// Resolve BEFORE mutating so an unknown role aborts with the table untouched.
	roleIDs, err := roleIDsForNames(ctx, tx, roleNames)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("clear roles: %w", err)
	}
	for _, rid := range roleIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_roles (user_id, role_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			userID, rid,
		); err != nil {
			return fmt.Errorf("assign role: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit roles: %w", err)
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
func roleIDsForNames(ctx context.Context, tx *sql.Tx, roleNames []string) ([]string, error) {
	ids := make([]string, 0, len(roleNames))
	for _, name := range roleNames {
		var rid string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM roles WHERE name = ?`, name).Scan(&rid); err != nil {
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
// user_roles junction.
func rolesForUser(ctx context.Context, db *sql.DB, userID string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT r.name
		   FROM user_roles ur
		   JOIN roles r ON r.id = ur.role_id
		  WHERE ur.user_id = ?`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("query user roles: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate roles: %w", err)
	}
	return names, nil
}
