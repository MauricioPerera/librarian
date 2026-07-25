package auth_test

// CONTRACT-16 T1 unit tests: the atomic replacement of a role's permission set
// (auth.SetRolePermissions) and its read counterpart (auth.RolePermissions).
// These exercise the auth package directly against a seeded temp DB, with no
// HTTP/UI layer involved (openDB/roleID live in auth_test.go).

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// grantedPerms reads a role's grants with a DIRECT SQL query — deliberately NOT
// through auth.RolePermissions, so the assertions are not self-fulfilling.
func grantedPerms(t *testing.T, db *compat.Store, role string) []string {
	t.Helper()
	rows, err := db.DB.Query(
		`SELECT p.name
		   FROM role_permissions rp
		   JOIN roles r       ON r.id = rp.role_id
		   JOIN permissions p ON p.id = rp.permission_id
		  WHERE r.name = ?`, role)
	if err != nil {
		t.Fatalf("query grants for %q: %v", role, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan grant: %v", err)
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// equalSets reports whether got equals want as a sorted set.
func equalSets(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// TestSetRolePermissionsReplacesWholeSet is the core T1 test: the function
// REPLACES the set (it is not an add), an empty set is valid and revokes
// everything, and the result is confirmed by direct SQL.
func TestSetRolePermissionsReplacesWholeSet(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	// Fresh role: no grants at all (the exact production state that broke).
	if got := grantedPerms(t, db, "editor"); len(got) != 0 {
		t.Fatalf("editor starts with grants: %v", got)
	}

	// Grant a set.
	if err := auth.SetRolePermissions(ctx, db, "editor", []string{"content.create", "content.update"}); err != nil {
		t.Fatalf("set 1: %v", err)
	}
	if got := grantedPerms(t, db, "editor"); !equalSets(got, []string{"content.create", "content.update"}) {
		t.Fatalf("after set 1 = %v, want [content.create content.update]", got)
	}

	// Replace with a DIFFERENT set: the old ones must be gone, not merged.
	if err := auth.SetRolePermissions(ctx, db, "editor", []string{"content.publish", "terms.manage"}); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	if got := grantedPerms(t, db, "editor"); !equalSets(got, []string{"content.publish", "terms.manage"}) {
		t.Fatalf("after set 2 = %v, want [content.publish terms.manage] (replacement, not merge)", got)
	}

	// The empty set is valid: it revokes everything.
	if err := auth.SetRolePermissions(ctx, db, "editor", nil); err != nil {
		t.Fatalf("set empty: %v", err)
	}
	if got := grantedPerms(t, db, "editor"); len(got) != 0 {
		t.Fatalf("after empty set = %v, want none", got)
	}

	// Another role is untouched throughout.
	if got := grantedPerms(t, db, "administrator"); len(got) != 0 {
		t.Fatalf("administrator collaterally modified: %v", got)
	}
}

// TestSetRolePermissionsUnknownRole covers the 404 sentinel: an unknown role
// name aborts with ErrRoleNotFound and mutates nothing.
func TestSetRolePermissionsUnknownRole(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	var before int
	db.DB.QueryRow(`SELECT COUNT(*) FROM role_permissions`).Scan(&before)

	err := auth.SetRolePermissions(ctx, db, "superadmin", []string{"content.create"})
	if !errors.Is(err, auth.ErrRoleNotFound) {
		t.Fatalf("err = %v, want ErrRoleNotFound", err)
	}
	var after int
	db.DB.QueryRow(`SELECT COUNT(*) FROM role_permissions`).Scan(&after)
	if after != before {
		t.Errorf("role_permissions mutated on unknown role: %d → %d", before, after)
	}
}

// TestSetRolePermissionsUnknownPermissionIsAtomic is the atomicity red-team: a
// set containing one VALID and one unknown permission must abort with
// ErrUnknownPermission and leave the role's PREVIOUS grants exactly as they were
// — the valid one must not sneak in.
func TestSetRolePermissionsUnknownPermissionIsAtomic(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	if err := auth.SetRolePermissions(ctx, db, "author", []string{"content.create"}); err != nil {
		t.Fatalf("seed grants: %v", err)
	}

	err := auth.SetRolePermissions(ctx, db, "author", []string{"content.publish", "articles.nuke"})
	if !errors.Is(err, auth.ErrUnknownPermission) {
		t.Fatalf("err = %v, want ErrUnknownPermission", err)
	}
	// Untouched: neither the delete nor the partial insert happened.
	if got := grantedPerms(t, db, "author"); !equalSets(got, []string{"content.create"}) {
		t.Fatalf("grants after rejected set = %v, want [content.create] (nothing mutated)", got)
	}
}

// TestSetRolePermissionsIsIdempotentOnRepeats is the red-team from the contract
// checklist: the same permission repeated N times in one call must not duplicate
// rows (composite PK + ON CONFLICT DO NOTHING), and re-applying the identical
// set twice must be a no-op.
func TestSetRolePermissionsIsIdempotentOnRepeats(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	if err := auth.SetRolePermissions(ctx, db, "contributor",
		[]string{"content.create", "content.create", "content.create"}); err != nil {
		t.Fatalf("set with repeats: %v", err)
	}
	var n int
	if err := db.DB.QueryRow(
		`SELECT COUNT(*) FROM role_permissions rp JOIN roles r ON r.id = rp.role_id WHERE r.name = ?`,
		"contributor").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Errorf("rows for repeated permission = %d, want 1 (composite PK must dedupe)", n)
	}

	// Applying the identical set again is a no-op.
	if err := auth.SetRolePermissions(ctx, db, "contributor", []string{"content.create"}); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if got := grantedPerms(t, db, "contributor"); !equalSets(got, []string{"content.create"}) {
		t.Errorf("after re-apply = %v, want [content.create]", got)
	}
}

// TestRolePermissionsRead covers the read counterpart: it returns the live
// grants for an existing role and found=false (never an error) for one outside
// the catalog.
func TestRolePermissionsRead(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	if err := auth.SetRolePermissions(ctx, db, "administrator",
		[]string{"roles.manage", "users.manage"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, found, err := auth.RolePermissions(ctx, db, "administrator")
	if err != nil || !found {
		t.Fatalf("RolePermissions: found=%v err=%v", found, err)
	}
	if !equalSets(got, []string{"roles.manage", "users.manage"}) {
		t.Errorf("read = %v, want [roles.manage users.manage]", got)
	}

	if _, found, err := auth.RolePermissions(ctx, db, "ghost-role"); err != nil || found {
		t.Errorf("unknown role: found=%v err=%v, want found=false err=nil", found, err)
	}
}
