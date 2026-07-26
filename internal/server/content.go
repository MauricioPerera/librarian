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
//   - READ (rewritten by CONTRACT-20): the read goes through the two routines
//     schema.BuildWith generates for the type, whose output columns are DECLARED
//     with the canonical family of each FieldType. compat therefore canonicalizes
//     every value before this package sees it, and rowJSON() maps that single
//     representation to the JSON type the FieldType demands — an integer comes
//     back as a JSON number and a boolean as a JSON boolean, not everything as a
//     string. The row is assembled as a map[string]any and marshalled by
//     encoding/json, which is what makes one generic code path able to produce a
//     differently-shaped object per type.
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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
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

// quoteIdentifier is the ONLY place a dynamic name becomes SQL text in this
// package. Its body moved to schema.QuoteIdentifier in CONTRACT-18 (the table
// rebuild in internal/store needs the SAME function to build its copy
// statements, and store cannot import server), so there is still exactly one
// implementation, one gate and one error message; this stays as the local name
// every call site in this file already uses.
func quoteIdentifier(name string) (string, error) {
	return schema.QuoteIdentifier(name)
}

// quoteTable is the ONLY way this file names a dynamic TABLE in SQL
// (CONTRACT-17). It goes through def.TableName(), i.e. through
// schema.DynamicTableName, so the public name (`eventos`, used in the URL) and
// the real table name (`cpt_eventos`, used in the query) can never be confused
// — the two symmetric bugs being "the query hits the bare name and finds
// nothing" and "the prefixed name leaks into a response".
//
// The prefixed name is re-validated by quoteIdentifier exactly like any other
// identifier: schema.MaxTypeNameLength guarantees it still fits the identifier
// budget, so a legal type name always yields a legal table name.
func quoteTable(def schema.ContentTypeDefinition) (string, error) {
	return quoteIdentifier(def.TableName())
}

// resolveType turns the {type} path segment into a PERSISTED definition. The
// segment is used only as a bound comparison value inside FetchContentType; an
// unknown, malformed or hostile name simply matches no definition and produces
// a 404 — with no query against any dynamic table ever being built.
func (h *handlers) resolveType(w http.ResponseWriter, r *http.Request) (schema.ContentTypeDefinition, bool) {
	def, err := store.FetchContentType(r.Context(), h.store, r.PathValue("type"))
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		writeError(w, http.StatusNotFound, "content type not found")
		return schema.ContentTypeDefinition{}, false
	case err != nil:
		h.writeOperationFailure(w, r, err, "could not read content type")
		return schema.ContentTypeDefinition{}, false
	}
	return def, true
}

