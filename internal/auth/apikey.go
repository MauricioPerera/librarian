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

// RevokeAPIKeyByID sets revoked_at on the row matching the given id (the list
// row's identifier, NOT the plaintext secret), marking it rejected on all
// subsequent verification. This is the revocation path the admin UI needs:
// unlike RevokeAPIKey, it does not require the plaintext secret, which the UI
// never has (MintAPIKey returns it once at creation and it is never stored).
// Revocation is idempotent — revoking an already-revoked or unknown id affects
// zero rows and is a no-op success, so a double click or a repeated call never
// errors. The id is bound as a parameter, never interpolated.
func RevokeAPIKeyByID(ctx context.Context, db *sql.DB, id string) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = CURRENT_TIMESTAMP WHERE id = ? AND revoked_at IS NULL`,
		id,
	); err != nil {
		return fmt.Errorf("revoke api key by id: %w", err)
	}
	return nil
}

// APIKeyRecord is the admin-facing view of an api_keys row for the CONTRACT-09
// UI: id, label, the NAME of the bound role (resolved via a JOIN, not the raw
// role_id), the creation timestamp, and the revocation state. It deliberately
// carries NO key_hash and NO secret — neither is ever surfaced to the UI.
type APIKeyRecord struct {
	ID        string
	Label     string
	RoleName  string
	CreatedAt string
	Revoked   bool
	RevokedAt string
}

// ListAPIKeys returns every api_keys row as an APIKeyRecord, newest first, with
// the bound role's NAME resolved through a single JOIN to roles (not an N+1
// per-row lookup). It never selects key_hash, so the secret's hash cannot leak
// into the UI even by accident. revoked_at is scanned as a nullable string (the
// SQLite driver returns CURRENT_TIMESTAMP as text); a non-empty value marks the
// key revoked and is surfaced as Revoked + RevokedAt.
func ListAPIKeys(ctx context.Context, db *sql.DB) ([]APIKeyRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT k.id, k.label, r.name, k.created_at, k.revoked_at
		   FROM api_keys k
		   JOIN roles r ON r.id = k.role_id
		  ORDER BY k.created_at DESC, k.label`,
	)
	if err != nil {
		return nil, fmt.Errorf("query api keys: %w", err)
	}
	defer rows.Close()
	var out []APIKeyRecord
	for rows.Next() {
		rec, err := scanAPIKeyRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate api keys: %w", err)
	}
	return out, nil
}

// GetAPIKey loads a single api_keys row as an APIKeyRecord by id, with the same
// role-name JOIN and the same never-select-key_hash guarantee as ListAPIKeys.
// found is false when no row matches (a missing or malformed id) — never a raw
// SQL error — so the handler maps it to a 404 after an idempotent revoke rather
// than a 500. It is used to re-render the single row fragment after a revoke.
func GetAPIKey(ctx context.Context, db *sql.DB, id string) (APIKeyRecord, bool, error) {
	row := db.QueryRowContext(ctx,
		`SELECT k.id, k.label, r.name, k.created_at, k.revoked_at
		   FROM api_keys k
		   JOIN roles r ON r.id = k.role_id
		  WHERE k.id = ?`,
		id,
	)
	rec, err := scanAPIKeyRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKeyRecord{}, false, nil
	}
	if err != nil {
		return APIKeyRecord{}, false, err
	}
	return rec, true, nil
}

// scanRow is the minimal surface shared by *sql.Row and *sql.Rows so the record
// scan logic is written once for both the list and single-row paths.
type scanRow interface {
	Scan(dest ...any) error
}

// scanAPIKeyRecord scans one api_keys row (already joined to roles) into an
// APIKeyRecord, translating the nullable revoked_at timestamp into the Revoked
// flag + RevokedAt string.
func scanAPIKeyRecord(row scanRow) (APIKeyRecord, error) {
	var (
		rec       APIKeyRecord
		revokedAt sql.NullString
	)
	if err := row.Scan(&rec.ID, &rec.Label, &rec.RoleName, &rec.CreatedAt, &revokedAt); err != nil {
		return APIKeyRecord{}, err
	}
	if revokedAt.Valid && revokedAt.String != "" {
		rec.Revoked = true
		rec.RevokedAt = revokedAt.String
	}
	return rec, nil
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
