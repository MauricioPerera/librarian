package server_test

// CONTRACT-30 acceptance tests — the relation selector of the generic content
// form, and the SILENT LOSS it closes.
//
// THE CENTRAL CASE IS A BROWSER SIMULATION, ON PURPOSE. The defect was not "the
// server drops a relation it was sent": it was "the form never sends it, so the
// server's own documented rule (absent ⇒ NULL) erases it". A test that posts
// `autor=<id>` by hand would never have seen it happen, because a hand-written
// POST is not what a browser does. So uiFormValues below reads the RENDERED form
// and submits exactly what a browser would submit from it — every input, the
// hidden companion of every checkbox, and the SELECTED option of every <select>.
// If a control is missing from the page, it is missing from the submission, and
// the loss reproduces.
//
// Everything goes through the real mux with a real session (openUITLS + loginUI)
// and never through the JSON API. Reuses grant, getBody, uiDo, uiCreateContentType,
// uiCreateTypeWithReference and nonexistentID from the sibling test files.

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var (
	formCardRe = regexp.MustCompile(`(?s)form-card.*?</form>`)
	inputTagRe = regexp.MustCompile(`<input[^>]*>`)
	selectRe   = regexp.MustCompile(`(?s)<select[^>]*name="([^"]*)"[^>]*>(.*?)</select>`)
	optionRe   = regexp.MustCompile(`<option value="([^"]*)"([^>]*)>`)
	attrNameRe = regexp.MustCompile(`\bname="([^"]*)"`)
	attrValRe  = regexp.MustCompile(`\bvalue="([^"]*)"`)
	attrTypeRe = regexp.MustCompile(`\btype="([^"]*)"`)
)

// uiFormValues extracts what a browser would submit from a rendered content
// form: every input in document order (so a boolean's hidden "false" companion
// keeps arriving before its checkbox), an unchecked checkbox omitted, and the
// selected option of every <select>. It is scoped to the page's own form-card so
// the layout's controls never leak in.
func uiFormValues(t *testing.T, page string) url.Values {
	t.Helper()
	card := formCardRe.FindString(page)
	if card == "" {
		t.Fatalf("no content form found in the page: %.400q", page)
	}
	form := url.Values{}
	for _, tag := range inputTagRe.FindAllString(card, -1) {
		name := submatch(attrNameRe, tag)
		if name == "" {
			continue
		}
		if submatch(attrTypeRe, tag) == "checkbox" && !strings.Contains(tag, " checked") {
			continue // an unchecked box is not submitted at all
		}
		form.Add(name, submatch(attrValRe, tag))
	}
	for _, sel := range selectRe.FindAllStringSubmatch(card, -1) {
		for _, opt := range optionRe.FindAllStringSubmatch(sel[2], -1) {
			if strings.Contains(opt[2], "selected") {
				form.Add(sel[1], opt[1])
			}
		}
	}
	return form
}

func submatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// uiCreateRow posts the create form of a dynamic type and returns status+body.
func uiCreateRow(t *testing.T, client *http.Client, srv *httptest.Server, typeName string, form url.Values) (int, string) {
	t.Helper()
	resp, err := client.PostForm(srv.URL+"/admin/content/"+typeName, form)
	if err != nil {
		t.Fatalf("create %s row: %v", typeName, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

// storedRelation reads a relation column straight from the database, which is
// the only witness that cannot be fooled by a rendering detail.
func storedRelation(t *testing.T, db *sql.DB, table, column, id string) sql.NullString {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(`SELECT `+column+` FROM `+table+` WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("select %s.%s: %v", table, column, err)
	}
	return v
}

// relationFixture builds the measured scenario: `autores` (one text field),
// `libros` with a `titulo` field and an `autor` relation to `autores`, and one
// author row created from the panel. It returns the author's id.
func relationFixture(t *testing.T, db *sql.DB, client *http.Client, srv *httptest.Server, authorName string) string {
	t.Helper()
	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "libros",
		[][2]string{{"titulo", "text"}}, [][2]string{{"autor", "autores"}}); status != http.StatusOK {
		t.Fatalf("create libros: %d %.400q", status, body)
	}
	if status, body := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {authorName}}); status != http.StatusOK {
		t.Fatalf("create autor: %d %.400q", status, body)
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM cpt_autores WHERE nombre = ?`, authorName).Scan(&id); err != nil {
		t.Fatalf("id of the author: %v", err)
	}
	return id
}

// TestPanelEditDoesNotDropTheRelation is THE regression case of this contract —
// the one that must FAIL against main. It reproduces the measured sequence: a
// row created with a relation, then an edit that changes ONLY another field,
// submitted exactly as the browser would submit the rendered edit form.
func TestPanelEditDoesNotDropTheRelation(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	autorID := relationFixture(t, db, client, srv, "Borges")

	// 1 — the create form OFFERS the relation as a <select> with an empty option.
	_, newForm := getBody(t, client, srv.URL+"/admin/content/libros/new")
	if !strings.Contains(newForm, `<select name="autor"`) {
		t.Fatalf("the create form has no relation selector: %.900q", newForm)
	}
	if !strings.Contains(newForm, `<option value="" selected>— sin relación —`) {
		t.Fatalf("the selector has no explicit empty option: %.900q", newForm)
	}
	if !strings.Contains(newForm, `<option value="`+autorID+`"`) {
		t.Fatalf("the selector does not offer the existing author: %.900q", newForm)
	}

	// 2 — create the book WITH the relation, through the form.
	if status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Ficciones"}, "autor": {autorID}}); status != http.StatusOK {
		t.Fatalf("create libro: %d %.400q", status, body)
	}
	var libroID string
	if err := db.QueryRow(`SELECT id FROM cpt_libros WHERE titulo = ?`, "Ficciones").Scan(&libroID); err != nil {
		t.Fatalf("id of the book: %v", err)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", libroID); !got.Valid || got.String != autorID {
		t.Fatalf("the relation was not stored on create: %v, want %q", got, autorID)
	}

	// 3 — the EDIT form carries the relation, preselected.
	_, editForm := getBody(t, client, srv.URL+"/admin/content/libros/"+libroID+"/edit")
	if !strings.Contains(editForm, `name="autor"`) {
		t.Fatalf("the edit form does not contain the relation control — this is the defect: %.900q", editForm)
	}
	if !strings.Contains(editForm, `<option value="`+autorID+`" selected>`) {
		t.Fatalf("the stored relation is not preselected in the edit form: %.900q", editForm)
	}

	// 4 — submit that form as a browser would, changing ONLY the title.
	form := uiFormValues(t, editForm)
	if got := form.Get("autor"); got != autorID {
		t.Fatalf("the rendered form would submit autor=%q, want %q", got, autorID)
	}
	form.Set("titulo", "Ficciones (2a ed)")
	resp := uiDo(t, client, http.MethodPut, srv.URL+"/admin/content/libros/"+libroID, form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200", resp.StatusCode)
	}

	// 5 — the title changed and the relation SURVIVED.
	var titulo string
	if err := db.QueryRow(`SELECT titulo FROM cpt_libros WHERE id = ?`, libroID).Scan(&titulo); err != nil {
		t.Fatalf("select title: %v", err)
	}
	if titulo != "Ficciones (2a ed)" {
		t.Errorf("titulo = %q, want the edited one", titulo)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", libroID); !got.Valid || got.String != autorID {
		t.Fatalf("EDITING ANOTHER FIELD ERASED THE RELATION: autor = %v, want %q", got, autorID)
	}
	t.Log("OK: an edit from the panel that touches another field keeps the relation")
}

// TestPanelCreatesWithAndWithoutRelation covers both halves of creation: the
// empty option means "sin relación" and stores NULL, deliberately.
func TestPanelCreatesWithAndWithoutRelation(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	autorID := relationFixture(t, db, client, srv, "Cortázar")

	if status, _ := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Con autor"}, "autor": {autorID}}); status != http.StatusOK {
		t.Fatalf("create with relation: %d", status)
	}
	// Exactly what the form submits when the empty option is chosen.
	if status, _ := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Sin autor"}, "autor": {""}}); status != http.StatusOK {
		t.Fatalf("create without relation: %d", status)
	}

	for _, tc := range []struct {
		titulo string
		want   sql.NullString
	}{
		{"Con autor", sql.NullString{String: autorID, Valid: true}},
		{"Sin autor", sql.NullString{}},
	} {
		var got sql.NullString
		if err := db.QueryRow(`SELECT autor FROM cpt_libros WHERE titulo = ?`, tc.titulo).Scan(&got); err != nil {
			t.Fatalf("select %q: %v", tc.titulo, err)
		}
		if got != tc.want {
			t.Errorf("%q stored autor = %v, want %v", tc.titulo, got, tc.want)
		}
	}
}

