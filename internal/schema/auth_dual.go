package schema

// CONTRACT-19 T1 — the canonical VIEWS and ROUTINES that let internal/auth run
// on SQLite and PostgreSQL from the same code.
//
// Everything here is part of the canonical schema (it is returned by Build()),
// so it travels with `--dump-schema` / `compat copy` and is covered by the
// existing JSON round-trip, exactly like the tables.
//
// Two facts from compat's grammar shape this file, and both are structural, not
// stylistic:
//
//   - A read action ("select") has NO joins. Every JOIN internal/auth used to
//     write by hand lives in a VIEW here, which compat compiles to both engines.
//   - A read action DECLARES its output columns with their canonical family
//     (a view carries no output types), and its ORDER BY is declared in the
//     schema, never supplied by the caller — a column name cannot be bound as a
//     placeholder, so a caller-supplied order key would be an injection path.
//
// Every listing routine declares an ORDER BY even when it has no LIMIT/OFFSET
// (where compat would only make it mandatory). Without a total order the two
// engines are free to return the same rows in different sequence, and that is a
// divergence no test which does not compare the ORDER would ever catch.

import "github.com/MauricioPerera/sqlite-postgres-compat/compat"

// Canonical view names. Each one replaces a JOIN that used to be hand-written
// SQL inside internal/auth.
const (
	// ViewUserRoleNames exposes (user_id, role_name) — the user_roles → roles
	// junction resolved to role NAMES.
	ViewUserRoleNames = "user_role_names"
	// ViewRolePermissionNames exposes (role_id, permission_name) — the
	// role_permissions → permissions junction resolved to permission NAMES.
	ViewRolePermissionNames = "role_permission_names"
	// ViewAPIKeyRecords exposes the admin-facing shape of an api_keys row with
	// the bound role NAME resolved. It deliberately does NOT project key_hash,
	// so the hash cannot reach a read routine even by mistake. ListAPIKeys and
	// GetAPIKey are the SAME view with a different WHERE.
	ViewAPIKeyRecords = "api_key_records"
)

// Canonical routine names. The `auth_` prefix marks the consumer they belong to
// and keeps them from colliding with routines a later contract adds.
const (
	// Read routines (compat.Store.QueryRoutine).
	RoutineUserCredentialsByEmail = "auth_user_credentials_by_email"
	RoutineUserByID               = "auth_user_by_id"
	RoutineListUsers              = "auth_list_users"
	RoutineUserRoleNames          = "auth_user_role_names"
	RoutineRoleIDByName           = "auth_role_id_by_name"
	RoutinePermissionIDByName     = "auth_permission_id_by_name"
	RoutineRolePermissionNames    = "auth_role_permission_names"
	RoutineListAPIKeys            = "auth_list_api_keys"
	RoutineAPIKeyByID             = "auth_api_key_by_id"
	RoutineAPIKeyByHash           = "auth_api_key_by_hash"

	// Write routines (compat.Store.CallRoutine). Each one is a SINGLE write, so
	// the transaction CallRoutine opens on its own is the whole operation.
	RoutineInsertAPIKey       = "auth_insert_api_key"
	RoutineRevokeAPIKeyByHash = "auth_revoke_api_key_by_hash"
	RoutineRevokeAPIKeyByID   = "auth_revoke_api_key_by_id"
	RoutineUpdateUserStatus   = "auth_update_user_status"
)

// Canonical families used by the auth views/routines, named once so a column and
// the routine that reads it cannot drift apart.
var (
	authUUID      = compat.Type{Family: compat.UUIDType}
	authText      = compat.Type{Family: compat.TextType}
	authTimestamp = compat.Type{Family: compat.TimestampType}
)

// expression constructors ------------------------------------------------------

func authColumn(name string) compat.Expression {
	return compat.Expression{Kind: "column", Value: name}
}

func authParam(name string) compat.Expression {
	return compat.Expression{Kind: "parameter", Value: name}
}

func authEq(left, right compat.Expression) compat.Expression {
	return compat.Expression{Kind: "eq", Args: []compat.Expression{left, right}}
}

