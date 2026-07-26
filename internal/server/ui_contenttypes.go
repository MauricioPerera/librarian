package server

// CONTRACT-15 T2 — admin UI for the DEFINITIONS of dynamic content types.
//
//	GET  /admin/content-types            list the persisted definitions (session)
//	GET  /admin/content-types/new        the create form                (session)
//	GET  /admin/content-types/new/field  one more blank field row       (session)
//	POST /admin/content-types            create                         (content_types.manage)
//
// It reimplements NOTHING: the write goes through store.CreateContentType (the
// same atomic definition+table transaction the JSON API of CONTRACT-13 uses),
// under the same h.schemaMu, and the read through store.LoadDefinitions. This
// file is presentation only.
//
// HOW THE FORM DEFINES **N** FIELDS (design decision, documented in the report).
// The number of fields is variable, so the form needs to grow. The chosen
// mechanism is htmx — already embedded, no new dependency and no hand-written
// JavaScript: an "Agregar campo" button issues
// `hx-get="/admin/content-types/new/field"` with `hx-swap="beforeend"` into the
// field container, and the server returns ONE more blank row fragment rendered
// from the SAME fixed template the full form uses. Templates are still parsed
// at package init from a fixed file list; nothing is generated at runtime.
//
// The rows use REPEATED input names (`field_name` / `field_type`) rather than
// indexed ones (`field[0][name]`): Go's r.PostForm preserves submission order,
// so the two parallel slices zip positionally, and a row the admin left blank
// is simply skipped. That keeps the fragment stateless — it does not need to
// know which index it is — which is exactly what makes appending rows work with
// no JavaScript of our own.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
)

// initialContentTypeFieldRows is how many blank field rows the create form
// starts with. Purely cosmetic: the admin adds more with the htmx button and
// blank rows are ignored on submit.
const initialContentTypeFieldRows = 3

var (
	adminContentTypesListTmpl = mustParseFS("templates/layout.html", "templates/content_types_list.html")
	adminContentTypesNewTmpl  = mustParseFS("templates/layout.html", "templates/content_types_new.html", "templates/content_type_field_row.html")
	// Parsed standalone so the htmx "add one more row" route can render the
	// fragment alone, exactly as ui_articles.go does with the article row.
	adminContentTypeFieldRowTmpl = mustParseFS("templates/content_type_field_row.html")
	// CONTRACT-18: the edit form and its own row fragment. The edit row is a
	// DIFFERENT template from the create row because it carries one extra thing
	// the create form has no notion of: the field's stored identity
	// (content_type_fields.id), which is what turns "a different name in this
	// position" into a RENAME instead of a drop-plus-add.
	adminContentTypesEditTmpl        = mustParseFS("templates/layout.html", "templates/content_types_edit.html", "templates/content_type_edit_field_row.html")
	adminContentTypeEditFieldRowTmpl = mustParseFS("templates/content_type_edit_field_row.html")
)

// fieldTypeOption is one <option> of the per-row field-type selector. The
// catalog is schema.FieldTypes, so the UI can never offer a type the validator
// would reject.
type fieldTypeOption struct {
	Value    string
	Selected bool
}

// contentTypeFieldRow is ONE row of the field editor: the typed name and the
// selector state. It is the view model of both the full form and the htmx
// fragment, so the appended row is byte-identical to the pre-rendered ones.
type contentTypeFieldRow struct {
	Name  string
	Types []fieldTypeOption
}

// adminContentTypesListPage is the view model of the definitions list.
type adminContentTypesListPage struct {
	pageData
	Types []contentTypeView
}

// adminContentTypeNewPage is the view model of the create form. Name and Fields
// are echoed back verbatim after a validation error so the admin never loses
// what they typed.
type adminContentTypeNewPage struct {
	pageData
	Name   string
	Fields []contentTypeFieldRow
	Error  string
}

