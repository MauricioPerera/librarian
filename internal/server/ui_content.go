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
//
// CONTRACT-30 — THE RELATIONS, AND THE SILENT LOSS THEY USED TO CAUSE.
// Until this contract this file walked ONLY def.Fields. The relations declared
// by CONTRACT-27 were never rendered, so the panel's POST/PUT body never carried
// them, so bindValues applied its documented "absent ⇒ NULL" rule to them and
// updateContentRow — which writes EVERY own column, not only the changed ones —
// wiped the relation to NULL. Editing the title of a book from the panel dropped
// its author, with a 200 and no message. None of those three pieces is wrong on
// its own: "absent ⇒ NULL" is the right rule for the JSON API (see bindValues),
// and a PUT that replaces every own column is the semantics articles and
// products already have. The missing fourth piece was HERE, and it is what this
// file now adds: the form offers the relations, so the body carries them, so
// nothing is absent and nothing is silently reset. The JSON surface is untouched.
//
// CONTRACT-31 — THE RELATIONS, READABLE IN THE LISTING AND BOUNDED IN COST.
// Two consequences of the previous contract are closed here, and they are the
// same problem on two screens: turning a relation into something a person can
// READ costs a read of the target, and that cost has to be paid once per
// distinct target — never once per relation (the form's defect) and never once
// per row (the defect a naive listing would have introduced).
//
//   - THE LISTING now has one column per declared relation, filled with the SAME
//     text the selector shows, because both call referenceOptionLabel. Two rows
//     pointing at different targets stopped looking identical.
//   - THE COST is bounded by construction, not by care: the ids are already in
//     the rows the listing read (a relation is one of the type's own columns),
//     and they are translated against ONE page of target rows per distinct
//     target type — see referenceCells and referenceTargetCache. A listing of a
//     hundred rows issues exactly the queries a listing of three does.
//   - THE FORM shares that cache, so two relations pointing at the same target
//     stopped reading it twice.
//
// The bound has a price and it is stated rather than hidden: the translation is
// as bounded as the selector's option list, so a relation pointing at a row
// older than that page renders as unresolvedReferenceLabel — visibly SET, and
// visibly not translated — instead of costing one more query per row.

import (
	"context"
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

// contentReferenceOption is ONE <option> of a relation selector: the id that is
// SUBMITTED and the text a person READS. The two are deliberately different
// things — see referenceOptionLabel — and only Value ever reaches the database.
type contentReferenceOption struct {
	Value    string
	Label    string
	Selected bool
}

// contentReferenceInput is ONE relation control. Value is the currently selected
// id ("" ⇒ no relation), kept next to the options so the template can mark the
// empty option as selected without searching the slice.
//
// Truncated says the option list is a PREFIX of the target's rows. It is part of
// the view model rather than a template detail because the form has to SAY it:
// an admin cannot pick a row that is not offered, and a silent cut here would be
// the very same class of failure this contract exists to close — the panel
// deciding something about the data without telling anyone.
type contentReferenceInput struct {
	Name      string
	Target    string
	Value     string
	Options   []contentReferenceOption
	Truncated bool
	Limit     int
}

// contentRowView is one row of the generic list: the cells in own-column order
// (fields then relations, ownColumnNames) plus the id/type needed by the edit
// and delete controls.
type contentRowView struct {
	Type      string
	ID        string
	Cells     []string
	CreatedAt string
}

// adminContentListPage is the view model of the generic listing. Columns are
// the type's own column names, in declaration order: its fields and then its
// relations (ownColumnNames), which is the same order the cells are built in.
type adminContentListPage struct {
	pageData
	Type    string
	Columns []string
	Rows    []contentRowView
}

// adminContentFormPage is the view model of the generic create/edit forms.
type adminContentFormPage struct {
	pageData
	Type       string
	ID         string
	Fields     []contentFieldInput
	References []contentReferenceInput
	Error      string
}

// contentFormBinding is everything one submitted content form yields: the values
// to write, the controls to re-render if it is refused, and why. It is a struct
// rather than five return values because the relation controls made the tuple
// unreadable, and because Values must never be used without checking OK.
type contentFormBinding struct {
	Values     []any
	Fields     []contentFieldInput
	References []contentReferenceInput
	Message    string
	OK         bool
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
		h.httpOperationFailure(w, r, err)
		return schema.ContentTypeDefinition{}, false
	}
	return def, true
}

