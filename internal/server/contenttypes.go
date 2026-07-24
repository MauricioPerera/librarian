package server

// contenttypes.go implements CONTRACT-13 T3: the JSON API that registers a
// DYNAMIC content type and creates its real table at runtime.
//
// Scope, per DEFINITION-CPT-DINAMICOS.md and the contract, is CREATE-ONLY:
//
//	POST   /content-types          create a definition + its table (content_types.manage)
//	GET    /content-types          list definitions (valid identity only)
//	GET    /content-types/{name}   one definition with its fields (valid identity only)
//
// There is deliberately NO PUT and NO DELETE. Editing the fields of an applied
// type has no clean path — sqlite-postgres-compat has zero ALTER TABLE support,
// and building a migration mechanism outside compat would introduce a second
// source of truth for the schema, contradicting the principle that holds the
// rest of the project together. Dropping a type was never in scope.
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

type contentTypeField struct {
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
func (h *handlers) handleGetContentType(w http.ResponseWriter, r *http.Request) {
	def, err := store.FetchContentType(r.Context(), h.db, r.PathValue("name"))
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		writeError(w, http.StatusNotFound, "content type not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not read content type")
		return
	}
	writeJSON(w, http.StatusOK, viewOf(def))
}
