// Package schema declares the canonical librarian data model using the
// engine-neutral model of sqlite-postgres-compat. The whole schema is built in
// Go (never as raw SQL), so it inherits compat's validation, dual-engine DDL
// compilation (SQLite + PostgreSQL) and its no-cutover export capability from
// day one — the invariant that motivates the whole project.
package schema

import "github.com/MauricioPerera/sqlite-postgres-compat/compat"

// Engine target versions used across the project. SQLite is the primary
// runtime engine (embedded libSQL); Postgres is only an on-demand export
// target, but the schema must compile for it too (exportability invariant).
var (
	// SQLiteVersion is the target version used for the embedded libSQL engine.
	SQLiteVersion = compat.Version{Major: 3}
	// PostgresVersion is the target version used only to prove exportability.
	PostgresVersion = compat.Version{Major: 17}
)

// SQLiteTarget / PostgresTarget are the compiled DDL targets for each engine.
var (
	SQLiteTarget   = compat.Target{Engine: compat.SQLite, Version: SQLiteVersion}
	PostgresTarget = compat.Target{Engine: compat.Postgres, Version: PostgresVersion}
)

// Seed catalogs. Roles and permissions are fixed in code (not editable at
// runtime in v1, per DEFINITION.md). These slices are the source of truth for
// seeding rows; the schema itself only declares the tables.
var (
	// Roles is the fixed role catalog.
	Roles = []string{"administrator", "editor", "author", "contributor"}
	// Permissions is the fixed permission catalog. content.update is added by
	// CONTRACT-03 T1 — the rest date from CONTRACT-02. The seed (store.
	// SeedCatalogs) is idempotent and data-driven off this slice, so adding the
	// row here is the only change required; it needs no seed-logic change and
	// does not touch the schema DDL (it is a catalog row, not a table).
	//
	// terms.manage is added by CONTRACT-12 — the FIRST time in the project that a
	// genuinely new permission is added to the catalog (every prior contract
	// reused an existing content.*/users.manage/roles.manage grant). It is
	// justified because no existing permission covers "administer the catalog of
	// categories/tags": content.* governs authoring individual pieces of content,
	// not the shared taxonomy the content is filed under; users.manage/roles.manage
	// are the account/RBAC domain. terms.manage gates BOTH the CRUD of terms and
	// the (re)assignment of terms to content.
	//
	// content_types.manage is added by CONTRACT-13 — the SECOND genuinely new
	// permission of the project (after terms.manage). It is dedicated and
	// separate on purpose: creating a dynamic content type CREATES A REAL TABLE
	// in the production database, an irreversible, higher-risk action than
	// administering existing content or taxonomy. DEFINITION-CPT-DINAMICOS.md
	// decided explicitly that it be an ASSIGNABLE permission rather than a
	// capability hardcoded to the administrator role. Like every other entry
	// here it is picked up automatically by the idempotent, data-driven seed
	// (store.SeedCatalogs) — adding the string is the only change required.
	Permissions = []string{
		"content.create",
		"content.update",
		"content.publish",
		"content.delete",
		"users.manage",
		"roles.manage",
		"terms.manage",
		"content_types.manage",
	}
	// Taxonomies is the fixed taxonomy catalog (CONTRACT-12): the kinds of term a
	// piece of content can be filed under. Like Roles/Permissions it is code-fixed
	// (not editable at runtime in v1) and seeded data-driven from this slice via
	// store.SeedCatalogs (the same seedNames helper). "category" is hierarchical
	// (terms may have a parent), "tag" is flat — but the schema is identical for
	// both; the distinction is a UI/usage convention, not a structural one.
	Taxonomies = []string{"category", "tag"}
)

// column constructors ---------------------------------------------------------

func uuidColumn(name string, nullable bool) compat.Column {
	return compat.Column{Name: name, Type: compat.Type{Family: compat.UUIDType}, Nullable: nullable}
}

func textColumn(name string, nullable bool) compat.Column {
	return compat.Column{Name: name, Type: compat.Type{Family: compat.TextType}, Nullable: nullable}
}

func jsonColumn(name string, nullable bool) compat.Column {
	return compat.Column{Name: name, Type: compat.Type{Family: compat.JSONType}, Nullable: nullable}
}

