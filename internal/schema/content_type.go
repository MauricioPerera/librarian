package schema

import "github.com/MauricioPerera/sqlite-postgres-compat/compat"

// ContentType builds a first-class content-type table following the librarian
// convention (per DEFINITION.md: content types are fixed in code, each is a
// real typed table with a JSON `metadata` escape column equivalent to
// wp_postmeta). Every content type gets, consistently and without boilerplate:
//
//   - id          uuid PK, DEFAULT gen_random_uuid()
//   - author_id   uuid NOT NULL, FK -> users(id) ON DELETE CASCADE
//   - <ownColumns> the type-specific columns supplied by the caller
//   - created_at  timestamp NOT NULL, DEFAULT CURRENT_TIMESTAMP
//   - updated_at  timestamp NOT NULL, DEFAULT CURRENT_TIMESTAMP
//   - metadata    json (nullable) — extensible fields without a migration
//
// Design decision (contract left this open): author_id is NOT NULL and its FK
// cascades on delete, so removing a user removes their content. This keeps the
// referential graph closed with a NOT NULL author (SET NULL would contradict
// NOT NULL); a future contract can revisit this if soft-delete/orphaning of
// content is required.
//
// The caller-supplied ownColumns are inserted between author_id and the
// timestamp/metadata trailer so the type-specific fields read naturally in the
// table definition. Callers must not include reserved names (id, author_id,
// created_at, updated_at, metadata); doing so produces a duplicate-column error
// from Schema.Validate, surfacing the mistake explicitly.
func ContentType(name string, ownColumns []compat.Column) compat.Table {
	columns := make([]compat.Column, 0, len(ownColumns)+5)
	columns = append(columns, idColumn(), uuidColumn("author_id", false))
	columns = append(columns, ownColumns...)
	columns = append(columns, timestampsAndMetadata()...)

	return compat.Table{
		Name:    name,
		Columns: columns,
		Constraints: []compat.Constraint{
			primaryKey("id"),
			foreignKeyCascade("author_id", "users", "id"),
		},
	}
}
