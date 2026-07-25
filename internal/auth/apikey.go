package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
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
//
// CONTRACT-19: a single INSERT, so it goes through CallRoutine — the transaction
// the routine opens IS the whole operation. created_at is supplied by the
// application in the canonical RFC3339Nano form instead of being left to the
// column DEFAULT, because the two engines render CURRENT_TIMESTAMP as different
// TEXT and that column is the primary sort key of ListAPIKeys; see the routine
// declaration in internal/schema/auth_dual.go. The id keeps its DEFAULT
// gen_random_uuid(), which compat compiles for both engines, because nothing
// here needs to know it — but compat's insert action requires every column of
// the row, so it is generated in Go like the user id (the column DEFAULT stays
// as a safety net for writes that do not come from the app).
func MintAPIKey(ctx context.Context, store *compat.Store, label, roleID string) (string, error) {
	secret, err := generateSecret()
	if err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	keyHash := hashSecret(secret)
	id, err := dual.NewUUID()
	if err != nil {
		return "", fmt.Errorf("generate api key id: %w", err)
	}

	if err := store.CallRoutine(ctx, authSchema, schema.RoutineInsertAPIKey, map[string]compat.Value{
		"id":         dual.UUIDValue(id),
		"label":      dual.TextValue(label),
		"key_hash":   dual.TextValue(keyHash),
		"role_id":    dual.UUIDValue(roleID),
		"created_at": timestampValue(now()),
	}); err != nil {
		return "", fmt.Errorf("insert api key: %w", err)
	}
	return secret, nil
}