// rowJSON turns ONE canonicalized routine row of a dynamic table into a
// JSON-ready object.
//
// THIS FUNCTION IS THE POINT OF THE content.go DECISION IN CONTRACT-20, so the
// reasoning lives here. It replaces scanRow + jsonValue + driverText +
// driverInt: four functions whose whole job was to GUESS what the driver had
// handed back. driverInt accepted int64, int, bool, float64, string and []byte
// for a single integer column; the boolean branch tried an int and fell back to
// a Go bool. Those were not defensive niceties — they were the per-engine
// branching this contract exists to delete, because the answer genuinely differs:
// SQLite stores a BOOLEAN as INTEGER and returns int64, PostgreSQL stores it as
// BOOLEAN and returns bool; SQLite stores a DECIMAL as TEXT and PostgreSQL as
// NUMERIC. Reading through a routine that DECLARES the family (the two routines
// generated per type in schema.BuildWith) makes compat canonicalize the value
// before this package ever sees it, so there is one representation to map and
// the mapping is total.
//
// The per-type JSON mapping is unchanged from jsonValue, and so is every
// response byte:
//   - text/date  → JSON string. A date is canonical `YYYY-MM-DD` TEXT on both
//     engines, returned verbatim with no timezone folding.
//   - integer    → JSON number.
//   - boolean    → JSON true/false, never 0/1.
//   - decimal    → JSON STRING, deliberately: emitting a number would
//     re-introduce the IEEE-754 rounding the TEXT/NUMERIC storage choice exists
//     to avoid. Same choice as products.price (CONTRACT-11).
//   - NULL       → JSON null for every type (every dynamic column is nullable by
//     construction — see schema.DynamicTable). NULL-ness is read from the
//     canonical KIND, never from an empty string.
func rowJSON(row compat.Row, def schema.ContentTypeDefinition) (map[string]any, error) {
	out := map[string]any{
		colID:        dual.RowText(row, colID),
		colAuthorID:  dual.RowText(row, colAuthorID),
		colCreatedAt: dual.RowText(row, colCreatedAt),
		colUpdatedAt: dual.RowText(row, colUpdatedAt),
		colMetadata:  nil,
	}
	// metadata is the JSON escape column. It is not writable through this
	// surface; when a row has one (written by another path) it is surfaced as
	// raw JSON, never as a re-encoded string.
	if !dual.RowIsNull(row, colMetadata) {
		if text := strings.TrimSpace(dual.RowText(row, colMetadata)); text != "" {
			out[colMetadata] = json.RawMessage(text)
		}
	}
	for _, f := range def.Fields {
		if dual.RowIsNull(row, f.Name) {
			out[f.Name] = nil
			continue
		}
		text := dual.RowText(row, f.Name)
		switch f.Type {
		case schema.FieldText, schema.FieldDate, schema.FieldDecimal:
			out[f.Name] = text
		case schema.FieldInteger:
			n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", f.Name, err)
			}
			out[f.Name] = n
		case schema.FieldBoolean:
			// The canonical text of the boolean family is exactly "true"/"false"
			// on both engines — that IS the canonicalization compat performs.
			out[f.Name] = text == "true"
		default:
			return nil, fmt.Errorf("field %q has unknown type %q", f.Name, string(f.Type))
		}
	}
	// CONTRACT-27: a relation is a uuid column, surfaced as a JSON string (the id
	// of the referenced row) or null. It reads exactly like `author_id` — which is
	// the point: a reference is structurally the same thing as the author FK every
	// content table has already had since CONTRACT-01.
	for _, r := range def.References {
		if dual.RowIsNull(row, r.Name) {
			out[r.Name] = nil
			continue
		}
		out[r.Name] = dual.RowText(row, r.Name)
	}
	return out, nil
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
//
// CONTRACT-27: the RELATIONS are bound after the fields, in the same order
// schema.DynamicTable puts their columns in the table, so one slice drives the
// whole write. The rule above is unchanged for them: a relation absent from the
// body, or explicitly null, is stored as NULL — the column is nullable by
// construction, which is what makes a relation optional.
func bindValues(def schema.ContentTypeDefinition, body map[string]json.RawMessage) ([]any, error) {
	known := make(map[string]struct{}, len(def.Fields)+len(def.References))
	for _, f := range def.Fields {
		known[f.Name] = struct{}{}
	}
	for _, r := range def.References {
		known[r.Name] = struct{}{}
	}
	for key := range body {
		if _, ok := known[key]; !ok {
			return nil, errBadField{fmt.Sprintf("unknown field %q for content type %q", key, def.Name)}
		}
	}
	values := make([]any, 0, len(def.Fields)+len(def.References))
	for _, f := range def.Fields {
		v, err := bindValue(f, body[f.Name])
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	for _, r := range def.References {
		v, err := bindReference(r, body[r.Name])
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, nil
}

// uuidPattern is the canonical 8-4-4-4-12 hexadecimal form, lowercase only.
//
// LOWERCASE ONLY, for the same reason schema.identifierPattern is: PostgreSQL's
// `uuid` type normalises `AB…` and `ab…` to the same value, SQLite stores the
// TEXT verbatim and compares it byte for byte. Accepting both cases would make
// the SAME request find a row on one engine and not on the other — the
// dual-engine divergence this project exists to prevent. Every id this project
// ever emits is lowercase (dual.NewUUID), so nothing legitimate is rejected.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// bindReference validates ONE body value for a declared relation and returns the
// value to bind. Like bindValue, every rejection is an errBadField (→ 400).
//
// THE FORMAT CHECK IS NOT COSMETIC. The column is `uuid` on PostgreSQL, so a
// value that is not a uuid makes the driver fail with `invalid input syntax for
// type uuid` — a 500 for what is plainly a bad request — while SQLite (where the
// carrier is TEXT) would happily store the garbage and simply never match
// anything. Rejecting the shape here is what makes the two engines answer the
// same 400 to the same request.
func bindReference(r schema.ReferenceDefinition, raw json.RawMessage) (any, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, errBadField{fmt.Sprintf("reference %q must be a JSON string holding the id of a %q row, or null", r.Name, r.Target)}
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if !uuidPattern.MatchString(v) {
		return nil, errBadField{fmt.Sprintf("reference %q must be the id of a %q row: a lowercase uuid in 8-4-4-4-12 form", r.Name, r.Target)}
	}
	return v, nil
}

// checkReferenceTargets verifies that every non-null relation value names a row
// that actually exists in the target type's table, and turns a miss into a 400.
//
// WHY IT EXISTS WHEN THE FOREIGN KEY ALREADY ENFORCES THIS. The FK is the real
// guarantee and it is not being replaced: it is what makes the constraint hold
// against every writer, including one that is not this process. What this check
// adds is the MESSAGE. It knows which relation was bad and which id it named, so
// it can answer *"reference \"autor\" points at \"autores\", and no \"autores\"
// row has the id X"* — a sentence a person can act on.
//
// CONTRACT-28 UPDATE — THE RESIDUAL WINDOW IS NOW CAUGHT, AND THIS CHECK STAYS.
// The window itself is unchanged and cannot be removed: the referenced row can
// still be deleted between this SELECT and the INSERT that follows. What changed
// is what happens in it. compat v0.5.0 added Store.IsForeignKeyViolation, so the
// driver's rejection is no longer indistinguishable from any other failed
// statement, and the write sites classify it into the SAME 400 this check would
// have produced (see foreignKeyRaceOnWrite). The two are a division of labour,
// not a duplication:
//
//   - THIS CHECK is the normal path and owns the good message, because it is the
//     only one of the two that knows WHICH reference failed and WHICH id missed.
//   - THE CLASSIFIER is the net under the race. All it knows is "a foreign key
//     rejected this write" — by compat's explicit documentation it cannot even
//     say which DIRECTION of the violation happened — so its message is
//     necessarily generic. That is the price of catching an interleaving, and it
//     is only ever paid in the interleaving.
//
// Deleting this check to keep only the classifier would therefore trade every
// ordinary bad-reference message for the generic one. Deleting the classifier
// would put the 500 back. Both stay.
func (h *handlers) checkReferenceTargets(ctx context.Context, def schema.ContentTypeDefinition, values []any) error {
	offset := len(def.Fields)
	for i, r := range def.References {
		value := values[offset+i]
		if value == nil {
			continue
		}
		id, ok := value.(string)
		if !ok { // unreachable: bindReference returns a string or nil.
			return errBadField{fmt.Sprintf("reference %q has an unexpected value", r.Name)}
		}
		table, err := quoteIdentifier(schema.DynamicTableName(r.Target))
		if err != nil {
			return err
		}
		var found string
		err = h.db.QueryRowContext(ctx,
			`SELECT `+quote(colID)+` FROM `+table+` WHERE `+quote(colID)+` = `+dual.Bind(h.engine(), 1), id).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return errBadField{fmt.Sprintf("reference %q points at %q, and no %q row has the id %q", r.Name, r.Target, r.Target, id)}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// foreignKeyRaceOnWrite and foreignKeyRaceOnDelete are CONTRACT-28: the net that
// turns the residual race window of the two pre-checks into the status code the
// pre-check would have produced, instead of a 500.
//
// THE DIRECTION COMES FROM THE CALL SITE, NEVER FROM THE ERROR. compat's
// IsForeignKeyViolation is documented as unable to tell the two directions apart
// — both engines report the identical code for "this row points at a parent that
// does not exist" and for "a child still points at the row you are deleting" —
// and the only way to separate them from the error itself would be to read the
// message text, which is exactly what classifying by structured code exists to
// avoid. It does not need to: the statement that failed is known here. An INSERT
// or an UPDATE of a content row can only fail this way because a reference it
// carries names a row that is gone; a DELETE of a content row can only fail this
// way because something still references the row. So there are two functions,
// one per statement, and neither inspects the message.
//
// The message is deliberately GENERIC, and that is not a regression: at this
// point the specific row is genuinely unknown. The pre-check ran and passed, so
// whatever it would have named was still there when it looked. Anything more
// precise would be an invention.
//
// A nil error, or an error that is not a foreign-key rejection, returns nil — the
// caller then falls through to its ordinary failure handling, so an unrelated
// database failure keeps its 500 (and, through writeOperationFailure, its 503
// when the pool is down).
//
// SCOPE, stated because it is a real consequence: a content table's author_id is
// also a foreign key (to users). An INSERT whose author was deleted between the
// login and the write is likewise a reference to something that no longer
// exists, so it lands on the same 400 with the same sentence. That is the honest
// answer for it too — the request named a user that is gone — and no code path
// here reads the error to guess otherwise.
func (h *handlers) foreignKeyRaceOnWrite(def schema.ContentTypeDefinition, err error) error {
	if err == nil || !h.store.IsForeignKeyViolation(err) {
		return nil
	}
	return errBadField{fmt.Sprintf(
		"a reference of this %q row points at a row that no longer exists: it was deleted after the reference was checked and before the write. Nothing was stored — re-read the target and retry",
		def.Name)}
}

// KNOWN GAP, ON SQLITE ONLY, for foreignKeyRaceOnDelete. It catches the race on
// PostgreSQL (SQLSTATE 23503) but NOT on SQLite, and the cause is structural
// rather than a bug here: CONTRACT-27 declares every relation ON DELETE RESTRICT
// (schema.foreignKeyRestrict argues why), and SQLite implements the parent-side
// refusal of a RESTRICT through its internal foreign-key trigger program, so it
// reports SQLITE_CONSTRAINT_TRIGGER (1811) instead of SQLITE_CONSTRAINT_FOREIGNKEY
// (787). compat v0.5.0's IsForeignKeyViolation accepts only 787, deliberately and
// with 1811 named in its documentation as a different rejection that must not be
// folded in — so the predicate is false and this one interleaving keeps its 500
// on SQLite. Measured with identical DDL: NO ACTION → 787 (classified), RESTRICT
// → 1811 (not classified); the child-side INSERT is 787 either way, which is why
// the create and update races DO close on both engines.
//
// It is left open rather than worked around, because every workaround available
// inside librarian is forbidden by this contract: reading the error text,
// re-implementing an engine-specific code table here, or reversing CONTRACT-27's
// RESTRICT decision. The dual-engine battery asserts the current behaviour
// exactly, so widening the predicate in compat makes that test fail and this
// paragraph get deleted instead of rotting. Nothing is corrupted meanwhile — the
// foreign key still refuses the delete and the row survives.
func (h *handlers) foreignKeyRaceOnDelete(def schema.ContentTypeDefinition, err error) error {
	if err == nil || !h.store.IsForeignKeyViolation(err) {
		return nil
	}
	return errBadField{fmt.Sprintf(
		"this %q row cannot be deleted: something started referencing it after the check and before the delete. This project never emits ON DELETE CASCADE, so nothing was destroyed — re-read who references it, clear or delete those rows first",
		def.Name)}
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
		h.writeOperationFailure(w, r, err, "could not list content")
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
		h.writeOperationFailure(w, r, err, "could not read content")
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
	// CONTRACT-27: a relation pointing at a row that does not exist is a 400 with
	// a sentence, not a foreign-key error from the driver. See
	// checkReferenceTargets.
	if err := h.checkReferenceTargets(r.Context(), def, values); err != nil {
		writeBindError(w, err)
		return
	}
	h.openReferenceRaceWindow(r.Context(), "create")
	newID, err := h.insertContentRow(r.Context(), def, id.UserID, values)
	if err != nil {
		// CONTRACT-28: the check above passed, so a foreign-key rejection HERE can
		// only mean the target vanished in between. Same 400, generic message.
		if raced := h.foreignKeyRaceOnWrite(def, err); raced != nil {
			writeBindError(w, raced)
			return
		}
		h.writeOperationFailure(w, r, err, "could not create content")
		return
	}
	row, found, err := h.fetchContentRow(r.Context(), def, newID)
	if err != nil || !found {
		h.writeOperationFailure(w, r, err, "could not create content")
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
	if err := h.checkReferenceTargets(r.Context(), def, values); err != nil {
		writeBindError(w, err)
		return
	}
	h.openReferenceRaceWindow(r.Context(), "update")
	n, err := h.updateContentRow(r.Context(), def, r.PathValue("id"), values)
	if err != nil {
		// CONTRACT-28: same net as the create path — an UPDATE that moves a
		// reference onto a row deleted in between is a 400, not a 500.
		if raced := h.foreignKeyRaceOnWrite(def, err); raced != nil {
			writeBindError(w, raced)
			return
		}
		h.writeOperationFailure(w, r, err, "could not update content")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "content not found")
		return
	}
	row, found, err := h.fetchContentRow(r.Context(), def, r.PathValue("id"))
	if err != nil || !found {
		h.writeOperationFailure(w, r, err, "could not update content")
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
	// CONTRACT-27: deleting a REFERENCED row is refused by the foreign key, which
	// is the point — a reference is never cascaded away. Asking first is what
	// turns that into a 400 NAMING who points at the row and how many, instead of
	// a 500 built out of a driver message.
	//
	// CONTRACT-28: and if a referring row appears after the count, the foreign key
	// still refuses the DELETE — that rejection is now classified into the same
	// 400 rather than a 500. Mirror image of the create path, and the same
	// division of labour: the count owns the actionable message, the classifier is
	// the net under the interleaving. See foreignKeyRaceOnDelete.
	if err := h.checkNoIncomingReferences(r.Context(), def, r.PathValue("id")); err != nil {
		writeBindError(w, err)
		return
	}
	h.openReferenceRaceWindow(r.Context(), "delete")
	n, err := h.deleteContentRow(r.Context(), def, r.PathValue("id"))
	if err != nil {
		if raced := h.foreignKeyRaceOnDelete(def, err); raced != nil {
			writeBindError(w, raced)
			return
		}
		h.writeOperationFailure(w, r, err, "could not delete content")
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

// checkNoIncomingReferences refuses the deletion of a row that other rows point
// at, naming who points at it and how many.
//
// It asks the REGISTRY which types reference this one (store.ReferencesTo) and
// then counts, in each of those types' real tables, the rows whose foreign-key
// column holds this id. Both the table name and the column name come from the
// registry — never from the request — and go through the same quoteIdentifier
// gate as every other interpolated identifier in this file; the id is bound.
//
// The residual window is the mirror of checkReferenceTargets': a referring row
// created between the count and the DELETE still hits the real foreign key, so
// the deletion is refused either way and nothing is lost.
//
// CONTRACT-28 UPDATE: that interleaving no longer degrades to a 500 either. The
// DELETE site classifies the driver's rejection with compat v0.5.0's
// Store.IsForeignKeyViolation and answers the same 400 — with a generic message,
// because at that point the count has already passed and the referrer that
// appeared afterwards has not been identified. This function is what still
// produces the sentence naming WHO references the row and HOW MANY, which is why
// it remains the normal path rather than being replaced by the classifier.
func (h *handlers) checkNoIncomingReferences(ctx context.Context, def schema.ContentTypeDefinition, id string) error {
	if id == "" || !uuidPattern.MatchString(id) {
		// A malformed id matches no row, so the DELETE is a 404 and there is
		// nothing to protect. Returning here also keeps a non-uuid out of a
		// PostgreSQL `uuid` comparison, which would be a driver error.
		return nil
	}
	referrers, err := store.ReferencesTo(ctx, h.store, def.Name)
	if err != nil {
		return err
	}
	for _, ref := range referrers {
		table, err := quoteIdentifier(schema.DynamicTableName(ref.TypeName))
		if err != nil {
			return err
		}
		column, err := quoteIdentifier(ref.ReferenceName)
		if err != nil {
			return err
		}
		var count int64
		if err := h.db.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE `+column+` = `+dual.Bind(h.engine(), 1), id).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return errBadField{fmt.Sprintf(
				"this %q row cannot be deleted: %d %q row(s) reference it through %q. This project never emits ON DELETE CASCADE, so those rows are not destroyed silently — clear or delete them first",
				def.Name, count, ref.TypeName, ref.ReferenceName)}
		}
	}
	return nil
}

// ownColumnNames is THE order of a dynamic type's own columns: its scalar fields
// in declaration order, then its relations in declaration order (CONTRACT-27).
//
// It exists so that the ONE order is spelled once and shared by everything that
// zips a definition against a value slice — bindValues, checkReferenceTargets,
// insertContentRow and updateContentRow. It matches, deliberately and by
// construction, the order schema.DynamicTable emits the physical columns in and
// the order schema.dynamicContentColumns declares the routines' output in. A
// second, independent spelling of that order is exactly how a value would end up
// written into the wrong column.
func ownColumnNames(def schema.ContentTypeDefinition) []string {
	out := make([]string, 0, len(def.Fields)+len(def.References))
	for _, f := range def.Fields {
		out = append(out, f.Name)
	}
	for _, r := range def.References {
		out = append(out, r.Name)
	}
	return out
}

// --- Data access -------------------------------------------------------------

// dynamicSchema composes the canonical schema that DECLARES this type's two read
// routines. It is a pure function of the persisted definition — schema.BuildWith
// generates the table and, since CONTRACT-20, its `content_list_*` /
// `content_read_*` routines in the same place — so it costs no database round
// trip. Only this one type is composed, not every type in the instance: a read
// of `eventos` needs `eventos` declared, and nothing else.
//
// CONTRACT-23: it is composed for THIS installation's capabilities. The dynamic
// half never depends on them (no field type is a vector), and the routine being
// executed here is always the dynamic one, so the choice is unobservable in the
// result — it is threaded anyway because a composed schema that describes a
// different installation than the one it runs on has no reason to exist.
//
// CONTRACT-27: it goes through schema.ReadSchemaFor rather than BuildWithFor,
// because a composed schema must contain the TARGET of every foreign key it
// declares. ReadSchemaFor adds the targets as placeholders instead of loading
// every definition in the instance, which would put a registry query on the hot
// path of every list and every detail read. See its documentation for why a
// placeholder is sound HERE and nowhere else.
func (h *handlers) dynamicSchema(def schema.ContentTypeDefinition) (compat.Schema, error) {
	return schema.ReadSchemaFor(h.caps, def)
}

// listContentRows returns a page of rows ordered by created_at DESC.
//
// CONTRACT-20: schema.DynamicListRoutine(def). The declared order is
// (created_at DESC, id ASC) — created_at alone is not total, and a paginated
// read without a total order can select DIFFERENT rows for the same
// LIMIT/OFFSET on each engine, which is the worst failure mode in this file.
func (h *handlers) listContentRows(ctx context.Context, def schema.ContentTypeDefinition, limit, offset int) ([]map[string]any, error) {
	dyn, err := h.dynamicSchema(def)
	if err != nil {
		return nil, err
	}
	rows, err := h.store.QueryRoutine(ctx, dyn, schema.DynamicListRoutine(def), map[string]compat.Value{
		"page_limit":  integerValue(limit),
		"page_offset": integerValue(offset),
	})
	if err != nil {
		return nil, err
	}
	// Never nil: an empty type must serialise as [], not null.
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item, err := rowJSON(row, def)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// fetchContentRow loads one row by id. found=false for a missing OR malformed
// id (it is a bound parameter, so it just matches nothing); err is non-nil only
// on a real database failure.
func (h *handlers) fetchContentRow(ctx context.Context, def schema.ContentTypeDefinition, id string) (map[string]any, bool, error) {
	dyn, err := h.dynamicSchema(def)
	if err != nil {
		return nil, false, err
	}
	rows, err := h.store.QueryRoutine(ctx, dyn, schema.DynamicReadRoutine(def),
		map[string]compat.Value{"row_id": dual.UUIDValue(id)})
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	item, err := rowJSON(rows[0], def)
	if err != nil {
		return nil, false, err
	}
	return item, true, nil
}

// insertContentRow inserts a row with the given author and per-field values (in
// declaration order) and returns the generated id. A type with zero fields
// inserts the author alone — still a valid row.
//
// CONTRACT-20: raw SQL composed with compat.Placeholder. The column COUNT is
// only known at runtime, which a routine's static action list cannot express;
// the id and both timestamps are written by the application (dual.go) instead of
// relying on RETURNING and the DEFAULTs, because created_at is this type's
// primary sort key and the two engines render CURRENT_TIMESTAMP differently.
// The security model of this file is unchanged: identifiers still come only from
// the persisted definition through quoteIdentifier, values are still all bound.
func (h *handlers) insertContentRow(ctx context.Context, def schema.ContentTypeDefinition, authorID string, values []any) (string, error) {
	table, err := quoteTable(def)
	if err != nil {
		return "", err
	}
	id, err := dual.NewUUID()
	if err != nil {
		return "", err
	}
	engine := h.engine()
	stamp := nowCanonical()
	columns := []string{colID, colAuthorID, colCreatedAt, colUpdatedAt}
	args := []any{id, authorID, stamp, stamp}
	for i, name := range ownColumnNames(def) {
		quoted, err := quoteIdentifier(name)
		if err != nil {
			return "", err
		}
		columns = append(columns, quoted)
		args = append(args, values[i])
	}
	query := `INSERT INTO ` + table + ` (` + strings.Join(columns, ", ") + `) VALUES (` +
		bindList(engine, 1, len(args)) + `)`
	if _, err := h.db.ExecContext(ctx, query, args...); err != nil {
		return "", err
	}
	return id, nil
}

// updateContentRow replaces every own field of one row and bumps updated_at.
// It returns RowsAffected (0 ⇒ 404). A type with zero fields still touches
// updated_at, so the 0/1 answer stays meaningful.
//
// CONTRACT-20: this is the statement the contract names as the reason writes
// cannot be routines. CallRoutine returns only `error`; the row count IS the
// answer that decides 404 vs 200 here, and a prior existence read would add a
// round trip and a race the code does not have today.
func (h *handlers) updateContentRow(ctx context.Context, def schema.ContentTypeDefinition, id string, values []any) (int64, error) {
	table, err := quoteTable(def)
	if err != nil {
		return 0, err
	}
	engine := h.engine()
	own := ownColumnNames(def)
	assignments := make([]string, 0, len(own)+1)
	args := make([]any, 0, len(own)+2)
	position := 1
	for i, name := range own {
		quoted, err := quoteIdentifier(name)
		if err != nil {
			return 0, err
		}
		assignments = append(assignments, quoted+" = "+dual.Bind(engine, position))
		args = append(args, values[i])
		position++
	}
	assignments = append(assignments, quote(colUpdatedAt)+" = "+dual.Bind(engine, position))
	args = append(args, nowCanonical())
	position++
	query := `UPDATE ` + table + ` SET ` + strings.Join(assignments, ", ") +
		` WHERE ` + quote(colID) + ` = ` + dual.Bind(engine, position)
	args = append(args, id)
	res, err := h.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// deleteContentRow deletes one row and returns RowsAffected (0 ⇒ 404). Raw SQL
// for the same reason updateContentRow is.
func (h *handlers) deleteContentRow(ctx context.Context, def schema.ContentTypeDefinition, id string) (int64, error) {
	table, err := quoteTable(def)
	if err != nil {
		return 0, err
	}
	engine := h.engine()
	query := `DELETE FROM ` + table + ` WHERE ` + quote(colID) + ` = ` + dual.Bind(engine, 1)
	res, err := h.db.ExecContext(ctx, query, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