// registerAdminContentTypeRoutes wires the /admin/content-types HTML surface.
// Reads require only a session (like every other read in the project); the
// single write is gated by content_types.manage — the SAME permission the JSON
// API uses, no new permission.
func (h *handlers) registerAdminContentTypeRoutes(mux *http.ServeMux) {
	mux.Handle("GET /admin/content-types", h.requireSession(http.HandlerFunc(h.handleAdminContentTypesList)))
	mux.Handle("GET /admin/content-types/new", h.requireSession(http.HandlerFunc(h.handleAdminContentTypeNewForm)))
	mux.Handle("GET /admin/content-types/new/field", h.requireSession(http.HandlerFunc(h.handleAdminContentTypeFieldRow)))
	mux.Handle("POST /admin/content-types", h.requireSessionPermission("content_types.manage")(http.HandlerFunc(h.handleAdminContentTypeCreate)))
	// CONTRACT-18 T3 — editing the fields of an applied type. Same permission,
	// same mutex, same store operation as the JSON API: this file stays
	// presentation-only.
	mux.Handle("GET /admin/content-types/{name}/edit", h.requireSession(http.HandlerFunc(h.handleAdminContentTypeEditForm)))
	mux.Handle("GET /admin/content-types/edit/field", h.requireSession(http.HandlerFunc(h.handleAdminContentTypeEditFieldRow)))
	mux.Handle("POST /admin/content-types/{name}", h.requireSessionPermission("content_types.manage")(http.HandlerFunc(h.handleAdminContentTypeEdit)))
}

