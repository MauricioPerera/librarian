package server

// contenttypes.go implements CONTRACT-13 T3: the JSON API that registers a
// DYNAMIC content type and creates its real table at runtime.
//
// Scope, per DEFINITION-CPT-DINAMICOS.md and the contract, is CREATE-ONLY:
//
//	POST   /content-types          create a definition + its table (content_types.manage)
//	GET    /content-types          list definitions (valid identity only)
//	GET    /content-types/{name}   one definition with its fields (valid identity only)
//	PUT    /content-types/{name}   EDIT the fields of an applied type (content_types.manage)
//	DELETE /content-types/{name}   DELETE the type, its fields and its table (content_types.manage)
//
// CONTRACT-18 adds the PUT. The old comment here said editing had "no clean
// path" because compat had no ALTER TABLE — that reasoning is unchanged, and
// the PUT does NOT add one: it composes the operations compat already expresses
// (create, copy, drop) into ONE transaction (store.EditContentType).
//
// CONTRACT-26 adds the DELETE, which closes the lifecycle (create → edit →
// delete). It is the most destructive route of the product — it takes the table
// and every row in it — so its confirmation is stricter than the PUT's, not
// looser: see contentTypeDeleteRequest below.
//
// THE EDIT PAYLOAD (the API-shape decision of CONTRACT-18, argued in the
// report). A field's identity is content_type_fields.id, NOT its name — a
// rename is only distinguishable from "drop one, add another" if the caller
// says which existing field a new name belongs to. So:
//
//	{"fields": [{"id": "<uuid>", "name": "titulo", "type": "text"},
//	            {"name": "resumen", "type": "text"}],
//	 "confirm_remove": ["subtitulo"]}
//
// An entry WITH an id is that stored field (its data is carried over; a
// different name is a rename). An entry WITHOUT an id is a new field (NULL for
// every existing row). A stored field no entry mentions is REMOVED, and because
// that destroys data irreversibly it must be named in confirm_remove — a
// boolean flag would let a stale client agree to losing a field it never saw.
// The ids are read from GET /content-types/{name}, which now returns them.
//
// Everything risky is concentrated in ONE place: schema.ValidateIdentifier
// (T1). No name reaches a compat.Table, a compiled DDL statement, or any query
// without passing it first.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
)

// contentTypeView is the JSON representation of a dynamic content-type
// definition. It intentionally mirrors schema.ContentTypeDefinition one-to-one
// (name + ordered fields), so what a client posts is what a client reads back.
// CONTRACT-27 adds References, the relations the type declares. It is
// `omitempty`, so every response of contracts 13-26 for a type WITHOUT relations
// is byte-identical to what it was — the addition is strictly additive, exactly
// like contentTypeField.ID was.
type contentTypeView struct {
	Name       string                 `json:"name"`
	Fields     []contentTypeField     `json:"fields"`
	References []contentTypeReference `json:"references,omitempty"`
}

// contentTypeReference is ONE declared relation in a payload: the column name it
// produces and the PUBLIC name of the dynamic content type it points at.
//
// It is a SEPARATE list from `fields` and not a sixth field type, for the reason
// argued in internal/schema/relations.go: the stored vocabulary of
// content_type_fields.field_type is pinned by a schema-level CHECK that
// EnsureSchema cannot alter on an installation that already exists. The API
// shape follows the storage shape rather than papering over it, so what a client
// posts is what the registry holds.
type contentTypeReference struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

// contentTypeField is one field in a definition payload. ID is the stored
// identity (content_type_fields.id): it is EMITTED by GET /content-types/{name}
// and CONSUMED by PUT /content-types/{name} to express renames. It is
// `omitempty`, so every response shape of contracts 13-17 that never had an id
// (the create response, the list) is byte-identical to before — the addition is
// strictly additive.
type contentTypeField struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// viewOf converts a definition to its JSON view, normalising a nil field slice
// to an empty array so the response shape is stable.
func viewOf(d schema.ContentTypeDefinition) contentTypeView {
	fields := make([]contentTypeField, 0, len(d.Fields))
	for _, f := range d.Fields {
		fields = append(fields, contentTypeField{Name: f.Name, Type: string(f.Type)})
	}
	return contentTypeView{Name: d.Name, Fields: fields, References: referenceViews(d.References)}
}

