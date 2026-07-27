package schema

// CONTRACT-20 T1 — the canonical VIEWS and ROUTINES that let internal/server run
// on SQLite and PostgreSQL from the same code. It continues CONTRACT-19
// (internal/auth) with the same shape and the same rules; see auth_dual.go for
// the two structural facts of compat's grammar that drive both files (a read
// action has no joins — the JOIN goes in a view; a read action DECLARES its
// output columns with their canonical family).
//
// Everything here is part of the canonical schema (returned by Build()), so it
// travels with `--dump-schema` / `compat copy` and is covered by the existing
// JSON round-trip, exactly like the tables and the CONTRACT-19 objects.
//
// WHY THESE ARE READS AND (almost) NOTHING ELSE. CONTRACT-20 divides
// internal/server's 45 statements by a single mechanical criterion:
//
//   - A READ goes through a routine. That is not a stylistic preference: a
//     routine canonicalizes every scanned value by its DECLARED family, which is
//     the only thing that makes the three physically-divergent families read
//     identically on both engines — boolean (INTEGER vs BOOLEAN), decimal (TEXT
//     vs NUMERIC) and vector (TEXT carrier vs native pgvector). Raw SQL hands the
//     consumer whatever the driver decided, which is exactly the engine-tied
//     behavior this contract exists to delete.
//   - A WRITE stays raw SQL composed with compat.Placeholder, because every write
//     in internal/server either needs RowsAffected to decide 404-vs-200 (which
//     CallRoutine cannot report — it returns only error) or runs inside a
//     transaction the consumer owns (which CallRoutine cannot join — it opens its
//     own). Raw SQL with Placeholder and standard statements IS dual-engine; it
//     is a legitimate answer, not a leftover.
//
// EVERY listing routine declares a TOTAL ORDER — one whose key set is unique —
// even where compat would only require an ORDER BY for a paginated read. A
// partial order lets the two engines return the same rows in a different
// sequence whenever there is a tie, and that is the divergence no test which
// does not compare the ORDER will ever catch.

