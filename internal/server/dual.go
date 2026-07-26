package server

// CONTRACT-20 — the engine-neutral plumbing every statement in this package now
// uses.
//
// Before that contract every statement in `server` was hand-written SQL with `?`
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
//
// CONTRACT-20B moved everything this file shared verbatim with
// internal/auth/dual.go — bind, newUUID, rowText, rowIsNull, textValue,
// uuidValue, dedupe, the txQuerier interface and the byte-wise ordering helpers —
// to internal/dual. What remains here is what only `server` has: the per-request
// engine accessor, the bind LIST, identifier quoting, the fixed-width canonical
// instant its paginated listings depend on, and the routine plumbing bound to
// this package's schema handle.

import (
	"context"
	"fmt"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// engine is the engine the request's connection is bound to. Every raw statement
// composes its placeholders against it; nothing else in this package is allowed
// to know which engine it is.
func (h *handlers) engine() compat.Engine {
	return h.store.Target.Engine
}

// bindList composes "p1, p2, ... pn" starting at the given 1-based position, for
// the two places that need a list: an IN predicate over a variable-size set of
// role names, and the VALUES list of a dynamic INSERT whose column count is only
// known at runtime. The caller passes its arguments in exactly this order, which
// is the contract compat.Placeholder documents (SQLite binds by appearance,
// PostgreSQL by number; they agree only when the emitted sequence is 1,2,3,…).
//
// It stays in this package because no other consumer composes a variable-arity
// statement — CONTRACT-20B moves only what is genuinely duplicated.
func bindList(engine compat.Engine, first, count int) string {
	out := ""
	for i := 0; i < count; i++ {
		if i > 0 {
			out += ", "
		}
		out += dual.Bind(engine, first+i)
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

// nowCanonical returns the current instant in the canonical timestamp form of the
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
	return dual.Now()
}

// canonicalTimestampLayout is RFC3339Nano with a FIXED-WIDTH nanosecond field.
// CONTRACT-20C moved the definition to dual.TimestampLayout, so the application
// has exactly ONE writer format; the name stays here as the local alias the rest
// of this package reads.
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
//
// CONTRACT-20B kept it OUT of internal/auth on the grounds that auth sorts in Go
// over the trimmed value compat hands back, so the fixed width bought nothing
// there while changing the stored text. CONTRACT-20C shares it after all: auth
// now compares created_at as an instant, so its stored text no longer decides any
// order, and one writer format for the whole application is what makes a SQL
// ORDER BY over created_at correct. See internal/auth/dual.go and the 20C report.
const canonicalTimestampLayout = dual.TimestampLayout

// integerValue builds a canonical integer routine argument. It stays local: this
// is the only package that passes a bound LIMIT/OFFSET to a routine.
func integerValue(v int) compat.Value {
	return compat.Value{Kind: compat.IntegerValue, Value: fmt.Sprintf("%d", v)}
}

// rowTextPointer returns a *string that is nil exactly when the column is NULL.
// It is the shape the JSON views of this package use for a nullable text/uuid
// column (article.PublishedAt, term.ParentID), and no other package needs it.
func rowTextPointer(row compat.Row, column string) *string {
	if dual.RowIsNull(row, column) {
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
	return h.store.QueryRoutine(ctx, h.codeSchema(), routine, args)
}

// codeSchema is the canonical code schema THIS INSTALLATION's routines are
// declared in (CONTRACT-23). It picks between two values built once at init
// rather than composing per request: the choice is fixed for the life of the
// process (it is fixed for the life of the INSTALLATION), and a routine's
// declared columns are what compat compiles into the SELECT list, so an
// installation without the vector capability must not read a column its
// articles table does not have.
func (h *handlers) codeSchema() compat.Schema {
	if h.caps.VectorDisabled {
		return serverSchemaNoVector
	}
	return serverSchema
}

// serverSchema is the canonical schema the code-defined routines are declared
// in. Like internal/auth's, it is built once: QueryRoutine only needs the
// declaration of the routine plus the relation it reads, and no routine over the
// code tables refers to a dynamic type. Reads over a DYNAMIC type build their
// own schema per request from the persisted definition (content.go), because
// that is the only place the type's column set is known.
var serverSchema = schema.Build()

// serverSchemaNoVector is the same schema for an installation that did not
// declare the vector capability. Built at init like its sibling: both are pure
// functions of the code, so there is nothing to defer.
var serverSchemaNoVector = schema.BuildFor(schema.Capabilities{VectorDisabled: true})
