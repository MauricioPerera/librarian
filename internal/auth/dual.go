package auth

// CONTRACT-19 — the engine-neutral plumbing every function in this package uses.
//
// Before this contract every statement in `auth` was hand-written SQL with `?`
// placeholders, which is SQLite syntax; PostgreSQL's driver requires `$n`. The
// package now reaches the database through exactly three doors, and NO other:
//
//  1. compat.Store.QueryRoutine — parameterized reads declared in the canonical
//     schema (internal/schema/auth_dual.go). Rows come back canonicalized by
//     column family, so the scan is identical on both engines.
//  2. compat.Store.CallRoutine — single writes declared in the same schema.
//  3. Raw SQL composed with compat.Placeholder, for the three operations that
//     replace a set of VARIABLE size (a DELETE plus N INSERTs that must commit
//     together) inside a transaction this package owns. Neither routine door can
//     express that: a routine has a STATIC action list, and both QueryRoutine and
//     CallRoutine open their OWN transaction, so they can neither be nested in a
//     caller's transaction nor composed atomically with each other.
//
// Which door each operation uses is decided in CONTRACT-19 and is not a local
// judgement call.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"time"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// authSchema is the canonical schema the routines are declared in. It is built
// once: schema.Build() is a pure function of the code, and QueryRoutine/
// CallRoutine only need the declaration of the routine plus the relation it
// reads. The dynamic content-type tables (which is the only thing
// store.CanonicalSchema adds) are irrelevant to every auth routine, so this
// package never needs a database round-trip to know its own schema.
var authSchema = schema.Build()

// now returns the current instant in the canonical timestamp form of the
// `timestamp` family: RFC3339Nano, UTC, stored as TEXT on BOTH engines.
//
// This is the resolution CONTRACT-19 mandates for the CURRENT_TIMESTAMP that
// used to be written inside UPDATE statements: a routine assignment resolves to
// a parameter or a literal, never to an engine function. It is not a new format
// — it is the one compat declares for the family, and the one canonicalValue
// normalizes every timestamp to when it is read back.
func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// newUUID returns a random RFC 4122 v4 UUID in the canonical 8-4-4-4-12 text
// form, using crypto/rand (the same source the API-key secret uses).
//
// CONTRACT-19 resolves `INSERT ... RETURNING id` this way: the id column keeps
// its DEFAULT gen_random_uuid() as a safety net for writes that do not come from
// the application, but the application generates the identifier itself and binds
// it as one more value. That removes the only RETURNING in the package (compat
// deliberately does not implement RETURNING, because a consumer can resolve it
// on its own) and, better, makes the identifier known BEFORE the write instead
// of after it.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// value constructors ------------------------------------------------------------
//
// A routine argument is a compat.Value: an explicit canonical kind plus its text.
// Declaring the kind at the call site is what lets compat bind the value the way
// the destination engine expects (see driverValue) instead of guessing from the
// Go type.

func textValue(v string) compat.Value {
	return compat.Value{Kind: compat.TextValue, Value: v}
}

func uuidValue(v string) compat.Value {
	return compat.Value{Kind: compat.UUIDValue, Value: v}
}

func timestampValue(v string) compat.Value {
	return compat.Value{Kind: compat.TimestampValue, Value: v}
}

// row accessors -----------------------------------------------------------------

// rowText returns the canonical text of a column of a routine result row. A NULL
// (or an absent column, which Schema.Validate makes impossible) reads as "".
func rowText(row compat.Row, column string) string {
	return row[column].Value
}

// rowIsNull reports whether a column of a routine result row is SQL NULL. It
// tests the canonical KIND, not the emptiness of the text, so a genuinely empty
// string is not mistaken for a NULL.
func rowIsNull(row compat.Row, column string) bool {
	return row[column].Kind == compat.NullValue
}

// raw-SQL helpers ---------------------------------------------------------------

// txQuerier is the subset of *sql.Tx the raw-SQL helpers need. It exists so the
// name-resolution helpers can be read (and reviewed) without caring whether they
// run on a transaction or a pool — they always run on the caller's transaction.
type txQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// bind composes the engine's bind marker for the 1-based argument position. It
// is a thin, local name for compat.Placeholder so a raw statement reads as SQL
// with holes rather than as string concatenation, and so there is exactly ONE
// place in this package that knows about `?` vs `$n` — compat's.
func bind(engine compat.Engine, position int) string {
	return compat.Placeholder(engine, position)
}

// dedupe returns ids without repetitions, preserving first-appearance order.
//
// It replaces the `ON CONFLICT DO NOTHING` the three set-replacement INSERTs
// used to carry. CONTRACT-19 asks to verify rather than assume that the conflict
// clause is needed: after the DELETE that precedes them the target set is empty,
// so the ONLY way to hit the composite primary key is a name repeated in the
// CALLER's list (documented as harmless in CONTRACT-16). Removing the duplicates
// in Go makes the conflict impossible by construction, which keeps the exact
// same observable behavior without depending on an upsert clause whose accepted
// spelling differs between engine versions.
func dedupe(ids []string) []string {
	if len(ids) < 2 {
		return ids
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