import (
	"fmt"

	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// Canonical view names introduced by CONTRACT-20. Each replaces a JOIN that used
// to be hand-written SQL inside internal/server.
const (
	// ViewRoleNamePermissionNames exposes (role_name, permission_name) — the
	// role_permissions junction resolved to BOTH names. authz.go's JWT branch
	// filters it by a variable-size list of role NAMES, which is why it needs a
	// different view from CONTRACT-19's ViewRolePermissionNames (keyed by
	// role_id, which the API-key branch uses and which is reused here as-is).
	ViewRoleNamePermissionNames = "role_name_permission_names"
	// ViewTermRecords exposes the public shape of a terms row with its taxonomy
	// NAME resolved. Listing terms and reading one by id are the SAME view with a
	// different WHERE.
	ViewTermRecords = "term_records"
	// ViewArticleAssignedTerms / ViewProductAssignedTerms expose the terms
	// assigned to one piece of content, through its junction table. They are two
	// views rather than one parameterized query because a read action's relation
	// is a declared name, never a caller-supplied one — which is precisely the
	// guarantee that made the old `FROM `+junction+`` interpolation acceptable
	// only under a hand-audited "these are internal constants" comment.
	ViewArticleAssignedTerms = "article_assigned_terms"
	ViewProductAssignedTerms = "product_assigned_terms"
)

// Canonical routine names. The `server_` prefix marks the consumer they belong
// to and keeps them from colliding with CONTRACT-19's `auth_` routines.
const (
	// authz.go
	RoutinePermissionsByRoleID = "server_permissions_by_role_id"

	// ui_apikeys.go / ui_roles.go / ui_users.go
	RoutineRoleIDByNameServer = "server_role_id_by_name"
	RoutineRoleNameByID       = "server_role_name_by_id"
	RoutineListRoles          = "server_list_roles"

	// terms.go
	RoutineListTerms           = "server_list_terms"
	RoutineTermByID            = "server_term_by_id"
	RoutineArticleAssignedTerm = "server_article_assigned_terms"
	RoutineProductAssignedTerm = "server_product_assigned_terms"

	// products.go
	RoutineListProducts  = "server_list_products"
	RoutineProductByID   = "server_product_by_id"
	RoutineProductExists = "server_product_exists"

	// articles.go
	RoutineListArticles         = "server_list_articles"
	RoutineArticleByID          = "server_article_by_id"
	RoutineArticleExists        = "server_article_exists"
	RoutineArticlePublishedAt   = "server_article_published_at"
	RoutineArticleEmbeddingOnly = "server_article_embedding"
)

// Canonical families used by the server views/routines, named once so a column
// and the routine that reads it cannot drift apart.
var (
	serverUUID      = compat.Type{Family: compat.UUIDType}
	serverText      = compat.Type{Family: compat.TextType}
	serverJSON      = compat.Type{Family: compat.JSONType}
	serverTimestamp = compat.Type{Family: compat.TimestampType}
	serverDecimal   = compat.Type{Family: compat.DecimalType}
	serverInteger   = compat.Type{Family: compat.IntegerType}
	serverVector    = compat.Type{Family: compat.VectorType, Arguments: []int{EmbeddingDimension}}
)

// ordering constructors ---------------------------------------------------------

// ptrExpr takes the address of a composed expression, so a routine WHERE (which
// is an optional *Expression) can be written inline like every other field.
func ptrExpr(e compat.Expression) *compat.Expression {
	return &e
}

func serverOrder(column string) compat.RoutineOrdering {
	return compat.RoutineOrdering{Column: column}
}

func serverOrderDesc(column string) compat.RoutineOrdering {
	return compat.RoutineOrdering{Column: column, Descending: true}
}

// serverReadByID builds the extremely common shape "read these columns of this
// relation where id = :id". Written once so eight routines cannot drift.
func serverReadByID(name, relation, keyColumn, parameter string, columns []compat.RoutineResultColumn) compat.Routine {
	return compat.Routine{
		Name:       name,
		Parameters: []compat.RoutineParameter{authParameter(parameter, serverUUID)},
		Actions: []compat.RoutineAction{{
			Kind:     "select",
			Relation: relation,
			Columns:  columns,
			Where:    ptrExpr(authEq(authColumn(keyColumn), authParam(parameter))),
		}},
	}
}

// serverListPaged builds a paginated listing: the caller supplies limit and
// offset as VALUES (a bound placeholder each), never the order, which is
// declared here and is total.
func serverListPaged(name, relation string, columns []compat.RoutineResultColumn, order []compat.RoutineOrdering) compat.Routine {
	return compat.Routine{
		Name: name,
		Parameters: []compat.RoutineParameter{
			authParameter("page_limit", serverInteger),
			authParameter("page_offset", serverInteger),
		},
		Actions: []compat.RoutineAction{{
			Kind:     "select",
			Relation: relation,
			Columns:  columns,
			OrderBy:  order,
			Limit:    &compat.Expression{Kind: "parameter", Value: "page_limit"},
			Offset:   &compat.Expression{Kind: "parameter", Value: "page_offset"},
		}},
	}
}

// views --------------------------------------------------------------------------

// serverViews returns the four views that replace internal/server's
// hand-written JOINs: one in authz.go (the JWT permission resolution) and three
// in terms.go (listing/reading a term, and the assigned terms of an article and
// of a product).
//
// Every projection is aliased, for the same reason CONTRACT-19 aliases all of
// its own: compat rejects a view projecting an unaliased EXPRESSION as a read
// source, and aliasing uniformly keeps the exposed name independent of the table
// alias used in the FROM.
func serverViews() []compat.View {
	assigned := func(name, junction, idColumn, contentColumn string) compat.View {
		return compat.View{
			Name: name,
			Query: compat.SelectQuery{
				Columns: []compat.Projection{
					authProjection(authColumn("j."+idColumn), contentColumn),
					authProjection(authColumn("t.id"), "term_id"),
					authProjection(authColumn("t.name"), "term_name"),
					authProjection(authColumn("t.slug"), "term_slug"),
					authProjection(authColumn("x.name"), "taxonomy"),
				},
				From: compat.TableSource{Table: junction, Alias: "j"},
				Joins: []compat.Join{
					{
						Kind:  "inner",
						Table: compat.TableSource{Table: "terms", Alias: "t"},
						On:    authEq(authColumn("t.id"), authColumn("j.term_id")),
					},
					{
						Kind:  "inner",
						Table: compat.TableSource{Table: "taxonomies", Alias: "x"},
						On:    authEq(authColumn("x.id"), authColumn("t.taxonomy_id")),
					},
				},
			},
		}
	}
	return []compat.View{
		{
			Name: ViewRoleNamePermissionNames,
			Query: compat.SelectQuery{
				Columns: []compat.Projection{
					authProjection(authColumn("r.name"), "role_name"),
					authProjection(authColumn("p.name"), "permission_name"),
				},
				From: compat.TableSource{Table: "role_permissions", Alias: "rp"},
				Joins: []compat.Join{
					{
						Kind:  "inner",
						Table: compat.TableSource{Table: "roles", Alias: "r"},
						On:    authEq(authColumn("r.id"), authColumn("rp.role_id")),
					},
					{
						Kind:  "inner",
						Table: compat.TableSource{Table: "permissions", Alias: "p"},
						On:    authEq(authColumn("p.id"), authColumn("rp.permission_id")),
					},
				},
			},
		},
		{
			Name: ViewTermRecords,
			Query: compat.SelectQuery{
				Columns: []compat.Projection{
					authProjection(authColumn("t.id"), "id"),
					authProjection(authColumn("x.name"), "taxonomy"),
					authProjection(authColumn("t.name"), "name"),
					authProjection(authColumn("t.slug"), "slug"),
					authProjection(authColumn("t.parent_id"), "parent_id"),
				},
				From: compat.TableSource{Table: "terms", Alias: "t"},
				Joins: []compat.Join{{
					Kind:  "inner",
					Table: compat.TableSource{Table: "taxonomies", Alias: "x"},
					On:    authEq(authColumn("x.id"), authColumn("t.taxonomy_id")),
				}},
			},
		},
		assigned(ViewArticleAssignedTerms, "article_terms", "article_id", "article_id"),
		assigned(ViewProductAssignedTerms, "product_terms", "product_id", "product_id"),
	}
}

// routines -------------------------------------------------------------------------

// termRecordColumns is the declared output of the term_records view.
func termRecordColumns() []compat.RoutineResultColumn {
	return []compat.RoutineResultColumn{
		authResult("id", serverUUID),
		authResult("taxonomy", serverText),
		authResult("name", serverText),
		authResult("slug", serverText),
		authResult("parent_id", serverUUID),
	}
}

// assignedTermColumns is the declared output of the two *_assigned_terms views.
func assignedTermColumns(contentColumn string) []compat.RoutineResultColumn {
	return []compat.RoutineResultColumn{
		authResult(contentColumn, serverUUID),
		authResult("term_id", serverUUID),
		authResult("term_name", serverText),
		authResult("term_slug", serverText),
		authResult("taxonomy", serverText),
	}
}

// articleColumns is the declared output of an articles read. embedding is
// declared as vector(EmbeddingDimension), which is what makes the value arrive
// canonicalized ('[c1,c2,...]', no spaces) regardless of whether it came out of
// SQLite's TEXT carrier or PostgreSQL's native pgvector column.
//
// CONTRACT-23 T1: it is declared only when the installation declares the vector
// capability. A routine's result columns are what compat COMPILES into the
// SELECT list, so declaring a column the table does not have is not a cosmetic
// mismatch — it is a statement that fails on every read.
func articleColumns(caps Capabilities) []compat.RoutineResultColumn {
	columns := []compat.RoutineResultColumn{
		authResult("id", serverUUID),
		authResult("author_id", serverUUID),
		authResult("title", serverText),
		authResult("body", serverText),
		authResult("published_at", serverTimestamp),
	}
	if caps.Vector() {
		columns = append(columns, authResult("embedding", serverVector))
	}
	return append(columns,
		authResult("created_at", serverTimestamp),
		authResult("updated_at", serverTimestamp),
	)
}

// productColumns is the declared output of a products read. price is declared
// decimal, which is what makes it arrive as the same canonical text from
// SQLite's TEXT column and from PostgreSQL's NUMERIC one.
func productColumns() []compat.RoutineResultColumn {
	return []compat.RoutineResultColumn{
		authResult("id", serverUUID),
		authResult("author_id", serverUUID),
		authResult("title", serverText),
		authResult("body", serverText),
		authResult("price", serverDecimal),
		authResult("sku", serverText),
		authResult("created_at", serverTimestamp),
		authResult("updated_at", serverTimestamp),
	}
}

// serverRoutines returns every read routine internal/server executes over the
// CODE-defined part of the schema. The routines over DYNAMIC content types are
// generated per type in dynamicContentRoutines (dynamic.go's BuildWith), because
// their column set is only known at runtime.
func serverRoutines(caps Capabilities) []compat.Routine {
	routines := []compat.Routine{
		// --- authz.go -------------------------------------------------------
		// The API-key branch: one role id, its permission names. The JWT branch
		// filters by a VARIABLE-size list of role names and therefore cannot be a
		// routine (a routine WHERE has no IN with caller-supplied arity); it uses
		// the ViewRoleNamePermissionNames view with raw SQL, so the JOIN still
		// lives in the schema and only the IN list is composed.
		{
			Name:       RoutinePermissionsByRoleID,
			Parameters: []compat.RoutineParameter{authParameter("role_id", serverUUID)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: ViewRolePermissionNames,
				Columns:  []compat.RoutineResultColumn{authResult("permission_name", serverText)},
				Where:    ptrExpr(authEq(authColumn("role_id"), authParam("role_id"))),
				OrderBy:  []compat.RoutineOrdering{serverOrder("permission_name")},
			}},
		},

		// --- ui_apikeys.go / ui_roles.go / ui_users.go ----------------------
		{
			Name:       RoutineRoleIDByNameServer,
			Parameters: []compat.RoutineParameter{authParameter("role_name", serverText)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: "roles",
				Columns:  []compat.RoutineResultColumn{authResult("id", serverUUID)},
				Where:    ptrExpr(authEq(authColumn("name"), authParam("role_name"))),
			}},
		},
		serverReadByID(RoutineRoleNameByID, "roles", "id", "role_id",
			[]compat.RoutineResultColumn{authResult("name", serverText)}),
		{
			Name: RoutineListRoles,
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: "roles",
				Columns: []compat.RoutineResultColumn{
					authResult("id", serverUUID),
					authResult("name", serverText),
				},
				// name is UNIQUE on roles, so this order is TOTAL.
				OrderBy: []compat.RoutineOrdering{serverOrder("name")},
			}},
		},

		// --- terms.go -------------------------------------------------------
		{
			Name: RoutineListTerms,
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: ViewTermRecords,
				Columns:  termRecordColumns(),
				// (taxonomy, name) was the ORIGINAL order and is NOT total: two
				// terms with the same name in the same taxonomy tie, and each
				// engine breaks that tie its own way. slug is UNIQUE within a
				// taxonomy, so appending it makes the order total without ever
				// changing the sequence of a set that had no tie.
				OrderBy: []compat.RoutineOrdering{
					serverOrder("taxonomy"), serverOrder("name"), serverOrder("slug"),
				},
			}},
		},
		serverReadByID(RoutineTermByID, ViewTermRecords, "id", "term_id", termRecordColumns()),
		{
			Name:       RoutineArticleAssignedTerm,
			Parameters: []compat.RoutineParameter{authParameter("content_id", serverUUID)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: ViewArticleAssignedTerms,
				Columns:  assignedTermColumns("article_id"),
				Where:    ptrExpr(authEq(authColumn("article_id"), authParam("content_id"))),
				OrderBy: []compat.RoutineOrdering{
					serverOrder("taxonomy"), serverOrder("term_name"), serverOrder("term_slug"),
				},
			}},
		},
		{
			Name:       RoutineProductAssignedTerm,
			Parameters: []compat.RoutineParameter{authParameter("content_id", serverUUID)},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: ViewProductAssignedTerms,
				Columns:  assignedTermColumns("product_id"),
				Where:    ptrExpr(authEq(authColumn("product_id"), authParam("content_id"))),
				OrderBy: []compat.RoutineOrdering{
					serverOrder("taxonomy"), serverOrder("term_name"), serverOrder("term_slug"),
				},
			}},
		},

		// --- products.go ----------------------------------------------------
		// created_at DESC alone is NOT total (two products written in the same
		// instant tie); id is the primary key, so it settles every tie.
		serverListPaged(RoutineListProducts, "products", productColumns(),
			[]compat.RoutineOrdering{serverOrderDesc("created_at"), serverOrder("id")}),
		serverReadByID(RoutineProductByID, "products", "id", "product_id", productColumns()),
		serverReadByID(RoutineProductExists, "products", "id", "product_id",
			[]compat.RoutineResultColumn{authResult("id", serverUUID)}),

		// --- articles.go ----------------------------------------------------
		serverListPaged(RoutineListArticles, "articles", articleColumns(caps),
			[]compat.RoutineOrdering{serverOrderDesc("created_at"), serverOrder("id")}),
		serverReadByID(RoutineArticleByID, "articles", "id", "article_id", articleColumns(caps)),
		// A dedicated existence probe rather than reusing the by-id read: the
		// full read drags a 1536-component vector across the wire, and this
		// runs before every update/publish.
		serverReadByID(RoutineArticleExists, "articles", "id", "article_id",
			[]compat.RoutineResultColumn{authResult("id", serverUUID)}),
		serverReadByID(RoutineArticlePublishedAt, "articles", "id", "article_id",
			[]compat.RoutineResultColumn{authResult("published_at", serverTimestamp)}),
	}
	// CONTRACT-23 T1: the embedding-only read exists ONLY as a read of the
	// vector column, so an installation without the capability has nothing for it
	// to select. It is appended rather than declared inline so the order of every
	// pre-existing routine is untouched when the capability is on.
	if caps.Vector() {
		routines = append(routines,
			serverReadByID(RoutineArticleEmbeddingOnly, "articles", "id", "article_id",
				[]compat.RoutineResultColumn{authResult("embedding", serverVector)}))
	}
	return routines
}

