package server

// CONTRACT-15 T3 — the GENERIC admin UI over the CONTENT of any dynamic type.
// One set of handlers and 4 fixed templates serve every type that exists now or
// will ever be created, because the type's persisted definition is passed to
// the templates AS DATA and the fields are rendered with a `range`.
//
//	GET    /admin/content/{type}             list        (session)
//	GET    /admin/content/{type}/new         create form (session)
//	POST   /admin/content/{type}             create      (content.create)
//	GET    /admin/content/{type}/{id}/edit   edit form   (session)
//	PUT    /admin/content/{type}/{id}        update      (content.update)
//	DELETE /admin/content/{type}/{id}        delete      (content.delete)
//
// NO DATA ACCESS AND NO VALIDATION IS REIMPLEMENTED HERE. Every read/write goes
// through the CONTRACT-14 helpers in content.go (listContentRows,
// fetchContentRow, insertContentRow, updateContentRow, deleteContentRow) and
// every value is validated by the very same bindValues/bindValue that the JSON
// API uses: this file's only job is to translate an HTML form into the
// map[string]json.RawMessage that bindValues already understands, and to
// translate a stored row back into form controls. The security rule of
// CONTRACT-14 therefore holds unchanged — the {type} segment is resolved
// against a PERSISTED definition (store.FetchContentType) before any query is
// built, and an unknown or hostile {type} is a 404 with no query against any
// dynamic table ever constructed. The only difference from the API is the 404
// body: HTML, never the JSON envelope.
//
// FIELD TYPE → FORM CONTROL (documented in the report):
//
//	text     → <input type="text">
//	integer  → <input type="number" step="1">
//	decimal  → <input type="text" inputmode="decimal">   NOT number: the
//	           browser's decimal step can rewrite the value, and this project
//	           stores decimals as canonical TEXT on purpose (products.price).
//	boolean  → checkbox (+ a hidden companion, see below)
//	date     → <input type="date">   (canonical YYYY-MM-DD, what the column holds)
//
// THE CHECKBOX TRAP AND HOW IT IS CLOSED. An unchecked checkbox is NOT
// submitted at all. Under the CONTRACT-14 rule "absent field → NULL", an
// unchecked box would store NULL instead of false — a silent divergence between
// the JSON API and the HTML form. The fix is a HIDDEN input with the same name
// and value "false" rendered immediately BEFORE the checkbox (whose value is
// "true"). The browser therefore always submits at least one value for a
// boolean field: ["false"] when unchecked, ["false","true"] when checked. The
// form reader below takes the LAST value, so the box's state wins when it is
// checked and "false" is what arrives when it is not. A boolean field can never
// be written as NULL from the form — that is the deliberate consequence, and it
// is the right one: a checkbox has two states, not three.
//
// FALSE vs NULL IN THE EDIT FORM. Every other type distinguishes them by an
// empty control (empty input ⇒ the stored value is NULL, and submitting it
// empty writes NULL back). A checkbox cannot: false and NULL both render
// unchecked. So a NULL boolean is rendered unchecked PLUS an explicit "(sin
// valor)" marker next to it, and the edit form's help text says that saving
// will turn it into false. Nothing is hidden from the admin.

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
)

var (
	adminContentListTmpl = mustParseFS("templates/layout.html", "templates/content_list.html", "templates/content_row.html")
	adminContentNewTmpl  = mustParseFS("templates/layout.html", "templates/content_new.html", "templates/content_fields.html")
	adminContentEditTmpl = mustParseFS("templates/layout.html", "templates/content_edit.html", "templates/content_fields.html")
)

// contentFieldInput is ONE form control: everything the shared field template
// needs to render the right widget preloaded with the right state. Kind is the
// declared FieldType as a string, which is what the template switches on.
type contentFieldInput struct {
	Name    string
	Kind    string
	Value   string
	Checked bool
	// IsNull marks a stored NULL, so the edit form can tell a false boolean
	// apart from an absent one (a checkbox alone cannot).
	IsNull bool
}

// contentRowView is one row of the generic list: the cells in field-declaration
// order plus the id/type needed by the edit and delete controls.
type contentRowView struct {
	Type      string
	ID        string
	Cells     []string
	CreatedAt string
}

