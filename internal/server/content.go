package server

// content.go implements CONTRACT-14: a GENERIC JSON CRUD surface over ANY
// dynamic content type, driven entirely by its persisted definition — there is
// no Go file per type, and adding a type through CONTRACT-13 immediately makes
// these five routes work for it.
//
//	GET    /content/{type}        list  (valid identity only)
//	POST   /content/{type}        create (content.create)
//	GET    /content/{type}/{id}   detail (valid identity only)
//	PUT    /content/{type}/{id}   update (content.update)
//	DELETE /content/{type}/{id}   delete (content.delete)
//
// THE SECURITY MODEL, in one paragraph, because it is the whole point of this
// file: a SQL IDENTIFIER (a table or column name) cannot be bound with a `?`
// placeholder — it has to be interpolated. A VALUE always can, and therefore
// always is. So this file keeps the two on strictly separate paths:
//
//   - Every identifier that reaches a query comes from a definition READ BACK
//     FROM THE DATABASE (store.FetchContentType), never from the request. The
//     {type} path segment is only ever used as a BOUND comparison value to find
//     that definition; if no definition matches, the request is a 404 and no
//     query against a dynamic table is ever built. Field names come from the
//     definition's Fields, never from the request body — a body key is matched
//     AGAINST the definition and then discarded. Both are additionally re-run
//     through schema.ValidateIdentifier at interpolation time (quoteIdentifier
//     below) as a fail-closed guard: persisted rows were validated on the way in
//     and again on the way out by LoadContentTypeDefinitions, so a failure here
//     would be an internal bug, not a client error.
//   - Every value — including a value that looks exactly like SQL — is passed as
//     a `?` parameter. `";DROP TABLE users;--"` stored in a text field is stored
//     and returned verbatim, as data.
//
// RUNTIME TYPING (the technical core of the contract). The column set of a
// dynamic table is known only at runtime, so both directions are driven by the
// definition's []FieldDefinition:
//
//   - READ: the SELECT list is built from the definition, and the scan targets
//     are a []any of *any (the driver's natural Go value). jsonValue() then
//     converts each raw driver value to the JSON type its FieldType demands, so
//     an integer comes back as a JSON number and a boolean as a JSON boolean —
//     not everything as a string. The row is assembled as a map[string]any and
//     marshalled by encoding/json, which is what makes one generic code path
//     able to produce a differently-shaped object per type.
//   - WRITE: bindValue() validates each body value AGAINST its declared
//     FieldType and returns the Go value to bind. A JSON type that does not
//     match the declared FieldType is a 400 with a clear message — never a 500,
//     and never a corrupt stored value.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
)

// commonColumns are the columns ContentType() injects into EVERY content table,
// dynamic or code-defined. They are compile-time constants of this file, never
// derived from a request, so they are interpolated literally and safely.
//
// They are ALSO the reason a body key can never overwrite them: every one of
// them is in schema.ReservedNames(), so no dynamic FIELD can be called `id`,
// `author_id`, `created_at`, `updated_at` or `metadata` (CONTRACT-13 rejects
// such a definition with a 400). A body carrying one of those keys therefore
// matches no field of the type and is rejected as an unknown field — the same
// 400 as any other typo. There is no code path in which a request-supplied
// value reaches one of these columns: `id` and the timestamps are set by column
// defaults, `author_id` is taken from the authenticated identity, and
// `metadata` is not writable through this surface at all.
const (
	colID        = "id"
	colAuthorID  = "author_id"
	colCreatedAt = "created_at"
	colUpdatedAt = "updated_at"
	colMetadata  = "metadata"
)

// quoteIdentifier is the ONLY place a dynamic name becomes SQL text. It re-runs
// the CONTRACT-13 T1 gate before quoting, so an identifier can only be
// interpolated if it still satisfies `[a-z][a-z0-9_]*` (which contains no
// quote, no semicolon, no space and no unicode). The double quotes are then
// belt-and-braces, not the protection: the alphabet alone makes escaping moot.
func quoteIdentifier(name string) (string, error) {
	if err := schema.ValidateIdentifier(name); err != nil {
		return "", fmt.Errorf("refusing to build a query with identifier %q: %w", name, err)
	}
	return `"` + name + `"`, nil
}