// handleAdminContentTypesList renders every persisted definition with its
// fields, plus a link into each type's content listing.
func (h *handlers) handleAdminContentTypesList(w http.ResponseWriter, r *http.Request) {
	defs, err := store.LoadDefinitions(r.Context(), h.store)
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	views := make([]contentTypeView, 0, len(defs))
	for _, d := range defs {
		views = append(views, viewOf(d))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminContentTypesListTmpl.ExecuteTemplate(w, "layout", adminContentTypesListPage{
		pageData: h.page(r, "Tipos de contenido — librarian"),
		Types:    views,
	})
}

// handleAdminContentTypeNewForm renders the empty create form with a few blank
// field rows.
func (h *handlers) handleAdminContentTypeNewForm(w http.ResponseWriter, r *http.Request) {
	rows := make([]contentTypeFieldRow, 0, initialContentTypeFieldRows)
	for i := 0; i < initialContentTypeFieldRows; i++ {
		rows = append(rows, blankContentTypeFieldRow())
	}
	renderContentTypeNew(w, http.StatusOK, adminContentTypeNewPage{
		pageData: h.page(r, "Nuevo tipo de contenido — librarian"),
		Fields:   rows,
	})
}

// handleAdminContentTypeFieldRow returns ONE blank field row as an HTML
// fragment for htmx to append. It requires a session (it is part of the admin
// UI) but no permission: it renders no data and writes nothing.
func (h *handlers) handleAdminContentTypeFieldRow(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminContentTypeFieldRowTmpl.ExecuteTemplate(w, "content_type_field_row", blankContentTypeFieldRow())
}

// handleAdminContentTypeCreate handles the create form POST
// (content_types.manage). An invalid name, an invalid field or a duplicate name
// re-renders the form with the error and the admin's input preserved — never a
// 500 and never the raw JSON envelope, exactly like the CONTRACT-08 user form.
func (h *handlers) handleAdminContentTypeCreate(w http.ResponseWriter, r *http.Request) {
	def, rows, msg, ok := parseContentTypeForm(r)
	if !ok {
		h.rerenderContentTypeNew(w, r, def.Name, rows, msg)
		return
	}
	// The T1 gate first, before anything touches the database — same order as
	// the JSON handler, so the UI cannot be a laxer door into the same action.
	if err := def.Validate(); err != nil {
		h.rerenderContentTypeNew(w, r, def.Name, rows, "Definición inválida: "+err.Error())
		return
	}

	h.schemaMu.Lock()
	err := store.CreateContentType(r.Context(), h.store, def)
	h.schemaMu.Unlock()

	switch {
	case errors.Is(err, store.ErrDuplicateContentType):
		h.rerenderContentTypeNew(w, r, def.Name, rows, "Ya existe un tipo de contenido con ese nombre.")
		return
	case err != nil:
		// A definition compat itself refuses (or any other creation failure) is
		// still shown as a form error, not a 500 page: the admin can fix the
		// definition and retry, and nothing was persisted (the whole creation is
		// one transaction).
		h.rerenderContentTypeNew(w, r, def.Name, rows, "No se pudo crear el tipo de contenido: "+err.Error())
		return
	}
	http.Redirect(w, r, "/admin/content-types", http.StatusSeeOther)
}

// contentTypeEditFieldRow is ONE row of the EDIT form. It is the create row
// plus the stored identity: ID empty means "a new field", ID set means "this is
// that stored field", so a changed Name in the same row is a rename.
// TypeValue is the row's selected type as a plain string, needed because the
// selector is disabled for an existing field (its type cannot change) and a
// disabled control submits nothing — the hidden twin carries the value.
type contentTypeEditFieldRow struct {
	ID        string
	Name      string
	TypeValue string
	Types     []fieldTypeOption
}

// adminContentTypeEditPage is the view model of the edit form.
//
// PendingRemovals is the heart of T3: when the submitted form implies removing
// fields, the page is re-rendered listing EACH field by name and asking for a
// second, explicit submit. The names double as hidden `confirm_remove` inputs,
// so the confirmation the store demands is exactly the list the admin was shown
// — it cannot silently cover a field that appeared in between.
type adminContentTypeEditPage struct {
	pageData
	Name            string
	Fields          []contentTypeEditFieldRow
	Error           string
	PendingRemovals []string
}

// handleAdminContentTypeEditForm renders the edit form for one type, with one
// row per stored field carrying its identity.
func (h *handlers) handleAdminContentTypeEditForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	_, fields, err := store.LoadContentTypeFields(r.Context(), h.store, name)
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		h.renderNotFound(w, r)
		return
	case err != nil:
		h.httpOperationFailure(w, r, err)
		return
	}
	rows := make([]contentTypeEditFieldRow, 0, len(fields)+1)
	for _, f := range fields {
		rows = append(rows, contentTypeEditFieldRow{
			ID:        f.ID,
			Name:      f.Name,
			TypeValue: string(f.Type),
			Types:     fieldTypeOptions(string(f.Type)),
		})
	}
	rows = append(rows, blankContentTypeEditFieldRow())
	h.renderContentTypeEdit(w, r, http.StatusOK, name, rows, "", nil)
}

// handleAdminContentTypeEditFieldRow returns ONE blank row (no identity ⇒ a new
// field) for htmx to append, exactly like the create form's fragment route.
func (h *handlers) handleAdminContentTypeEditFieldRow(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminContentTypeEditFieldRowTmpl.ExecuteTemplate(w, "content_type_edit_field_row", blankContentTypeEditFieldRow())
}

