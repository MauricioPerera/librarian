package schema

// identifier.go implements CONTRACT-13 T1: the strict identifier validator that
// every dynamically-supplied table or column name MUST pass before it is ever
// used to build a compat.Table, compiled into DDL, or interpolated into a query.
//
// WHY THIS LIVES IN librarian, NOT IN compat (verified reading compat's source):
// compat's Schema.Validate() only rejects an EMPTY name, one of its four
// reserved __compat_* tables, and a duplicate — any other string passes
// (spaces, quotes, semicolons, unicode, 500 characters, leading digits).
// compat's own DDL is safe regardless, because quoteIdentifier escapes embedded
// double quotes correctly. But librarian's generic CRUD layer (CONTRACT-14) has
// to interpolate a dynamic table name into its OWN queries — a SQL identifier
// cannot be bound with a `?` placeholder — and there is no net there. So the
// strict validation is librarian's responsibility, applied at the moment a name
// enters the system (definition creation), not at the moment it is used.
//
// WHY LOWERCASE IS FORCED (not merely "reject duplicates"): SQLite folds case
// even for QUOTED identifiers, PostgreSQL does not. A type named `MiTipo` and
// another named `mitipo` would collide on SQLite and coexist on Postgres — a
// silent dual-engine divergence that breaks the exportability invariant the
// whole project exists to guarantee. Restricting the alphabet to lowercase
// removes the divergence at the source instead of trying to detect it later.

import (
	"fmt"
	"regexp"
)

// MaxIdentifierLength is the maximum length (in bytes; the allowed alphabet is
// ASCII so bytes == characters) of a dynamically-supplied identifier.
//
// JUSTIFICATION — 32 is derived from the tightest real constraint in the stack,
// not picked for aesthetics:
//   - PostgreSQL truncates identifiers at NAMEDATALEN-1 = 63 bytes. A truncated
//     identifier is the worst failure mode available: two distinct names could
//     silently become the same object on Postgres while staying distinct on
//     SQLite (which has no practical limit) — the exact dual-engine divergence
//     the project forbids.
//   - compat DERIVES longer names from a table name. The longest derivation in
//     the package is the change-capture trigger:
//     "__compat_capture_" + <table> + "_" + <kind>, i.e. 17 + len + 1 + 6
//     (kind ∈ {insert, update, delete}) = 24 + len(table).
//     Staying under Postgres' 63 therefore requires len(table) <= 39.
//   - 32 sits comfortably under that hard ceiling (32 + 24 = 56), leaving 7
//     bytes of headroom for any future compat-derived prefix, and is still far
//     more than a human needs for a content-type or field name.
const MaxIdentifierLength = 32

// identifierPattern is the ONLY accepted shape: a lowercase ASCII letter
// followed by lowercase ASCII letters, digits and underscores. This single
// pattern is what rejects quotes, semicolons, spaces, uppercase, unicode,
// leading digits and the empty string — there is no escaping/sanitising path,
// only accept or reject.
var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ReservedNames returns the set of identifiers a dynamic content type or field
// may never use. It is DERIVED, never hardcoded, from three sources:
//
//  1. Every table name produced by Build() — the code-defined schema. Deriving
//     it means that adding a code table in a future contract automatically
//     reserves that name, with no list to keep in sync.
//  2. compat's four internal tables (__compat_schema, __compat_applied_changes,
//     __compat_capture_state, __compat_change_journal). These already fail the
//     pattern (they start with '_'), but they are listed explicitly so the
//     reservation is auditable and survives any future loosening of the pattern.
//  3. Every column name ContentType() injects (id, author_id, created_at,
//     updated_at, metadata) — also derived, by calling ContentType with no own
//     columns and reading back what it produced. A field colliding with one of
//     these would make Schema.Validate() fail with an opaque duplicate-column
//     error at apply time instead of a clean 400 at request time.
//
// The three sets are deliberately merged into ONE list applied to BOTH type
// names and field names. It is slightly stricter than strictly necessary (a
// field named "users" is harmless), but a single reserved set is far easier to
// reason about and audit than two overlapping ones, and no legitimate content
// model needs those words.
func ReservedNames() map[string]struct{} {
	reserved := map[string]struct{}{
		"__compat_schema":          {},
		"__compat_applied_changes": {},
		"__compat_capture_state":   {},
		"__compat_change_journal":  {},
	}
	for _, table := range Build().Tables {
		reserved[table.Name] = struct{}{}
	}
	// Derive the injected column names from ContentType itself (its signature is
	// untouched — this only calls it), so they can never drift apart.
	for _, column := range ContentType("x", nil).Columns {
		reserved[column.Name] = struct{}{}
	}
	return reserved
}

// ValidateIdentifier is THE gate. Every dynamically-supplied name — content
// type or field — must pass it before being used anywhere. It returns a
// descriptive error suitable for a 400 response body (it never echoes the
// offending value back verbatim beyond %q quoting, and never reveals SQL).
func ValidateIdentifier(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if !identifierPattern.MatchString(name) {
		return fmt.Errorf("name %q is invalid: must match [a-z][a-z0-9_]* (lowercase ASCII letters, digits and underscores, starting with a letter)", name)
	}
	if len(name) > MaxIdentifierLength {
		return fmt.Errorf("name %q is invalid: longer than %d characters", name, MaxIdentifierLength)
	}
	if _, ok := ReservedNames()[name]; ok {
		return fmt.Errorf("name %q is reserved and cannot be used", name)
	}
	return nil
}
