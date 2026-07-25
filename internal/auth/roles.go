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
//
// CONTRACT-19: SetRolePermissions is the third and last operation that cannot go
// through a routine. Its atomicity — one DELETE plus N INSERTs committing
// together — is a deliberate guarantee of CONTRACT-16, and CallRoutine opens its
// own transaction per call, so splitting it across calls would lose exactly what
// this function exists to provide. It therefore composes raw SQL with
// compat.Placeholder inside the transaction it owns. Its READ counterpart,
// RolePermissions, runs outside any transaction and goes through QueryRoutine
// over the role_permission_names view.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
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
// A repeated permission name in the input is harmless: the resolved ids are
// deduped before the inserts, so the composite primary key can never be hit and
// the operation is idempotent. (It used to rely on `ON CONFLICT DO NOTHING`;
// CONTRACT-19 removed the upsert clause because after the DELETE above the only
// possible conflict is a duplicate in the CALLER's list, which dedupe makes
// impossible by construction rather than merely tolerated.) All ids are bound as
// parameters; nothing is interpolated.
func SetRolePermissions(ctx context.Context, store *compat.Store, roleName string, permissionNames []string) error {
	engine := store.Target.Engine
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var roleID string
	lookupRole := `SELECT "id" FROM "roles" WHERE "name" = ` + dual.Bind(engine, 1)
	if err := tx.QueryRowContext(ctx, lookupRole, roleName).Scan(&roleID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w %q", ErrRoleNotFound, roleName)
		}
		return fmt.Errorf("lookup role: %w", err)
	}

	// Resolve BEFORE mutating so an unknown permission aborts with the table
	// untouched — same invariant as SetUserRoles.
	permIDs, err := permissionIDsForNames(ctx, tx, engine, permissionNames)
	if err != nil {
		return err
	}

	clear := `DELETE FROM "role_permissions" WHERE "role_id" = ` + dual.Bind(engine, 1)
	if _, err := tx.ExecContext(ctx, clear, roleID); err != nil {
		return fmt.Errorf("clear role permissions: %w", err)
	}
	grant := `INSERT INTO "role_permissions" ("role_id", "permission_id") VALUES (` +
		dual.Bind(engine, 1) + `, ` + dual.Bind(engine, 2) + `)`
	for _, pid := range dual.Dedupe(permIDs) {
		if _, err := tx.ExecContext(ctx, grant, roleID, pid); err != nil {
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
// error), so the caller maps it to a 404.
//
// CONTRACT-20B — THIS is the listing that carried the defect, and it carried it
// in PRODUCTION, not in a hypothetical. The routine declares
// `ORDER BY permission_name`, which CONTRACT-19 took to mean both engines return
// the same sequence. They do not: PostgreSQL orders TEXT by the database
// COLLATION and SQLite orders it by BYTES, and the FIXED catalog
// (schema.Permissions) contains `content.update` and `content_types.manage` —
// the exact pair CONTRACT-20 measured diverging:
//
//	SQLite  : content.update , content_types.manage
//	Postgres: content_types.manage , content.update
//
// The `administrator` role of production holds all eight catalog permissions, so
// the divergent pair is in the database today; it stayed invisible only because
// the application is still SQLite-only and because the CONTRACT-19 battery's
// fixtures never granted both names to the same role.
//
// The routine's ORDER BY stays as a stable base and the final order is imposed
// here in Go, by BYTE comparison — engine-independent by construction, and the
// same sequence SQLite produces today, so nothing production observes changes.
func RolePermissions(ctx context.Context, store *compat.Store, roleName string) ([]string, bool, error) {
	roleRows, err := store.QueryRoutine(ctx, authSchema, schema.RoutineRoleIDByName, map[string]compat.Value{
		"name": dual.TextValue(roleName),
	})
	if err != nil {
		return nil, false, fmt.Errorf("lookup role: %w", err)
	}
	if len(roleRows) == 0 {
		return nil, false, nil
	}
	rows, err := store.QueryRoutine(ctx, authSchema, schema.RoutineRolePermissionNames, map[string]compat.Value{
		"role_id": dual.UUIDValue(dual.RowText(roleRows[0], "id")),
	})
	if err != nil {
		return nil, false, fmt.Errorf("query role permissions: %w", err)
	}
	var names []string
	for _, row := range rows {
		names = append(names, dual.RowText(row, "permission_name"))
	}
	dual.SortStrings(names)
	return names, true, nil
}

// permissionIDsForNames resolves permission names to their catalog ids inside tx.
// It returns ErrUnknownPermission (wrapped with the offending name) if any name
// is absent from the permissions table — the mirror of roleIDsForNames.
//
// Like roleIDsForNames it stays a raw statement instead of the
// auth_permission_id_by_name routine, because it must read inside the caller's
// transaction and QueryRoutine would open its own.
func permissionIDsForNames(ctx context.Context, tx dual.TxQuerier, engine compat.Engine, permissionNames []string) ([]string, error) {
	statement := `SELECT "id" FROM "permissions" WHERE "name" = ` + dual.Bind(engine, 1)
	ids := make([]string, 0, len(permissionNames))
	for _, name := range permissionNames {
		var pid string
		if err := tx.QueryRowContext(ctx, statement, name).Scan(&pid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w %q", ErrUnknownPermission, name)
			}
			return nil, fmt.Errorf("query permission %q: %w", name, err)
		}
		ids = append(ids, pid)
	}
	return ids, nil
}