// adminContentListPage is the view model of the generic listing. Columns are
// the type's own field names, in declaration order.
type adminContentListPage struct {
	pageData
	Type    string
	Columns []string
	Rows    []contentRowView
}

// adminContentFormPage is the view model of the generic create/edit forms.
type adminContentFormPage struct {
	pageData
	Type   string
	ID     string
	Fields []contentFieldInput
	Error  string
}

// registerAdminContentRoutes wires the generic /admin/content/{type} surface.
// Reads require only a session; writes are gated by the SAME generic content.*
// permissions that gate articles, products and the CONTRACT-14 JSON API. No new
// permission is introduced: content_types.manage gates DEFINING a type, not
// loading content into one.
func (h *handlers) registerAdminContentRoutes(mux *http.ServeMux) {
	mux.Handle("GET /admin/content/{type}", h.requireSession(http.HandlerFunc(h.handleAdminContentList)))
	mux.Handle("GET /admin/content/{type}/new", h.requireSession(http.HandlerFunc(h.handleAdminContentNewForm)))
	mux.Handle("POST /admin/content/{type}", h.requireSessionPermission("content.create")(http.HandlerFunc(h.handleAdminContentCreate)))
	mux.Handle("GET /admin/content/{type}/{id}/edit", h.requireSession(http.HandlerFunc(h.handleAdminContentEditForm)))
	mux.Handle("PUT /admin/content/{type}/{id}", h.requireSessionPermission("content.update")(http.HandlerFunc(h.handleAdminContentUpdate)))
	mux.Handle("DELETE /admin/content/{type}/{id}", h.requireSessionPermission("content.delete")(http.HandlerFunc(h.handleAdminContentDelete)))
}

// resolveTypeUI is the HTML twin of content.go's resolveType: same lookup, same
// "the segment is only ever a BOUND comparison value" guarantee, but a 404 HTML
// page instead of the JSON envelope. It is the ONLY door into every handler
// below, so no {type} from a request can reach a query unresolved.
func (h *handlers) resolveTypeUI(w http.ResponseWriter, r *http.Request) (schema.ContentTypeDefinition, bool) {
	def, err := store.FetchContentType(r.Context(), h.store, r.PathValue("type"))
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		h.renderNotFound(w, r)
		return schema.ContentTypeDefinition{}, false
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return schema.ContentTypeDefinition{}, false
	}
	return def, true
}