func timestampColumn(name string, nullable bool) compat.Column {
	return compat.Column{Name: name, Type: compat.Type{Family: compat.TimestampType}, Nullable: nullable}
}

// decimalColumn builds a decimal/NUMERIC column with NO fixed precision/scale
// (CONTRACT-11 T1). compat compiles compat.DecimalType with zero type arguments
// to NUMERIC (arbitrary precision) on Postgres and to TEXT — the canonical
// textual carrier — on SQLite, because SQLite's only numeric affinity able to
// hold a value here (REAL) is IEEE-754 and cannot preserve arbitrary-precision
// decimals byte-for-byte. Storing the canonical decimal text keeps the value
// identical across both engines (the exportability invariant). Precision/scale
// is deliberately left unset: this contract does not require a bounded money
// type, and an unbounded NUMERIC is the safest cross-engine default. Mirrors the
// textColumn/jsonColumn helper shape exactly.
func decimalColumn(name string, nullable bool) compat.Column {
	return compat.Column{Name: name, Type: compat.Type{Family: compat.DecimalType}, Nullable: nullable}
}

// EmbeddingDimension is the fixed dimension of the articles.embedding vector
// column. CONTRACT-05 T1: a concrete, justified value, not an arbitrary one.
// 1536 is the output dimension of OpenAI's text-embedding-3-small and the
// earlier text-embedding-ada-002 — the most widely deployed production
// embedding API. v1 only stores vectors the client already computed (no
// generation pipeline, per DEFINITION.md), so the dimension is whatever the
// client's model produces; 1536 pins the schema to a real, common model so the
// column is declared as vector(1536) on Postgres (and TEXT on SQLite) and the
// server can validate the exact component count on write.
const EmbeddingDimension = 1536

// vectorColumn builds a vector(N) column. compat compiles it to TEXT on SQLite
// (the interoperable carrier, '[c1,c2,...]') and to native vector(N) on
// Postgres (which requires the pgvector extension on the destination — see
// CONTRACT-05). The single argument is the fixed dimension; Schema.Validate
// rejects a non-positive or multi-argument vector. nullable controls whether
// an unset vector is allowed (articles.embedding is nullable: an article with
// no embedding computed yet is valid — the client adds it later).
func vectorColumn(name string, dimension int, nullable bool) compat.Column {
	return compat.Column{
		Name:     name,
		Type:     compat.Type{Family: compat.VectorType, Arguments: []int{dimension}},
		Nullable: nullable,
	}
}

// idColumn is the standard surrogate primary-key column: a UUID defaulting to
// gen_random_uuid() (supported by both engines through compat's grammar).
func idColumn() compat.Column {
	c := uuidColumn("id", false)
	c.Default = &compat.Expression{Kind: "gen_random_uuid"}
	return c
}

// nowColumn is a timestamp column defaulting to CURRENT_TIMESTAMP.
func nowColumn(name string) compat.Column {
	c := timestampColumn(name, false)
	c.Default = &compat.Expression{Kind: "current_timestamp"}
	return c
}

// timestampsAndMetadata returns the created_at/updated_at/metadata trailer that
// every first-class table in librarian shares.
func timestampsAndMetadata() []compat.Column {
	return []compat.Column{
		nowColumn("created_at"),
		nowColumn("updated_at"),
		jsonColumn("metadata", true),
	}
}

// constraint constructors -----------------------------------------------------

func primaryKey(columns ...string) compat.Constraint {
	return compat.Constraint{Kind: compat.PrimaryKey, Columns: columns}
}

func unique(columns ...string) compat.Constraint {
	return compat.Constraint{Kind: compat.UniqueKey, Columns: columns}
}

// foreignKeyCascade is an FK that cascades on delete of the referenced row.
func foreignKeyCascade(column, refTable, refColumn string) compat.Constraint {
	return compat.Constraint{
		Kind:    compat.ForeignKey,
		Columns: []string{column},
		References: &compat.Reference{
			Table:    refTable,
			Columns:  []string{refColumn},
			OnDelete: compat.Cascade,
		},
	}
}

