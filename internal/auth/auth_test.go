package auth_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/store"
)

// openDB opens a temp SQLite file, applies the schema, and seeds the role
// catalog so user creation / API-key minting can reference roles. Returns the
// *sql.DB handle (compat.Store.DB) and a close func.
func openDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "auth.db")
	sdb, err := store.Open(dbPath)
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
	return sdb.DB, func() { _ = sdb.Close() }
}

// roleID looks up a role id by name — needed to mint API keys against a role.
func roleID(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM roles WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatalf("lookup role %q: %v", name, err)
	}
	return id
}

// --- T2: user creation + credential verification -----------------------------

// TestCreateUserAndVerify covers the T2 acceptance criterion: a user created
// with a password verifies with the correct password and returns its roles,
// and rejects the wrong password with the same error as a non-existent user.
func TestCreateUserAndVerify(t *testing.T) {
	db, close := openDB(t)
	defer close()
	ctx := context.Background()

	user, err := auth.CreateUser(ctx, db, "alice@example.com", "correct-horse-battery-staple", []string{"editor", "author"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.Email != "alice@example.com" {
		t.Fatalf("email = %q", user.Email)
	}
	if len(user.Roles) != 2 {
		t.Fatalf("roles = %v, want 2", user.Roles)
	}

	// Correct password → user with roles.
	got, err := auth.VerifyCredentials(ctx, db, "alice@example.com", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("verify correct password: %v", err)
	}
	if got.ID != user.ID {
		t.Fatalf("id = %q, want %q", got.ID, user.ID)
	}
	if len(got.Roles) != 2 {
		t.Fatalf("roles = %v, want 2", got.Roles)
	}

	// Wrong password → ErrInvalidCredentials (generic).
	if _, err := auth.VerifyCredentials(ctx, db, "alice@example.com", "WRONG"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong password err = %v, want ErrInvalidCredentials", err)
	}

	// Non-existent user → the SAME error (anti-enumeration).
	if _, err := auth.VerifyCredentials(ctx, db, "ghost@example.com", "WRONG"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown user err = %v, want ErrInvalidCredentials", err)
	}
}

// TestVerifyCredentialsIdenticalMessage is the explicit red-team check: the
// error text for "user does not exist" and "wrong password" must be identical
// so a caller cannot enumerate valid emails.
func TestVerifyCredentialsIdenticalMessage(t *testing.T) {
	db, close := openDB(t)
	defer close()
	ctx := context.Background()

	if _, err := auth.CreateUser(ctx, db, "bob@example.com", "s3cret", []string{"author"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, errWrong := auth.VerifyCredentials(ctx, db, "bob@example.com", "nope")
	_, errMissing := auth.VerifyCredentials(ctx, db, "nobody@example.com", "nope")
	if errWrong == nil || errMissing == nil {
		t.Fatal("expected errors for both")
	}
	if errWrong.Error() != errMissing.Error() {
		t.Fatalf("error messages differ — enumeration leak:\n wrong=%q\n missing=%q", errWrong.Error(), errMissing.Error())
	}
}

// --- T3: JWT issue + verify --------------------------------------------------

func TestIssueAndVerifyJWT(t *testing.T) {
	const secret = "super-secret-test-key"
	user := &auth.User{ID: "user-123", Email: "alice@example.com", Roles: []string{"editor", "author"}}

	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	token, err := auth.IssueJWT(secret, user, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}

	claims, err := auth.VerifyJWT(secret, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != user.ID {
		t.Errorf("sub = %q, want %q", claims.Subject, user.ID)
	}
	if claims.Email != user.Email {
		t.Errorf("email = %q, want %q", claims.Email, user.Email)
	}
	if len(claims.Roles) != 2 || claims.Roles[0] != "editor" || claims.Roles[1] != "author" {
		t.Errorf("roles = %v, want %v", claims.Roles, user.Roles)
	}
	// Expiry is 24h after issue.
	wantExp := now.Add(24 * time.Hour)
	if !claims.ExpiresAt.Time.Equal(wantExp) {
		t.Errorf("exp = %v, want %v", claims.ExpiresAt.Time, wantExp)
	}

	// Wrong secret → ErrInvalidToken.
	if _, err := auth.VerifyJWT("different-secret", token); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("wrong-secret err = %v, want ErrInvalidToken", err)
	}

	// Expired token → ErrInvalidToken.
	expired, err := auth.IssueJWT(secret, user, now.Add(-25*time.Hour))
	if err != nil {
		t.Fatalf("issue expired: %v", err)
	}
	if _, err := auth.VerifyJWT(secret, expired); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expired err = %v, want ErrInvalidToken", err)
	}
}

// TestIssueJWTRejectsEmptySecret guards the fail-closed invariant in the
// signing path itself.
func TestIssueJWTRejectsEmptySecret(t *testing.T) {
	_, err := auth.IssueJWT("", &auth.User{ID: "x", Email: "x@x"}, time.Now())
	if err == nil {
		t.Fatal("expected error issuing JWT with empty secret")
	}
}

// --- T4: API key mint + verify + revoke -------------------------------------

func TestMintAndVerifyAPIKey(t *testing.T) {
	db, close := openDB(t)
	defer close()
	ctx := context.Background()

	rid := roleID(t, db, "editor")
	secret, err := auth.MintAPIKey(ctx, db, "ci-runner", rid)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if secret == "" {
		t.Fatal("empty secret returned")
	}

	// The plaintext secret verifies against the DB (looked up by hash).
	key, err := auth.VerifyAPIKey(ctx, db, secret)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if key.Label != "ci-runner" {
		t.Errorf("label = %q, want ci-runner", key.Label)
	}
	if key.RoleID != rid {
		t.Errorf("role_id = %q, want %q", key.RoleID, rid)
	}

	// A bogus secret is rejected.
	if _, err := auth.VerifyAPIKey(ctx, db, "lbk_notarealkey"); !errors.Is(err, auth.ErrAPIKeyRejected) {
		t.Fatalf("bogus err = %v, want ErrAPIKeyRejected", err)
	}
	// Empty secret is rejected without a DB hit.
	if _, err := auth.VerifyAPIKey(ctx, db, ""); !errors.Is(err, auth.ErrAPIKeyRejected) {
		t.Fatalf("empty err = %v, want ErrAPIKeyRejected", err)
	}
}

// TestRevokedAPIKeyRejected covers the T4 acceptance criterion: a key with
// revoked_at set is rejected on verification.
func TestRevokedAPIKeyRejected(t *testing.T) {
	db, close := openDB(t)
	defer close()
	ctx := context.Background()

	rid := roleID(t, db, "administrator")
	secret, err := auth.MintAPIKey(ctx, db, "to-be-revoked", rid)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Sanity: verifies before revocation.
	if _, err := auth.VerifyAPIKey(ctx, db, secret); err != nil {
		t.Fatalf("verify pre-revoke: %v", err)
	}
	if err := auth.RevokeAPIKey(ctx, db, secret); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// After revocation: rejected.
	if _, err := auth.VerifyAPIKey(ctx, db, secret); !errors.Is(err, auth.ErrAPIKeyRejected) {
		t.Fatalf("revoked err = %v, want ErrAPIKeyRejected", err)
	}
	// Revoking again is idempotent (no error).
	if err := auth.RevokeAPIKey(ctx, db, secret); err != nil {
		t.Fatalf("re-revoke: %v", err)
	}
}