// resolveType turns the {type} path segment into a PERSISTED definition. The
// segment is used only as a bound comparison value inside FetchContentType; an
// unknown, malformed or hostile name simply matches no definition and produces
// a 404 — with no query against any dynamic table ever being built.
func (h *handlers) resolveType(w http.ResponseWriter, r *http.Request) (schema.ContentTypeDefinition, bool) {
	def, err := store.FetchContentType(r.Context(), h.db, r.PathValue("type"))
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		writeError(w, http.StatusNotFound, "content type not found")
		return schema.ContentTypeDefinition{}, false
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not read content type")
		return schema.ContentTypeDefinition{}, false
	}
	return def, true
}

// selectClause builds the fully-qualified SELECT list and the quoted table name
// for a definition: the common columns plus one column per declared field, in
// declaration order, so the scan targets line up positionally.
func selectClause(def schema.ContentTypeDefinition) (table string, columns []string, err error) {
	table, err = quoteIdentifier(def.Name)
	if err != nil {
		return "", nil, err
	}
	columns = []string{colID, colAuthorID, colCreatedAt, colUpdatedAt, colMetadata}
	for _, f := range def.Fields {
		quoted, err := quoteIdentifier(f.Name)
		if err != nil {
			return "", nil, err
		}
		columns = append(columns, quoted)
	}
	return table, columns, nil
}

// scanRow scans ONE row of a dynamic table whose shape is known only at
// runtime, and returns it as a JSON-ready object.
//
// The five common columns have fixed Go types. The dynamic tail is scanned into
// a []any of *any — the driver hands back whatever it natively stores (int64
// for INTEGER, string for TEXT, nil for NULL) — and jsonValue() then maps each
// one to the JSON type its declared FieldType requires.
func scanRow(s interface{ Scan(dest ...any) error }, def schema.ContentTypeDefinition) (map[string]any, error) {
	var (
		id, authorID, createdAt, updatedAt string
		metadata                           sql.NullString
	)
	raw := make([]any, len(def.Fields))
	dest := make([]any, 0, 5+len(def.Fields))
	dest = append(dest, &id, &authorID, &createdAt, &updatedAt, &metadata)
	for i := range raw {
		dest = append(dest, &raw[i])
	}
	if err := s.Scan(dest...); err != nil {
		return nil, err
	}

	row := map[string]any{
		colID:        id,
		colAuthorID:  authorID,
		colCreatedAt: createdAt,
		colUpdatedAt: updatedAt,
		colMetadata:  nil,
	}
	// metadata is the JSON escape column (TEXT on both engines). It is not
	// writable through this surface; when a row has one (written by another
	// path) it is surfaced as raw JSON, never as a re-encoded string.
	if metadata.Valid && strings.TrimSpace(metadata.String) != "" && json.Valid([]byte(metadata.String)) {
		row[colMetadata] = json.RawMessage(metadata.String)
	}
	for i, f := range def.Fields {
		v, err := jsonValue(f, raw[i])
		if err != nil {
			return nil, err
		}
		row[f.Name] = v
	}
	return row, nil
}

// jsonValue converts one raw driver value into the JSON value its declared
// FieldType demands. NULL stays JSON null for every type (every dynamic column
// is nullable by construction — see schema.DynamicTable).
//
// Per-type mapping and why:
//   - text/date  → JSON string. A date is stored as canonical `YYYY-MM-DD` TEXT
//     on both engines (compat maps DateType to TEXT deliberately), so the exact
//     stored text is returned, with no timezone folding.
//   - integer    → JSON number (int64). SQLite stores it as INTEGER.
//   - boolean    → JSON true/false. SQLite stores it as INTEGER 0/1, so the
//     conversion happens here rather than leaking 0/1 to the client.
//   - decimal    → JSON STRING, deliberately. compat maps DecimalType to TEXT on
//     SQLite precisely because IEEE-754 cannot hold an arbitrary-precision
//     decimal; emitting a JSON number would re-introduce the rounding the
//     storage decision exists to avoid. This is the SAME choice products.price
//     already makes (CONTRACT-11), so the two surfaces agree. Writing accepts
//     both a JSON number and a JSON string.
func jsonValue(f schema.FieldDefinition, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch f.Type {
	case schema.FieldText, schema.FieldDate, schema.FieldDecimal:
		return driverText(v)
	case schema.FieldInteger:
		return driverInt(v)
	case schema.FieldBoolean:
		n, err := driverInt(v)
		if err != nil {
			if b, ok := v.(bool); ok {
				return b, nil
			}
			return nil, err
		}
		return n != 0, nil
	default:
		return nil, fmt.Errorf("field %q has unknown type %q", f.Name, string(f.Type))
	}
}