// referenceViews converts the relations of a definition to their JSON view. It
// returns nil (not an empty slice) for a type with no relations, so `omitempty`
// actually omits the key and the pre-CONTRACT-27 response shape is preserved
// byte for byte.
func referenceViews(references []schema.ReferenceDefinition) []contentTypeReference {
	if len(references) == 0 {
		return nil
	}
	out := make([]contentTypeReference, 0, len(references))
	for _, r := range references {
		out = append(out, contentTypeReference{Name: r.Name, Target: r.Target})
	}
	return out
}

// definitionOf converts a decoded request body to the schema-level definition.
// No trimming beyond whitespace: a name with inner spaces must be REJECTED by
// the validator, not silently repaired.
func definitionOf(v contentTypeView) schema.ContentTypeDefinition {
	d := schema.ContentTypeDefinition{Name: strings.TrimSpace(v.Name)}
	for _, f := range v.Fields {
		d.Fields = append(d.Fields, schema.FieldDefinition{
			Name: strings.TrimSpace(f.Name),
			Type: schema.FieldType(strings.TrimSpace(f.Type)),
		})
	}
	for _, r := range v.References {
		d.References = append(d.References, schema.ReferenceDefinition{
			Name:   strings.TrimSpace(r.Name),
			Target: strings.TrimSpace(r.Target),
		})
	}
	return d
}

// handleCreateContentType registers a dynamic content type and creates its real
// table (content_types.manage).
//
// A rejected name — hostile or merely malformed — returns 400 with NOTHING
// persisted and NOTHING created: validation happens before any database work,
// and the persist+create step that follows is a single transaction.
//
// Schema-creating requests are serialized by h.schemaMu. The diff-then-create
// sequence inside store.CreateContentType reads which tables are missing and
// then creates one; two concurrent creations of DIFFERENT types could otherwise
// interleave so that both compute the same "missing" set and the second tries
// to CREATE a table the first already made. Serializing is free in practice
// (compat pins the SQLite pool to a single connection anyway, and defining a
// content type is a rare administrative action) and removes the race entirely.
// A duplicate NAME is not left to this mutex: it is decided by the
// schema-level UNIQUE(name), which holds even across processes.
func (h *handlers) handleCreateContentType(w http.ResponseWriter, r *http.Request) {
	var req contentTypeView
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	def := definitionOf(req)

	// The T1 gate, run here and FIRST: a hostile or malformed name is a 400
	// decided before a single byte touches the database, so "400 with nothing
	// persisted and nothing created" is true by construction rather than by
	// rollback. store.CreateContentType re-runs the same check as a fail-closed
	// guard for any non-HTTP caller; reaching a validation error from there
	// would mean the two disagree, which is an internal bug, not a client one.
	if err := def.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	h.schemaMu.Lock()
	defer h.schemaMu.Unlock()

	err := store.CreateContentType(r.Context(), h.store, def)
	switch {
	case errors.Is(err, store.ErrDuplicateContentType):
		writeError(w, http.StatusBadRequest, "a content type with this name already exists")
		return
	case errors.Is(err, store.ErrUnknownReferenceTarget):
		// CONTRACT-27 T2 — the ORDER refusal. It is the client's mistake (it asked
		// for a relation to something that is not there), so it is a 400 carrying
		// the store's own sentence, which already says what to create first.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		h.writeOperationFailure(w, r, err, "could not create content type")
		return
	}
	writeJSON(w, http.StatusCreated, viewOf(def))
}