// handleAdminContentList renders the rows of a dynamic type, one column per
// declared field AND one per declared relation. A type with ZERO fields still
// lists its rows (id + created), which is why the columns are a range and not a
// hardcoded header.
//
// CONTRACT-31: the relation columns are why this handler no longer walks
// def.Fields to build its header — the order is taken from ownColumnNames, the
// single spelling of "this type's own columns" that bindValues and
// updateContentRow already zip against, so a cell can never drift under the
// wrong header. The ids the cells translate are read from the rows this listing
// ALREADY has (a relation is one of the type's own columns, so listContentRows
// returns it); nothing is re-read per row.
func (h *handlers) handleAdminContentList(w http.ResponseWriter, r *http.Request) {
	def, ok := h.resolveTypeUI(w, r)
	if !ok {
		return
	}
	rows, err := h.listContentRows(r.Context(), def, 100, 0)
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	relations, err := h.referenceCells(r.Context(), def, rows)
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	columns := ownColumnNames(def)
	views := make([]contentRowView, 0, len(rows))
	for i, row := range rows {
		cells := make([]string, 0, len(columns))
		for _, f := range def.Fields {
			cells = append(cells, cellValue(row[f.Name]))
		}
		cells = append(cells, relations[i]...)
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
	// CONTRACT-30: a create form starts with every relation on "sin relación",
	// which is also what an omitted relation stores. The selectors are built from
	// the target's CURRENT rows, so a form opened now cannot offer a row that was
	// deleted before it was opened.
	references, err := h.referenceInputs(r.Context(), def, nil)
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	renderContentForm(w, adminContentNewTmpl, http.StatusOK, adminContentFormPage{
		pageData:   h.page(r, "Nuevo "+def.Name+" — librarian"),
		Type:       def.Name,
		Fields:     emptyFieldInputs(def),
		References: references,
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
	bound, err := h.bindContentForm(r.Context(), def, r)
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	refused := func(msg string) {
		renderContentForm(w, adminContentNewTmpl, http.StatusBadRequest, adminContentFormPage{
			pageData:   h.page(r, "Nuevo "+def.Name+" — librarian"),
			Type:       def.Name,
			Fields:     bound.Fields,
			References: bound.References,
			Error:      msg,
		})
	}
	if !bound.OK {
		refused(bound.Message)
		return
	}
	// CONTRACT-30: the SAME existence check the JSON API runs (checkReferenceTargets),
	// reached through the form. The <select> is a convenience and never a validation:
	// a hand-crafted POST naming a row that does not exist has to land on the same
	// 400 with the same sentence it gets on the JSON surface, not on a driver error.
	if err := h.checkReferenceTargets(r.Context(), def, bound.Values); err != nil {
		refused(referenceRefusal(err, "No se pudo crear el contenido."))
		return
	}
	if _, err := h.insertContentRow(r.Context(), def, id.UserID, bound.Values); err != nil {
		// CONTRACT-28's net, applied here too: a target deleted between the check
		// and the INSERT is a 400 with the race's own sentence, not the generic
		// "no se pudo crear" that would hide what actually happened.
		if raced := h.foreignKeyRaceOnWrite(def, err); raced != nil {
			refused(referenceRefusal(raced, "No se pudo crear el contenido."))
			return
		}
		refused("No se pudo crear el contenido.")
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
		h.httpOperationFailure(w, r, err)
		return
	}
	if !found {
		h.renderNotFound(w, r)
		return
	}
	// CONTRACT-30: the relation arrives PRESELECTED with what the row actually
	// holds. Anything else would put the admin one save away from clearing a
	// relation they never touched — which is exactly the defect this fixes.
	references, err := h.referenceInputs(r.Context(), def, storedReferences(def, row))
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	renderContentForm(w, adminContentEditTmpl, http.StatusOK, adminContentFormPage{
		pageData:   h.page(r, "Editar "+def.Name+" — librarian"),
		Type:       def.Name,
		ID:         r.PathValue("id"),
		Fields:     fieldInputsFromRow(def, row),
		References: references,
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
	bound, err := h.bindContentForm(r.Context(), def, r)
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	refused := func(msg string) {
		renderContentForm(w, adminContentEditTmpl, http.StatusBadRequest, adminContentFormPage{
			pageData:   h.page(r, "Editar "+def.Name+" — librarian"),
			Type:       def.Name,
			ID:         rowID,
			Fields:     bound.Fields,
			References: bound.References,
			Error:      msg,
		})
	}
	if !bound.OK {
		refused(bound.Message)
		return
	}
	if err := h.checkReferenceTargets(r.Context(), def, bound.Values); err != nil {
		refused(referenceRefusal(err, "No se pudo actualizar el contenido."))
		return
	}
	n, err := h.updateContentRow(r.Context(), def, rowID, bound.Values)
	if err != nil {
		if raced := h.foreignKeyRaceOnWrite(def, err); raced != nil {
			refused(referenceRefusal(raced, "No se pudo actualizar el contenido."))
			return
		}
		h.httpOperationFailure(w, r, err)
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
		h.httpOperationFailure(w, r, err)
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
//
// CONTRACT-30: it now walks def.References as well, so the body it builds has
// the SAME shape the JSON API sends. `nil` selections are still expressed —
// the empty <option> submits "", which is left out of the body and therefore
// stored as NULL, deliberately and visibly. What can no longer happen is a
// relation being absent because nobody ever asked about it.
func (h *handlers) bindContentForm(ctx context.Context, def schema.ContentTypeDefinition, r *http.Request) (contentFormBinding, error) {
	if err := r.ParseForm(); err != nil {
		references, refErr := h.referenceInputs(ctx, def, nil)
		if refErr != nil {
			return contentFormBinding{}, refErr
		}
		return contentFormBinding{Fields: emptyFieldInputs(def), References: references, Message: "Formulario inválido."}, nil
	}
	body := make(map[string]json.RawMessage, len(def.Fields)+len(def.References))
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
			references, refErr := h.referenceInputs(ctx, def, submittedReferences(def, r))
			if refErr != nil {
				return contentFormBinding{}, refErr
			}
			return contentFormBinding{Fields: inputs, References: references, Message: err.Error()}, nil
		}
		body[f.Name] = encoded
	}
	// The selected id travels as a JSON string, which is exactly what bindReference
	// expects: the uuid shape check, the "or null" rule and the 400 message all stay
	// where they already were, and this file adds no second validator.
	selected := submittedReferences(def, r)
	for _, ref := range def.References {
		id := selected[ref.Name]
		if id == "" {
			continue
		}
		encoded, err := json.Marshal(id)
		if err != nil { // unreachable: a Go string always marshals.
			return contentFormBinding{}, err
		}
		body[ref.Name] = encoded
	}
	references, err := h.referenceInputs(ctx, def, selected)
	if err != nil {
		return contentFormBinding{}, err
	}
	values, err := bindValues(def, body)
	if err != nil {
		var bad errBadField
		if errors.As(err, &bad) {
			return contentFormBinding{Fields: inputs, References: references, Message: "Revisá los campos: " + bad.msg}, nil
		}
		return contentFormBinding{Fields: inputs, References: references, Message: "No se pudo procesar el formulario."}, nil
	}
	return contentFormBinding{Values: values, Fields: inputs, References: references, OK: true}, nil
}

// submittedReferences reads the relation half of a submitted form: name → the id
// the admin chose, "" meaning the explicit "sin relación" option. A relation
// whose control was not rendered at all (the target has no rows yet) is simply
// absent, which is the same thing.
func submittedReferences(def schema.ContentTypeDefinition, r *http.Request) map[string]string {
	out := make(map[string]string, len(def.References))
	for _, ref := range def.References {
		out[ref.Name] = lastFormValue(r, ref.Name)
	}
	return out
}

// storedReferences reads the relation half of a STORED row (rowJSON's output, so
// a relation is a string id or nil) into the same name → id map the form uses.
func storedReferences(def schema.ContentTypeDefinition, row map[string]any) map[string]string {
	out := make(map[string]string, len(def.References))
	for _, ref := range def.References {
		if id, ok := row[ref.Name].(string); ok {
			out[ref.Name] = id
		}
	}
	return out
}

// referenceRefusal renders a reference failure for the FORM. errBadField is the
// project's "the request is wrong" sentinel and its message is written to be read
// by a person (checkReferenceTargets names the relation, the target type and the
// id that missed), so it is surfaced verbatim; anything else is an internal
// failure whose text must not reach the page.
func referenceRefusal(err error, fallback string) string {
	var bad errBadField
	if errors.As(err, &bad) {
		return bad.msg
	}
	return fallback
}

// referenceOptionLimit is how many target rows ONE relation selector offers.
//
// THE NUMBER IS A DECISION, NOT A DEFAULT. It is deliberately the same 100 the
// generic listing uses (handleAdminContentList), so the rows an admin can CHOOSE
// are exactly the rows the same admin can SEE on the target's first page — two
// different cutoffs would mean a row visible in one place and unselectable in the
// other, with no explanation for the difference. A <select> is also the wrong
// control long before a table is: an unsearchable list of thousands of options is
// unusable, and giving it a proper answer (a search/typeahead picker) is a
// different piece of work than this one. Until that exists, the honest behaviour
// is to cut at a stated number and SAY SO — see contentReferenceInput.Truncated
// and the template's aviso. An admin who is told the list is partial can still
// reach the row through the JSON API; an admin who is not told simply loses it.
const referenceOptionLimit = 100

// referenceTargetPage is ONE relation target, resolved ONCE: its definition read
// back from the registry, plus the page of its rows the panel is willing to work
// with. Those two reads are everything a relation needs to become readable, in
// the form AND in the listing, so they are cached together.
//
// labels is the id → text index of that page, built on first use. It is what
// turns the listing's translation from "one lookup per row" into "one map
// lookup per row": the ids to translate are already in the listed rows, so once
// this page exists no further query depends on how many rows are shown.
type referenceTargetPage struct {
	def       schema.ContentTypeDefinition
	rows      []map[string]any
	truncated bool
	labels    map[string]string
}

// label answers with the readable text of one target id, and whether this page
// contains that row at all. The caller decides what to say when it does not —
// see unresolvedReferenceLabel — because the honest answer differs between the
// form (which can still re-fetch the ONE current value) and the listing (which
// must not, or the cost would grow with the rows).
func (p *referenceTargetPage) label(id string) (string, bool) {
	if p.labels == nil {
		p.labels = make(map[string]string, len(p.rows))
		for _, row := range p.rows {
			rowID, _ := row[colID].(string)
			p.labels[rowID] = referenceOptionLabel(p.def, row)
		}
	}
	text, ok := p.labels[id]
	return text, ok
}

// referenceTargetCache resolves each DISTINCT target type at most once per
// request.
//
// CONTRACT-31 T2 — WHY IT EXISTS. Before it, referenceInputs paid a
// FetchContentType plus a listContentRows for EVERY declared relation, so a type
// with `autor` and `prologuista` both pointing at `autores` read the same
// definition and the same page of rows twice, for byte-identical results. The
// cost of drawing the form was therefore the number of RELATIONS; it is now the
// number of distinct TARGETS, which is the number of genuinely different answers
// there are to fetch.
//
// It is deliberately per-request state and not a field of handlers: a page of
// target rows is a snapshot, and caching it across requests would offer an admin
// options that were deleted minutes ago — trading this contract's cost problem
// for CONTRACT-30's correctness one.
type referenceTargetCache struct {
	h     *handlers
	pages map[string]*referenceTargetPage
}

func (h *handlers) newReferenceTargetCache() *referenceTargetCache {
	return &referenceTargetCache{h: h, pages: make(map[string]*referenceTargetPage)}
}

// page resolves one target type, reading it at most once. The target name comes
// from the persisted definition and is read BACK from the registry exactly like
// the {type} segment is — the name in a definition is never trusted as an
// identifier.
//
// It asks for one row MORE than it will keep, which is how truncation is
// detected without a second COUNT round trip.
func (c *referenceTargetCache) page(ctx context.Context, target string) (*referenceTargetPage, error) {
	if page, ok := c.pages[target]; ok {
		return page, nil
	}
	def, err := store.FetchContentType(ctx, c.h.store, target)
	if err != nil {
		return nil, err
	}
	rows, err := c.h.listContentRows(ctx, def, referenceOptionLimit+1, 0)
	if err != nil {
		return nil, err
	}
	page := &referenceTargetPage{def: def}
	if len(rows) > referenceOptionLimit {
		rows = rows[:referenceOptionLimit]
		page.truncated = true
	}
	page.rows = rows
	c.pages[target] = page
	return page, nil
}

// referenceCells translates the relation columns of an ALREADY READ page of rows
// into the text the listing shows, one slice of cells per row, in declaration
// order.
//
// CONTRACT-31 T1 — THE COST, WHICH IS THE POINT. The ids are not fetched: they
// travel in the rows the listing already has, because a relation is one of the
// type's own columns. So the only reads here are the ones referenceTargetCache
// makes, and there is at most one set of them PER DISTINCT TARGET TYPE — never
// per row. A hundred rows pointing at a hundred different targets of the same
// type costs exactly what three rows pointing at one target cost.
//
// A target is read LAZILY, on the first row that actually names one of its rows:
// a relation whose every listed value is NULL asks the database nothing, and a
// type with no relations at all leaves this function on its first line with the
// listing it had before this contract.
//
// THE ROW THAT IS NOT ON THE PAGE. Translating is bounded, so an id can name a
// row outside the page — see unresolvedReferenceLabel for what is shown and why
// it is not resolved with one more query.
func (h *handlers) referenceCells(ctx context.Context, def schema.ContentTypeDefinition, rows []map[string]any) ([][]string, error) {
	cells := make([][]string, len(rows))
	if len(def.References) == 0 {
		return cells, nil
	}
	for i := range cells {
		cells[i] = make([]string, 0, len(def.References))
	}
	cache := h.newReferenceTargetCache()
	for _, ref := range def.References {
		var page *referenceTargetPage
		for i, row := range rows {
			id, _ := row[ref.Name].(string)
			if id == "" {
				// A relation that is not set is a NULL like any other NULL of this
				// listing: the em dash, so "no apunta a nada" and "apunta a algo que
				// no supe traducir" can never be read as the same thing.
				cells[i] = append(cells[i], cellValue(nil))
				continue
			}
			if page == nil {
				var err error
				if page, err = cache.page(ctx, ref.Target); err != nil {
					return nil, err
				}
			}
			text, found := page.label(id)
			if !found {
				text = unresolvedReferenceLabel(id)
			}
			cells[i] = append(cells[i], text)
		}
	}
	return cells, nil
}

// unresolvedReferenceLabel is what a SET relation looks like when the bounded
// translation did not contain its target row.
//
// WHY THIS EXISTS AT ALL. The listing translates against the same
// referenceOptionLimit page the selector offers, so a relation pointing at an
// older row than those falls outside it. Resolving it with an extra read would
// put one query per unresolved row back on the page — precisely the cost this
// contract exists to bound — so it is NOT resolved, and the cell says so.
//
// WHAT IT MUST NOT BE. Not empty and not the em dash: those are what a NULL
// looks like, and a relation that IS set must never render like one that is not.
// Not an invented name either. So it is the id's first eight characters — the
// same discriminator referenceOptionLabel uses, and enough to find the row
// through the edit form or the JSON API — carried by the same "texto · id"
// shape the resolved labels have, so the column reads as one column.
//
// The wording is "sin resolver" rather than "fuera de las N más recientes"
// because both causes are indistinguishable from here and the second one would
// sometimes be a lie: the target row can also have vanished in the window
// between the listing read and this translation (rare — a relation's target is
// ON DELETE RESTRICT, so it takes clearing the relation first — but real).
func unresolvedReferenceLabel(id string) string {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	return "(sin resolver) · " + short
}

// referenceInputs builds one selector per declared relation, preselected with
// `selected[name]` (the stored id when the edit form opens, the submitted id when
// a refused form is re-rendered, "" for a create form).
//
// It reads the target's rows from the REGISTRY-resolved definition through the
// same listContentRows every other read uses — no new SQL, so no new placeholder
// dialect to get wrong.
//
// CONTRACT-31 T2: the reads go through referenceTargetCache, so two relations
// pointing at the SAME target resolve it once and share the page. The selectors
// stay independent — two controls, two names, two current values — because what
// is shared is the ANSWER, not the control.
//
// THE PRESELECTED ROW IS ADDED BACK WHEN THE CUT WOULD HIDE IT. A relation may
// point at a row that is not in the newest `referenceOptionLimit`; if the option
// were missing, the <select> would fall back to the empty option and the next
// save would clear a relation nobody touched — this contract's defect, rebuilt
// out of the fix for it. So a current value that is not among the offered rows is
// fetched on its own and prepended, still marked selected.
func (h *handlers) referenceInputs(ctx context.Context, def schema.ContentTypeDefinition, selected map[string]string) ([]contentReferenceInput, error) {
	out := make([]contentReferenceInput, 0, len(def.References))
	cache := h.newReferenceTargetCache()
	for _, ref := range def.References {
		page, err := cache.page(ctx, ref.Target)
		if err != nil {
			return nil, err
		}
		current := strings.TrimSpace(selected[ref.Name])
		in := contentReferenceInput{
			Name:      ref.Name,
			Target:    ref.Target,
			Value:     current,
			Limit:     referenceOptionLimit,
			Truncated: page.truncated,
		}
		found := false
		options := make([]contentReferenceOption, 0, len(page.rows))
		for _, row := range page.rows {
			id, _ := row[colID].(string)
			if id == current {
				found = true
			}
			options = append(options, contentReferenceOption{
				Value:    id,
				Label:    referenceOptionLabel(page.def, row),
				Selected: id == current,
			})
		}
		if current != "" && !found {
			option, err := h.referenceOptionByID(ctx, page.def, current)
			if err != nil {
				return nil, err
			}
			options = append([]contentReferenceOption{option}, options...)
		}
		in.Options = options
		out = append(out, in)
	}
	return out, nil
}

// referenceOptionByID builds the option for ONE id that the offered page did not
// contain. A row that no longer exists (or an id typed by hand into a crafted
// request) still becomes an option labelled with the id itself: the value has to
// survive the re-render so the admin sees what was submitted, and rejecting it is
// checkReferenceTargets' job, not the selector's.
func (h *handlers) referenceOptionByID(ctx context.Context, target schema.ContentTypeDefinition, id string) (contentReferenceOption, error) {
	option := contentReferenceOption{Value: id, Label: id, Selected: true}
	if !uuidPattern.MatchString(id) {
		// Not a uuid: fetching it would compare a non-uuid against a PostgreSQL
		// `uuid` column. bindReference already refuses it with its own 400.
		return option, nil
	}
	row, found, err := h.fetchContentRow(ctx, target, id)
	if err != nil {
		return contentReferenceOption{}, err
	}
	if found {
		option.Label = referenceOptionLabel(target, row)
	}
	return option, nil
}

// referenceOptionLabel is THE label rule, and it is only ever cosmetic — the
// <option>'s value is the id, always.
//
// THE RULE: the target type's FIRST DECLARED FIELD, followed by the first eight
// characters of the id. The first field is the closest thing a dynamic type has
// to a title — nothing in the definition model marks one, and inventing a
// "display field" here would be a second source of truth the definition API
// cannot express and the admin never agreed to (the same argument bindValues
// makes for not inventing required-ness). Declaration order is chosen by whoever
// created the type and is stable, so the label is deterministic and identical on
// both engines.
//
// THE ID PREFIX IS NOT DECORATION. Dynamic types have no uniqueness constraint on
// any field, so two rows can legitimately render the same text; without a
// discriminator the admin would pick blind between identical options. Eight
// characters is what the listing shows in its row anchors and is enough to tell
// two rows apart by eye.
//
// THE TWO REQUIRED EDGE CASES, both legal and both handled here: a target type
// with NO declared fields has nothing to read, so the label is the bare id (which
// is all that exists to say about that row); and a NULL — or empty — value in the
// label field renders "(sin <campo>)" rather than an empty option, so the row
// stays selectable and the emptiness is stated rather than implied.
func referenceOptionLabel(target schema.ContentTypeDefinition, row map[string]any) string {
	id, _ := row[colID].(string)
	if len(target.Fields) == 0 {
		return id
	}
	field := target.Fields[0]
	label := strings.TrimSpace(displayValue(row[field.Name]))
	if label == "" {
		label = "(sin " + field.Name + ")"
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	return label + " · " + short
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