// driverText normalises the driver's text representations ([]byte or string).
func driverText(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	default:
		return "", fmt.Errorf("unexpected stored value of type %T for a text column", v)
	}
}

// driverInt normalises the driver's integer representations. float64 and text
// are accepted defensively (a different driver or a value written outside this
// surface) so a read can never turn into a 500 for a well-formed row.
func driverInt(v any) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case float64:
		return int64(t), nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	case []byte:
		return strconv.ParseInt(strings.TrimSpace(string(t)), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected stored value of type %T for an integer column", v)
	}
}

// errBadField is the sentinel for "the client's body is wrong" — always a 400,
// never a 500. Its message is safe to return verbatim: it names the offending
// FIELD and the expected JSON type, and never echoes SQL.
type errBadField struct{ msg string }

func (e errBadField) Error() string { return e.msg }

// bindValues validates a decoded request body against the definition and
// returns, in declaration order, the value to bind for every field of the type.
//
// DESIGN DECISION — MISSING FIELDS ARE NULL, on BOTH create and update.
// CONTRACT-13's definition model has no notion of "required" (a field is just a
// name + a type), and schema.DynamicTable makes every dynamic column NULLABLE
// for exactly that reason. Inventing a required-ness rule here would be a
// second, invisible source of truth that the definition API cannot express and
// the admin never agreed to. So: a field absent from the body, or explicitly
// null, is stored as NULL.
//
// The rule is identical for POST and PUT, which makes PUT a FULL REPLACEMENT of
// the row's own fields — the same semantics articles and products already have
// (their PUT sets every own column from the body, so an omitted one is reset).
// A partial update would be PATCH; there is no PATCH in this project.
//
// An unknown key is a 400 rather than being ignored: silently dropping data the
// client believed it was storing is the worse failure. This is also what stops
// a body from touching a common column — `id`, `author_id`, `created_at`,
// `updated_at` and `metadata` are reserved names, so no field can be called
// that, so such a key matches nothing and is rejected as unknown.
func bindValues(def schema.ContentTypeDefinition, body map[string]json.RawMessage) ([]any, error) {
	known := make(map[string]struct{}, len(def.Fields))
	for _, f := range def.Fields {
		known[f.Name] = struct{}{}
	}
	for key := range body {
		if _, ok := known[key]; !ok {
			return nil, errBadField{fmt.Sprintf("unknown field %q for content type %q", key, def.Name)}
		}
	}
	values := make([]any, 0, len(def.Fields))
	for _, f := range def.Fields {
		v, err := bindValue(f, body[f.Name])
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, nil
}

// bindValue validates ONE body value against its declared FieldType and returns
// the Go value to bind as a `?` parameter. Every rejection is an errBadField
// (→ 400 with a clear message), so a wrong JSON type can never become a 500 nor
// a corrupt stored value.
func bindValue(f schema.FieldDefinition, raw json.RawMessage) (any, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, nil
	}
	switch f.Type {
	case schema.FieldText:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, errBadField{fmt.Sprintf("field %q must be a JSON string (declared type text)", f.Name)}
		}
		return v, nil

	case schema.FieldInteger:
		// A JSON string, a fractional value or exponent notation is rejected: the
		// column is an integer and a silent truncation would be a corrupt store.
		if s[0] == '"' || strings.ContainsAny(s, ".eE") {
			return nil, errBadField{fmt.Sprintf("field %q must be a whole JSON number (declared type integer)", f.Name)}
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, errBadField{fmt.Sprintf("field %q must be a whole JSON number (declared type integer)", f.Name)}
		}
		return n, nil

	case schema.FieldDecimal:
		// Accepts a JSON number (9.99) or a JSON string ("9.99") and stores the
		// canonical decimal TEXT, never a float — same helper, same guarantee as
		// products.price (CONTRACT-11).
		text, ok := canonicalDecimal(raw)
		if !ok {
			return nil, errBadField{fmt.Sprintf("field %q must be a decimal number, as a JSON number or string (declared type decimal)", f.Name)}
		}
		return text, nil

	case schema.FieldBoolean:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, errBadField{fmt.Sprintf("field %q must be a JSON boolean (declared type boolean)", f.Name)}
		}
		return v, nil

	case schema.FieldDate:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, errBadField{fmt.Sprintf("field %q must be a JSON string in YYYY-MM-DD form (declared type date)", f.Name)}
		}
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return nil, errBadField{fmt.Sprintf("field %q must be a date in YYYY-MM-DD form (declared type date)", f.Name)}
		}
		return v, nil

	default:
		// Unreachable: a persisted definition is re-validated on load.
		return nil, errBadField{fmt.Sprintf("field %q has unsupported type %q", f.Name, string(f.Type))}
	}
}

