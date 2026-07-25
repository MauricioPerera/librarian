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
//
// CONTRACT-18 adds the PUT. The old comment here said editing had "no clean
// path" because compat had no ALTER TABLE — that reasoning is unchanged, and
// the PUT does NOT add one: it composes the operations compat already expresses
// (create, copy, drop) into ONE transaction (store.EditContentType). There is
// still NO DELETE: dropping a type was never in scope.
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
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
)

// contentTypeView is the JSON representation of a dynamic content-type
// definition. It intentionally mirrors schema.ContentTypeDefinition one-to-one
// (name + ordered fields), so what a client posts is what a client reads back.
type contentTypeView struct {
	Name   string             `json:"name"`
	Fields []contentTypeField `json:"fields"`
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
	return contentTypeView{Name: d.Name, Fields: fields}
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

	err := store.CreateContentType(r.Context(), store.FromDB(h.db), def)
	switch {
	case errors.Is(err, store.ErrDuplicateContentType):
		writeError(w, http.StatusBadRequest, "a content type with this name already exists")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not create content type")
		return
	}
	writeJSON(w, http.StatusCreated, viewOf(def))
}

// handleListContentTypes lists every dynamic content-type definition. Requires
// only a valid identity — reading is not permission-gated, like the rest of the
// project.
func (h *handlers) handleListContentTypes(w http.ResponseWriter, r *http.Request) {
	defs, err := store.LoadDefinitions(r.Context(), store.FromDB(h.db))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list content types")
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
	def, err := store.FetchContentType(r.Context(), h.db, name)
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		writeError(w, http.StatusNotFound, "content type not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not read content type")
		return
	}
	_, fields, err := store.LoadContentTypeFields(r.Context(), h.db, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read content type")
		return
	}
	writeJSON(w, http.StatusOK, viewWithIDs(def.Name, fields))
}

// viewWithIDs is viewOf for the persisted (identity-carrying) field rows.
func viewWithIDs(name string, fields []store.PersistedField) contentTypeView {
	out := make([]contentTypeField, 0, len(fields))
	for _, f := range fields {
		out = append(out, contentTypeField{ID: f.ID, Name: f.Name, Type: string(f.Type)})
	}
	return contentTypeView{Name: name, Fields: out}
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
	typeID, stored, err := store.LoadContentTypeFields(r.Context(), h.db, name)
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		writeError(w, http.StatusNotFound, "content type not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not read content type")
		return
	}
	if _, err := store.PlanContentTypeEdit(name, typeID, stored, edits); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	h.schemaMu.Lock()
	plan, err := store.EditContentType(r.Context(), store.FromDB(h.db), name, edits, confirm)
	h.schemaMu.Unlock()

	var loss store.DataLossError
	var unexpected store.UnexpectedConfirmationError
	switch {
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
		writeError(w, http.StatusInternalServerError, "could not edit content type")
		return
	}

	// Read the result back from the registry rather than echoing the request:
	// the response then reflects what the database actually holds, ids included.
	_, fields, err := store.LoadContentTypeFields(r.Context(), h.db, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read content type")
		return
	}
	view := viewWithIDs(name, fields)
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

// emptyIfNil normalises a nil slice to [] so the response shape is stable.
func emptyIfNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