// --- dynamic content types -------------------------------------------------------

// DynamicListRoutine / DynamicReadRoutine are the canonical names of the two
// read routines generated for ONE dynamic content type. They are derived from
// the REAL table name (already carrying DynamicTablePrefix and already validated
// by ValidateIdentifier through DynamicTable), so two types can never collide
// and no caller-supplied text ever reaches a routine name.
func DynamicListRoutine(def ContentTypeDefinition) string {
	return "content_list_" + def.TableName()
}

// DynamicReadRoutine names the by-id read routine of one dynamic content type.
func DynamicReadRoutine(def ContentTypeDefinition) string {
	return "content_read_" + def.TableName()
}

// dynamicContentColumns declares the output of a dynamic content read: the five
// common columns ContentType() injects, then one per declared field in
// declaration order, each with the canonical family of its FieldType.
//
// THIS is the decision CONTRACT-20 left open, and the reason it went this way.
// The alternative was to keep content.go's reads as raw SQL. Raw SQL returns
// whatever the driver decided, and content.go's scanner had to guess: its
// driverInt accepts int64, int, bool, float64, string AND []byte for one
// integer column, and its boolean branch falls back from int to bool. Those
// fallbacks are not defensive programming, they ARE the per-engine branching
// this contract exists to remove — SQLite hands back int64 for a BOOLEAN column
// (it stores INTEGER) and PostgreSQL hands back a Go bool (it stores BOOLEAN),
// and the same split exists for decimal (TEXT vs NUMERIC). Declaring the family
// moves that knowledge into the schema, where it is stated once and checked, and
// the value arrives already canonical on both engines. Generating two routines
// per type is cheap and mechanical; the guessing was neither.
func dynamicContentColumns(def ContentTypeDefinition) ([]compat.RoutineResultColumn, error) {
	columns := []compat.RoutineResultColumn{
		authResult("id", serverUUID),
		authResult("author_id", serverUUID),
		authResult("created_at", serverTimestamp),
		authResult("updated_at", serverTimestamp),
		authResult("metadata", serverJSON),
	}
	for _, f := range def.Fields {
		family, ok := f.Type.family()
		if !ok { // unreachable after Validate; a hard fail-closed guard.
			return nil, fmt.Errorf("field %q has unknown type %q", f.Name, string(f.Type))
		}
		columns = append(columns, authResult(f.Name, compat.Type{Family: family}))
	}
	// CONTRACT-27: the relation columns, in the SAME order DynamicTable puts them
	// in the table (fields first, then references). A routine's result columns are
	// what compat compiles into the SELECT list, so omitting them would make the
	// value unreadable through the generic CRUD layer even though the column
	// exists — the mirror of the CONTRACT-23 note above.
	for _, r := range def.References {
		columns = append(columns, authResult(r.Name, serverUUID))
	}
	return columns, nil
}

