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

// DynamicTablePrefix (CONTRACT-17) is the prefix EVERY table produced by a
// dynamic content type carries. It is a STRUCTURAL guarantee, not a convention:
//
//   - A dynamic type named `eventos` produces the table `cpt_eventos`. The two
//     namespaces — code tables and dynamic tables — therefore cannot intersect,
//     so a future contract adding a code table named `eventos` can no longer
//     produce a duplicate table in the composed schema (which made
//     Schema.Validate() fail and the service refuse to start: hueco 3 of
//     docs/PENDIENTES.md).
//   - The guarantee only holds while NO code table starts with this prefix.
//     Code tables are Go literals and never pass through this validator, so the
//     enforcement is a TEST over Build() (see TestNoCodeTableUsesDynamicPrefix).
//     Without that test the prefix would be a convention; with it, it is a
//     guarantee.
//
// `cpt_` is chosen for "content type" (the vocabulary DEFINITION-CPT-
// DINAMICOS.md already uses), it is short (4 bytes of the identifier budget),
// and it matches the identifier pattern, so a prefixed name is still a legal
// identifier and needs no special-casing anywhere.
//
// This is NEVER shown to, nor written by, a user: every public surface
// (/content/{type}, /admin/content/{type}, the definitions API, the sidebar)
// keeps using the bare name the admin chose.
const DynamicTablePrefix = "cpt_"

// MaxTypeNameLength is the maximum length of a dynamic content-type NAME.
//
// DECISION (CONTRACT-17 red-team): MaxIdentifierLength applies to the REAL
// TABLE name, i.e. to the PREFIXED one — because the whole justification of the
// 32-byte budget is downstream: Postgres' 63-byte NAMEDATALEN and compat's
// derived names ("__compat_capture_" + <table> + "_" + <kind>, 24 + len(table)).
// Those derivations are applied to the real table, which now carries the
// prefix. So the type name budget shrinks by len(prefix) and a 32-character
// type name is no longer accepted: it would produce a 36-byte table, breaking
// the invariant the constant exists to protect. FIELD names are not prefixed
// and keep the full MaxIdentifierLength.
const MaxTypeNameLength = MaxIdentifierLength - len(DynamicTablePrefix)

// DynamicTableName is THE single place where a dynamic type's public name
// becomes its real table name. Everything that needs the table name (the
// schema builder, the generic CRUD layer, the store) calls this — the
// concatenation is never repeated, so the two namespaces cannot drift apart.
func DynamicTableName(typeName string) string {
	return DynamicTablePrefix + typeName
}

// StagingTablePrefix (CONTRACT-18) is the prefix of the TRANSIENT table used
// while a dynamic type's fields are being edited. Editing a type rebuilds its
// table (compat has neither ALTER TABLE nor RENAME), and the rebuild needs a
// place to park the rows while the original is dropped and re-created under its
// unchanged name.
//
// WHY IT IS DISJOINT FROM EVERYTHING ELSE — this is a structural argument, not
// a naming convention:
//
//   - It cannot collide with a DYNAMIC table. A dynamic table is
//     DynamicTablePrefix + <type name> = "cpt_" + name, so its 4th character is
//     always '_'. A staging name's 4th character is 'm'. The underscore in
//     "cpt_" is what makes the two families provably disjoint: a type called
//     `mp_eventos` produces `cpt_mp_eventos`, never `cptmp_eventos`.
//   - It cannot collide with a CODE table, and that half is enforced by a test
//     over Build() (TestNoCodeTableUsesStagingPrefix), exactly like CONTRACT-17
//     does for `cpt_`. Code tables are Go literals and never pass through any
//     runtime validator, so only a test can guarantee it.
//
// It is NEVER shown to a user and never appears in any response: the staging
// table exists only inside the single transaction that rebuilds a type.
const StagingTablePrefix = "cptmp_"

// StagingTableName is the FIXED name of the rebuild staging table.
//
// DECISION (CONTRACT-18) — it is a constant, NOT derived from the type name:
//
//   - A derived name ("cptmp_" + <type>) would be 6 + up to 28 = 34 bytes and
//     would therefore FAIL ValidateIdentifier for a long-but-legal type name.
//     The rebuild would then be impossible for exactly the types whose names sit
//     at the top of the allowed budget — a silent, arbitrary restriction.
//   - Nothing is gained by a per-type name: the table lives inside ONE
//     transaction, schema-mutating requests are serialized by the HTTP layer's
//     schemaMu, and compat pins the SQLite pool to a single connection, so two
//     rebuilds can never be in flight at the same time.
//
// At 13 bytes it is always a legal identifier, whatever the type is called.
const StagingTableName = StagingTablePrefix + "rebuild"

