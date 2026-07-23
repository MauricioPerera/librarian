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
// credential failure — unknown email AND wrong password — so the two cases
// are indistinguishable to callers (anti-enumeration). It carries no detail.
var ErrInvalidCredentials = errors.New("invalid credentials")

// User is the application-level view of a row in users plus its assigned role
// names (resolved through user_roles → roles).
type User struct {
	ID    string
	Email string
	Roles []string
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
// wrong password — it returns ErrInvalidCredentials (the same error, same
// message), so a caller cannot distinguish the two cases. A dummy bcrypt
// compare is run on the missing-email branch to equalize timing.
func VerifyCredentials(ctx context.Context, db *sql.DB, email, password string) (*User, error) {
	var (
		id   string
		hash string
	)
	err := db.QueryRowContext(ctx,
		`SELECT id, password_hash FROM users WHERE email = ?`,
		email,
	).Scan(&id, &hash)
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

	roles, err := rolesForUser(ctx, db, id)
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Email: email, Roles: roles}, nil
}

// roleIDsForNames resolves role names to their catalog ids inside tx. Returns
// an error if any name is absent from the roles table.
func roleIDsForNames(ctx context.Context, tx *sql.Tx, roleNames []string) ([]string, error) {
	ids := make([]string, 0, len(roleNames))
	for _, name := range roleNames {
		var rid string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM roles WHERE name = ?`, name).Scan(&rid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("unknown role %q", name)
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