// DynamicSearchRoutine names the label-search routine of one dynamic content
// type (CONTRACT-32). It is declared ONLY for a type DynamicSearchField accepts;
// asking for it on any other type is a "routine not found" from QueryRoutine,
// which is why every caller must gate on DynamicSearchField first.
func DynamicSearchRoutine(def ContentTypeDefinition) string {
	return "content_search_" + def.TableName()
}

// DynamicSearchField answers WHICH column a search over this type filters on,
// and whether searching it is possible at all.
//
// IT IS THE FIRST DECLARED FIELD, and that is not a coincidence: it is exactly
// the field referenceOptionLabel builds the readable label from (see
// internal/server/ui_content.go). Filtering on anything else would mean the
// panel searches one thing and shows another, so typing what you can READ in an
// option would fail to find it.
//
// IT MUST BE OF TYPE text, and that is not squeamishness either. The filter
// compiles to instr (SQLite) / strpos (PostgreSQL), and strpos REFUSES a
// non-text argument while instr happily coerces one and answers something.
// Declaring the routine for such a type would therefore produce a routine that
// works on one engine and fails on the other: the precise divergence this
// project forbids. (Before CONTRACT-33 the filter was `like`, and the same
// restriction held for a different reason — PostgreSQL has no LIKE operator for
// integer: `ERROR: operator does not exist: integer ~~ unknown`. compat v0.7.0
// now type-checks the `contains` operands itself, so this gate agrees with a
// rule the compiler also enforces instead of being the only thing standing.)
// A type whose first field is not text (or that declares no field at all, which
// is legal) simply gets no search routine, and the panel says so instead of
// offering a control that cannot work.
func DynamicSearchField(def ContentTypeDefinition) (FieldDefinition, bool) {
	if len(def.Fields) == 0 || def.Fields[0].Type != FieldText {
		return FieldDefinition{}, false
	}
	return def.Fields[0], true
}