func authAnd(left, right compat.Expression) compat.Expression {
	return compat.Expression{Kind: "and", Args: []compat.Expression{left, right}}
}

func authIsNull(argument compat.Expression) compat.Expression {
	return compat.Expression{Kind: "is_null", Args: []compat.Expression{argument}}
}

func authProjection(expression compat.Expression, alias string) compat.Projection {
	return compat.Projection{Expression: expression, Alias: alias}
}

func authResult(name string, family compat.Type) compat.RoutineResultColumn {
	return compat.RoutineResultColumn{Name: name, Type: family}
}

func authParameter(name string, family compat.Type) compat.RoutineParameter {
	return compat.RoutineParameter{Name: name, Type: family}
}

func authAssign(column string, value compat.Expression) compat.Assignment {
	return compat.Assignment{Column: column, Value: value}
}

// views ------------------------------------------------------------------------

// authViews returns the three views that replace the four hand-written JOINs of
// internal/auth (api_keys ⋈ roles is used TWICE — listing and single-row — so it
// is one view read with two different WHEREs).
//
// Every projection is aliased. compat rejects a view that projects an unaliased
// EXPRESSION as a read source (its result name is engine-defined); a bare column
// would be fine, but aliasing all of them keeps the exposed name explicit and
// independent of the table alias used in the FROM.
func authViews() []compat.View {
	return []compat.View{
		{
			Name: ViewUserRoleNames,
			Query: compat.SelectQuery{
				Columns: []compat.Projection{
					authProjection(authColumn("ur.user_id"), "user_id"),
					authProjection(authColumn("r.name"), "role_name"),
				},
				From: compat.TableSource{Table: "user_roles", Alias: "ur"},
				Joins: []compat.Join{{
					Kind:  "inner",
					Table: compat.TableSource{Table: "roles", Alias: "r"},
					On:    authEq(authColumn("r.id"), authColumn("ur.role_id")),
				}},
			},
		},
		{
			Name: ViewRolePermissionNames,
			Query: compat.SelectQuery{
				Columns: []compat.Projection{
					authProjection(authColumn("rp.role_id"), "role_id"),
					authProjection(authColumn("p.name"), "permission_name"),
				},
				From: compat.TableSource{Table: "role_permissions", Alias: "rp"},
				Joins: []compat.Join{{
					Kind:  "inner",
					Table: compat.TableSource{Table: "permissions", Alias: "p"},
					On:    authEq(authColumn("p.id"), authColumn("rp.permission_id")),
				}},
			},
		},
		{
			Name: ViewAPIKeyRecords,
			Query: compat.SelectQuery{
				Columns: []compat.Projection{
					authProjection(authColumn("k.id"), "id"),
					authProjection(authColumn("k.label"), "label"),
					authProjection(authColumn("r.name"), "role_name"),
					authProjection(authColumn("k.created_at"), "created_at"),
					authProjection(authColumn("k.revoked_at"), "revoked_at"),
				},
				From: compat.TableSource{Table: "api_keys", Alias: "k"},
				Joins: []compat.Join{{
					Kind:  "inner",
					Table: compat.TableSource{Table: "roles", Alias: "r"},
					On:    authEq(authColumn("r.id"), authColumn("k.role_id")),
				}},
			},
		},
	}
}

// routines ---------------------------------------------------------------------

// apiKeyRecordColumns is the declared output shape of ViewAPIKeyRecords, shared
// by the listing and the single-row read so the two can never drift.
func apiKeyRecordColumns() []compat.RoutineResultColumn {
	return []compat.RoutineResultColumn{
		authResult("id", authUUID),
		authResult("label", authText),
		authResult("role_name", authText),
		authResult("created_at", authTimestamp),
		authResult("revoked_at", authTimestamp),
	}
}

