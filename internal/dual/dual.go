// Package dual holds the engine-neutral plumbing shared by every librarian
// package that talks to the database through sqlite-postgres-compat.
//
// CONTRACT-20B T1. CONTRACT-19 created internal/auth/dual.go and CONTRACT-20
// created internal/server/dual.go; the two ended up with the SAME six functions
// written twice (bind, newUUID, rowText, rowIsNull, textValue, uuidValue), plus
// dedupe and the txQuerier interface, and the collation fix of this contract
// would have made the stable-order helpers a ninth and tenth copy. Two copies
// that are identical today diverge the moment somebody fixes an edge case in one
// of them — and these are precisely the functions where an edge case means "one
// engine does something else".
//
// WHERE IT LIVES, AND WHY HERE. internal/auth cannot import internal/server (the
// dependency runs the other way), so the shared home has to be neutral with
// respect to all three consumers. The import graph before this contract was:
//
//	internal/schema  → (nothing internal)
//	internal/auth    → internal/schema
//	internal/store   → internal/schema
//	internal/server  → internal/auth, internal/schema, internal/store
//
// This package imports NOTHING from librarian — only compat and the standard
// library — so it is a LEAF, below internal/schema. That is what makes it usable
// from internal/store, which CONTRACT-21 will need, with an import cycle
// impossible by construction rather than merely absent today. Putting these
// helpers in internal/schema would also have compiled, but schema declares the
// canonical MODEL (tables, views, routines); binding markers, UUID generation and
// row accessors are the plumbing that EXECUTES against it, and mixing the two
// would make schema import-heavy for consumers that only want the declarations.
//
// WHAT IS NOT HERE, ON PURPOSE. Only what was genuinely duplicated moved.
// bindList, quote, integerValue, timestampValue and rowTextPointer live in a
// single package each and stay there; consolidating them too would create a
// shared surface nobody shares, which is as bad as not consolidating.
package dual

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"sort"

	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// Bind composes the engine's bind marker for the 1-based argument position. It
// is a thin name for compat.Placeholder so a raw statement reads as SQL with
// holes rather than as string concatenation, and so there is exactly ONE place
// in librarian that knows about `?` vs `$n` — compat's.
func Bind(engine compat.Engine, position int) string {
	return compat.Placeholder(engine, position)
}