// handleListContentTypes lists every dynamic content-type definition. Requires
// only a valid identity — reading is not permission-gated, like the rest of the
// project.
func (h *handlers) handleListContentTypes(w http.ResponseWriter, r *http.Request) {
	defs, err := store.LoadDefinitions(r.Context(), h.store)
	if err != nil {
		h.writeOperationFailure(w, r, err, "could not list content types")
		return
	}
	out := make([]contentTypeView, 0, len(defs))
	for _, d := range defs {
		out = append(out, viewOf(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"content_types": out})
}

// handleGetContentType returns one definition with its fields. An unknown (or
// syntactically impossible) name → 404, never 500.
//
// CONTRACT-18: each field now also carries its stored id, because that id is
// what an edit uses to say "this is the same field, renamed". This is the ONE
// route that emits it — it is the route an editor reads before editing.
func (h *handlers) handleGetContentType(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	def, err := store.FetchContentType(r.Context(), h.store, name)
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		writeError(w, http.StatusNotFound, "content type not found")
		return
	case err != nil:
		h.writeOperationFailure(w, r, err, "could not read content type")
		return
	}
	_, fields, err := store.LoadContentTypeFields(r.Context(), h.store, name)
	if err != nil {
		h.writeOperationFailure(w, r, err, "could not read content type")
		return
	}
	// CONTRACT-27: the relations come from the definition already read above.
	// They carry no id in the payload because, unlike a field, a relation has no
	// editable identity: it is declared at creation and never renamed.
	writeJSON(w, http.StatusOK, viewWithIDs(def.Name, fields, def.References))
}

// viewWithIDs is viewOf for the persisted (identity-carrying) field rows.
func viewWithIDs(name string, fields []store.PersistedField, references []schema.ReferenceDefinition) contentTypeView {
	out := make([]contentTypeField, 0, len(fields))
	for _, f := range fields {
		out = append(out, contentTypeField{ID: f.ID, Name: f.Name, Type: string(f.Type)})
	}
	return contentTypeView{Name: name, Fields: out, References: referenceViews(references)}
}

// contentTypeEditRequest is the PUT body. Name is optional; when present it
// must equal the path name — RENAMING A TYPE IS NOT PART OF THIS CONTRACT (the
// public name is what /content/{type} and every stored URL use), and silently
// ignoring a different name would be worse than refusing it.
type contentTypeEditRequest struct {
	Name          string             `json:"name"`
	Fields        []contentTypeField `json:"fields"`
	ConfirmRemove []string           `json:"confirm_remove"`
}

