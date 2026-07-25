package server

// CONTRACT-20 — the engine-neutral plumbing every statement in this package now
// uses. It is the exact counterpart of internal/auth/dual.go (CONTRACT-19); the
// two files exist separately only because they serve different packages.
//
// Before this contract every statement in `server` was hand-written SQL with `?`
// placeholders, which is SQLite syntax; PostgreSQL's driver requires `$n`. The
// package now reaches the database through exactly two doors, and NO other:
//
//  1. compat.Store.QueryRoutine — every READ, declared in the canonical schema
//     (internal/schema/server_dual.go, and BuildWith for the dynamic types).
//     Rows come back canonicalized by DECLARED column family, which is the only
//     thing that makes a boolean, a decimal and a vector scan identically on
//     both engines.
//  2. Raw SQL composed with compat.Placeholder — every WRITE, plus the two reads
//     that cannot be a routine: the permission lookup by a variable-size list of
//     role names (a routine WHERE has no IN with caller-supplied arity) and the
//     existence/resolution probes that run INSIDE a transaction this package
//     owns (QueryRoutine opens its own, so it cannot join one).
//
// WHY NO CallRoutine ANYWHERE IN THIS PACKAGE. It is not an oversight, it is the
// criterion CONTRACT-20 states: CallRoutine returns only `error`, so it cannot
// report RowsAffected. Every write in internal/server either uses RowsAffected
// to decide 404-vs-200 (articles, products, terms, dynamic content) or runs
// inside a consumer transaction (insertTerm, updateTerm, setContentTerms).
// Simulating the row count with a prior read would add a round trip and a race
// where there is none today, which the contract forbids explicitly.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// engine is the engine the request's connection is bound to. Every raw statement
// composes its placeholders against it; nothing else in this package is allowed
// to know which engine it is.
func (h *handlers) engine() compat.Engine {
	return h.store.Target.Engine
}

// bind composes the engine's bind marker for the 1-based argument position. It
// is a thin, local name for compat.Placeholder so a raw statement reads as SQL
// with holes rather than as string concatenation, and so there is exactly ONE
// place that knows about `?` vs `$n` — compat's.
func bind(engine compat.Engine, position int) string {
	return compat.Placeholder(engine, position)
}

// bindList composes "p1, p2, ... pn" starting at the given 1-based position, for
// the two places that need a list: an IN predicate over a variable-size set of
// role names, and the VALUES list of a dynamic INSERT whose column count is only
// known at runtime. The caller passes its arguments in exactly this order, which
// is the contract compat.Placeholder documents (SQLite binds by appearance,
// PostgreSQL by number; they agree only when the emitted sequence is 1,2,3,…).
func bindList(engine compat.Engine, first, count int) string {
	out := ""
	for i := 0; i < count; i++ {
		if i > 0 {
			out += ", "
		}
		out += bind(engine, first+i)
	}
	return out
}

// quote is the identifier quoting used by every raw statement this package
// composes. It is schema.QuoteIdentifier through the existing local name
// (quoteIdentifier in content.go), except that the names used here are
// compile-time constants of this package, so a failure is impossible and the
// error would only obscure the call site.
//
// Quoting matters for dual-engine correctness, not only for safety: an unquoted
// identifier is folded to lower case by PostgreSQL and left alone by SQLite, so
// two engines could resolve the same written name to different columns. compat
// quotes every identifier it emits for the same reason.
func quote(name string) string {
	return `"` + name + `"`
}

// now returns the current instant in the canonical timestamp form of the
// `timestamp` family: RFC3339Nano, UTC, stored as TEXT on BOTH engines.
//
// This replaces every CURRENT_TIMESTAMP this package used to write inside an
// INSERT/UPDATE, and — like CONTRACT-19 decision 4 — it is also written for the
// created_at that used to be left to the column DEFAULT. That second part is not
// cosmetic: created_at is the PRIMARY SORT KEY of listArticles, listProducts and
// listContentRows, it is TEXT on both engines, and the two engines render
// CURRENT_TIMESTAMP differently ("2026-07-25 18:38:25" on SQLite, second
// resolution; "2026-07-25 18:38:26.471192+00" on PostgreSQL). Sorting those two
// texts does not give the same sequence. Writing the instant from the
// application in the form compat DECLARES for the family makes the stored text —
// and therefore the order — identical. The column DEFAULTs stay as a safety net
// for writes that do not come from the application.
func nowCanonical() string {
	return time.Now().UTC().Format(canonicalTimestampLayout)
}

