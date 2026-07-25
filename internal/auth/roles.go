package auth

// CONTRACT-16 T1 — the data layer for role↔permission GRANTS.
//
// This is the write path that `role_permissions` never had (docs/PENDIENTES.md
// hueco 1): until now the only way to change what a role can do was direct SQL
// against the database, which is how production had to be repaired when the
// table was discovered empty.
//
// The shape is deliberately IDENTICAL to SetUserRoles (users.go): a single
// transaction, verify the target entity exists first, resolve EVERY supplied
// name against the fixed catalog BEFORE mutating anything (one unknown name
// aborts with the table untouched), delete the current set, insert the new one.
// Replacement of the whole set — never per-row add/remove — so the caller's
// intent ("these are the role's permissions") is what gets persisted, with no
// read-modify-write race between two admins.
//
// The catalogs themselves (schema.Roles / schema.Permissions) stay fixed in
// code: this file edits GRANTS, it never creates or deletes a role or a
// permission.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrRoleNotFound is returned by SetRolePermissions when the target role name is
// not in the roles catalog. It is a sentinel so the HTML handler can map it to a
// 404 rather than a 500.
var ErrRoleNotFound = errors.New("role not found")

// ErrUnknownPermission is returned when a supplied permission name is not in the
// fixed permission catalog (schema.Permissions, seeded into `permissions`). It is
// a sentinel so a crafted request maps to a 400 (client error), never a 500.
// permissionIDsForNames wraps it with the offending name.
var ErrUnknownPermission = errors.New("unknown permission")

// SetRolePermissions replaces a role's ENTIRE permission set with
// permissionNames, atomically. It runs in a single transaction: resolve the role
// (→ ErrRoleNotFound / 404), resolve every permission name against the seeded
// catalog (an unknown name → ErrUnknownPermission / 400, with nothing changed),
// delete the role's current role_permissions rows, then insert the new set.
//
// An empty permissionNames is VALID and leaves the role with no permissions —
// that is the legitimate "revoke everything from this role" operation. The
// anti-lockout protection is NOT here: it belongs to the caller that knows WHO is
// performing the change (see the guard in internal/server/ui_roles.go), because
// this function is also the one a bootstrap/repair path would use.
//
// A repeated permission name in the input is harmless: role_permissions has a
// composite primary key and the insert uses ON CONFLICT DO NOTHING, so the
// operation is idempotent and cannot duplicate rows. All ids are bound as
// parameters; nothing is interpolated.
func SetRolePermissions(ctx context.Context, db *sql.DB, roleName string, permissionNames []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var roleID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM roles WHERE name = ?`, roleName).Scan(&roleID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w %q", ErrRoleNotFound, roleName)
		}
		return fmt.Errorf("lookup role: %w", err)
	}

	// Resolve BEFORE mutating so an unknown permission aborts with the table
	// untouched — same invariant as SetUserRoles.
	permIDs, err := permissionIDsForNames(ctx, tx, permissionNames)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM role_permissions WHERE role_id = ?`, roleID); err != nil {
		return fmt.Errorf("clear role permissions: %w", err)
	}
	for _, pid := range permIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO role_permissions (role_id, permission_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			roleID, pid,
		); err != nil {
			return fmt.Errorf("grant permission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit role permissions: %w", err)
	}
	return nil
}

// RolePermissions returns the permission names currently granted to a role, by
// role NAME. found is false when the role is not in the catalog (never a raw SQL
// error), so the caller maps it to a 404. Names are ordered for a stable view.
func RolePermissions(ctx context.Context, db *sql.DB, roleName string) ([]string, bool, error) {
	var roleID string
	err := db.QueryRowContext(ctx, `SELECT id FROM roles WHERE name = ?`, roleName).Scan(&roleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lookup role: %w", err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT p.name
		   FROM role_permissions rp
		   JOIN permissions p ON p.id = rp.permission_id
		  WHERE rp.role_id = ?
		  ORDER BY p.name`,
		roleID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("query role permissions: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, false, fmt.Errorf("scan permission: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate permissions: %w", err)
	}
	return names, true, nil
}

// permissionIDsForNames resolves permission names to their catalog ids inside tx.
// It returns ErrUnknownPermission (wrapped with the offending name) if any name
// is absent from the permissions table — the mirror of roleIDsForNames.
func permissionIDsForNames(ctx context.Context, tx *sql.Tx, permissionNames []string) ([]string, error) {
	ids := make([]string, 0, len(permissionNames))
	for _, name := range permissionNames {
		var pid string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM permissions WHERE name = ?`, name).Scan(&pid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w %q", ErrUnknownPermission, name)
			}
			return nil, fmt.Errorf("query permission %q: %w", name, err)
		}
		ids = append(ids, pid)
	}
	return ids, nil
}
