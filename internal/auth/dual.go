package auth

// CONTRACT-19 — the engine-neutral plumbing every function in this package uses.
//
// Before that contract every statement in `auth` was hand-written SQL with `?`
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
//
// CONTRACT-20B moved everything this file shared verbatim with
// internal/server/dual.go — bind, newUUID, rowText, rowIsNull, textValue,
// uuidValue, dedupe and the txQuerier interface — to internal/dual, the single
// copy every database-touching package now uses. What is left here is what is
// genuinely specific to `auth`: its canonical schema handle, its timestamp form,
// and the one value constructor no other package needs.

import (
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
//
// CONTRACT-20B — WHY THIS IS *NOT* internal/server's FIXED-WIDTH LAYOUT, which is
// the obvious thing to copy and would be a mistake here. internal/server writes a
// fixed-width nanosecond field because its three PAGINATED listings sort
// created_at INSIDE the engine, where the STORED text is what gets compared and a
// variable width breaks the byte-vs-collation agreement. Nothing in `auth` is
// paginated: after this contract every auth listing is ordered in Go, over the
// value compat hands back — and compat re-renders every timestamp it reads with
// time.RFC3339Nano, which TRIMS trailing zeros. A fixed-width value written here
// would therefore be trimmed again on the way out and change nothing about the
// order, while changing the TEXT stored in api_keys.created_at and with it the
// sequence SQLite produces across rows written before and after the change. This
// contract does not authorize an observable order change in production, so the
// format stays exactly as CONTRACT-19 wrote it. See the report's red-team entry.
func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// timestampValue builds a canonical timestamp argument. It stays local (rather
// than moving to internal/dual with the rest) because `auth` is the only package
// that binds a timestamp as a ROUTINE argument: internal/server writes its
// timestamps through raw SQL, where the value crosses as a plain driver string.
func timestampValue(v string) compat.Value {
	return compat.Value{Kind: compat.TimestampValue, Value: v}
}