// foreignKeySetNull is an FK that nulls the referencing column when the
// referenced row is deleted (compat.SetNull). CONTRACT-12 uses it for
// terms.parent_id: deleting a parent term must NOT delete its children (unlike
// cascade); it orphans them by leaving parent_id NULL, exactly as WordPress does
// when a parent category is removed. The referencing column MUST be nullable for
// SET NULL to be valid — terms.parent_id is.
func foreignKeySetNull(column, refTable, refColumn string) compat.Constraint {
	return compat.Constraint{
		Kind:    compat.ForeignKey,
		Columns: []string{column},
		References: &compat.Reference{
			Table:    refTable,
			Columns:  []string{refColumn},
			OnDelete: compat.SetNull,
		},
	}
}

// foreignKeyRestrict is an FK that REFUSES to delete the referenced row while a
// referring row exists (compat.Restrict). CONTRACT-27 uses it for every relation
// between dynamic content types.
//
// THE CHOICE BETWEEN `restrict` AND `no_action`, which the contract demands be
// argued rather than assumed. They are NOT synonyms, on either engine:
//
//   - PostgreSQL: NO ACTION defers its check to the END of the statement, and it
//     is the only one of the two that a DEFERRABLE constraint can postpone to
//     COMMIT. RESTRICT is checked immediately when the row is deleted and can
//     never be deferred.
//   - SQLite: the same split, documented in the same words — RESTRICT fires as
//     soon as the parent row is deleted, NO ACTION means "do nothing now" and the
//     violation surfaces when the FK check runs at the end of the statement (or,
//     for a deferred constraint, at COMMIT).
//
// RESTRICT is chosen for three reasons that survive both engines:
//
//  1. It is EXPLICIT IN THE COMPILED DDL. compat emits nothing at all for
//     NoAction (see compileForeignKey: the clause is skipped when the action is
//     empty or no_action), because it is the engine default. An exported schema
//     would then be indistinguishable from one where nobody thought about the
//     question. The rule this contract makes is deliberate and has to be
//     readable in the catalog and in `--dump-schema`.
//  2. It fails ON THE DELETE, not at the end of the statement. The error then
//     names the operation that caused it, which is the entire point of the T3
//     guard's sibling case (deleting a referenced ROW).
//  3. It cannot be silently deferred. compat has no way to declare or manipulate
//     deferrability, so a constraint that COULD be postponed would be postponed
//     by something outside this project's model, and the refusal would surface at
//     COMMIT — far from the statement that caused it.
//
// It is NOT Cascade, and that is the contract's own decision: deleting a
// referenced row must never destroy the rows pointing at it in silence.
func foreignKeyRestrict(column, refTable, refColumn string) compat.Constraint {
	return compat.Constraint{
		Kind:    compat.ForeignKey,
		Columns: []string{column},
		References: &compat.Reference{
			Table:    refTable,
			Columns:  []string{refColumn},
			OnDelete: compat.Restrict,
		},
	}
}

// table builders --------------------------------------------------------------

func usersTable() compat.Table {
	cols := []compat.Column{
		idColumn(),
		textColumn("email", false),
		textColumn("password_hash", false),
		textColumn("status", false),
	}
	cols = append(cols, timestampsAndMetadata()...)
	return compat.Table{
		Name:    "users",
		Columns: cols,
		Constraints: []compat.Constraint{
			primaryKey("id"),
			unique("email"),
			{
				Kind:    compat.Check,
				Columns: []string{"status"},
				Expression: &compat.Expression{
					Kind: "in",
					Args: []compat.Expression{
						{Kind: "column", Value: "status"},
						{Kind: "string", Value: "active"},
						{Kind: "string", Value: "suspended"},
						{Kind: "string", Value: "invited"},
					},
				},
			},
		},
	}
}

// catalogTable builds a fixed catalog table (roles, permissions): id + unique
// name. No metadata trailer — these are code-seeded lookup tables.
func catalogTable(name string) compat.Table {
	return compat.Table{
		Name: name,
		Columns: []compat.Column{
			idColumn(),
			textColumn("name", false),
		},
		Constraints: []compat.Constraint{
			primaryKey("id"),
			unique("name"),
		},
	}
}