// dynamicContentRoutines generates the read routines of ONE dynamic content
// type: list, read-by-id and — when DynamicSearchField accepts it — search.
// All of them are reads; every write on a dynamic table needs RowsAffected to
// answer 404 vs 200, so writes stay raw SQL composed with compat.Placeholder.
func dynamicContentRoutines(def ContentTypeDefinition) ([]compat.Routine, error) {
	columns, err := dynamicContentColumns(def)
	if err != nil {
		return nil, err
	}
	table := def.TableName()
	routines := []compat.Routine{
		serverListPaged(DynamicListRoutine(def), table, columns,
			[]compat.RoutineOrdering{serverOrderDesc("created_at"), serverOrder("id")}),
		serverReadByID(DynamicReadRoutine(def), table, "id", "row_id", columns),
	}
	// CONTRACT-32 T2 — THE FILTER IS PUSHED TO THE DATABASE, on purpose. The
	// alternative the contract weighed (compat.Store.SearchText) matches in Go and
	// is therefore identical on both engines by construction, but it reads the
	// WHOLE table on every keystroke — undoing the bound CONTRACT-31 just
	// established. This routine reads a bounded page instead, so the cost of a
	// search does not grow with the target table.
	//
	// CONTRACT-33 — THE OPERATOR IS `contains`, NOT `like`, AND THAT IS FORCED.
	// compat v0.7.0 REFUSES `like` in a routine WHERE: it compiled to LIKE on
	// SQLite and ILIKE on PostgreSQL, whose case folding genuinely differs, so the
	// same pattern selected different rows per engine. `contains` compiles to
	// instr/strpos: a plain substring test, CASE-SENSITIVE, with NO wildcards and
	// no escape character, identical on both engines by construction.
	//
	// The parameter is therefore a NEEDLE, not a pattern: the caller binds the
	// text as typed, with no %…% around it and no metacharacter substitution.
	// Anything it did to a pattern before is now not just unnecessary but wrong —
	// a `%` here is a percent sign.
	//
	// The cost of the swap is stated where it is paid, not here: the search became
	// case-sensitive, because the database can no longer fold and folding in Go
	// would mean reading rows the WHERE already discarded. See
	// internal/server/ui_content.go and docs/PENDIENTES.md. This declaration only
	// says WHICH column is filtered, WITH what parameter, in WHICH order, and that
	// the order is total so the page is the same page on both engines.
	//
	// CONTRACT-34 — WHICH COLUMN IS FILTERED NOW DEPENDS ON THE TYPE, and that is
	// the entire fix for the case-sensitivity regression above. For a FOLDED type
	// the WHERE runs over SearchFoldColumn, which the application keeps as the
	// lowercased copy of the search field, and the caller binds the needle through
	// the SAME FoldSearchText. Both sides are already folded, so the engine keeps
	// doing the ONE thing both engines do identically — a plain exact substring
	// test — and the fold never happens in the database, which is what keeps this
	// portable. For an UNFOLDED type (every type created before this contract:
	// EnsureSchema never alters an existing table) nothing changes and the search
	// stays case-sensitive; the panel SAYS which of the two modes applies.
	if field, ok := DynamicSearchField(def); ok {
		searchColumn := field.Name
		if def.Folded {
			searchColumn = SearchFoldColumn
		}
		routines = append(routines, compat.Routine{
			Name: DynamicSearchRoutine(def),
			Parameters: []compat.RoutineParameter{
				authParameter("search_text", serverText),
				authParameter("page_limit", serverInteger),
				authParameter("page_offset", serverInteger),
			},
			Actions: []compat.RoutineAction{{
				Kind:     "select",
				Relation: table,
				Columns:  columns,
				Where: ptrExpr(compat.Expression{Kind: "contains", Args: []compat.Expression{
					authColumn(searchColumn), authParam("search_text"),
				}}),
				// The SAME total order the list routine declares, so "the N most
				// recent that match" is the same notion of recent the unfiltered
				// list uses, and a tie can never select different rows per engine.
				OrderBy: []compat.RoutineOrdering{serverOrderDesc("created_at"), serverOrder("id")},
				Limit:   &compat.Expression{Kind: "parameter", Value: "page_limit"},
				Offset:  &compat.Expression{Kind: "parameter", Value: "page_offset"},
			}},
		})
	}
	return routines, nil
}
