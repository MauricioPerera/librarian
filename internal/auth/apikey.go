package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// keyPrefix tags librarian-issued API key secrets so they are visually
// distinguishable from JWTs and other bearer values.
const keyPrefix = "lbk_"

// ErrAPIKeyRejected is the generic error for any API key verification failure
// (unknown key, revoked key). It carries no detail so it cannot leak which.
var ErrAPIKeyRejected = errors.New("api key rejected")

// APIKey is the application-level view of a verified api_keys row.
type APIKey struct {
	ID     string
	Label  string
	RoleID string
}

// MintAPIKey generates a new high-entropy API key secret with crypto/rand (not
// math/rand), persists only its SHA-256 hash, and returns the plaintext secret
// to the caller. The plaintext is recoverable ONLY here, at creation; it is
// never stored and never logged. bcrypt is deliberately not used: the secret
// already has high entropy, so a slow hash buys nothing and a plain SHA-256
// lookup is sufficient.
func MintAPIKey(ctx context.Context, db *sql.DB, label, roleID string) (string, error) {
	secret, err := generateSecret()
	if err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	keyHash := hashSecret(secret)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO api_keys (label, key_hash, role_id) VALUES (?, ?, ?)`,
		label, keyHash, roleID,
	); err != nil {
		return "", fmt.Errorf("insert api key: %w", err)
	}
	return secret, nil
}

// RevokeAPIKey sets revoked_at on the row matching the given plaintext secret,
// marking it as rejected on all subsequent verification. The row is looked up
// by its hash, not by the plaintext secret.
func RevokeAPIKey(ctx context.Context, db *sql.DB, secret string) error {
	keyHash := hashSecret(secret)
	res, err := db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = CURRENT_TIMESTAMP WHERE key_hash = ? AND revoked_at IS NULL`,
		keyHash,
	)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already revoked or unknown — either way not an active key. Revocation
		// is idempotent, so this is a no-op success.
	}
	return nil
}

// VerifyAPIKey looks up the plaintext secret by its SHA-256 hash and returns
// the key's identity if the row exists and is not revoked. Verification is a
// single exact-match SQL lookup (WHERE key_hash = ?); the plaintext secret is
// never compared in Go, so there is no timing side-channel on the secret in
// application code — the equality check happens inside the DB engine, against
// the stored hash, not against the secret.
func VerifyAPIKey(ctx context.Context, db *sql.DB, secret string) (*APIKey, error) {
	if secret == "" {
		return nil, ErrAPIKeyRejected
	}
	keyHash := hashSecret(secret)
	var (
		id        string
		label     string
		roleID    string
		revokedAt sql.NullString
	)
	err := db.QueryRowContext(ctx,
		`SELECT id, label, role_id, revoked_at FROM api_keys WHERE key_hash = ?`,
		keyHash,
	).Scan(&id, &label, &roleID, &revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrAPIKeyRejected
	case err != nil:
		return nil, fmt.Errorf("query api key: %w", err)
	}
	// revoked_at is a nullable timestamp. On SQLite the driver returns it as a
	// string (CURRENT_TIMESTAMP), so we scan into NullString rather than
	// NullTime. A non-null value (any non-empty timestamp) means revoked.
	if revokedAt.Valid && revokedAt.String != "" {
		return nil, ErrAPIKeyRejected
	}
	return &APIKey{ID: id, Label: label, RoleID: roleID}, nil
}

// generateSecret produces 32 crypto-random bytes and base64url-encodes them
// (unpadded), prefixed with keyPrefix — 256 bits of entropy, URL-safe.
func generateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return keyPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashSecret returns the lowercase hex SHA-256 digest of the secret. hex is
// chosen over base64 for a plain-text, case-insensitive equality column.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