// TestPanelSetsAndClearsTheRelation: an edit can ADD a relation that was not
// there and REMOVE one that was — removing is a legitimate operation and the
// empty option is how it is expressed on purpose.
func TestPanelSetsAndClearsTheRelation(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	autorID := relationFixture(t, db, client, srv, "Bioy")
	if status, _ := uiCreateRow(t, client, srv, "libros", url.Values{"titulo": {"Huérfano"}, "autor": {""}}); status != http.StatusOK {
		t.Fatalf("create: %d", status)
	}
	var libroID string
	if err := db.QueryRow(`SELECT id FROM cpt_libros`).Scan(&libroID); err != nil {
		t.Fatalf("id: %v", err)
	}

	// The edit form of a row with NO relation preselects the empty option.
	_, page := getBody(t, client, srv.URL+"/admin/content/libros/"+libroID+"/edit")
	if !strings.Contains(page, `<option value="" selected>`) {
		t.Errorf("a row without relation does not preselect the empty option: %.700q", page)
	}

	// Add it.
	resp := uiDo(t, client, http.MethodPut, srv.URL+"/admin/content/libros/"+libroID,
		url.Values{"titulo": {"Huérfano"}, "autor": {autorID}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set relation status = %d", resp.StatusCode)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", libroID); !got.Valid || got.String != autorID {
		t.Fatalf("the relation was not set: %v", got)
	}

	// Remove it with the empty option.
	resp = uiDo(t, client, http.MethodPut, srv.URL+"/admin/content/libros/"+libroID,
		url.Values{"titulo": {"Huérfano"}, "autor": {""}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear relation status = %d", resp.StatusCode)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", libroID); got.Valid {
		t.Fatalf("the relation was not cleared: %v", got)
	}
}

// TestPanelSaysWhenThereIsNothingToPointAt is the same criterion CONTRACT-27
// applied to the type form (no-reference-targets): with no rows in the target
// type the form EXPLAINS it instead of rendering an unusable control.
func TestPanelSaysWhenThereIsNothingToPointAt(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "libros",
		[][2]string{{"titulo", "text"}}, [][2]string{{"autor", "autores"}}); status != http.StatusOK {
		t.Fatalf("create libros: %d %.400q", status, body)
	}

	_, page := getBody(t, client, srv.URL+"/admin/content/libros/new")
	if !strings.Contains(page, `id="no-reference-rows-autor"`) {
		t.Fatalf("the form does not explain that there is no row to point at: %.900q", page)
	}
	if strings.Contains(page, `<select name="autor"`) {
		t.Fatalf("an unusable selector was rendered anyway: %.900q", page)
	}

	// And the type is still usable: the relation simply stays NULL.
	if status, body := uiCreateRow(t, client, srv, "libros", url.Values{"titulo": {"Sin destino"}}); status != http.StatusOK {
		t.Fatalf("create with no possible target: %d %.400q", status, body)
	}
	var autor sql.NullString
	if err := db.QueryRow(`SELECT autor FROM cpt_libros`).Scan(&autor); err != nil {
		t.Fatalf("select: %v", err)
	}
	if autor.Valid {
		t.Errorf("autor = %v, want NULL", autor)
	}
}

// TestPanelSelectorIsNotAValidation: the <select> is a convenience. A crafted
// POST naming an id that does not exist — or that is not a uuid at all — is
// still a 400 with the message the JSON API gives, never a 500, and nothing is
// inserted.
func TestPanelSelectorIsNotAValidation(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	autorID := relationFixture(t, db, client, srv, "Piglia")

	status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Inventado"}, "autor": {nonexistentID}})
	if status != http.StatusBadRequest {
		t.Fatalf("a nonexistent target: status = %d, want 400: %.400q", status, body)
	}
	if !strings.Contains(body, "row has the id") || !strings.Contains(body, "autores") {
		t.Errorf("checkReferenceTargets' message was not surfaced: %.600q", body)
	}
	if strings.Contains(body, `{"error"`) {
		t.Errorf("the raw JSON envelope leaked into the panel: %.300q", body)
	}

	status, body = uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Basura"}, "autor": {"no-es-un-uuid"}})
	if status != http.StatusBadRequest {
		t.Fatalf("a malformed id: status = %d, want 400: %.400q", status, body)
	}
	if !strings.Contains(body, "Revisá los campos") {
		t.Errorf("bindReference's message was not surfaced: %.600q", body)
	}

	// The refused form keeps what was submitted for the OTHER controls, and the
	// selector is still usable afterwards.
	if !strings.Contains(body, `value="Basura"`) {
		t.Errorf("the typed title was lost on the refused form: %.600q", body)
	}
	if !strings.Contains(body, `<option value="`+autorID+`"`) {
		t.Errorf("the selector lost its options on the refused form: %.600q", body)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cpt_libros`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows inserted despite the refusals", n)
	}
}

// TestPanelSaysWhenTheSelectorIsTruncated is the acceptance of the row cap: past
// the limit the form SAYS it is showing a prefix, and — the part that matters —
// a row whose relation points OUTSIDE that prefix still arrives preselected, so
// saving does not silently reset it.
func TestPanelSaysWhenTheSelectorIsTruncated(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	oldestID := relationFixture(t, db, client, srv, "autor_0000")

	// A book pointing at the OLDEST author, while it is still offered.
	if status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Viejo"}, "autor": {oldestID}}); status != http.StatusOK {
		t.Fatalf("create libro: %d %.400q", status, body)
	}
	var libroID string
	if err := db.QueryRow(`SELECT id FROM cpt_libros`).Scan(&libroID); err != nil {
		t.Fatalf("id: %v", err)
	}

	// Push the oldest author out of the offered page (the list is created_at DESC
	// and the cap is 100).
	for i := 1; i <= 105; i++ {
		if status, body := uiCreateRow(t, client, srv, "autores",
			url.Values{"nombre": {"autor_" + strconvItoa(i)}}); status != http.StatusOK {
			t.Fatalf("create autor %d: %d %.300q", i, status, body)
		}
	}

	_, page := getBody(t, client, srv.URL+"/admin/content/libros/new")
	if !strings.Contains(page, `id="reference-truncated-autor"`) {
		t.Fatalf("the form does not say the selector is truncated: %.900q", page)
	}
	if !strings.Contains(page, "Se están ofreciendo solo las 100 filas más recientes") {
		t.Errorf("the truncation notice does not state the number: %.900q", page)
	}
	sel := selectRe.FindStringSubmatch(formCardRe.FindString(page))
	if sel == nil {
		t.Fatalf("no selector in the create form: %.900q", page)
	}
	if got := len(optionRe.FindAllString(sel[2], -1)); got != 101 { // 100 rows + the empty option
		t.Errorf("the selector offers %d options, want 101 (100 rows + «sin relación»)", got)
	}

	// THE POINT: the book's author is no longer in the newest 100, and the edit
	// form still offers it, selected.
	_, editForm := getBody(t, client, srv.URL+"/admin/content/libros/"+libroID+"/edit")
	if !strings.Contains(editForm, `<option value="`+oldestID+`" selected>`) {
		t.Fatalf("the current relation is missing from the truncated selector: %.900q", editForm)
	}
	form := uiFormValues(t, editForm)
	if got := form.Get("autor"); got != oldestID {
		t.Fatalf("the rendered form would submit autor=%q, want %q", got, oldestID)
	}
	form.Set("titulo", "Viejo (revisado)")
	resp := uiDo(t, client, http.MethodPut, srv.URL+"/admin/content/libros/"+libroID, form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp.StatusCode)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", libroID); !got.Valid || got.String != oldestID {
		t.Fatalf("a relation outside the offered page was erased by an unrelated edit: %v", got)
	}
}

// TestPanelLabelsRowsOfAFieldlessAndNullTarget covers the two edge cases the
// label rule must survive: a target type with NO declared fields, and a NULL in
// the field used as the label. In both the option is still selectable and its
// value is still the id.
func TestPanelLabelsRowsOfAFieldlessAndNullTarget(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	// A target with zero fields.
	uiCreateContentType(t, client, srv, "marcadores")
	if status, body := uiCreateTypeWithReference(t, client, srv, "notas",
		[][2]string{{"texto", "text"}}, [][2]string{{"marcador", "marcadores"}}); status != http.StatusOK {
		t.Fatalf("create notas: %d %.400q", status, body)
	}
	if status, _ := uiCreateRow(t, client, srv, "marcadores", url.Values{}); status != http.StatusOK {
		t.Fatalf("create marcador: %d", status)
	}
	var marcadorID string
	if err := db.QueryRow(`SELECT id FROM cpt_marcadores`).Scan(&marcadorID); err != nil {
		t.Fatalf("id: %v", err)
	}
	_, page := getBody(t, client, srv.URL+"/admin/content/notas/new")
	if !strings.Contains(page, `<option value="`+marcadorID+`">`+marcadorID+`<`) {
		t.Fatalf("a field-less target is not labelled by its id: %.900q", page)
	}
	if status, _ := uiCreateRow(t, client, srv, "notas",
		url.Values{"texto": {"apunte"}, "marcador": {marcadorID}}); status != http.StatusOK {
		t.Fatalf("create nota with relation: %d", status)
	}

	// A target whose label field is NULL.
	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "libros",
		[][2]string{{"titulo", "text"}}, [][2]string{{"autor", "autores"}}); status != http.StatusOK {
		t.Fatalf("create libros: %d %.400q", status, body)
	}
	if status, _ := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {""}}); status != http.StatusOK {
		t.Fatalf("create anonymous autor: %d", status)
	}
	var anonID string
	if err := db.QueryRow(`SELECT id FROM cpt_autores`).Scan(&anonID); err != nil {
		t.Fatalf("id: %v", err)
	}
	_, page = getBody(t, client, srv.URL+"/admin/content/libros/new")
	if !strings.Contains(page, `<option value="`+anonID+`">(sin nombre) ·`) {
		t.Fatalf("a NULL label field does not produce a readable option: %.900q", page)
	}
	if status, _ := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"anónimo"}, "autor": {anonID}}); status != http.StatusOK {
		t.Fatalf("create libro pointing at the NULL-labelled author: %d", status)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", firstID(t, db, "cpt_libros")); !got.Valid || got.String != anonID {
		t.Errorf("the relation to the NULL-labelled row was not stored: %v", got)
	}
}

// TestPanelHandlesTwoRelationsToTheSameTarget is the red-team item: two
// relations of the SAME type pointing at the SAME target must be two INDEPENDENT
// selectors, not one shared control.
func TestPanelHandlesTwoRelationsToTheSameTarget(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "libros",
		[][2]string{{"titulo", "text"}},
		[][2]string{{"autor", "autores"}, {"prologuista", "autores"}}); status != http.StatusOK {
		t.Fatalf("create libros with two relations: %d %.400q", status, body)
	}
	for _, n := range []string{"uno", "dos"} {
		if status, _ := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {n}}); status != http.StatusOK {
			t.Fatalf("create autor %q: %d", n, status)
		}
	}
	idOf := func(name string) string {
		var id string
		if err := db.QueryRow(`SELECT id FROM cpt_autores WHERE nombre = ?`, name).Scan(&id); err != nil {
			t.Fatalf("id of %q: %v", name, err)
		}
		return id
	}
	unoID, dosID := idOf("uno"), idOf("dos")

	_, page := getBody(t, client, srv.URL+"/admin/content/libros/new")
	if !strings.Contains(page, `<select name="autor"`) || !strings.Contains(page, `<select name="prologuista"`) {
		t.Fatalf("the two relations are not two independent selectors: %.1200q", page)
	}

	if status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Dos"}, "autor": {unoID}, "prologuista": {dosID}}); status != http.StatusOK {
		t.Fatalf("create: %d %.400q", status, body)
	}
	libroID := firstID(t, db, "cpt_libros")
	if got := storedRelation(t, db, "cpt_libros", "autor", libroID); got.String != unoID {
		t.Errorf("autor = %v, want %q", got, unoID)
	}
	if got := storedRelation(t, db, "cpt_libros", "prologuista", libroID); got.String != dosID {
		t.Errorf("prologuista = %v, want %q", got, dosID)
	}

	// An edit that touches only the title keeps BOTH, submitted as a browser would.
	_, editForm := getBody(t, client, srv.URL+"/admin/content/libros/"+libroID+"/edit")
	form := uiFormValues(t, editForm)
	form.Set("titulo", "Dos (2a)")
	resp := uiDo(t, client, http.MethodPut, srv.URL+"/admin/content/libros/"+libroID, form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update: %d", resp.StatusCode)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", libroID); got.String != unoID {
		t.Errorf("autor lost on edit: %v", got)
	}
	if got := storedRelation(t, db, "cpt_libros", "prologuista", libroID); got.String != dosID {
		t.Errorf("prologuista lost on edit: %v", got)
	}
}

// TestPanelRelationVanishingBetweenDrawAndSubmit is the other red-team item: the
// chosen row is deleted after the form was drawn. The submit is a 400 with the
// message that names the miss, never a 500 and never a driver error.
func TestPanelRelationVanishingBetweenDrawAndSubmit(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update", "content.delete")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	autorID := relationFixture(t, db, client, srv, "Efímero")

	// The form is drawn while the author still exists.
	_, page := getBody(t, client, srv.URL+"/admin/content/libros/new")
	form := uiFormValues(t, page)
	form.Set("titulo", "Tarde")
	form.Set("autor", autorID)

	// The author is deleted from the panel (nothing references it yet).
	resp := uiDo(t, client, http.MethodDelete, srv.URL+"/admin/content/autores/"+autorID, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete autor: %d", resp.StatusCode)
	}

	status, body := uiCreateRow(t, client, srv, "libros", form)
	if status != http.StatusBadRequest {
		t.Fatalf("stale option: status = %d, want 400: %.400q", status, body)
	}
	if !strings.Contains(body, "row has the id") || !strings.Contains(body, "autores") {
		t.Errorf("the refusal does not name the miss: %.600q", body)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cpt_libros`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows inserted with a stale reference", n)
	}
}

// TestPanelRelationSurvivesAValidationErrorOnAnotherField is the last red-team
// item: a form refused because ANOTHER field is invalid comes back with the
// chosen relation still selected, so a second submit does not quietly drop it.
func TestPanelRelationSurvivesAValidationErrorOnAnotherField(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "libros",
		[][2]string{{"titulo", "text"}, {"paginas", "integer"}},
		[][2]string{{"autor", "autores"}}); status != http.StatusOK {
		t.Fatalf("create libros: %d %.400q", status, body)
	}
	if status, _ := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {"Saer"}}); status != http.StatusOK {
		t.Fatalf("create autor: %d", status)
	}
	autorID := firstID(t, db, "cpt_autores")

	status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"El limonero"}, "paginas": {"muchas"}, "autor": {autorID}})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid integer: status = %d, want 400", status)
	}
	if !strings.Contains(body, `<option value="`+autorID+`" selected>`) {
		t.Fatalf("the chosen relation was lost when another field failed: %.900q", body)
	}

	// Resubmitting the re-rendered form (fixing only the bad field) stores it.
	form := uiFormValues(t, body)
	form.Set("paginas", "120")
	if status, body := uiCreateRow(t, client, srv, "libros", form); status != http.StatusOK {
		t.Fatalf("resubmit: %d %.400q", status, body)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", firstID(t, db, "cpt_libros")); !got.Valid || got.String != autorID {
		t.Fatalf("the relation did not survive the retry: %v", got)
	}
}

// firstID returns the id of the single row a fixture table is expected to hold.
func firstID(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM ` + table).Scan(&id); err != nil {
		t.Fatalf("first id of %s: %v", table, err)
	}
	return id
}
