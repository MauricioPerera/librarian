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
}

// handleAdminContentTypesList renders every persisted definition with its
// fields, plus a link into each type's content listing.
func (h *handlers) handleAdminContentTypesList(w http.ResponseWriter, r *http.Request) {
	defs, err := store.LoadDefinitions(r.Context(), store.FromDB(h.db))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
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
	err := store.CreateContentType(r.Context(), store.FromDB(h.db), def)
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