// apiKeysTable builds the api_keys table (CONTRACT-02 T4): one row per issued
// API key. The plaintext secret is never stored — only its SHA-256 hash
// (key_hash, UNIQUE, looked up exactly by the DB on verification). role_id FK
// cascades on delete of the role. revoked_at is nullable; a non-null value
// marks the key as revoked (rejected at verification time). created_at
// defaults to now. id defaults to gen_random_uuid() on both engines.
func apiKeysTable() compat.Table {
	return compat.Table{
		Name: "api_keys",
		Columns: []compat.Column{
			idColumn(),
			textColumn("label", false),
			textColumn("key_hash", false),
			uuidColumn("role_id", false),
			nowColumn("created_at"),
			timestampColumn("revoked_at", true),
		},
		Constraints: []compat.Constraint{
			primaryKey("id"),
			unique("key_hash"),
			foreignKeyCascade("role_id", "roles", "id"),
		},
	}
}

// junctionTable builds an M:N link table with a composite PK and both FKs
// cascading on delete.
func junctionTable(name, leftCol, leftTable, rightCol, rightTable string) compat.Table {
	return compat.Table{
		Name: name,
		Columns: []compat.Column{
			uuidColumn(leftCol, false),
			uuidColumn(rightCol, false),
		},
		Constraints: []compat.Constraint{
			primaryKey(leftCol, rightCol),
			foreignKeyCascade(leftCol, leftTable, "id"),
			foreignKeyCascade(rightCol, rightTable, "id"),
		},
	}
}

// productsTable builds the products content type (CONTRACT-11 T1) through the
// same reusable ContentType helper articles uses — proving the pattern
// generalizes to a second type with own columns BEYOND title/body (price, sku).
// The unique("sku") constraint is appended after ContentType returns: ContentType
// intentionally takes only a name + own columns (its signature is fixed and is
// NOT touched here), so a table-level UNIQUE is added by extending the table's
// Constraints — the same way api_keys/users add their own uniques. A duplicate
// sku is thereby rejected at the schema level (not merely by the application),
// which is the real concurrency guarantee (an app-level check can lose a race).
func productsTable() compat.Table {
	t := ContentType("products", []compat.Column{
		textColumn("title", false),
		textColumn("body", false), // product description
		decimalColumn("price", false),
		textColumn("sku", false),
	})
	t.Constraints = append(t.Constraints, unique("sku"))
	return t
}

// termsTable builds the terms table (CONTRACT-12 T1). Unlike a content type it
// is NOT built through ContentType: a term is not content — it has no author_id
// and no created_at/updated_at/metadata trailer; it is admin-managed reference
// data (like users, created at runtime — as opposed to taxonomies, which are a
// code-fixed catalog). Columns:
//   - id: surrogate UUID PK.
//   - taxonomy_id: which taxonomy this term belongs to (FK cascade — deleting a
//     taxonomy would delete its terms; taxonomies are code-fixed so this never
//     happens in practice, but the integrity rule is declared honestly).
//   - name / slug: the human label and its url-safe handle.
//   - parent_id: optional self-reference for hierarchy (a category under another
//     category). NULLABLE, FK to terms.id with SET NULL on delete so removing a
//     parent orphans (does not delete) its children.
//
// The UNIQUE(taxonomy_id, slug) constraint makes a slug unique WITHIN a taxonomy
// but allows the SAME slug across different taxonomies (a "news" category and a
// "news" tag can coexist) — the composite is the point. It is a schema-level
// guarantee (cannot lose a concurrent race), translated to a clean 400 by the
// server, exactly like products.sku.
func termsTable() compat.Table {
	return compat.Table{
		Name: "terms",
		Columns: []compat.Column{
			idColumn(),
			uuidColumn("taxonomy_id", false),
			textColumn("name", false),
			textColumn("slug", false),
			uuidColumn("parent_id", true),
		},
		Constraints: []compat.Constraint{
			primaryKey("id"),
			unique("taxonomy_id", "slug"),
			foreignKeyCascade("taxonomy_id", "taxonomies", "id"),
			foreignKeySetNull("parent_id", "terms", "id"),
		},
	}
}

// integerColumn builds a plain INTEGER column (CONTRACT-13). Mirrors the
// textColumn/jsonColumn helper shape exactly.
func integerColumn(name string, nullable bool) compat.Column {
	return compat.Column{Name: name, Type: compat.Type{Family: compat.IntegerType}, Nullable: nullable}
}