// handleEditContentType rebuilds the real table of an existing dynamic content
// type with a new field list (content_types.manage).
//
// Ordering, and why: everything that can be decided WITHOUT writing is decided
// first — the body, the identifier gate, the field mapping — so a rejected edit
// provably never touched the database (store.PlanContentTypeEdit is pure).
// Only then is the mutex taken and the rebuild run; store.EditContentType
// re-runs the same planning inside itself as a fail-closed guard for non-HTTP
// callers, exactly as store.CreateContentType re-runs Validate().
//
// Concurrency: this is a SCHEMA-MUTATING request, so it enters the SAME régime
// as creation — h.schemaMu, the mutex that already serializes the
// diff-then-create sequence. A rebuild that interleaved with a creation could
// compute its composed schema from a registry that changed underneath it.
func (h *handlers) handleEditContentType(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req contentTypeEditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if trimmed := strings.TrimSpace(req.Name); trimmed != "" && trimmed != name {
		writeError(w, http.StatusBadRequest, "the name of a content type cannot be changed")
		return
	}
	edits := make([]store.FieldEdit, 0, len(req.Fields))
	for _, f := range req.Fields {
		edits = append(edits, store.FieldEdit{
			ID:   strings.TrimSpace(f.ID),
			Name: strings.TrimSpace(f.Name),
			Type: schema.FieldType(strings.TrimSpace(f.Type)),
		})
	}
	confirm := make([]string, 0, len(req.ConfirmRemove))
	for _, c := range req.ConfirmRemove {
		confirm = append(confirm, strings.TrimSpace(c))
	}

	// Read-only pre-flight: 404 for an unknown type, 400 for anything the plan
	// rejects — with NOTHING written, by construction.
	typeID, stored, err := store.LoadContentTypeFields(r.Context(), h.store, name)
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		writeError(w, http.StatusNotFound, "content type not found")
		return
	case err != nil:
		h.writeOperationFailure(w, r, err, "could not read content type")
		return
	}
	// CONTRACT-27: the relations the type already has take part in the plan — a
	// new field named like one of them is a duplicate column, and must be a 400
	// here rather than an opaque failure from compat at apply time.
	references, err := store.LoadContentTypeReferences(r.Context(), h.store, typeID)
	if err != nil {
		h.writeOperationFailure(w, r, err, "could not read content type")
		return
	}
	if _, err := store.PlanContentTypeEdit(name, typeID, stored, references, edits); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	h.schemaMu.Lock()
	plan, err := store.EditContentType(r.Context(), h.store, name, edits, confirm)
	h.schemaMu.Unlock()

	var loss store.DataLossError
	var unexpected store.UnexpectedConfirmationError
	var referenced store.ReferencedTypeError
	switch {
	case errors.As(err, &referenced):
		// CONTRACT-27 T3. It is a 400 and not a 500: the request is impossible,
		// not broken, and the body says exactly which type has to go first. It is
		// deliberately NOT a 409 — every "refused, and here is what to do about it"
		// answer of this project (the data-loss refusal of CONTRACT-18, the
		// confirmation refusal of CONTRACT-26) is a 400 with an actionable body,
		// and adding a second convention would make them inconsistent.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":            referenced.Error(),
			"referenced_by":    referrerViews(referenced.Referrers),
			"nothing_was_done": true,
		})
		return
	case errors.As(err, &loss):
		// The refusal that names EXACTLY what would be destroyed, so the caller
		// can confirm it item by item. Nothing was written.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":            loss.Error(),
			"removes_data_of":  loss.Fields,
			"confirm_remove":   loss.Fields,
			"nothing_was_done": true,
		})
		return
	case errors.As(err, &unexpected):
		writeError(w, http.StatusBadRequest, unexpected.Error())
		return
	case errors.Is(err, store.ErrContentTypeNotFound):
		writeError(w, http.StatusNotFound, "content type not found")
		return
	case errors.Is(err, store.ErrUnknownField), errors.Is(err, store.ErrFieldTypeChange):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		h.writeOperationFailure(w, r, err, "could not edit content type")
		return
	}

	// Read the result back from the registry rather than echoing the request:
	// the response then reflects what the database actually holds, ids included.
	_, fields, err := store.LoadContentTypeFields(r.Context(), h.store, name)
	if err != nil {
		h.writeOperationFailure(w, r, err, "could not read content type")
		return
	}
	view := viewWithIDs(name, fields, references)
	renamed := make([]map[string]string, 0, len(plan.Renamed))
	for _, p := range plan.Renamed {
		renamed = append(renamed, map[string]string{"from": p[0], "to": p[1]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    view.Name,
		"fields":  view.Fields,
		"added":   emptyIfNil(plan.Added),
		"removed": emptyIfNil(plan.Removed),
		"renamed": renamed,
	})
}

// referrerViews renders the T3 refusal's referrers as a stable JSON array, so a
// client can act on the fact (list the types to delete first) instead of parsing
// the sentence. Never nil: an empty array is the honest shape when the key is
// present at all.
func referrerViews(referrers []store.Referrer) []map[string]string {
	out := make([]map[string]string, 0, len(referrers))
	for _, r := range referrers {
		out = append(out, map[string]string{"content_type": r.TypeName, "reference": r.ReferenceName})
	}
	return out
}

