package server

// CONTRACT-16 T2 — direct unit tests of the anti-lockout guard.
//
// This is the ONLY test file in package `server` (everything else is the
// external `server_test`). It is internal on purpose: the guard's rule must be
// verifiable for BOTH identity kinds, and an API-key identity cannot reach the
// editor over HTTP (the surface is session/cookie-only, by design — see the
// report). Testing actorKeepsRolesManage directly is therefore the honest way to
// show that an API key bound to the edited role is stopped by the same rule as a
// human, rather than asserting it only in prose.
//
// The four cases the contract enumerates are covered here as (a)…(d), and the
// HTTP-level consequences of (a)…(c) are additionally proven end-to-end in
// server_ui_roles_test.go.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// guardDB opens a seeded temp database and returns handlers wired to it.
func guardDB(t *testing.T) (*handlers, *compat.Store, func()) {
	t.Helper()
	sdb, err := store.Open(filepath.Join(t.TempDir(), "guard.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if err := store.EnsureSchema(ctx, sdb); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.SeedCatalogs(ctx, sdb.DB); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return &handlers{db: sdb.DB, store: sdb, jwtSecret: "guard-secret"}, sdb, func() { _ = sdb.Close() }
}

// setPerms is a shorthand for the real data function under test's sibling.
func setPerms(t *testing.T, db *compat.Store, role string, perms ...string) {
	t.Helper()
	if err := auth.SetRolePermissions(context.Background(), db, role, perms); err != nil {
		t.Fatalf("set perms for %q: %v", role, err)
	}
}

// TestGuardCaseA_SingleRoleRemovingFromItselfIsRejected — case (a): the actor
// holds roles.manage through exactly ONE role and edits THAT role, dropping
// roles.manage. The resulting state leaves them unable to administer → rejected.
func TestGuardCaseA_SingleRoleRemovingFromItselfIsRejected(t *testing.T) {
	h, db, done := guardDB(t)
	defer done()
	setPerms(t, db, "administrator", "roles.manage", "users.manage")

	actor := &Identity{Kind: "jwt", Roles: []string{"administrator"}}
	keeps, err := h.actorKeepsRolesManage(context.Background(), actor, "administrator",
		[]string{"users.manage"}) // roles.manage dropped
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if keeps {
		t.Errorf("case (a): guard allowed the actor to strip their only source of roles.manage")
	}
}

// TestGuardCaseB_RemovingFromAnotherRoleIsAllowed — case (b): the same actor
// revokes roles.manage from a role they do NOT hold. Their own effective set is
// untouched → allowed.
func TestGuardCaseB_RemovingFromAnotherRoleIsAllowed(t *testing.T) {
	h, db, done := guardDB(t)
	defer done()
	setPerms(t, db, "administrator", "roles.manage")
	setPerms(t, db, "editor", "roles.manage", "content.create")

	actor := &Identity{Kind: "jwt", Roles: []string{"administrator"}}
	keeps, err := h.actorKeepsRolesManage(context.Background(), actor, "editor",
		[]string{"content.create"}) // strip roles.manage from a role the actor lacks
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if !keeps {
		t.Errorf("case (b): guard blocked an edit to a role the actor does not hold")
	}
}

// TestGuardCaseC_TwoGrantingRolesRemoveOneIsAllowedThenSecondRejected — case
// (c) plus the contract's red-team follow-up: with TWO roles that both grant
// roles.manage, removing it from one is allowed (the other still supplies it);
// once that is applied, the SECOND removal must be rejected. The guard cannot be
// evaded by splitting the lockout across two operations.
func TestGuardCaseC_TwoGrantingRolesRemoveOneIsAllowedThenSecondRejected(t *testing.T) {
	h, db, done := guardDB(t)
	defer done()
	ctx := context.Background()
	setPerms(t, db, "administrator", "roles.manage")
	setPerms(t, db, "editor", "roles.manage", "content.create")

	actor := &Identity{Kind: "jwt", Roles: []string{"administrator", "editor"}}

	// First removal: allowed — administrator still grants it.
	keeps, err := h.actorKeepsRolesManage(ctx, actor, "editor", []string{"content.create"})
	if err != nil {
		t.Fatalf("guard 1: %v", err)
	}
	if !keeps {
		t.Fatalf("case (c): first removal blocked although the other role still grants roles.manage")
	}
	// Apply it for real, then attempt the second removal.
	setPerms(t, db, "editor", "content.create")

	keeps, err = h.actorKeepsRolesManage(ctx, actor, "administrator", []string{})
	if err != nil {
		t.Fatalf("guard 2: %v", err)
	}
	if keeps {
		t.Errorf("case (c) red-team: guard allowed the SECOND removal — lockout achieved in two steps")
	}
}

// TestGuardCaseD_APIKeyIdentity — case (d): an API key is bound to a single
// role. If that key's role is the one being edited and the new set drops
// roles.manage, the key locks ITSELF out → rejected, by the exact same code path
// as a human. Editing a different role is allowed. The key's role is resolved
// from its role_id (an API-key Identity carries no role names).
func TestGuardCaseD_APIKeyIdentity(t *testing.T) {
	h, db, done := guardDB(t)
	defer done()
	ctx := context.Background()
	setPerms(t, db, "administrator", "roles.manage", "users.manage")
	setPerms(t, db, "editor", "roles.manage")

	var adminRoleID string
	if err := db.DB.QueryRow(`SELECT id FROM roles WHERE name = ?`, "administrator").Scan(&adminRoleID); err != nil {
		t.Fatalf("role id: %v", err)
	}
	key := &Identity{Kind: "apikey", RoleID: adminRoleID, Label: "ci-key"}

	// Self-lockout attempt → rejected.
	keeps, err := h.actorKeepsRolesManage(ctx, key, "administrator", []string{"users.manage"})
	if err != nil {
		t.Fatalf("guard apikey self: %v", err)
	}
	if keeps {
		t.Errorf("case (d): an API key was allowed to strip roles.manage from its own bound role")
	}

	// Editing a different role → allowed (the key's own set is untouched).
	keeps, err = h.actorKeepsRolesManage(ctx, key, "editor", []string{})
	if err != nil {
		t.Fatalf("guard apikey other: %v", err)
	}
	if !keeps {
		t.Errorf("case (d): guard blocked an API key editing a role it is not bound to")
	}

	// Keeping roles.manage while editing its own role → allowed.
	keeps, err = h.actorKeepsRolesManage(ctx, key, "administrator", []string{"roles.manage"})
	if err != nil {
		t.Fatalf("guard apikey keep: %v", err)
	}
	if !keeps {
		t.Errorf("case (d): guard blocked an edit that preserves roles.manage")
	}

	// An API key whose bound role no longer exists resolves to no roles → the
	// guard fails closed only when it would actually matter (it holds no
	// roles.manage anyway, so the edited role is never "one of its own").
	orphan := &Identity{Kind: "apikey", RoleID: "00000000-0000-0000-0000-000000000000"}
	if keeps, err := h.actorKeepsRolesManage(ctx, orphan, "administrator", []string{}); err != nil || !keeps {
		t.Errorf("orphan key: keeps=%v err=%v, want keeps=true err=nil (edited role is not its own)", keeps, err)
	}
}

// TestGuardAllowsUnrelatedPermissionRemovalOnOwnRole is the contract's other
// red-team: editing your OWN role to drop an unrelated permission
// (content.create) while KEEPING roles.manage must be allowed — the guard is
// about roles.manage only, not about "do not touch your own role".
func TestGuardAllowsUnrelatedPermissionRemovalOnOwnRole(t *testing.T) {
	h, db, done := guardDB(t)
	defer done()
	setPerms(t, db, "administrator", "roles.manage", "content.create")

	actor := &Identity{Kind: "jwt", Roles: []string{"administrator"}}
	keeps, err := h.actorKeepsRolesManage(context.Background(), actor, "administrator",
		[]string{"roles.manage"}) // content.create dropped, roles.manage kept
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if !keeps {
		t.Errorf("guard blocked dropping an unrelated permission from the actor's own role")
	}
}

// TestGuardNilIdentityFailsClosed: no identity (impossible behind the
// middleware) must never be read as "allowed".
func TestGuardNilIdentityFailsClosed(t *testing.T) {
	h, _, done := guardDB(t)
	defer done()
	if keeps, err := h.actorKeepsRolesManage(context.Background(), nil, "administrator", nil); err != nil || keeps {
		t.Errorf("nil identity: keeps=%v err=%v, want keeps=false", keeps, err)
	}
}