// canonicalTimestampLayout is RFC3339Nano with a FIXED-WIDTH nanosecond field.
//
// It is not a new format — it is a valid RFC3339Nano value, parsed by compat's
// timestamp family like any other and re-rendered trimmed when read back, so
// nothing observable changes. The fixed width exists for ORDER BY. Go's
// time.RFC3339Nano TRIMS trailing zeros, which makes the stored strings differ
// in LENGTH and in the position of the trailing `Z`; a timestamp column is TEXT
// on both engines, so it is ordered as text, and PostgreSQL orders text by the
// database COLLATION (typically en_US.utf8) while SQLite orders it by BYTES.
// A collation whose primary weight ignores punctuation can rank two
// different-shaped timestamps differently from a byte comparison. Fixing the
// width makes every stored timestamp the same length with punctuation at the
// same offsets, which is the condition under which the two orderings provably
// agree. This matters because created_at is the primary sort key of the three
// PAGINATED listings, where the order decides WHICH ROWS a page contains and
// cannot be re-sorted after the fact in Go.
const canonicalTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// COLLATION — the divergence this contract's battery actually caught, and the
// reason the helpers below exist.
//
// PostgreSQL orders TEXT by the database collation; SQLite orders it by bytes.
// On a stock PostgreSQL (en_US.utf8) `content_types.manage` sorts BEFORE
// `content.update`, because the collation gives `.` and `_` no primary weight;
// on SQLite `content.update` comes first, because `.` (0x2E) < `_` (0x5F). Same
// rows, different sequence, on the same declared `ORDER BY permission_name`.
//
// It cannot be fixed in the schema: expressing it would need `COLLATE "C"` on
// PostgreSQL and `COLLATE BINARY` on SQLite — different syntax, i.e. exactly the
// engine-specific SQL this contract removes — and compat's declared routine
// ORDER BY is a plain column name with no collation to give. So for every
// listing that is NOT paginated, the routine's ORDER BY provides a stable base
// and the FINAL order is imposed here, in Go, with plain byte comparison. That
// is engine-independent by construction: it is the application's order, not the
// database's opinion of the application's order.
//
// The three PAGINATED listings cannot use this (their LIMIT/OFFSET selects rows
// inside the engine, so re-sorting afterwards would reorder a page rather than
// choose it). They are safe by a different argument: their keys are created_at,
// made fixed-width above, and the primary-key UUID, whose hyphens sit at
// identical offsets in every value — both are shapes for which a collation-aware
// and a byte-wise comparison provably agree.

// sortStrings orders a slice of strings by BYTE value, in place.
func sortStrings(values []string) {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
}

// sortByKeys orders items by a sequence of string keys, comparing each key by
// BYTE value. It is the engine-independent replacement for a multi-column
// ORDER BY over text.
func sortByKeys[T any](items []T, keys func(T) []string) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := keys(items[i]), keys(items[j])
		for k := range a {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return false
	})
}

// newUUID returns a random RFC 4122 v4 UUID in the canonical 8-4-4-4-12 text
// form, using crypto/rand — the same helper and the same reasoning as
// internal/auth (CONTRACT-19 decision 3).
//
// It replaces every `INSERT ... RETURNING id` in this package. compat
// deliberately does not implement RETURNING (a consumer that needs a
// database-generated id passes it in instead), the id column keeps its DEFAULT
// gen_random_uuid() as a safety net, and the identifier becomes known BEFORE the
// write rather than after it.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// value constructors --------------------------------------------------------------
//
// A routine argument is a compat.Value: an explicit canonical KIND plus its
// text. Declaring the kind at the call site is what lets compat bind the value
// the way the destination engine expects instead of guessing from the Go type.

func textValue(v string) compat.Value {
	return compat.Value{Kind: compat.TextValue, Value: v}
}

func uuidValue(v string) compat.Value {
	return compat.Value{Kind: compat.UUIDValue, Value: v}
}

func integerValue(v int) compat.Value {
	return compat.Value{Kind: compat.IntegerValue, Value: fmt.Sprintf("%d", v)}
}

// row accessors --------------------------------------------------------------------

// rowText returns the canonical text of a column of a routine result row. A NULL
// reads as "".
func rowText(row compat.Row, column string) string {
	return row[column].Value
}

// rowIsNull reports whether a column of a routine result row is SQL NULL. It
// tests the canonical KIND, not the emptiness of the text, so a genuinely empty
// string is never mistaken for a NULL — the distinction articles.published_at,
// terms.parent_id and every nullable dynamic field depend on.
func rowIsNull(row compat.Row, column string) bool {
	return row[column].Kind == compat.NullValue
}

// rowTextPointer returns a *string that is nil exactly when the column is NULL.
// It is the shape the JSON views of this package use for a nullable text/uuid
// column (article.PublishedAt, term.ParentID).
func rowTextPointer(row compat.Row, column string) *string {
	if rowIsNull(row, column) {
		return nil
	}
	v := row[column].Value
	return &v
}

// queryOne runs a read routine expected to match at most one row. found=false
// when nothing matched, which every caller maps to 404 — a missing OR malformed
// id simply matches no row, so it can never surface as a raw SQL error.
func (h *handlers) queryOne(ctx context.Context, routine string, args map[string]compat.Value) (compat.Row, bool, error) {
	rows, err := h.queryRoutine(ctx, routine, args)
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	return rows[0], true, nil
}

// queryRoutine runs a read routine against the CODE-defined canonical schema.
// The schema is a pure function of the code (serverSchema below), so no database
// round trip is needed to know it.
func (h *handlers) queryRoutine(ctx context.Context, routine string, args map[string]compat.Value) ([]compat.Row, error) {
	return h.store.QueryRoutine(ctx, serverSchema, routine, args)
}

// serverSchema is the canonical schema the code-defined routines are declared
// in. Like internal/auth's, it is built once: QueryRoutine only needs the
// declaration of the routine plus the relation it reads, and no routine over the
// code tables refers to a dynamic type. Reads over a DYNAMIC type build their
// own schema per request from the persisted definition (content.go), because
// that is the only place the type's column set is known.
var serverSchema = schema.Build()

// txQuerier is the subset of *sql.Tx the raw-SQL helpers need, so a helper can
// be read without caring whether it runs on a transaction or a pool — the term
// resolvers always run on the caller's transaction.
type txQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