// emptyIfNil normalises a nil slice to [] so the response shape is stable.
func emptyIfNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// contentTypeDeleteRequest is the DELETE body — the guard of CONTRACT-26.
//
// WHY IT IS NOT `{"confirm": true}`. A boolean is sent just as easily by mistake
// as on purpose, and this operation destroys a table and every row in it.
// CONTRACT-18 already refused a boolean for the PARTIAL loss of removing fields
// and demanded the LIST OF NAMES being destroyed; this is the TOTAL loss, so the
// criterion is extended rather than relaxed. The caller must state:
//
//		{"confirm_name": "eventos", "confirm_rows": 3}
//
//	  - confirm_name must equal the type being deleted, so a client that had a
//	    DIFFERENT type on screen cannot succeed by accident;
//	  - confirm_rows must equal the number of rows that will be destroyed — a
//	    value nobody can produce without having asked the server for it, which is
//	    exactly the property "a confirmation that cannot be sent without having
//	    read it" requires. It is also a freshness token: if content was loaded
//	    between reading the count and confirming, the numbers no longer match and
//	    the deletion is refused instead of silently taking rows nobody saw.
//
// ConfirmRows is a POINTER so that "I confirm zero rows" is distinguishable from
// "I sent no count". Without that distinction an empty body would delete any
// type that happened to be empty.
//
// HOW A CALLER LEARNS THE COUNT: the refusal itself carries it. A DELETE with no
// (or a wrong) confirmation answers 400 with `rows` and with the exact
// `confirm_name`/`confirm_rows` to resend — the same echo-what-to-confirm shape
// the PUT uses for `removes_data_of`/`confirm_remove`. No existing route changed
// its response to carry the count.
type contentTypeDeleteRequest struct {
	ConfirmName string `json:"confirm_name"`
	ConfirmRows *int64 `json:"confirm_rows"`
}

// handleDeleteContentType deletes a dynamic content type — definition, fields
// and real table with all its rows — in one transaction
// (content_types.manage, the SAME permission creation and editing use; no new
// permission).
//
// Ordering mirrors handleEditContentType: everything decidable without writing
// is decided first (body, 404), then the mutex that already serializes
// schema-mutating requests is taken, and the store re-runs the confirmation
// check inside its own transaction as a fail-closed guard for non-HTTP callers.
func (h *handlers) handleDeleteContentType(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// An ABSENT body is a legitimate request here: it is how a client asks
	// "what would this destroy?" and gets the 400 that tells it. It must not be
	// confused with a malformed one, so the body is read first and only decoded
	// when there is something to decode.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var req contentTypeDeleteRequest
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	confirm := store.DeleteConfirmation{Name: strings.TrimSpace(req.ConfirmName)}
	if req.ConfirmRows != nil {
		confirm.Rows, confirm.RowsStated = *req.ConfirmRows, true
	}

	h.schemaMu.Lock()
	deleted, err := store.DeleteContentType(r.Context(), h.store, name, confirm)
	h.schemaMu.Unlock()

	var refusal store.DeleteConfirmationError
	var referenced store.ReferencedTypeError
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		writeError(w, http.StatusNotFound, "content type not found")
		return
	case errors.As(err, &referenced):
		// CONTRACT-27 T3 — the same refusal shape the PUT uses, for the same
		// reason and with the same status. Nothing was done: the guard runs
		// before the transaction opens.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":            referenced.Error(),
			"referenced_by":    referrerViews(referenced.Referrers),
			"nothing_was_done": true,
		})
		return
	case errors.As(err, &refusal):
		// The refusal that SHOWS what would be destroyed — including the row
		// count — and hands back the exact confirmation to resend. Nothing was
		// written: the whole check happens before, and inside, one transaction.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             refusal.Error(),
			"name":              refusal.TypeName,
			"table":             refusal.TableName,
			"rows":              refusal.Rows,
			"table_was_missing": refusal.TableMissing,
			"confirm_name":      refusal.TypeName,
			"confirm_rows":      refusal.Rows,
			"nothing_was_done":  true,
		})
		return
	case err != nil:
		h.writeOperationFailure(w, r, err, "could not delete content type")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":              deleted.TypeName,
		"table":             deleted.TableName,
		"deleted":           true,
		"rows_deleted":      deleted.Rows,
		"fields_deleted":    emptyIfNil(deleted.Fields),
		"table_was_missing": deleted.TableMissing,
	})
}