// checkIn builds a CHECK constraint restricting a column to a fixed set of
// string values, using the same compat expression shape users.status already
// uses (Kind "in" with the column followed by the literals). Factored out here
// because CONTRACT-13 needs a second one (content_type_fields.field_type) and
// the two must be built identically.
func checkIn(column string, values []string) compat.Constraint {
	args := make([]compat.Expression, 0, len(values)+1)
	args = append(args, compat.Expression{Kind: "column", Value: column})
	for _, v := range values {
		args = append(args, compat.Expression{Kind: "string", Value: v})
	}
	return compat.Constraint{
		Kind:       compat.Check,
		Columns:    []string{column},
		Expression: &compat.Expression{Kind: "in", Args: args},
	}
}

// contentTypesTable is the registry of DYNAMIC content types (CONTRACT-13 T1):
// one row per type defined at runtime by an admin. It is a CODE table declared
// here in Build() — the registry itself is never dynamic. It is NOT built
// through ContentType(): a type DEFINITION is not content (no author, no
// publish workflow, no metadata escape column); it is admin-managed reference
// data, like terms.
//
// UNIQUE(name) is the real uniqueness guarantee for a type name. It is a
// SCHEMA-level constraint, not an application-level "does this name already
// exist?" check, precisely because the latter can lose a concurrent race
// between two admins creating the same type name simultaneously — the same
// reasoning that put UNIQUE on products.sku and UNIQUE(taxonomy_id, slug) on
// terms. The server translates the violation into a clean 400.
//
// There is deliberately no updated_at, and CONTRACT-18 RE-EXAMINED that and
// KEPT IT — with a new reason, because the old one expired.
//
// The old reason: v1 was create-only (compat has no ALTER TABLE), so a column
// that could never change would be a lie. Since CONTRACT-18 a type's fields CAN
// be edited, so that argument no longer holds.
//
// The reason it stays out anyway:
//
//   - Adding a column to a CODE table is not something EnsureSchema can do: it
//     only creates MISSING tables and never touches an existing one (that
//     restriction is deliberate and is what makes a restart safe). Adding
//     updated_at would therefore require a hand-run migration on every deployed
//     database before the new binary could be trusted — a real, irreversible
//     operational cost.
//   - Nothing consumes it. No API response, no UI surface and no export
//     decision depends on when a type was last edited; it would be a column
//     added "because it is customary".
//   - The information is not lost: the edit is an administrative action that
//     goes through a permission-gated route and is visible in the service log,
//     and the CURRENT shape of a type is always readable from the registry.
//
// If a later contract needs "when was this type last edited" (an audit trail,
// an optimistic-concurrency token for the editor), it should add it
// deliberately, WITH its migration step — not inherit it from here.
func contentTypesTable() compat.Table {
	return compat.Table{
		Name: ContentTypesTable,
		Columns: []compat.Column{
			idColumn(),
			textColumn("name", false),
			nowColumn("created_at"),
		},
		Constraints: []compat.Constraint{
			primaryKey("id"),
			unique("name"),
		},
	}
}

// contentTypeFieldsTable is the per-field half of the registry (CONTRACT-13 T1):
// one row per field of a dynamic content type.
//
//   - content_type_id: FK to content_types, ON DELETE CASCADE. Deleting a type
//     is out of scope in v1, but the integrity rule is declared honestly (and it
//     is what makes the compensating cleanup of a half-created type a single
//     DELETE on the parent row).
//   - name / field_type: the column to create and which of the five allowed
//     scalar types it uses.
//   - ordinal: the field's position, so the produced table's column order is
//     stable and reproducible (required for a byte-exact schema round-trip).
//     Named "ordinal" rather than "position" because POSITION is a reserved word
//     in PostgreSQL; compat quotes identifiers so either would compile, but a
//     non-reserved word keeps hand-written diagnostic SQL readable.
//
// UNIQUE(content_type_id, name) makes a field name unique WITHIN a type while
// allowing the same field name across different types — schema-level, so two
// concurrent writers cannot both win. UNIQUE(content_type_id, ordinal) keeps
// the ordering unambiguous. The CHECK on field_type pins the stored vocabulary
// to exactly schema.FieldTypes, so a row written outside the API can never
// smuggle in an unsupported type family.
func contentTypeFieldsTable() compat.Table {
	return compat.Table{
		Name: ContentTypeFieldsTable,
		Columns: []compat.Column{
			idColumn(),
			uuidColumn("content_type_id", false),
			textColumn("name", false),
			textColumn("field_type", false),
			integerColumn("ordinal", false),
		},
		Constraints: []compat.Constraint{
			primaryKey("id"),
			unique("content_type_id", "name"),
			unique("content_type_id", "ordinal"),
			foreignKeyCascade("content_type_id", ContentTypesTable, "id"),
			checkIn("field_type", FieldTypeNames()),
		},
	}
}

