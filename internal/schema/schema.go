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
	Permissions = []string{
		"content.create",
		"content.update",
		"content.publish",
		"content.delete",
		"users.manage",
		"roles.manage",
		"terms.manage",
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

// Build returns the complete canonical librarian schema (T1 core tables + the
// example content type from T2). Tables are ordered so that referenced tables
// (users, roles, permissions) are created before the tables that reference them
// — required for FK creation on both engines and for a byte-exact round-trip.
func Build() compat.Schema {
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
			ContentType("articles", []compat.Column{
				textColumn("title", false),
				textColumn("body", false),
				timestampColumn("published_at", true),
				vectorColumn("embedding", EmbeddingDimension, true),
			}),
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
		},
	}
}