// handleAdminContentTypeEdit applies the edit form (content_types.manage).
//
// The two-step confirmation is the whole point of the handler: the FIRST submit
// that implies losing data never reaches the store — it comes back as the same
// form with the warning naming every field to be destroyed. Only a submit that
// carries the matching `confirm_remove` values proceeds, and the store re-checks
// them anyway (store.EditContentType refuses an unconfirmed removal), so the UI
// is not the only thing standing between an admin and lost data.
func (h *handlers) handleAdminContentTypeEdit(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	typeID, stored, err := store.LoadContentTypeFields(r.Context(), h.store, name)
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		h.renderNotFound(w, r)
		return
	case err != nil:
		h.httpOperationFailure(w, r, err)
		return
	}
	edits, rows, confirmed, ok := parseContentTypeEditForm(r, stored)
	if !ok {
		h.renderContentTypeEdit(w, r, http.StatusBadRequest, name, rows, "Formulario inválido.", nil)
		return
	}
	plan, err := store.PlanContentTypeEdit(name, typeID, stored, edits)
	if err != nil {
		h.renderContentTypeEdit(w, r, http.StatusBadRequest, name, rows, "Definición inválida: "+err.Error(), nil)
		return
	}
	if missing := unconfirmed(plan.Removed, confirmed); len(missing) > 0 {
		h.renderContentTypeEdit(w, r, http.StatusOK, name, rows, "", missing)
		return
	}

	h.schemaMu.Lock()
	_, err = store.EditContentType(r.Context(), h.store, name, edits, plan.Removed)
	h.schemaMu.Unlock()
	if err != nil {
		h.renderContentTypeEdit(w, r, http.StatusBadRequest, name, rows,
			"No se pudo editar el tipo de contenido: "+err.Error(), nil)
		return
	}
	http.Redirect(w, r, "/admin/content-types", http.StatusSeeOther)
}

