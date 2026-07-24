package auth_test

// CONTRACT-09 T1 unit tests for the new API-key data functions:
// ListAPIKeys (with the JOIN that resolves the role NAME, not the raw role_id),
// RevokeAPIKeyByID (revoke by row id, idempotent), and GetAPIKey (single-row
// fetch used to re-render a row after revoke). These are independent of the UI.
// They reuse openDB and roleID from auth_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/MauricioPerera/librarian/internal/auth"
)

// apiKeyIDByLabel finds a listed key's id by its label (labels are unique in
// these tests even though the schema does not enforce it).
func apiKeyIDByLabel(t *testing.T, keys []auth.APIKeyRecord, label string) string {
	t.Helper()
	for _, k := range keys {
		if k.Label == label {
			return k.ID
		}
	}
	t.Fatalf("no listed key with label %q", label)
	return ""
}

// TestListAPIKeysResolvesRoleName covers the T1 JOIN criterion: ListAPIKeys
// returns the role NAME (via JOIN roles), not the role_id, and never exposes a
// key_hash (the struct carries none). Newest-first ordering and the active
// (non-revoked) state are also asserted.
func TestListAPIKeysResolvesRoleName(t *testing.T) {
	db, close := openDB(t)
	defer close()
	ctx := context.Background()

	if _, err := auth.MintAPIKey(ctx, db, "ci-runner", roleID(t, db, "editor")); err != nil {
		t.Fatalf("mint editor key: %v", err)
	}
	if _, err := auth.MintAPIKey(ctx, db, "deploy-bot", roleID(t, db, "administrator")); err != nil {
		t.Fatalf("mint admin key: %v", err)
	}

	keys, err := auth.ListAPIKeys(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(keys))
	}

	byLabel := map[string]auth.APIKeyRecord{}
	for _, k := range keys {
		byLabel[k.Label] = k
	}
	ci, ok := byLabel["ci-runner"]
	if !ok {
		t.Fatal("ci-runner not listed")
	}
	// The bound role NAME must be resolved, not the id.
	if ci.RoleName != "editor" {
		t.Errorf("ci-runner RoleName = %q, want editor (JOIN not resolving name?)", ci.RoleName)
	}
	if ci.RoleName == roleID(t, db, "editor") {
		t.Errorf("RoleName is the raw role_id, not the name")
	}
	if byLabel["deploy-bot"].RoleName != "administrator" {
		t.Errorf("deploy-bot RoleName = %q, want administrator", byLabel["deploy-bot"].RoleName)
	}
	// Freshly minted → active, with a creation timestamp.
	if ci.Revoked {
		t.Error("freshly minted key reported revoked")
	}
	if ci.CreatedAt == "" {
		t.Error("empty CreatedAt")
	}
	if ci.ID == "" {
		t.Error("empty ID")
	}
}

// TestRevokeAPIKeyByID covers the T1 revoke-by-id criterion: revoking by the row
// id (not the plaintext secret) makes the key fail verification, is reflected in
// the listing as revoked, and is idempotent (re-revoke and unknown-id are no-op
// successes).
func TestRevokeAPIKeyByID(t *testing.T) {
	db, close := openDB(t)
	defer close()
	ctx := context.Background()

	secret, err := auth.MintAPIKey(ctx, db, "revoke-me", roleID(t, db, "administrator"))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Sanity: verifies before revocation.
	if _, err := auth.VerifyAPIKey(ctx, db, secret); err != nil {
		t.Fatalf("verify pre-revoke: %v", err)
	}

	keys, err := auth.ListAPIKeys(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	id := apiKeyIDByLabel(t, keys, "revoke-me")

	// Revoke by id (the UI's only available handle — it never has the secret).
	if err := auth.RevokeAPIKeyByID(ctx, db, id); err != nil {
		t.Fatalf("revoke by id: %v", err)
	}
	// Now the plaintext secret is rejected.
	if _, err := auth.VerifyAPIKey(ctx, db, secret); !errors.Is(err, auth.ErrAPIKeyRejected) {
		t.Fatalf("post-revoke verify err = %v, want ErrAPIKeyRejected", err)
	}

	// The listing reflects the revocation (row stays, marked revoked).
	keys, _ = auth.ListAPIKeys(ctx, db)
	rec, found := auth.APIKeyRecord{}, false
	for _, k := range keys {
		if k.ID == id {
			rec, found = k, true
		}
	}
	if !found {
		t.Fatal("revoked key disappeared from listing — must remain as history")
	}
	if !rec.Revoked {
		t.Error("listing does not mark the key revoked")
	}
	if rec.RevokedAt == "" {
		t.Error("RevokedAt empty for a revoked key")
	}

	// Idempotent: revoking again is a no-op success.
	if err := auth.RevokeAPIKeyByID(ctx, db, id); err != nil {
		t.Fatalf("re-revoke by id: %v", err)
	}
	// Unknown id is also a no-op success (never an error).
	if err := auth.RevokeAPIKeyByID(ctx, db, "11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatalf("revoke unknown id: %v", err)
	}
}

// TestGetAPIKey covers GetAPIKey: it returns the single record with the role
// name resolved and the revoked state, and reports found=false for a missing id
// (mapped to a 404 by the handler, never a 500).
func TestGetAPIKey(t *testing.T) {
	db, close := openDB(t)
	defer close()
	ctx := context.Background()

	if _, err := auth.MintAPIKey(ctx, db, "lookup-me", roleID(t, db, "author")); err != nil {
		t.Fatalf("mint: %v", err)
	}
	keys, _ := auth.ListAPIKeys(ctx, db)
	id := apiKeyIDByLabel(t, keys, "lookup-me")

	rec, found, err := auth.GetAPIKey(ctx, db, id)
	if err != nil || !found {
		t.Fatalf("GetAPIKey found=%v err=%v", found, err)
	}
	if rec.Label != "lookup-me" || rec.RoleName != "author" {
		t.Errorf("rec = %+v, want label lookup-me / role author", rec)
	}
	if rec.Revoked {
		t.Error("fresh key reported revoked")
	}

	// After revoke, GetAPIKey reflects it.
	if err := auth.RevokeAPIKeyByID(ctx, db, id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rec, _, _ = auth.GetAPIKey(ctx, db, id)
	if !rec.Revoked || rec.RevokedAt == "" {
		t.Errorf("post-revoke GetAPIKey = %+v, want revoked", rec)
	}

	// Missing id → found=false, no error.
	_, found, err = auth.GetAPIKey(ctx, db, "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("GetAPIKey missing err = %v, want nil", err)
	}
	if found {
		t.Error("GetAPIKey reported found for a missing id")
	}
}
