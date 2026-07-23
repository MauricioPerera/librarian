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
	// Permissions is the fixed permission catalog.
	Permissions = []string{
		"content.create",
		"content.publish",
		"content.delete",
		"users.manage",
		"roles.manage",
	}
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
			// T2 example content type, built through the reusable helper.
			ContentType("articles", []compat.Column{
				textColumn("title", false),
				textColumn("body", false),
				timestampColumn("published_at", true),
			}),
		},
	}
}