// RevokeAPIKey sets revoked_at on the row matching the given plaintext secret,
// marking it as rejected on all subsequent verification. The row is looked up
// by its hash, not by the plaintext secret.
//
// Revocation is idempotent: an already-revoked or unknown key matches no row
// (the routine's WHERE carries `revoked_at IS NULL`, which behaves identically
// on both engines) and the call is a no-op success. The previous implementation
// inspected RowsAffected only to do nothing with it; CallRoutine does not expose
// it, and nothing observable changes.
func RevokeAPIKey(ctx context.Context, store *compat.Store, secret string) error {
	if err := store.CallRoutine(ctx, authSchema, schema.RoutineRevokeAPIKeyByHash, map[string]compat.Value{
		"key_hash":   dual.TextValue(hashSecret(secret)),
		"revoked_at": timestampValue(now()),
	}); err != nil {
		return fmt.Errorf("revoke api key: %w", err)
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
func RevokeAPIKeyByID(ctx context.Context, store *compat.Store, id string) error {
	if err := store.CallRoutine(ctx, authSchema, schema.RoutineRevokeAPIKeyByID, map[string]compat.Value{
		"id":         dual.UUIDValue(id),
		"revoked_at": timestampValue(now()),
	}); err != nil {
		return fmt.Errorf("revoke api key by id: %w", err)
	}
	return nil
}

// APIKeyRecord is the admin-facing view of an api_keys row for the CONTRACT-09
// UI: id, label, the NAME of the bound role (resolved via the api_key_records
// view, not the raw role_id), the creation timestamp, and the revocation state.
// It deliberately carries NO key_hash and NO secret — neither is ever surfaced
// to the UI, and the view does not even project the hash.
type APIKeyRecord struct {
	ID        string
	Label     string
	RoleName  string
	CreatedAt string
	Revoked   bool
	RevokedAt string
}

// ListAPIKeys returns every api_keys row as an APIKeyRecord, newest first, with
// the bound role's NAME resolved through the api_key_records view (not an N+1
// per-row lookup). The view never projects key_hash, so the secret's hash cannot
// leak into the UI even by accident.
//
// The ordering (created_at DESC, label ASC) is declared in the routine as a
// stable base, and CONTRACT-20B imposes the FINAL order here, in Go, by BYTE
// comparison per key.
//
// The declared ORDER BY alone was not enough. `label` is FREE ADMIN TEXT and it
// is the tie-break key, so it is exactly the sort of value where punctuation and
// case decide the sequence — and PostgreSQL compares TEXT by the database
// COLLATION while SQLite compares it by BYTES. Two keys labelled `informes.web`
// and `informes_anuales`, created in the same instant, come back in opposite
// order on the two engines. The timestamps themselves come back canonicalized to
// RFC3339Nano by the `timestamp` family declared on the read action, identically
// on both engines, so comparing them here as text is comparing the SAME strings.
// This listing is not paginated, so ordering it after the read chooses the whole
// set rather than reshuffling a page.
func ListAPIKeys(ctx context.Context, store *compat.Store) ([]APIKeyRecord, error) {
	rows, err := store.QueryRoutine(ctx, authSchema, schema.RoutineListAPIKeys, nil)
	if err != nil {
		return nil, fmt.Errorf("query api keys: %w", err)
	}
	out := make([]APIKeyRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, apiKeyRecord(row))
	}
	// The id is a THIRD key the routine does not declare, so the order is TOTAL:
	// label is not unique, and two identically labelled keys created in the same
	// instant left each engine free to pick its own natural order. Same move
	// CONTRACT-20 made for its three partial orders. The id is a UUID, whose
	// hyphens sit at identical offsets in every value, so a byte comparison and a
	// collation comparison provably agree on it.
	//
	// CONTRACT-20C: created_at is compared as an INSTANT (dual.DescInstant), not
	// as the text that carries it. Comparing the text put a LATER key first
	// whenever its nanosecond field ended in zero or was absent, because compat
	// hands the value back rendered with time.RFC3339Nano, which trims trailing
	// zeros — same wrong answer on both engines. A created_at that does not parse
	// is returned as an ERROR rather than silently falling back to a text
	// comparison: the column is NOT NULL and compat refuses to canonicalize an
	// unparseable timestamp on read, so reaching this is a corrupted row, and a
	// 500 naming the key is the honest answer.
	if err := dual.SortByKeysE(out, func(k APIKeyRecord) ([]dual.Key, error) {
		created, err := dual.DescInstant(k.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("api key %s created_at: %w", k.ID, err)
		}
		return []dual.Key{created, dual.Asc(k.Label), dual.Asc(k.ID)}, nil
	}); err != nil {
		return nil, fmt.Errorf("order api keys: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// GetAPIKey loads a single api_keys row as an APIKeyRecord by id, from the same
// view and with the same never-select-key_hash guarantee as ListAPIKeys. found
// is false when no row matches (a missing or malformed id) — never a raw SQL
// error — so the handler maps it to a 404 after an idempotent revoke rather than
// a 500. It is used to re-render the single row fragment after a revoke.
func GetAPIKey(ctx context.Context, store *compat.Store, id string) (APIKeyRecord, bool, error) {
	rows, err := store.QueryRoutine(ctx, authSchema, schema.RoutineAPIKeyByID, map[string]compat.Value{
		"id": dual.UUIDValue(id),
	})
	if err != nil {
		return APIKeyRecord{}, false, fmt.Errorf("query api key: %w", err)
	}
	if len(rows) == 0 {
		return APIKeyRecord{}, false, nil
	}
	return apiKeyRecord(rows[0]), true, nil
}

// apiKeyRecord maps one api_key_records row to the UI record, translating the
// nullable revoked_at timestamp into the Revoked flag + RevokedAt string. NULL
// is tested by the canonical value KIND, not by an empty string.
func apiKeyRecord(row compat.Row) APIKeyRecord {
	rec := APIKeyRecord{
		ID:        dual.RowText(row, "id"),
		Label:     dual.RowText(row, "label"),
		RoleName:  dual.RowText(row, "role_name"),
		CreatedAt: dual.RowText(row, "created_at"),
	}
	if !dual.RowIsNull(row, "revoked_at") {
		rec.Revoked = true
		rec.RevokedAt = dual.RowText(row, "revoked_at")
	}
	return rec
}

// VerifyAPIKey looks up the plaintext secret by its SHA-256 hash and returns
// the key's identity if the row exists and is not revoked. Verification is a
// single exact-match lookup (WHERE key_hash = <bound parameter>); the plaintext
// secret is never compared in Go, so there is no timing side-channel on the
// secret in application code — the equality check happens inside the DB engine,
// against the stored hash, not against the secret.
func VerifyAPIKey(ctx context.Context, store *compat.Store, secret string) (*APIKey, error) {
	if secret == "" {
		return nil, ErrAPIKeyRejected
	}
	rows, err := store.QueryRoutine(ctx, authSchema, schema.RoutineAPIKeyByHash, map[string]compat.Value{
		"key_hash": dual.TextValue(hashSecret(secret)),
	})
	if err != nil {
		return nil, fmt.Errorf("query api key: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrAPIKeyRejected
	}
	// revoked_at is a nullable timestamp; a non-NULL value means revoked. The
	// test is on the canonical NULL kind, which is the same answer on both
	// engines regardless of how each driver surfaces a null column.
	if !dual.RowIsNull(rows[0], "revoked_at") {
		return nil, ErrAPIKeyRejected
	}
	return &APIKey{
		ID:     dual.RowText(rows[0], "id"),
		Label:  dual.RowText(rows[0], "label"),
		RoleID: dual.RowText(rows[0], "role_id"),
	}, nil
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