// unconfirmed returns the removals the submitted form did not confirm.
func unconfirmed(removed, confirmed []string) []string {
	got := make(map[string]struct{}, len(confirmed))
	for _, c := range confirmed {
		got[c] = struct{}{}
	}
	var missing []string
	for _, name := range removed {
		if _, ok := got[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// parseContentTypeEditForm reads the edit form into the field list plus the rows
// to echo back.
//
// A row whose NAME is blank is DROPPED from the list — for a row with an
// identity that means "remove this field", which is precisely why it then needs
// the explicit confirmation. Blank spare rows (no identity, no name) simply
// vanish, like in the create form.
//
// The TYPE of an existing field is taken from the STORED value, never from the
// form: its selector is disabled in the UI, and trusting the submitted value
// would let a hand-crafted POST attempt a type change that the store would then
// have to reject. Rejecting it is still correct (and tested through the API);
// pinning it here means the honest UI path can never produce one.
func parseContentTypeEditForm(r *http.Request, stored []store.PersistedField) ([]store.FieldEdit, []contentTypeEditFieldRow, []string, bool) {
	if err := r.ParseForm(); err != nil {
		return nil, nil, nil, false
	}
	typeOf := make(map[string]schema.FieldType, len(stored))
	for _, f := range stored {
		typeOf[f.ID] = f.Type
	}
	ids := r.PostForm["field_id"]
	names := r.PostForm["field_name"]
	types := r.PostForm["field_type"]

	edits := make([]store.FieldEdit, 0, len(names))
	rows := make([]contentTypeEditFieldRow, 0, len(names))
	for i, raw := range names {
		name := strings.TrimSpace(raw)
		id := ""
		if i < len(ids) {
			id = strings.TrimSpace(ids[i])
		}
		typeValue := ""
		if i < len(types) {
			typeValue = strings.TrimSpace(types[i])
		}
		if stored, ok := typeOf[id]; ok {
			typeValue = string(stored)
		}
		rows = append(rows, contentTypeEditFieldRow{
			ID:        id,
			Name:      name,
			TypeValue: typeValue,
			Types:     fieldTypeOptions(typeValue),
		})
		if name == "" {
			continue
		}
		edits = append(edits, store.FieldEdit{ID: id, Name: name, Type: schema.FieldType(typeValue)})
	}
	confirmed := make([]string, 0, len(r.PostForm["confirm_remove"]))
	for _, c := range r.PostForm["confirm_remove"] {
		confirmed = append(confirmed, strings.TrimSpace(c))
	}
	rows = append(rows, blankContentTypeEditFieldRow())
	return edits, rows, confirmed, true
}

// renderContentTypeEdit writes the edit form with the given status.
func (h *handlers) renderContentTypeEdit(w http.ResponseWriter, r *http.Request, status int, name string, rows []contentTypeEditFieldRow, msg string, pending []string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = adminContentTypesEditTmpl.ExecuteTemplate(w, "layout", adminContentTypeEditPage{
		pageData:        h.page(r, "Editar campos — librarian"),
		Name:            name,
		Fields:          rows,
		Error:           msg,
		PendingRemovals: pending,
	})
}

// blankContentTypeEditFieldRow is one empty row with no identity (⇒ a new
// field) and the default selector state.
func blankContentTypeEditFieldRow() contentTypeEditFieldRow {
	return contentTypeEditFieldRow{
		TypeValue: string(schema.FieldText),
		Types:     fieldTypeOptions(string(schema.FieldText)),
	}
}

// parseContentTypeForm reads the create form into a definition plus the rows to
// echo back on error. A row whose NAME is blank is ignored (the form always
// renders some spare rows); the type selector always submits a value, so the
// two slices zip positionally.
func parseContentTypeForm(r *http.Request) (schema.ContentTypeDefinition, []contentTypeFieldRow, string, bool) {
	if err := r.ParseForm(); err != nil {
		return schema.ContentTypeDefinition{}, defaultContentTypeFieldRows(), "Formulario inválido.", false
	}
	def := schema.ContentTypeDefinition{Name: strings.TrimSpace(r.PostFormValue("name"))}
	names := r.PostForm["field_name"]
	types := r.PostForm["field_type"]
	rows := make([]contentTypeFieldRow, 0, len(names))
	for i, raw := range names {
		typeValue := ""
		if i < len(types) {
			typeValue = strings.TrimSpace(types[i])
		}
		name := strings.TrimSpace(raw)
		rows = append(rows, contentTypeFieldRow{Name: name, Types: fieldTypeOptions(typeValue)})
		if name == "" {
			continue
		}
		def.Fields = append(def.Fields, schema.FieldDefinition{Name: name, Type: schema.FieldType(typeValue)})
	}
	if len(rows) == 0 {
		rows = defaultContentTypeFieldRows()
	}
	if def.Name == "" {
		return def, rows, "El nombre del tipo es obligatorio.", false
	}
	return def, rows, "", true
}

// rerenderContentTypeNew re-renders the create form with a 400 and the error.
func (h *handlers) rerenderContentTypeNew(w http.ResponseWriter, r *http.Request, name string, rows []contentTypeFieldRow, msg string) {
	renderContentTypeNew(w, http.StatusBadRequest, adminContentTypeNewPage{
		pageData: h.page(r, "Nuevo tipo de contenido — librarian"),
		Name:     name,
		Fields:   rows,
		Error:    msg,
	})
}

// renderContentTypeNew writes the create form with the given status.
func renderContentTypeNew(w http.ResponseWriter, status int, data adminContentTypeNewPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = adminContentTypesNewTmpl.ExecuteTemplate(w, "layout", data)
}

// fieldTypeOptions builds the selector options from the closed catalog
// schema.FieldTypes, pre-selecting selected when it is one of them.
func fieldTypeOptions(selected string) []fieldTypeOption {
	out := make([]fieldTypeOption, 0, len(schema.FieldTypes))
	for _, t := range schema.FieldTypes {
		out = append(out, fieldTypeOption{Value: string(t), Selected: string(t) == selected})
	}
	return out
}

// blankContentTypeFieldRow is one empty row with the default selector state.
func blankContentTypeFieldRow() contentTypeFieldRow {
	return contentTypeFieldRow{Types: fieldTypeOptions(string(schema.FieldText))}
}

// defaultContentTypeFieldRows is the fallback row set for a re-render when the
// submitted form carried no rows at all (a hand-crafted POST).
func defaultContentTypeFieldRows() []contentTypeFieldRow {
	rows := make([]contentTypeFieldRow, 0, initialContentTypeFieldRows)
	for i := 0; i < initialContentTypeFieldRows; i++ {
		rows = append(rows, blankContentTypeFieldRow())
	}
	return rows
}
