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
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
)

// initialContentTypeFieldRows is how many blank field rows the create form
// starts with. Purely cosmetic: the admin adds more with the htmx button and
// blank rows are ignored on submit.
const initialContentTypeFieldRows = 3

// initialContentTypeReferenceRows is how many blank RELATION rows the create
// form starts with (CONTRACT-27 T4). One, not three: most types declare no
// relation at all, and blank rows are ignored on submit exactly like blank field
// rows. It is rendered only when there IS at least one existing type to point
// at.
const initialContentTypeReferenceRows = 1

var (
	adminContentTypesListTmpl = mustParseFS("templates/layout.html", "templates/content_types_list.html")
	adminContentTypesNewTmpl  = mustParseFS("templates/layout.html", "templates/content_types_new.html",
		"templates/content_type_field_row.html", "templates/content_type_reference_row.html")
	// Parsed standalone so the htmx "add one more row" route can render the
	// fragment alone, exactly as ui_articles.go does with the article row.
	adminContentTypeFieldRowTmpl = mustParseFS("templates/content_type_field_row.html")
	// CONTRACT-27 T4: the relation row of the create form, and its standalone
	// parse for the htmx "add one more relation" route. It is a SEPARATE template
	// from the field row because it holds a different control — a selector over
	// the EXISTING content types, not over the closed field-type catalog — and
	// because its inputs feed a different pair of parallel form keys
	// (reference_name/reference_target). Still a FIXED file in the embedded list;
	// nothing is generated at runtime (the CONTRACT-15 rule).
	adminContentTypeReferenceRowTmpl = mustParseFS("templates/content_type_reference_row.html")
	// CONTRACT-18: the edit form and its own row fragment. The edit row is a
	// DIFFERENT template from the create row because it carries one extra thing
	// the create form has no notion of: the field's stored identity
	// (content_type_fields.id), which is what turns "a different name in this
	// position" into a RENAME instead of a drop-plus-add.
	adminContentTypesEditTmpl        = mustParseFS("templates/layout.html", "templates/content_types_edit.html", "templates/content_type_edit_field_row.html")
	adminContentTypeEditFieldRowTmpl = mustParseFS("templates/content_type_edit_field_row.html")
	// CONTRACT-26: the delete confirmation page. It is its own template because
	// it shows something no other page shows and no other page needs: HOW MANY
	// ROWS the action destroys, and the two hidden inputs that carry that number
	// (and the name) into the confirming submit.
	adminContentTypesDeleteTmpl = mustParseFS("templates/layout.html", "templates/content_types_delete.html")
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

// contentTypeTargetOption is one <option> of a relation's target selector: an
// EXISTING dynamic content type. The catalog is the registry itself, so the form
// can only ever offer a target that exists — which is the T2 order rule made
// unreachable to break from the panel, rather than merely checked afterwards.
type contentTypeTargetOption struct {
	Value    string
	Selected bool
}

// contentTypeReferenceRow is ONE row of the relation editor: the typed column
// name and the selector state. Like contentTypeFieldRow it is the view model of
// both the full form and the htmx fragment, so an appended row is byte-identical
// to a pre-rendered one.
type contentTypeReferenceRow struct {
	Name    string
	Targets []contentTypeTargetOption
}

// adminContentTypeNewPage is the view model of the create form. Name and Fields
// are echoed back verbatim after a validation error so the admin never loses
// what they typed.
//
// CONTRACT-27 T4 adds References and NoTargets. NoTargets is not cosmetic: on an
// installation with no dynamic types yet there is nothing to reference, and an
// empty <select> would be a control that cannot be used and an error the admin
// could not have predicted. The template then explains it instead.
type adminContentTypeNewPage struct {
	pageData
	Name       string
	Fields     []contentTypeFieldRow
	References []contentTypeReferenceRow
	NoTargets  bool
	Error      string
}

// registerAdminContentTypeRoutes wires the /admin/content-types HTML surface.
// Reads require only a session (like every other read in the project); the
// single write is gated by content_types.manage — the SAME permission the JSON
// API uses, no new permission.
func (h *handlers) registerAdminContentTypeRoutes(mux *http.ServeMux) {
	mux.Handle("GET /admin/content-types", h.requireSession(http.HandlerFunc(h.handleAdminContentTypesList)))
	mux.Handle("GET /admin/content-types/new", h.requireSession(http.HandlerFunc(h.handleAdminContentTypeNewForm)))
	mux.Handle("GET /admin/content-types/new/field", h.requireSession(http.HandlerFunc(h.handleAdminContentTypeFieldRow)))
	// CONTRACT-27 T4 — one more blank RELATION row. Session only, like the field
	// fragment: it writes nothing. It sits under the same /new/ prefix so the two
	// fragments of the create form stay together.
	mux.Handle("GET /admin/content-types/new/reference", h.requireSession(http.HandlerFunc(h.handleAdminContentTypeReferenceRow)))
	mux.Handle("POST /admin/content-types", h.requireSessionPermission("content_types.manage")(http.HandlerFunc(h.handleAdminContentTypeCreate)))
	// CONTRACT-18 T3 — editing the fields of an applied type. Same permission,
	// same mutex, same store operation as the JSON API: this file stays
	// presentation-only.
	mux.Handle("GET /admin/content-types/{name}/edit", h.requireSession(http.HandlerFunc(h.handleAdminContentTypeEditForm)))
	mux.Handle("GET /admin/content-types/edit/field", h.requireSession(http.HandlerFunc(h.handleAdminContentTypeEditFieldRow)))
	mux.Handle("POST /admin/content-types/{name}", h.requireSessionPermission("content_types.manage")(http.HandlerFunc(h.handleAdminContentTypeEdit)))
	// CONTRACT-26 T3 — deleting a type, in the SAME two steps the field editor
	// uses: nothing is destroyed until a second, explicit submit that carries
	// exactly what the first step SHOWED (the name and the row count).
	mux.Handle("GET /admin/content-types/{name}/delete", h.requireSession(http.HandlerFunc(h.handleAdminContentTypeDeleteForm)))
	mux.Handle("POST /admin/content-types/{name}/delete", h.requireSessionPermission("content_types.manage")(http.HandlerFunc(h.handleAdminContentTypeDelete)))
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
	targets, err := h.contentTypeTargets(r.Context())
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	rows := make([]contentTypeFieldRow, 0, initialContentTypeFieldRows)
	for i := 0; i < initialContentTypeFieldRows; i++ {
		rows = append(rows, blankContentTypeFieldRow())
	}
	references := make([]contentTypeReferenceRow, 0, initialContentTypeReferenceRows)
	if len(targets) > 0 {
		for i := 0; i < initialContentTypeReferenceRows; i++ {
			references = append(references, blankContentTypeReferenceRow(targets))
		}
	}
	renderContentTypeNew(w, http.StatusOK, adminContentTypeNewPage{
		pageData:   h.page(r, "Nuevo tipo de contenido — librarian"),
		Fields:     rows,
		References: references,
		NoTargets:  len(targets) == 0,
	})
}

// contentTypeTargets returns the public names of every existing dynamic content
// type — the only legal targets of a relation (CONTRACT-27: a reference to a
// CODE type is out of scope; relating to one still requires a code type).
//
// It reads the registry rather than a cached list, so the selector cannot offer
// a type that was deleted in another tab. LoadDefinitions already returns the
// definitions in the canonical byte-wise order, so the option list is
// deterministic and identical on both engines.
func (h *handlers) contentTypeTargets(ctx context.Context) ([]string, error) {
	defs, err := store.LoadDefinitions(ctx, h.store)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out, nil
}

// handleAdminContentTypeReferenceRow returns ONE blank relation row as an HTML
// fragment for htmx to append — the relation twin of
// handleAdminContentTypeFieldRow. Unlike that one it DOES read the database
// (the selector's options are the existing types), which is why it can fail; a
// failure renders nothing rather than an empty selector that would silently
// submit a target of "".
func (h *handlers) handleAdminContentTypeReferenceRow(w http.ResponseWriter, r *http.Request) {
	targets, err := h.contentTypeTargets(r.Context())
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	if len(targets) == 0 {
		h.httpOperationFailure(w, r, nil)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = adminContentTypeReferenceRowTmpl.ExecuteTemplate(w, "content_type_reference_row", blankContentTypeReferenceRow(targets))
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
	targets, err := h.contentTypeTargets(r.Context())
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	def, rows, refRows, msg, ok := parseContentTypeForm(r, targets)
	if !ok {
		h.rerenderContentTypeNew(w, r, def.Name, rows, refRows, targets, msg)
		return
	}
	// The T1 gate first, before anything touches the database — same order as
	// the JSON handler, so the UI cannot be a laxer door into the same action.
	if err := def.Validate(); err != nil {
		h.rerenderContentTypeNew(w, r, def.Name, rows, refRows, targets, "Definición inválida: "+err.Error())
		return
	}

	h.schemaMu.Lock()
	err = store.CreateContentType(r.Context(), h.store, def)
	h.schemaMu.Unlock()

	switch {
	case errors.Is(err, store.ErrDuplicateContentType):
		h.rerenderContentTypeNew(w, r, def.Name, rows, refRows, targets, "Ya existe un tipo de contenido con ese nombre.")
		return
	case err != nil:
		// A definition compat itself refuses (or any other creation failure) is
		// still shown as a form error, not a 500 page: the admin can fix the
		// definition and retry, and nothing was persisted (the whole creation is
		// one transaction).
		h.rerenderContentTypeNew(w, r, def.Name, rows, refRows, targets, "No se pudo crear el tipo de contenido: "+err.Error())
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
//
// Referrers (CONTRACT-27 T4) is the OTHER blocking state of this page, and it is
// stronger than PendingRemovals: a pending removal asks for a confirmation,
// while a referrer means the edit is IMPOSSIBLE until another content type is
// deleted. When it is non-empty the template renders the explanation and no
// form, so the panel cannot offer an action the store is going to refuse.
type adminContentTypeEditPage struct {
	pageData
	Name            string
	Fields          []contentTypeEditFieldRow
	Error           string
	PendingRemovals []string
	Referrers       []store.Referrer
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
	// CONTRACT-27 T4 — the guard of T3 has to be visible in the panel, and it has
	// to be visible HERE: on the page the admin opens to edit, not only after they
	// have filled a form in and submitted it. When something references this type
	// the template hides the form entirely and explains what to delete first.
	referrers, err := store.ReferencesTo(r.Context(), h.store, name)
	if err != nil {
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
	h.renderContentTypeEdit(w, r, http.StatusOK, name, rows, "", nil, referrers)
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
	references, err := store.LoadContentTypeReferences(r.Context(), h.store, typeID)
	if err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	edits, rows, confirmed, ok := parseContentTypeEditForm(r, stored)
	if !ok {
		h.renderContentTypeEdit(w, r, http.StatusBadRequest, name, rows, "Formulario inválido.", nil, nil)
		return
	}
	plan, err := store.PlanContentTypeEdit(name, typeID, stored, references, edits)
	if err != nil {
		h.renderContentTypeEdit(w, r, http.StatusBadRequest, name, rows, "Definición inválida: "+err.Error(), nil, nil)
		return
	}
	if missing := unconfirmed(plan.Removed, confirmed); len(missing) > 0 {
		h.renderContentTypeEdit(w, r, http.StatusOK, name, rows, "", missing, nil)
		return
	}

	h.schemaMu.Lock()
	_, err = store.EditContentType(r.Context(), h.store, name, edits, plan.Removed)
	h.schemaMu.Unlock()
	// CONTRACT-27 T3 in the panel: the refusal is rendered as the same blocking
	// notice the edit page shows on load, with the referring types named, rather
	// than as a generic "could not edit" carrying an engine message.
	var referenced store.ReferencedTypeError
	switch {
	case errors.As(err, &referenced):
		h.renderContentTypeEdit(w, r, http.StatusBadRequest, name, rows, "", nil, referenced.Referrers)
		return
	case err != nil:
		h.renderContentTypeEdit(w, r, http.StatusBadRequest, name, rows,
			"No se pudo editar el tipo de contenido: "+err.Error(), nil, nil)
		return
	}
	http.Redirect(w, r, "/admin/content-types", http.StatusSeeOther)
}

// adminContentTypeDeletePage is the view model of the delete confirmation page.
//
// Rows is the reason this page exists at all. The contract's requirement is that
// whoever confirms SEES how many rows the action destroys BEFORE confirming — a
// type with zero rows and one with ten thousand are not the same decision — and
// today there is no other way to find that out from the panel. The number is
// both displayed and carried in a hidden input, so the submit that follows
// confirms exactly what was on screen.
//
// Referrers (CONTRACT-27 T4) blocks the page in the same way it blocks the edit
// page: when something references this type the deletion is impossible, so the
// template explains what to delete first and renders no confirm button. Showing
// the row count next to a button that cannot work would be worse than not
// showing the page at all.
type adminContentTypeDeletePage struct {
	pageData
	Name         string
	Table        string
	Fields       []string
	Rows         int64
	TableMissing bool
	Error        string
	Referrers    []store.Referrer
}

// handleAdminContentTypeDeleteForm is STEP ONE: it shows what the deletion would
// destroy. It writes nothing — it is a plan, not an action — and it is the only
// place the row count comes from, which is what makes the second step
// impossible to reach without having seen it.
func (h *handlers) handleAdminContentTypeDeleteForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	plan, err := store.PlanContentTypeDeletion(r.Context(), h.store, name)
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		h.renderNotFound(w, r)
		return
	case err != nil:
		h.httpOperationFailure(w, r, err)
		return
	}
	h.renderContentTypeDelete(w, r, http.StatusOK, plan, "")
}

// handleAdminContentTypeDelete is STEP TWO (content_types.manage). It only
// proceeds when the submitted form carries the exact name and the exact row
// count the page showed; anything else comes back as the SAME page with the
// LIVE figures and an explanation, having destroyed nothing.
//
// The store re-checks the confirmation inside its own transaction anyway, so —
// exactly as with the field editor — the UI is not the only thing standing
// between an admin and irreversible loss.
func (h *handlers) handleAdminContentTypeDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		h.httpOperationFailure(w, r, err)
		return
	}
	confirm := store.DeleteConfirmation{Name: strings.TrimSpace(r.PostFormValue("confirm_name"))}
	if raw := strings.TrimSpace(r.PostFormValue("confirm_rows")); raw != "" {
		rows, err := strconv.ParseInt(raw, 10, 64)
		if err == nil {
			confirm.Rows, confirm.RowsStated = rows, true
		}
	}

	h.schemaMu.Lock()
	_, err := store.DeleteContentType(r.Context(), h.store, name, confirm)
	h.schemaMu.Unlock()

	var refusal store.DeleteConfirmationError
	var referenced store.ReferencedTypeError
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		h.renderNotFound(w, r)
		return
	case errors.As(err, &referenced):
		// CONTRACT-27 T3 in the panel. rerender re-reads the live plan, whose
		// Referrers field carries the same fact, so the page comes back showing the
		// blocking notice instead of the confirm button — no message needed beyond
		// what the notice itself says.
		h.rerenderContentTypeDelete(w, r, name, "")
		return
	case errors.As(err, &refusal):
		h.rerenderContentTypeDelete(w, r, name, refusal.Error())
		return
	case err != nil:
		h.rerenderContentTypeDelete(w, r, name, "No se pudo borrar el tipo de contenido: "+err.Error())
		return
	}
	http.Redirect(w, r, "/admin/content-types", http.StatusSeeOther)
}