// authRoutines returns every canonical routine internal/auth executes: the read
// ones through QueryRoutine and the single-write ones through CallRoutine.
//
// The three set-replacement operations of internal/auth (CreateUser,
// SetUserRoles, SetRolePermissions) are deliberately ABSENT: each is a DELETE
// followed by N INSERTs where N depends on a caller argument, committing
// together. A routine has a STATIC action list, and CallRoutine opens its own
// transaction, so neither the shape nor the atomicity can be expressed here.
// Those three compose raw SQL with compat.Placeholder inside the transaction
// they own (see internal/auth).
func authRoutines() []compat.Routine {
	return []compat.Routine{
		// --- reads ---------------------------------------------------------
		{
			Name:       RoutineUserCredentialsByEmail,
			Parameters: []compat.RoutineParameter{authParameter("email", authText)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: "users",
				Columns: []compat.RoutineResultColumn{
					authResult("id", authUUID),
					authResult("password_hash", authText),
					authResult("status", authText),
				},
				Where: ptr(authEq(authColumn("email"), authParam("email"))),
			}},
		},
		{
			Name:       RoutineUserByID,
			Parameters: []compat.RoutineParameter{authParameter("id", authUUID)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: "users",
				Columns: []compat.RoutineResultColumn{
					authResult("id", authUUID),
					authResult("email", authText),
					authResult("status", authText),
				},
				Where: ptr(authEq(authColumn("id"), authParam("id"))),
			}},
		},
		{
			Name: RoutineListUsers,
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: "users",
				Columns: []compat.RoutineResultColumn{
					authResult("id", authUUID),
					authResult("email", authText),
					authResult("status", authText),
				},
				// email is UNIQUE, so this is a TOTAL order: both engines must
				// return the same sequence, not merely the same set.
				OrderBy: []compat.RoutineOrdering{{Column: "email"}},
			}},
		},
		{
			Name:       RoutineUserRoleNames,
			Parameters: []compat.RoutineParameter{authParameter("user_id", authUUID)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: ViewUserRoleNames,
				Columns:  []compat.RoutineResultColumn{authResult("role_name", authText)},
				Where:    ptr(authEq(authColumn("user_id"), authParam("user_id"))),
				// A user cannot hold the same role twice (composite PK on
				// user_roles), so ordering by name is total.
				OrderBy: []compat.RoutineOrdering{{Column: "role_name"}},
			}},
		},
		{
			Name:       RoutineRoleIDByName,
			Parameters: []compat.RoutineParameter{authParameter("name", authText)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: "roles",
				Columns:  []compat.RoutineResultColumn{authResult("id", authUUID)},
				Where:    ptr(authEq(authColumn("name"), authParam("name"))),
			}},
		},
		{
			Name:       RoutinePermissionIDByName,
			Parameters: []compat.RoutineParameter{authParameter("name", authText)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: "permissions",
				Columns:  []compat.RoutineResultColumn{authResult("id", authUUID)},
				Where:    ptr(authEq(authColumn("name"), authParam("name"))),
			}},
		},
		{
			Name:       RoutineRolePermissionNames,
			Parameters: []compat.RoutineParameter{authParameter("role_id", authUUID)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: ViewRolePermissionNames,
				Columns:  []compat.RoutineResultColumn{authResult("permission_name", authText)},
				Where:    ptr(authEq(authColumn("role_id"), authParam("role_id"))),
				OrderBy:  []compat.RoutineOrdering{{Column: "permission_name"}},
			}},
		},
		{
			Name: RoutineListAPIKeys,
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: ViewAPIKeyRecords,
				Columns:  apiKeyRecordColumns(),
				// The exact order the hand-written SQL declared: newest first,
				// label breaking a tie. created_at is written by the application
				// in the canonical RFC3339Nano form (see auth.MintAPIKey), so the
				// text sort is chronological and IDENTICAL on both engines.
				OrderBy: []compat.RoutineOrdering{
					{Column: "created_at", Descending: true},
					{Column: "label"},
				},
			}},
		},
		{
			Name:       RoutineAPIKeyByID,
			Parameters: []compat.RoutineParameter{authParameter("id", authUUID)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: ViewAPIKeyRecords,
				Columns:  apiKeyRecordColumns(),
				Where:    ptr(authEq(authColumn("id"), authParam("id"))),
			}},
		},
		{
			Name:       RoutineAPIKeyByHash,
			Parameters: []compat.RoutineParameter{authParameter("key_hash", authText)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: "api_keys",
				Columns: []compat.RoutineResultColumn{
					authResult("id", authUUID),
					authResult("label", authText),
					authResult("role_id", authUUID),
					authResult("revoked_at", authTimestamp),
				},
				Where: ptr(authEq(authColumn("key_hash"), authParam("key_hash"))),
			}},
		},

		// --- writes --------------------------------------------------------
		{
			Name: RoutineInsertAPIKey,
			Parameters: []compat.RoutineParameter{
				authParameter("id", authUUID),
				authParameter("label", authText),
				authParameter("key_hash", authText),
				authParameter("role_id", authUUID),
				authParameter("created_at", authTimestamp),
			},
			Actions: []compat.RoutineAction{{
				Kind:  "insert",
				Table: "api_keys",
				// An insert action supplies EVERY non-generated column of the
				// table (compat's insertRow rejects a missing one rather than
				// silently relying on a default that may differ per engine), so
				// id and the NULL revoked_at are declared explicitly. The column
				// DEFAULTs (gen_random_uuid(), CURRENT_TIMESTAMP) stay in the
				// schema as a safety net for writes that do not come from the
				// application.
				Assignments: []compat.Assignment{
					authAssign("id", authParam("id")),
					authAssign("label", authParam("label")),
					authAssign("key_hash", authParam("key_hash")),
					authAssign("role_id", authParam("role_id")),
					// created_at is supplied instead of left to the column
					// DEFAULT CURRENT_TIMESTAMP: the two engines render that
					// default as different TEXT ("2024-01-01 10:00:00" on SQLite,
					// "2024-01-01 10:00:00.123456+00" on PostgreSQL), and this
					// column is the primary ORDER BY key of the key listing, so
					// the raw-text sort would diverge between engines. The
					// application writes the canonical RFC3339Nano form the
					// `timestamp` family declares. The column DEFAULT stays as a
					// safety net for writes that do not come from the app.
					authAssign("created_at", authParam("created_at")),
					// A freshly minted key is never revoked.
					authAssign("revoked_at", compat.Expression{Kind: "null"}),
				},
			}},
		},
		{
			Name: RoutineRevokeAPIKeyByHash,
			Parameters: []compat.RoutineParameter{
				authParameter("key_hash", authText),
				authParameter("revoked_at", authTimestamp),
			},
			Actions: []compat.RoutineAction{{
				Kind:        "update",
				Table:       "api_keys",
				Assignments: []compat.Assignment{authAssign("revoked_at", authParam("revoked_at"))},
				Where: ptr(authAnd(
					authEq(authColumn("key_hash"), authParam("key_hash")),
					authIsNull(authColumn("revoked_at")),
				)),
			}},
		},
		{
			Name: RoutineRevokeAPIKeyByID,
			Parameters: []compat.RoutineParameter{
				authParameter("id", authUUID),
				authParameter("revoked_at", authTimestamp),
			},
			Actions: []compat.RoutineAction{{
				Kind:        "update",
				Table:       "api_keys",
				Assignments: []compat.Assignment{authAssign("revoked_at", authParam("revoked_at"))},
				Where: ptr(authAnd(
					authEq(authColumn("id"), authParam("id")),
					authIsNull(authColumn("revoked_at")),
				)),
			}},
		},
		{
			Name: RoutineUpdateUserStatus,
			Parameters: []compat.RoutineParameter{
				authParameter("id", authUUID),
				authParameter("status", authText),
				authParameter("updated_at", authTimestamp),
			},
			Actions: []compat.RoutineAction{{
				Kind:  "update",
				Table: "users",
				Assignments: []compat.Assignment{
					authAssign("status", authParam("status")),
					// The hand-written statement used CURRENT_TIMESTAMP here. A
					// routine assignment resolves to a parameter or a literal,
					// never to an engine function, so the instant is passed in
					// (canonical RFC3339Nano, the form the `timestamp` family
					// declares — TEXT on both engines).
					authAssign("updated_at", authParam("updated_at")),
				},
				Where: ptr(authEq(authColumn("id"), authParam("id"))),
			}},
		},
	}
}

// ptr returns a pointer to v. compat's optional AST fields (Where, Limit,
// Offset) are pointers; this keeps the declarations above readable as literals.
func ptr[T any](v T) *T { return &v }
