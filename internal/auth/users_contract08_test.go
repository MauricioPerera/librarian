package auth_test

// CONTRACT-08 T1 unit tests: the VerifyCredentials status gate (the security fix)
// plus the new data functions (ListUsers, GetUser, UpdateUserStatus,
// SetUserRoles). These exercise the auth package directly against a seeded temp
// DB, independent of the HTTP/UI layer (openDB/roleID live in auth_test.go).

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
)

// TestVerifyCredentialsSuspendedRejected is the core security-fix test: a user
// with the CORRECT password but status != active is rejected with the SAME
// generic ErrInvalidCredentials as a wrong password / unknown user — no distinct
// "suspended" signal (anti-enumeration). Covers both suspended and invited (the
// red-team "invited can't log in" case), and confirms active still succeeds.
func TestVerifyCredentialsSuspendedRejected(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	if _, err := auth.CreateUser(ctx, db, "sus@example.com", "correct-pw", []string{"editor"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Baseline: active user with the correct password logs in.
	if _, err := auth.VerifyCredentials(ctx, db, "sus@example.com", "correct-pw"); err != nil {
		t.Fatalf("active user should log in: %v", err)
	}

	// The generic error every failure must return.
	_, errUnknown := auth.VerifyCredentials(ctx, db, "ghost@example.com", "correct-pw")
	if !errors.Is(errUnknown, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown user err = %v, want ErrInvalidCredentials", errUnknown)
	}

	for _, status := range []string{"suspended", "invited"} {
		if err := auth.UpdateUserStatus(ctx, db, userID(t, db, "sus@example.com"), status); err != nil {
			t.Fatalf("set status %q: %v", status, err)
		}
		_, err := auth.VerifyCredentials(ctx, db, "sus@example.com", "correct-pw")
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("status %q: err = %v, want ErrInvalidCredentials", status, err)
		}
		// The message must be byte-identical to the unknown-user case — any
		// divergence is an account-state enumeration leak.
		if err.Error() != errUnknown.Error() {
			t.Fatalf("status %q: message %q differs from unknown-user %q — enumeration leak",
				status, err.Error(), errUnknown.Error())
		}
	}

	// Reactivating restores login (round-trip of the fix).
	if err := auth.UpdateUserStatus(ctx, db, userID(t, db, "sus@example.com"), "active"); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if _, err := auth.VerifyCredentials(ctx, db, "sus@example.com", "correct-pw"); err != nil {
		t.Fatalf("reactivated user should log in again: %v", err)
	}
}

// TestListUsers covers ListUsers: it returns every user with status + roles,
// ordered by email.
func TestListUsers(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	if _, err := auth.CreateUser(ctx, db, "zeta@example.com", "pw", []string{"author"}); err != nil {
		t.Fatalf("create zeta: %v", err)
	}
	if _, err := auth.CreateUser(ctx, db, "alpha@example.com", "pw", []string{"editor", "administrator"}); err != nil {
		t.Fatalf("create alpha: %v", err)
	}

	users, err := auth.ListUsers(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("len = %d, want 2", len(users))
	}
	// Ordered by email → alpha first.
	if users[0].Email != "alpha@example.com" || users[1].Email != "zeta@example.com" {
		t.Fatalf("order = [%q,%q], want alpha,zeta", users[0].Email, users[1].Email)
	}
	if users[0].Status != "active" {
		t.Errorf("new user status = %q, want active", users[0].Status)
	}
	if len(users[0].Roles) != 2 {
		t.Errorf("alpha roles = %v, want 2", users[0].Roles)
	}
}

// TestGetUser covers GetUser: a present id returns email/status/roles; a missing
// id returns found=false (mapped to 404 by the handler), not an error.
func TestGetUser(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	created, err := auth.CreateUser(ctx, db, "get@example.com", "pw", []string{"contributor"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	u, found, err := auth.GetUser(ctx, db, created.ID)
	if err != nil || !found {
		t.Fatalf("get present: found=%v err=%v", found, err)
	}
	if u.Email != "get@example.com" || u.Status != "active" {
		t.Errorf("got %+v", u)
	}
	if len(u.Roles) != 1 || u.Roles[0] != "contributor" {
		t.Errorf("roles = %v, want [contributor]", u.Roles)
	}

	// Missing id → found=false, no error, no panic (malformed id included).
	if _, found, err := auth.GetUser(ctx, db, "11111111-1111-1111-1111-111111111111"); found || err != nil {
		t.Errorf("get missing: found=%v err=%v, want false,nil", found, err)
	}
	if _, found, err := auth.GetUser(ctx, db, "not-a-uuid"); found || err != nil {
		t.Errorf("get malformed: found=%v err=%v, want false,nil", found, err)
	}
}

// TestUpdateUserStatus covers the status setter: a valid transition persists; a
// missing id → ErrUserNotFound; an invalid value → ErrInvalidStatus (before any
// write).
func TestUpdateUserStatus(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	created, err := auth.CreateUser(ctx, db, "st@example.com", "pw", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := auth.UpdateUserStatus(ctx, db, created.ID, "suspended"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	u, _, _ := auth.GetUser(ctx, db, created.ID)
	if u.Status != "suspended" {
		t.Errorf("status = %q, want suspended", u.Status)
	}

	// Missing id.
	if err := auth.UpdateUserStatus(ctx, db, "11111111-1111-1111-1111-111111111111", "active"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("missing id err = %v, want ErrUserNotFound", err)
	}
	// Invalid status value — rejected, and the row is unchanged.
	if err := auth.UpdateUserStatus(ctx, db, created.ID, "banned"); !errors.Is(err, auth.ErrInvalidStatus) {
		t.Errorf("invalid status err = %v, want ErrInvalidStatus", err)
	}
	u, _, _ = auth.GetUser(ctx, db, created.ID)
	if u.Status != "suspended" {
		t.Errorf("status changed despite invalid value: %q", u.Status)
	}
}

// TestSetUserRoles covers the role-replacement function: it replaces the whole
// set (add + remove), rejects an unknown role with ErrUnknownRole leaving the
// existing roles intact, rejects a missing user with ErrUserNotFound, and allows
// clearing to zero roles.
func TestSetUserRoles(t *testing.T) {
	db, closeDB := openDB(t)
	defer closeDB()
	ctx := context.Background()

	created, err := auth.CreateUser(ctx, db, "rr@example.com", "pw", []string{"author"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Replace [author] → [editor, administrator].
	if err := auth.SetUserRoles(ctx, db, created.ID, []string{"editor", "administrator"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	u, _, _ := auth.GetUser(ctx, db, created.ID)
	if !sameSet(u.Roles, []string{"editor", "administrator"}) {
		t.Errorf("roles = %v, want {editor, administrator}", u.Roles)
	}

	// Unknown role (red-team: a name not in the fixed catalog) → rejected, roles
	// unchanged.
	err = auth.SetUserRoles(ctx, db, created.ID, []string{"editor", "superadmin"})
	if !errors.Is(err, auth.ErrUnknownRole) {
		t.Errorf("unknown role err = %v, want ErrUnknownRole", err)
	}
	u, _, _ = auth.GetUser(ctx, db, created.ID)
	if !sameSet(u.Roles, []string{"editor", "administrator"}) {
		t.Errorf("roles changed despite unknown-role rejection: %v", u.Roles)
	}

	// Missing user.
	if err := auth.SetUserRoles(ctx, db, "11111111-1111-1111-1111-111111111111", []string{"editor"}); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("missing user err = %v, want ErrUserNotFound", err)
	}

	// Clear all roles.
	if err := auth.SetUserRoles(ctx, db, created.ID, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	u, _, _ = auth.GetUser(ctx, db, created.ID)
	if len(u.Roles) != 0 {
		t.Errorf("roles after clear = %v, want empty", u.Roles)
	}
}

// userID looks up a user id by email — needed by the status-fix test, which
// starts from an email rather than the created id.
func userID(t *testing.T, db *sql.DB, email string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM users WHERE email = ?`, email).Scan(&id); err != nil {
		t.Fatalf("lookup user %q: %v", email, err)
	}
	return id
}

// sameSet reports whether a and b contain the same elements, ignoring order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, x := range a {
		m[x]++
	}
	for _, y := range b {
		m[y]--
		if m[y] < 0 {
			return false
		}
	}
	return true
}