// NewUUID returns a random RFC 4122 v4 UUID in the canonical 8-4-4-4-12 text
// form, using crypto/rand.
//
// It is how CONTRACT-19 and CONTRACT-20 resolved `INSERT ... RETURNING id`:
// compat deliberately does not implement RETURNING (a consumer that needs a
// database-generated id passes it in instead), the id columns keep their DEFAULT
// gen_random_uuid() as a safety net for writes that do not come from the
// application, and the identifier becomes known BEFORE the write rather than
// after it.
func NewUUID() (string, error) {
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
// A routine argument is a compat.Value: an explicit canonical KIND plus its text.
// Declaring the kind at the call site is what lets compat bind the value the way
// the destination engine expects instead of guessing from the Go type.

// TextValue builds a canonical text argument.
func TextValue(v string) compat.Value {
	return compat.Value{Kind: compat.TextValue, Value: v}
}

// UUIDValue builds a canonical uuid argument.
func UUIDValue(v string) compat.Value {
	return compat.Value{Kind: compat.UUIDValue, Value: v}
}

// row accessors -----------------------------------------------------------------

// RowText returns the canonical text of a column of a routine result row. A NULL
// (or an absent column, which Schema.Validate makes impossible) reads as "".
func RowText(row compat.Row, column string) string {
	return row[column].Value
}

// RowIsNull reports whether a column of a routine result row is SQL NULL. It
// tests the canonical KIND, not the emptiness of the text, so a genuinely empty
// string is never mistaken for a NULL — the distinction api_keys.revoked_at,
// articles.published_at, terms.parent_id and every nullable dynamic field depend
// on.
func RowIsNull(row compat.Row, column string) bool {
	return row[column].Kind == compat.NullValue
}

// raw-SQL helpers ----------------------------------------------------------------

// TxQuerier is the subset of *sql.Tx the raw-SQL helpers need. It exists so a
// helper can be read (and reviewed) without caring whether it runs on a
// transaction or a pool — the resolvers that use it always run on the caller's
// transaction.
type TxQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Dedupe returns ids without repetitions, preserving first-appearance order.
//
// It replaces the `ON CONFLICT DO NOTHING` that every set-replacement INSERT of
// this project used to carry. After the DELETE that precedes those inserts the
// target set is empty, so the ONLY way to hit a composite primary key is a value
// repeated in the CALLER's list. Removing the duplicates in Go makes the conflict
// impossible by construction, which keeps the exact same observable behavior
// without depending on an upsert clause whose accepted spelling differs between
// engines and versions.
func Dedupe(ids []string) []string {
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

// COLLATION — the stable ordering helpers, and why they exist ---------------------
//
// PostgreSQL orders TEXT by the DATABASE COLLATION; SQLite orders it by BYTES.
// On a stock PostgreSQL (en_US.utf8) `content_types.manage` sorts BEFORE
// `content.update`, because the collation gives `.` and `_` no primary weight; on
// SQLite `content.update` comes first, because `.` (0x2E) < `_` (0x5F). Same
// rows, different sequence, from the SAME declared `ORDER BY permission_name`.
// The same happens with case: `Redaccion` precedes `prensa` by bytes (0x52 <
// 0x70) and follows it under a case-insensitive collation.
//
// It cannot be fixed in the schema: expressing it would need `COLLATE "C"` on
// PostgreSQL and `COLLATE BINARY` on SQLite — different syntax, i.e. exactly the
// engine-specific SQL this series removes — and compat's declared routine ORDER
// BY is a plain column name with no collation to give. It is not a gap in compat
// either: the collation is a property of the PostgreSQL DEPLOYMENT, not something
// a library can decide on the consumer's behalf.
//
// So for every listing that is NOT paginated, the routine's ORDER BY stays as a
// stable base and the FINAL order is imposed here, in Go, by plain BYTE
// comparison. That is engine-independent by construction — it is the
// application's order rather than the database's opinion of it — and byte
// comparison specifically, because that is what SQLite already does, so the order
// PRODUCTION observes today does not change.
//
// A PAGINATED listing cannot use this: its LIMIT/OFFSET selects rows inside the
// engine, so re-sorting afterwards would reorder a page instead of choosing it.
// Those listings (all of them in internal/server) are safe by a different
// argument, documented there: their keys are a fixed-width timestamp and a UUID,
// shapes for which the two comparisons provably agree.

// SortStrings orders a slice of strings by BYTE value, in place.
func SortStrings(values []string) {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
}

// Key is one component of a multi-key byte-wise ordering: the text to compare
// and whether that component runs descending. It exists because a listing like
// api_keys is declared `ORDER BY created_at DESC, label ASC` — the directions
// differ per key, so a plain []string cannot express it.
type Key struct {
	Value      string
	Descending bool
}

// Asc builds an ascending sort key.
func Asc(value string) Key { return Key{Value: value} }

// Desc builds a descending sort key.
func Desc(value string) Key { return Key{Value: value, Descending: true} }

// Ascending builds an all-ascending key sequence, the common case.
func Ascending(values ...string) []Key {
	out := make([]Key, 0, len(values))
	for _, v := range values {
		out = append(out, Asc(v))
	}
	return out
}

// SortByKeys orders items by a sequence of Keys, comparing each by BYTE value and
// honouring its direction. It is the engine-independent replacement for a
// multi-column ORDER BY over text. The sort is STABLE, so items that compare
// equal on every key keep the sequence the routine's declared ORDER BY produced.
func SortByKeys[T any](items []T, keys func(T) []Key) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := keys(items[i]), keys(items[j])
		for k := range a {
			if a[k].Value == b[k].Value {
				continue
			}
			if a[k].Descending {
				return a[k].Value > b[k].Value
			}
			return a[k].Value < b[k].Value
		}
		return false
	})
}