// decodeContentBody decodes the request body as a flat JSON object of raw
// values. Anything that is not a JSON object (an array, a scalar, garbage) is a
// 400 — the same treatment articles/products give a malformed body.
func decodeContentBody(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	if body == nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	return body, true
}

// --- Handlers ----------------------------------------------------------------

// handleListContent lists rows of a dynamic type with the same simple
// ?limit=&offset= paging articles/products use. Requires only a valid identity.
// A type with zero rows returns an empty ARRAY, never null and never a 404.
func (h *handlers) handleListContent(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveType(w, r)
	if !ok {
		return
	}
	rows, err := h.listContentRows(r.Context(), def, queryIntDefault(r, "limit", 20), queryIntDefault(r, "offset", 0))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list content")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"type": def.Name, "items": rows})
}

// handleGetContent returns one row by id. Requires only a valid identity. A
// missing OR malformed id is a 404 — never a 500 (the id is a bound parameter,
// so a non-UUID simply matches nothing).
func (h *handlers) handleGetContent(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveType(w, r)
	if !ok {
		return
	}
	row, found, err := h.fetchContentRow(r.Context(), def, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read content")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "content not found")
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// handleCreateContent creates a row (content.create). The author is the
// authenticated user; an API-key identity is rejected with 403 rather than
// inserting a NULL author — author_id is NOT NULL with an FK to users, exactly
// as for articles/products.
func (h *handlers) handleCreateContent(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if id.Kind != "jwt" {
		writeError(w, http.StatusForbidden, "creating content requires a user identity (API keys have no author)")
		return
	}
	def, ok := h.resolveType(w, r)
	if !ok {
		return
	}
	body, ok := decodeContentBody(w, r)
	if !ok {
		return
	}
	values, err := bindValues(def, body)
	if err != nil {
		writeBindError(w, err)
		return
	}
	newID, err := h.insertContentRow(r.Context(), def, id.UserID, values)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create content")
		return
	}
	row, found, err := h.fetchContentRow(r.Context(), def, newID)
	if err != nil || !found {
		writeError(w, http.StatusInternalServerError, "could not create content")
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

// handleUpdateContent replaces the own fields of one row (content.update). An
// unknown id → 404; an unknown field or a wrong JSON type → 400.
func (h *handlers) handleUpdateContent(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveType(w, r)
	if !ok {
		return
	}
	body, ok := decodeContentBody(w, r)
	if !ok {
		return
	}
	values, err := bindValues(def, body)
	if err != nil {
		writeBindError(w, err)
		return
	}
	n, err := h.updateContentRow(r.Context(), def, r.PathValue("id"), values)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update content")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "content not found")
		return
	}
	row, found, err := h.fetchContentRow(r.Context(), def, r.PathValue("id"))
	if err != nil || !found {
		writeError(w, http.StatusInternalServerError, "could not update content")
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// handleDeleteContent deletes one row (content.delete). 404 when the id does
// not exist; 204 on success.
func (h *handlers) handleDeleteContent(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveType(w, r)
	if !ok {
		return
	}
	n, err := h.deleteContentRow(r.Context(), def, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete content")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "content not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeBindError maps a body-validation failure to 400 with its own message,
// and anything else (which would mean a persisted definition stopped validating
// — an internal bug, not a client error) to 500 with a generic message.
func writeBindError(w http.ResponseWriter, err error) {
	var bad errBadField
	if errors.As(err, &bad) {
		writeError(w, http.StatusBadRequest, bad.msg)
		return
	}
	writeError(w, http.StatusInternalServerError, "could not process content")
}

// --- Data access -------------------------------------------------------------

// listContentRows returns a page of rows ordered by created_at DESC.
func (h *handlers) listContentRows(ctx context.Context, def schema.ContentTypeDefinition, limit, offset int) ([]map[string]any, error) {
	table, columns, err := selectClause(def)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + strings.Join(columns, ", ") + ` FROM ` + table + ` ORDER BY ` + colCreatedAt + ` DESC LIMIT ? OFFSET ?`
	rows, err := h.db.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Never nil: an empty type must serialise as [], not null.
	out := make([]map[string]any, 0)
	for rows.Next() {
		row, err := scanRow(rows, def)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// fetchContentRow loads one row by id. found=false for a missing OR malformed
// id (it is a bound parameter, so it just matches nothing); err is non-nil only
// on a real database failure.
func (h *handlers) fetchContentRow(ctx context.Context, def schema.ContentTypeDefinition, id string) (map[string]any, bool, error) {
	table, columns, err := selectClause(def)
	if err != nil {
		return nil, false, err
	}
	query := `SELECT ` + strings.Join(columns, ", ") + ` FROM ` + table + ` WHERE ` + colID + ` = ?`
	row, err := scanRow(h.db.QueryRowContext(ctx, query, id), def)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return row, true, nil
}

// insertContentRow inserts a row with the given author and per-field values (in
// declaration order) and returns the generated id. A type with zero fields
// inserts the author alone — still a valid row.
func (h *handlers) insertContentRow(ctx context.Context, def schema.ContentTypeDefinition, authorID string, values []any) (string, error) {
	table, err := quoteIdentifier(def.Name)
	if err != nil {
		return "", err
	}
	columns := []string{colAuthorID}
	placeholders := []string{"?"}
	args := []any{authorID}
	for i, f := range def.Fields {
		quoted, err := quoteIdentifier(f.Name)
		if err != nil {
			return "", err
		}
		columns = append(columns, quoted)
		placeholders = append(placeholders, "?")
		args = append(args, values[i])
	}
	query := `INSERT INTO ` + table + ` (` + strings.Join(columns, ", ") + `) VALUES (` + strings.Join(placeholders, ", ") + `) RETURNING ` + colID
	var id string
	if err := h.db.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// updateContentRow replaces every own field of one row and bumps updated_at.
// It returns RowsAffected (0 ⇒ 404). A type with zero fields still touches
// updated_at, so the 0/1 answer stays meaningful.
func (h *handlers) updateContentRow(ctx context.Context, def schema.ContentTypeDefinition, id string, values []any) (int64, error) {
	table, err := quoteIdentifier(def.Name)
	if err != nil {
		return 0, err
	}
	assignments := make([]string, 0, len(def.Fields)+1)
	args := make([]any, 0, len(def.Fields)+1)
	for i, f := range def.Fields {
		quoted, err := quoteIdentifier(f.Name)
		if err != nil {
			return 0, err
		}
		assignments = append(assignments, quoted+" = ?")
		args = append(args, values[i])
	}
	assignments = append(assignments, colUpdatedAt+" = CURRENT_TIMESTAMP")
	args = append(args, id)
	query := `UPDATE ` + table + ` SET ` + strings.Join(assignments, ", ") + ` WHERE ` + colID + ` = ?`
	res, err := h.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// deleteContentRow deletes one row and returns RowsAffected (0 ⇒ 404).
func (h *handlers) deleteContentRow(ctx context.Context, def schema.ContentTypeDefinition, id string) (int64, error) {
	table, err := quoteIdentifier(def.Name)
	if err != nil {
		return 0, err
	}
	res, err := h.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+colID+` = ?`, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