// contentTypeReferencesTable is the RELATION half of the registry (CONTRACT-27
// T1): one row per relation declared by a dynamic content type. It is the third
// registry table and it is purely ADDITIVE — see relations.go for why the target
// of a reference could not be stored on content_type_fields.
//
//   - content_type_id: the type that DECLARES the reference. ON DELETE CASCADE,
//     identical to content_type_fields.content_type_id and for the identical
//     reason: this is the registry's own parent→child link, so deleting a type
//     takes its own declarations with it in one statement. It is NOT a relation
//     between dynamic types, which is the thing this contract forbids cascading.
//   - target_type_id: the type the reference POINTS AT. ON DELETE RESTRICT, so
//     "a type that is the target of a reference cannot be deleted" is a
//     guarantee OF THE DATABASE and not only of the application check in
//     store.ReferencesTo. The application check exists to produce a legible
//     refusal BEFORE anything is touched; this one exists so that no path
//     (including a hand-written DELETE) can get around it.
//   - name: the column the reference produces on the composed table. It follows
//     the same identifier gate as a field name and may not collide with one.
//   - ordinal: the reference's position, so the produced column order is stable
//     and reproducible — the same reason content_type_fields has one.
//
// UNIQUE(content_type_id, name) and UNIQUE(content_type_id, ordinal) mirror the
// field table exactly. There is deliberately NO CHECK constraint here: the
// vocabulary of this table is uuids and identifiers, not a closed set, so
// nothing about it would ever need to be altered later — which is precisely the
// property that made a new table the right answer.
func contentTypeReferencesTable() compat.Table {
	return compat.Table{
		Name: ContentTypeReferencesTable,
		Columns: []compat.Column{
			idColumn(),
			uuidColumn("content_type_id", false),
			uuidColumn("target_type_id", false),
			textColumn("name", false),
			integerColumn("ordinal", false),
		},
		Constraints: []compat.Constraint{
			primaryKey("id"),
			unique("content_type_id", "name"),
			unique("content_type_id", "ordinal"),
			foreignKeyCascade("content_type_id", ContentTypesTable, "id"),
			foreignKeyRestrict("target_type_id", ContentTypesTable, "id"),
		},
	}
}

// contentTypeFoldsTable is the CONTRACT-34 register: one row per dynamic
// content type whose real table carries SearchFoldColumn.
//
// WHY THE PRESENCE OF A ROW IS THE FLAG, AND WHY THERE IS NO BOOLEAN COLUMN. A
// `folded` boolean would need a default for the rows that already exist, which
// is the same problem as adding a column: it cannot be applied to a deployed
// database. A row that is either there or not needs no default and no
// migration — an installation upgrading to this binary gets an EMPTY table,
// which reads as "no type is folded", which is exactly the truth about its
// tables.
//
//   - content_type_id: the type. UNIQUE, so "folded" is a set membership and not
//     a counter — inserting twice is a constraint violation, not a second yes.
//     ON DELETE CASCADE, identical to content_type_fields.content_type_id and for
//     the identical reason: this is the registry's own parent→child link, so
//     deleting a type takes its fold marker with it in the same statement.
//
// There is deliberately no `created_at` and no other column: everything else
// about the fold is derivable (which field is folded is DynamicSearchField, what
// the fold IS is FoldSearchText), and a column nothing reads is a column that
// drifts.
func contentTypeFoldsTable() compat.Table {
	return compat.Table{
		Name: ContentTypeFoldsTable,
		Columns: []compat.Column{
			idColumn(),
			uuidColumn("content_type_id", false),
		},
		Constraints: []compat.Constraint{
			primaryKey("id"),
			unique("content_type_id"),
			foreignKeyCascade("content_type_id", ContentTypesTable, "id"),
		},
	}
}