// rerenderContentTypeDelete re-reads the LIVE plan and shows the page again with
// the error. Re-reading matters: the commonest reason to land here is that the
// row count moved, and echoing the stale number back would ask the admin to
// confirm a figure that is already wrong again.
func (h *handlers) rerenderContentTypeDelete(w http.ResponseWriter, r *http.Request, name, msg string) {
	plan, err := store.PlanContentTypeDeletion(r.Context(), h.store, name)
	switch {
	case errors.Is(err, store.ErrContentTypeNotFound):
		h.renderNotFound(w, r)
		return
	case err != nil:
		h.httpOperationFailure(w, r, err)
		return
	}
	h.renderContentTypeDelete(w, r, http.StatusBadRequest, plan, msg)
}

// renderContentTypeDelete writes the confirmation page with the given status.
func (h *handlers) renderContentTypeDelete(w http.ResponseWriter, r *http.Request, status int, plan store.ContentTypeDeletion, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = adminContentTypesDeleteTmpl.ExecuteTemplate(w, "layout", adminContentTypeDeletePage{
		pageData:     h.page(r, "Borrar tipo de contenido — librarian"),
		Name:         plan.TypeName,
		Table:        plan.TableName,
		Fields:       plan.Fields,
		Rows:         plan.Rows,
		TableMissing: plan.TableMissing,
		Error:        msg,
		Referrers:    plan.Referrers,
	})
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
func (h *handlers) renderContentTypeEdit(w http.ResponseWriter, r *http.Request, status int, name string, rows []contentTypeEditFieldRow, msg string, pending []string, referrers []store.Referrer) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = adminContentTypesEditTmpl.ExecuteTemplate(w, "layout", adminContentTypeEditPage{
		pageData:        h.page(r, "Editar campos — librarian"),
		Name:            name,
		Fields:          rows,
		Error:           msg,
		PendingRemovals: pending,
		Referrers:       referrers,
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
// CONTRACT-27 T4: the RELATION rows are read from the same kind of parallel
// repeated keys (`reference_name` / `reference_target`), zipped positionally by
// the submission order Go's r.PostForm preserves. A row whose NAME is blank is
// ignored, exactly like a blank field row, so the spare row costs nothing.
func parseContentTypeForm(r *http.Request, targets []string) (schema.ContentTypeDefinition, []contentTypeFieldRow, []contentTypeReferenceRow, string, bool) {
	if err := r.ParseForm(); err != nil {
		return schema.ContentTypeDefinition{}, defaultContentTypeFieldRows(), nil, "Formulario inválido.", false
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
	refNames := r.PostForm["reference_name"]
	refTargets := r.PostForm["reference_target"]
	refRows := make([]contentTypeReferenceRow, 0, len(refNames))
	for i, raw := range refNames {
		target := ""
		if i < len(refTargets) {
			target = strings.TrimSpace(refTargets[i])
		}
		name := strings.TrimSpace(raw)
		refRows = append(refRows, contentTypeReferenceRow{Name: name, Targets: contentTypeTargetOptions(targets, target)})
		if name == "" {
			continue
		}
		def.References = append(def.References, schema.ReferenceDefinition{Name: name, Target: target})
	}
	if len(refRows) == 0 && len(targets) > 0 {
		refRows = append(refRows, blankContentTypeReferenceRow(targets))
	}
	if def.Name == "" {
		return def, rows, refRows, "El nombre del tipo es obligatorio.", false
	}
	return def, rows, refRows, "", true
}

// rerenderContentTypeNew re-renders the create form with a 400 and the error.
func (h *handlers) rerenderContentTypeNew(w http.ResponseWriter, r *http.Request, name string, rows []contentTypeFieldRow, refRows []contentTypeReferenceRow, targets []string, msg string) {
	renderContentTypeNew(w, http.StatusBadRequest, adminContentTypeNewPage{
		pageData:   h.page(r, "Nuevo tipo de contenido — librarian"),
		Name:       name,
		Fields:     rows,
		References: refRows,
		NoTargets:  len(targets) == 0,
		Error:      msg,
	})
}

// contentTypeTargetOptions builds a relation's target selector from the existing
// dynamic types, pre-selecting selected when it is one of them.
func contentTypeTargetOptions(targets []string, selected string) []contentTypeTargetOption {
	out := make([]contentTypeTargetOption, 0, len(targets))
	for _, t := range targets {
		out = append(out, contentTypeTargetOption{Value: t, Selected: t == selected})
	}
	return out
}

// blankContentTypeReferenceRow is one empty relation row with nothing
// pre-selected, so an admin who leaves it untouched submits a blank NAME and the
// row is ignored.
func blankContentTypeReferenceRow(targets []string) contentTypeReferenceRow {
	return contentTypeReferenceRow{Targets: contentTypeTargetOptions(targets, "")}
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