// QuoteIdentifier is THE single place in the project where a dynamic name
// becomes SQL text. It re-runs the CONTRACT-13 T1 gate before quoting, so an
// identifier can only be interpolated if it still satisfies `[a-z][a-z0-9_]*`
// (which contains no quote, no semicolon, no space and no unicode). The double
// quotes are then belt-and-braces, not the protection: the alphabet alone makes
// escaping moot.
//
// It lives here rather than in internal/server (where CONTRACT-14 first wrote
// it) because CONTRACT-18 needs the very same function in internal/store to
// build the copy statements of a table rebuild, and store cannot import server.
// server/content.go's quoteIdentifier now delegates to this one, so there is
// still exactly ONE implementation and one error message.
func QuoteIdentifier(name string) (string, error) {
	if err := ValidateIdentifier(name); err != nil {
		return "", fmt.Errorf("refusing to build a query with identifier %q: %w", name, err)
	}
	return `"` + name + `"`, nil
}

// QuoteInternalIdentifier quotes a name this package itself owns (the staging
// table, whose name is a compile-time constant of this file). It exists because
// ValidateIdentifier legitimately rejects nothing about "cptmp_rebuild" — it
// passes the pattern — but the staging name is NOT a dynamically supplied name
// and must not be routed through the dynamic gate as if it were. Keeping the
// two entry points separate makes every call site say which kind of name it is
// handling.
func QuoteInternalIdentifier(name string) (string, error) {
	if !identifierPattern.MatchString(name) || len(name) > MaxIdentifierLength {
		return "", fmt.Errorf("internal identifier %q is not a legal identifier", name)
	}
	return `"` + name + `"`, nil
}

// identifierPattern is the ONLY accepted shape: a lowercase ASCII letter
// followed by lowercase ASCII letters, digits and underscores. This single
// pattern is what rejects quotes, semicolons, spaces, uppercase, unicode,
// leading digits and the empty string — there is no escaping/sanitising path,
// only accept or reject.
var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ReservedNames returns the set of identifiers a dynamic content type or field
// may never use. It is DERIVED, never hardcoded, from two sources:
//
//  1. compat's four internal tables (__compat_schema, __compat_applied_changes,
//     __compat_capture_state, __compat_change_journal). These already fail the
//     pattern (they start with '_'), but they are listed explicitly so the
//     reservation is auditable and survives any future loosening of the pattern.
//  2. Every column name ContentType() injects (id, author_id, created_at,
//     updated_at, metadata) — derived, by calling ContentType with no own
//     columns and reading back what it produced. A FIELD colliding with one of
//     these would make Schema.Validate() fail with an opaque duplicate-column
//     error at apply time instead of a clean 400 at request time. This is the
//     reservation that still carries real weight, and it is unchanged.
//
// DECISION (CONTRACT-17 T2) — the third source, "every table name produced by
// Build()", was REMOVED, deliberately:
//
//   - Its ONLY purpose was to stop a dynamic type from colliding with a code
//     table. With DynamicTablePrefix that collision is structurally impossible:
//     a type named `users` produces `cpt_users`, which no code table can ever
//     be called (enforced by TestNoCodeTableUsesDynamicPrefix). Keeping the
//     reservation would be a rule whose justification no longer exists.
//   - Keeping it also had a real cost: `articles`, `products`, `terms`,
//     `media`… are exactly the names an admin would legitimately choose for
//     their own type, and rejecting them would be an unexplainable "reserved"
//     error about an implementation detail the admin cannot see.
//   - The confusion argument (a type `users` living next to /users) is real but
//     cosmetic, and it is the admin's call to make: the two are different
//     surfaces (/content/users vs /users) and different tables (cpt_users vs
//     users). A structural impossibility beats a naming taboo.
//
// The remaining set is applied to BOTH type names and field names. It is
// slightly stricter than strictly necessary for a type name (a type named `id`
// is harmless now that it becomes `cpt_id`), but one reserved set is far easier
// to audit than two overlapping ones, and no legitimate content model needs
// those words as a type name either.
func ReservedNames() map[string]struct{} {
	reserved := map[string]struct{}{
		"__compat_schema":          {},
		"__compat_applied_changes": {},
		"__compat_capture_state":   {},
		"__compat_change_journal":  {},
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

// ValidateTypeName is the gate for a dynamic content type's PUBLIC name. It is
// ValidateIdentifier plus the tighter length budget: the name must still be a
// legal identifier AFTER the prefix is applied, because it is the PREFIXED name
// that becomes a real table and that compat derives its own names from.
//
// A type name that STARTS WITH the prefix (`cpt_x` → `cpt_cpt_x`) is
// deliberately ALLOWED: name → prefix+name is injective, so no two type names
// can ever produce the same table, and the collision this contract removes is
// between dynamic tables and CODE tables, not among dynamic ones (which is
// already decided by UNIQUE(name) on content_types). Forbidding it would be a
// rule with no failure to prevent.
func ValidateTypeName(name string) error {
	if err := ValidateIdentifier(name); err != nil {
		return err
	}
	if len(name) > MaxTypeNameLength {
		return fmt.Errorf("name %q is invalid: longer than %d characters (a content type name is limited to %d so its real table %q fits the %d-character identifier budget)",
			name, MaxTypeNameLength, MaxTypeNameLength, DynamicTableName(name), MaxIdentifierLength)
	}
	return nil
}