// BootstrapTable is the one-row table that records that the initial bootstrap
// (CONTRACT-22) has been performed on this installation.
//
// BootstrapMarkerID is the ONLY value its `id` column may hold. Together with
// the PRIMARY KEY on that column, the CHECK makes "at most one bootstrap, ever"
// a guarantee OF THE DATABASE: there is exactly one representable key, so a
// second INSERT — from a second run of the command, or from a run racing the
// first inside another process — violates the primary key. Nothing depends on
// the order in which the command performs its checks, and nothing depends on
// the operator remembering anything.
//
// A dedicated marker was chosen over the two alternatives that suggest
// themselves, and the choice matters:
//
//   - "refuse when a user already exists" is a fine POLICY (the command applies
//     it too, for a legible message) but it is not a GUARANTEE: two concurrent
//     transactions can both observe zero users. It is also reversible — deleting
//     the administrator would silently reopen the door.
//   - "refuse when role_permissions is non-empty" is worse: that is exactly the
//     state PRODUCTION was in for weeks (hueco 1) and it says nothing about
//     whether an identity was ever created.
//
// The table deliberately carries NO foreign key to `users`. An FK with ON DELETE
// CASCADE would delete this row along with the administrator, which is precisely
// "a path for creating administrators that survives its first use". The row is a
// permanent, standalone record of an irreversible event.
const (
	BootstrapTable    = "bootstrap"
	BootstrapMarkerID = "bootstrap"
)

// bootstrapTable builds the marker table described above. admin_user_id and
// admin_email are recorded so a refusal can say WHO holds the installation and
// WHEN it was claimed, rather than a bare "already done".
func bootstrapTable() compat.Table {
	return compat.Table{
		Name: BootstrapTable,
		Columns: []compat.Column{
			textColumn("id", false),
			uuidColumn("admin_user_id", false),
			textColumn("admin_email", false),
			timestampColumn("created_at", false),
		},
		Constraints: []compat.Constraint{
			primaryKey("id"),
			checkIn("id", []string{BootstrapMarkerID}),
		},
	}
}

// Build returns the CODE half of the canonical librarian schema (T1 core tables
// + the code-defined content types + the dynamic-type registry). Tables are
// ordered so that referenced tables (users, roles, permissions) are created
// before the tables that reference them — required for FK creation on both
// engines and for a byte-exact round-trip.
//
// IMPORTANT (CONTRACT-13): Build() is NO LONGER the canonical schema of a
// running instance. Since dynamic content types exist, the canonical schema is
// Build() PLUS the tables derived from the persisted definitions — see
// BuildWith(). Treating Build() as complete is exactly the bug this contract
// exists to remove: EnsureSchema would erase the dynamic tables from the
// __compat_schema metadata on every restart, and --dump-schema would hand
// `compat copy` a schema that silently omits those tables AND their data.
//
// CONTRACT-23 T1: Build() is BuildFor(Capabilities{}) — the code schema with
// every optional capability ENABLED, which is the shape every installation
// deployed before this contract has. It keeps its parameterless spelling on
// purpose: it is what internal/auth, the schema tests and the export fixtures
// mean, and none of them is affected by the choice.
func Build() compat.Schema {
	return BuildFor(Capabilities{})
}

