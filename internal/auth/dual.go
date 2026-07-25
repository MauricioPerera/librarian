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
	"github.com/MauricioPerera/librarian/internal/dual"
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
// CONTRACT-20B wrote a bare time.RFC3339Nano here and argued AGAINST adopting
// internal/server's fixed-width layout: nothing in `auth` is paginated, its order
// is imposed in Go over the value compat hands back (always re-rendered TRIMMED,
// whatever was stored), so the fixed width bought no ordering guarantee while
// changing the stored text of api_keys.created_at — and with it the sequence
// SQLite produced across rows written before and after the change.
//
// CONTRACT-20C REVISES that decision, because both halves of its premise moved:
//
//   - The Go order of ListAPIKeys no longer depends on the text at all: it
//     compares created_at as an INSTANT (dual.DescInstant). So the stored text
//     changing shape is now observationally FREE for this package — the argument
//     that blocked the change is what T1 removed.
//   - The remaining reason to care about the stored text is SQL: an ORDER BY
//     inside the engine cannot parse, it compares what is stored. Having ONE
//     writer format across the whole application is the precondition for that
//     order to be correct, and a per-package format is a defect waiting for the
//     next routine that sorts by created_at in SQL.
//
// So the layout is now dual.TimestampLayout, the single definition shared with
// internal/server. See the report, T2.
func now() string {
	return dual.Now()
}

// timestampValue builds a canonical timestamp argument. It stays local (rather
// than moving to internal/dual with the rest) because `auth` is the only package
// that binds a timestamp as a ROUTINE argument: internal/server writes its
// timestamps through raw SQL, where the value crosses as a plain driver string.
func timestampValue(v string) compat.Value {
	return compat.Value{Kind: compat.TimestampValue, Value: v}
}