// handleAdminContentList renders the rows of a dynamic type, one column per
// declared field. A type with ZERO fields still lists its rows (id + created),
// which is why the columns are a range and not a hardcoded header.
func (h *handlers) handleAdminContentList(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveTypeUI(w, r)
	if !ok {
		return
	}
	rows, err := h.listContentRows(r.Context(), def, 100, 0)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	columns := make([]string, 0, len(def.Fields))
	for _, f := range def.Fields {
		columns = append(columns, f.Name)
	}
	views := make([]contentRowView, 0, len(rows))
	for _, row := range rows {
		cells := make([]string, 0, len(def.Fields))
		for _, f := range def.Fields {
			cells = append(cells, cellValue(row[f.Name]))
		}
		views = append(views, contentRowView{
			Type:      def.Name,
			ID:        fmt.Sprint(row[colID]),
			Cells:     cells,
			CreatedAt: fmt.Sprint(row[colCreatedAt]),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminContentListTmpl.ExecuteTemplate(w, "layout", adminContentListPage{
		pageData: h.page(r, def.Name+" — librarian"),
		Type:     def.Name,
		Columns:  columns,
		Rows:     views,
	})
}

// handleAdminContentNewForm renders the empty create form for a type.
func (h *handlers) handleAdminContentNewForm(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveTypeUI(w, r)
	if !ok {
		return
	}
	renderContentForm(w, adminContentNewTmpl, http.StatusOK, adminContentFormPage{
		pageData: h.page(r, "Nuevo "+def.Name+" — librarian"),
		Type:     def.Name,
		Fields:   emptyFieldInputs(def),
	})
}

// handleAdminContentCreate handles the create form POST (content.create). The
// author is the session user, exactly as in the JSON API. A field validation
// failure re-renders the form (400) with the message AND the admin's input
// preserved; nothing is inserted.
func (h *handlers) handleAdminContentCreate(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveTypeUI(w, r)
	if !ok {
		return
	}
	id, ok := identityFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	values, inputs, msg, ok := h.bindContentForm(def, r)
	if !ok {
		renderContentForm(w, adminContentNewTmpl, http.StatusBadRequest, adminContentFormPage{
			pageData: h.page(r, "Nuevo "+def.Name+" — librarian"),
			Type:     def.Name,
			Fields:   inputs,
			Error:    msg,
		})
		return
	}
	if _, err := h.insertContentRow(r.Context(), def, id.UserID, values); err != nil {
		renderContentForm(w, adminContentNewTmpl, http.StatusBadRequest, adminContentFormPage{
			pageData: h.page(r, "Nuevo "+def.Name+" — librarian"),
			Type:     def.Name,
			Fields:   inputs,
			Error:    "No se pudo crear el contenido.",
		})
		return
	}
	http.Redirect(w, r, "/admin/content/"+def.Name, http.StatusSeeOther)
}

// handleAdminContentEditForm renders the edit form preloaded from the stored
// row. A missing/malformed id → 404 HTML (the id is a bound parameter, so a
// non-UUID simply matches nothing).
func (h *handlers) handleAdminContentEditForm(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveTypeUI(w, r)
	if !ok {
		return
	}
	row, found, err := h.fetchContentRow(r.Context(), def, r.PathValue("id"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		h.renderNotFound(w, r)
		return
	}
	renderContentForm(w, adminContentEditTmpl, http.StatusOK, adminContentFormPage{
		pageData: h.page(r, "Editar "+def.Name+" — librarian"),
		Type:     def.Name,
		ID:       r.PathValue("id"),
		Fields:   fieldInputsFromRow(def, row),
	})
}

// handleAdminContentUpdate handles hx-put from the edit form (content.update).
// Like the JSON PUT it is a FULL replacement of the row's own fields. A missing
// id → 404 HTML; a field validation failure → the edit form re-rendered (400)
// with the message and the submitted values.
func (h *handlers) handleAdminContentUpdate(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveTypeUI(w, r)
	if !ok {
		return
	}
	rowID := r.PathValue("id")
	values, inputs, msg, ok := h.bindContentForm(def, r)
	if !ok {
		renderContentForm(w, adminContentEditTmpl, http.StatusBadRequest, adminContentFormPage{
			pageData: h.page(r, "Editar "+def.Name+" — librarian"),
			Type:     def.Name,
			ID:       rowID,
			Fields:   inputs,
			Error:    msg,
		})
		return
	}
	n, err := h.updateContentRow(r.Context(), def, rowID, values)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		h.renderNotFound(w, r)
		return
	}
	w.Header().Set("HX-Redirect", "/admin/content/"+def.Name)
	w.WriteHeader(http.StatusOK)
}

// handleAdminContentDelete handles hx-delete from a list row (content.delete).
// An unknown id → 404 HTML; on success an empty 200 so htmx's outerHTML swap
// removes the row — the same protocol as /admin/products.
func (h *handlers) handleAdminContentDelete(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveTypeUI(w, r)
	if !ok {
		return
	}
	n, err := h.deleteContentRow(r.Context(), def, r.PathValue("id"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		h.renderNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// Empty body → htmx outerHTML swap removes the row.
}

// bindContentForm is the ONE translation from an HTML form to the CONTRACT-14
// binder. It turns the posted form into the map[string]json.RawMessage shape
// bindValues already validates, so the UI and the JSON API accept and reject
// exactly the same things, with the same messages. It also returns the form
// controls to re-render on failure, carrying what the admin actually typed.
func (h *handlers) bindContentForm(def schema.ContentTypeDefinition, r *http.Request) ([]any, []contentFieldInput, string, bool) {
	if err := r.ParseForm(); err != nil {
		return nil, emptyFieldInputs(def), "Formulario inválido.", false
	}
	body := make(map[string]json.RawMessage, len(def.Fields))
	inputs := make([]contentFieldInput, 0, len(def.Fields))
	for _, f := range def.Fields {
		raw := lastFormValue(r, f.Name)
		inputs = append(inputs, contentFieldInput{
			Name:    f.Name,
			Kind:    string(f.Type),
			Value:   raw,
			Checked: f.Type == schema.FieldBoolean && raw == "true",
		})
		// An empty control means "no value": the field is simply left out of the
		// body, which bindValues stores as NULL — the identical rule the JSON API
		// applies to an omitted key. Booleans never reach this branch because the
		// hidden companion input always submits "false".
		if raw == "" {
			continue
		}
		encoded, err := encodeFormValue(f, raw)
		if err != nil {
			return nil, inputs, err.Error(), false
		}
		body[f.Name] = encoded
	}
	values, err := bindValues(def, body)
	if err != nil {
		var bad errBadField
		if errors.As(err, &bad) {
			return nil, inputs, "Revisá los campos: " + bad.msg, false
		}
		return nil, inputs, "No se pudo procesar el formulario.", false
	}
	return values, inputs, "", true
}

// encodeFormValue turns ONE raw form string into the JSON token bindValue
// expects for that declared type. It does NOT validate — it only chooses the
// JSON shape; the actual acceptance/rejection stays in bindValue, so there is
// exactly one validator for both surfaces.
//
//   - text / decimal / date → a JSON string (a decimal is a string on purpose:
//     bindValue canonicalises it to TEXT, never through a float).
//   - integer               → the raw token, so "abc" or "1.5" is rejected by
//     bindValue with its own precise message rather than being silently coerced.
//   - boolean               → the literal true/false produced by the hidden +
//     checkbox pair.
func encodeFormValue(f schema.FieldDefinition, raw string) (json.RawMessage, error) {
	switch f.Type {
	case schema.FieldInteger:
		return json.RawMessage(raw), nil
	case schema.FieldBoolean:
		if raw == "true" {
			return json.RawMessage("true"), nil
		}
		return json.RawMessage("false"), nil
	default:
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("el campo %q no se pudo procesar", f.Name)
		}
		return encoded, nil
	}
}

// lastFormValue returns the LAST submitted value for a name, trimmed. "Last"
// is what closes the checkbox trap: a boolean field submits ["false"] when the
// box is unchecked and ["false","true"] when it is checked, so the checkbox's
// own value wins whenever it is present.
func lastFormValue(r *http.Request, name string) string {
	vals := r.PostForm[name]
	if len(vals) == 0 {
		return ""
	}
	return strings.TrimSpace(vals[len(vals)-1])
}

// emptyFieldInputs builds the blank controls for a create form.
func emptyFieldInputs(def schema.ContentTypeDefinition) []contentFieldInput {
	out := make([]contentFieldInput, 0, len(def.Fields))
	for _, f := range def.Fields {
		out = append(out, contentFieldInput{Name: f.Name, Kind: string(f.Type)})
	}
	return out
}

// fieldInputsFromRow preloads the controls from a stored row (the map produced
// by the CONTRACT-14 scanRow/jsonValue pair, so the Go types are already the
// ones the declared FieldType promises). A nil value is a stored NULL and is
// flagged as such, which is what lets the edit form show an empty control (or,
// for a boolean, the explicit "(sin valor)" marker) instead of pretending the
// value is a zero.
func fieldInputsFromRow(def schema.ContentTypeDefinition, row map[string]any) []contentFieldInput {
	out := make([]contentFieldInput, 0, len(def.Fields))
	for _, f := range def.Fields {
		in := contentFieldInput{Name: f.Name, Kind: string(f.Type)}
		v := row[f.Name]
		switch {
		case v == nil:
			in.IsNull = true
		case f.Type == schema.FieldBoolean:
			b, _ := v.(bool)
			in.Checked = b
			in.Value = strconv.FormatBool(b)
		default:
			in.Value = displayValue(v)
		}
		out = append(out, in)
	}
	return out
}

// displayValue renders one stored value for a table cell or a text control. A
// NULL is an em dash in a cell and an empty string in a control (see the call
// sites); a boolean is shown as sí/no in the listing.
func displayValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "sí"
		}
		return "no"
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return fmt.Sprint(t)
	}
}

// cellValue is displayValue for a TABLE cell: a stored NULL is shown as an em
// dash so an empty string and a NULL are visibly different in the listing.
func cellValue(v any) string {
	if v == nil {
		return "—"
	}
	return displayValue(v)
}

// renderContentForm writes a generic create/edit form with the given status.
func renderContentForm(w http.ResponseWriter, tmpl *template.Template, status int, data adminContentFormPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tmpl.ExecuteTemplate(w, "layout", data)
}