// BuildFor returns the CODE half of the canonical schema for a declared set of
// capabilities. The ONLY thing caps changes today is articles.embedding (and the
// routines that read it) — see capabilities.go for why that column is what makes
// pgvector a hard requirement of an installation that may never use it.
func BuildFor(caps Capabilities) compat.Schema {
	return compat.Schema{
		Tables: []compat.Table{
			usersTable(),
			catalogTable("roles"),
			catalogTable("permissions"),
			junctionTable("role_permissions", "role_id", "roles", "permission_id", "permissions"),
			junctionTable("user_roles", "user_id", "users", "role_id", "roles"),
			apiKeysTable(),
			// T2 example content type, built through the reusable helper.
			// CONTRACT-05 T1: an embedding vector(1536) column is added as one
			// more own column via ContentType (its signature is unchanged — adding
			// a column is adding a compat.Column, not a new parameter). It is
			// nullable: an article with no embedding yet is valid.
			// CONTRACT-23 T1: the embedding column is appended only when the
			// installation declares the vector capability. This is the single
			// declaration site of the ONLY vector(N) column in the schema, so
			// omitting it here is what removes the pgvector requirement — and it
			// is why the choice belongs to the schema and not to the API layer.
			ContentType("articles", articleOwnColumns(caps)),
			// CONTRACT-11 T1: a SECOND content type, built through the SAME
			// ContentType helper, with own columns beyond title/body (a decimal
			// price and a unique sku). products has no published_at / publish
			// workflow — it is simple CRUD, unlike articles.
			productsTable(),
			// CONTRACT-12 T1: taxonomies + terms + the per-content-type junction
			// tables. Ordered so every FK target precedes its referrer (required
			// for FK creation on both engines and for a byte-exact round-trip):
			// taxonomies before terms (terms.taxonomy_id → taxonomies), terms
			// before the junctions (both reference terms), and articles/products
			// already exist above (the junctions reference them too).
			catalogTable("taxonomies"),
			termsTable(),
			junctionTable("article_terms", "article_id", "articles", "term_id", "terms"),
			junctionTable("product_terms", "product_id", "products", "term_id", "terms"),
			// CONTRACT-13 T1: the registry of DYNAMIC content types. Two plain
			// code tables (the registry is never itself dynamic), ordered parent
			// before child so the FK target exists first. Because they are part
			// of Build(), ReservedNames() reserves their names automatically —
			// a dynamic type can never be called "content_types".
			contentTypesTable(),
			contentTypeFieldsTable(),
			// CONTRACT-27 T1: the RELATION half of the registry. It goes after
			// content_type_fields so the order of every pre-existing table is
			// untouched, and after content_types because BOTH its foreign keys
			// point there. Being part of Build() it is created by EnsureSchema on
			// a fresh AND on an already-deployed database (it is a MISSING table,
			// which is the only thing EnsureSchema does), and ReservedNames()
			// needs no entry for it — a dynamic type called
			// "content_type_references" produces "cpt_content_type_references".
			contentTypeReferencesTable(),
			// CONTRACT-34 T1/T2: the register of which dynamic types carry the
			// folded search column. It goes after content_type_references for the
			// same reason that one went after content_type_fields — the position of
			// every pre-existing table is untouched — and after content_types
			// because its single foreign key points there.
			//
			// THE FACT THAT IT IS A NEW *TABLE* IS THE WHOLE MECHANISM, not an
			// implementation detail. EnsureSchema creates MISSING TABLES and never
			// alters an existing one, so a new table reaches every already-deployed
			// installation on the next restart, with no hand-run migration and no
			// destructive step — while a new COLUMN on content_types could not have
			// reached any of them. That asymmetry is what let the per-type "is this
			// type folded?" answer be persisted at all.
			contentTypeFoldsTable(),
			// CONTRACT-22 T1: the one-row bootstrap marker. It references nothing
			// and nothing references it, so its position in the list is free; it
			// goes last so the order of every pre-existing table is untouched.
			// Being part of Build() it is automatically reserved by ReservedNames()
			// — no dynamic content type can be called "bootstrap".
			bootstrapTable(),
		},
		// CONTRACT-19 T1: the views that replace internal/auth's hand-written
		// JOINs and the routines (read + single-write) it executes. They are
		// part of the canonical schema like the tables, so they travel with
		// --dump-schema / `compat copy` and are covered by the JSON round-trip.
		// CONTRACT-20 T1 appends internal/server's own views and read routines
		// (server_dual.go) to internal/auth's. They are separate functions, and
		// separately prefixed, so each contract's objects stay legible; they are
		// one schema because there is only one canonical schema.
		Views:    append(authViews(), serverViews()...),
		Routines: append(authRoutines(), serverRoutines(caps)...),
	}
}

// articleOwnColumns returns the own (non-common) columns of the articles content
// type. CONTRACT-05 declared embedding as one more own column via ContentType
// (whose signature is unchanged — adding a column is adding a compat.Column, not
// a new parameter); CONTRACT-23 makes that one column conditional. It is
// nullable either way: an article with no embedding yet is valid.
func articleOwnColumns(caps Capabilities) []compat.Column {
	own := []compat.Column{
		textColumn("title", false),
		textColumn("body", false),
		timestampColumn("published_at", true),
	}
	if caps.Vector() {
		own = append(own, vectorColumn("embedding", EmbeddingDimension, true))
	}
	return own
}
