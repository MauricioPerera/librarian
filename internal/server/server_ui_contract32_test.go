package server_test

// CONTRACT-32 acceptance tests — searching the target row of a relation, WITHOUT
// rebuilding the silent loss CONTRACT-30 closed.
//
// THE CENTRAL CASE IS AGAIN A BROWSER SIMULATION. The defect this contract is
// most at risk of reintroducing is not "the search returns the wrong rows": it is
// "the search returns rows that do not include the CURRENT one, the <select>
// falls back to «sin relación», and the next save clears a relation nobody
// touched". Only a test that submits what the RENDERED control would submit can
// see that happen, so TestSearchDoesNotDropTheCurrentRelation reads the fragment
// htmx would swap in, replaces the option block in the page exactly as the
// browser would, and posts the result through uiFormValues.
//
// Everything goes through the real mux with a real session (openUITLS + loginUI).
// Reuses grant, getBody, uiDo, uiCreateContentType, uiCreateTypeWithReference,
// uiCreateRow, uiFormValues, storedRelation, relationFixture, strconvItoa and
// nonexistentID from the sibling test files.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// uiSearchReference asks for the options fragment the search box would ask for:
// the relation, the typed text, and — exactly as hx-include does — the value the
// <select> currently holds.
func uiSearchReference(t *testing.T, client *http.Client, srv *httptest.Server, typeName, relation, query, current string) (int, string) {
	t.Helper()
	params := url.Values{"name": {relation}, "q-" + relation: {query}}
	if current != "" {
		params.Set(relation, current)
	}
	return getBody(t, client, srv.URL+"/admin/content/"+typeName+"/reference?"+params.Encode())
}

// fragmentOptionValues returns the option values of the single <select> in an
// options fragment, in document order, and whether the fragment has a select at
// all.
func fragmentOptionValues(t *testing.T, fragment string) ([]string, bool) {
	t.Helper()
	sel := selectRe.FindStringSubmatch(fragment)
	if sel == nil {
		return nil, false
	}
	out := []string{}
	for _, opt := range optionRe.FindAllStringSubmatch(sel[2], -1) {
		out = append(out, opt[1])
	}
	return out, true
}

// manyAuthors creates n extra authors on top of the fixture's one, so the
// oldest is pushed out of the offered page of 100.
func manyAuthors(t *testing.T, client *http.Client, srv *httptest.Server, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if status, body := uiCreateRow(t, client, srv, "autores",
			url.Values{"nombre": {"relleno_" + strconvItoa(i)}}); status != http.StatusOK {
			t.Fatalf("create relleno %d: %d %.300q", i, status, body)
		}
	}
}

// TestSearchReachesARowBeyondTheCap is T1: the row the cut left out is reachable
// from the panel, with no JavaScript of this project's own, and choosing it
// actually stores it.
func TestSearchReachesARowBeyondTheCap(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	oldestID := relationFixture(t, db, client, srv, "Sarmiento")
	manyAuthors(t, client, srv, 105)

	// The plain form no longer offers the oldest author, and says the list is cut.
	_, page := getBody(t, client, srv.URL+"/admin/content/libros/new")
	if strings.Contains(page, `<option value="`+oldestID+`"`) {
		t.Fatalf("the oldest author is still in the offered page — the fixture does not reproduce the problem")
	}
	if !strings.Contains(page, `id="reference-truncated-autor"`) {
		t.Fatalf("the form does not say the list is truncated: %.900q", page)
	}
	// T1: the search control exists, is server-driven, and carries the current
	// value with it.
	if !strings.Contains(page, `name="q-autor"`) {
		t.Fatalf("the form has no search control for the relation: %.900q", page)
	}
	if !strings.Contains(page, `hx-get="/admin/content/libros/reference?name=autor"`) {
		t.Fatalf("the search control does not reload from the server: %.900q", page)
	}
	if !strings.Contains(page, `hx-include="#reference-select-autor"`) {
		t.Fatalf("the search does not send the current value with it: %.900q", page)
	}
	if card := formCardRe.FindString(page); strings.Contains(card, "<script") {
		t.Errorf("the form grew a script of its own: %.900q", card)
	}

	// The search finds it — and it was NOT among the rows the form had.
	status, fragment := uiSearchReference(t, client, srv, "libros", "autor", "Sarmiento", "")
	if status != http.StatusOK {
		t.Fatalf("search status = %d: %.300q", status, fragment)
	}
	values, ok := fragmentOptionValues(t, fragment)
	if !ok {
		t.Fatalf("the fragment has no selector: %.400q", fragment)
	}
	if len(values) != 2 || values[0] != "" || values[1] != oldestID {
		t.Fatalf("the search offers %v, want the empty option plus %q", values, oldestID)
	}
	// CONTRACT-33 T4 asks for this proof WITH THE EXACT BOX, so the box is printed
	// rather than described: what an admin gets back after typing the name of a row
	// the offered page had already cut off.
	t.Logf("THE EXACT BOX the search returns for %q (the row is beyond the cap of %d):\n%s",
		"Sarmiento", 100, strings.TrimSpace(fragment))

	// And picking it actually stores it: the search is a way to REACH the row,
	// not a separate write path.
	if status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Facundo"}, "autor": {oldestID}}); status != http.StatusOK {
		t.Fatalf("create with the searched author: %d %.400q", status, body)
	}
	var libroID string
	if err := db.QueryRow(`SELECT id FROM cpt_libros WHERE titulo = ?`, "Facundo").Scan(&libroID); err != nil {
		t.Fatalf("id of the book: %v", err)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", libroID); !got.Valid || got.String != oldestID {
		t.Fatalf("autor = %v, want the row reached through the search (%q)", got, oldestID)
	}
	t.Log("OK: a row outside the offered page was reached, chosen and stored from the panel")
}

// TestSearchDoesNotDropTheCurrentRelation is THE regression case of this
// contract — the one that must FAIL if the rescue of the current value is
// removed from referenceInput.
//
// It searches for text that matches NOTHING related to the stored author, swaps
// the returned fragment into the page exactly as htmx would, and submits the
// resulting form. The relation must still be there afterwards: the admin typed in
// a search box and pressed save, which is not consent to erase a relation.
func TestSearchDoesNotDropTheCurrentRelation(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	autorID := relationFixture(t, db, client, srv, "Cortázar")
	if status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Rayuela"}, "autor": {autorID}}); status != http.StatusOK {
		t.Fatalf("create libro: %d %.400q", status, body)
	}
	libroID := firstID(t, db, "cpt_libros")

	_, editForm := getBody(t, client, srv.URL+"/admin/content/libros/"+libroID+"/edit")
	if !strings.Contains(editForm, `<option value="`+autorID+`" selected>`) {
		t.Fatalf("the stored relation is not preselected: %.900q", editForm)
	}

	// The admin types something that matches nothing. htmx sends the CURRENT value
	// of the <select> along with the text; the fragment comes back.
	_, fragment := uiSearchReference(t, client, srv, "libros", "autor", "zzz", autorID)
	if !strings.Contains(fragment, `id="reference-no-matches-autor"`) {
		t.Fatalf("a fruitless search does not say so: %.400q", fragment)
	}
	if !strings.Contains(fragment, `<option value="`+autorID+`" selected>`) {
		t.Fatalf("THE CURRENT RELATION IS MISSING FROM THE SEARCH RESULT — the next save would erase it: %.400q", fragment)
	}

	// Swap it in exactly where htmx would (outerHTML over #reference-options-autor)
	// and submit the form the browser now holds.
	swapped := replaceOptionsBlock(t, editForm, "autor", fragment)
	form := uiFormValues(t, swapped)
	if got := form.Get("autor"); got != autorID {
		t.Fatalf("after searching, the form would submit autor=%q, want %q", got, autorID)
	}
	form.Set("titulo", "Rayuela (2a ed)")
	resp := uiDo(t, client, http.MethodPut, srv.URL+"/admin/content/libros/"+libroID, form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200", resp.StatusCode)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", libroID); !got.Valid || got.String != autorID {
		t.Fatalf("SEARCHING AND SAVING ERASED THE RELATION: autor = %v, want %q", got, autorID)
	}
	t.Log("OK: a search that matches nothing keeps the current relation selected and stored")
}

// replaceOptionsBlock performs the swap htmx performs: the fragment replaces the
// element it is targeted at, and NOTHING else of the page changes.
func replaceOptionsBlock(t *testing.T, page, relation, fragment string) string {
	t.Helper()
	open := strings.Index(page, `<div id="reference-options-`+relation+`">`)
	if open < 0 {
		t.Fatalf("no options block for %q in the page: %.900q", relation, page)
	}
	// The block has no nested <div>, so its first </div> closes it.
	closeAt := strings.Index(page[open:], "</div>")
	if closeAt < 0 {
		t.Fatalf("unterminated options block for %q", relation)
	}
	return page[:open] + fragment + page[open+closeAt+len("</div>"):]
}

// TestSearchKeepsTheRestOfTheFormIntact is the red-team item that would make the
// feature worse than not having it: a swap that reaches beyond the option list
// takes away whatever the person was in the middle of typing.
func TestSearchKeepsTheRestOfTheFormIntact(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	autorID := relationFixture(t, db, client, srv, "Bioy")
	if status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"La invención"}, "autor": {autorID}}); status != http.StatusOK {
		t.Fatalf("create libro: %d %.400q", status, body)
	}
	libroID := firstID(t, db, "cpt_libros")
	_, editForm := getBody(t, client, srv.URL+"/admin/content/libros/"+libroID+"/edit")

	// The person edits the title but has not saved, then searches.
	halfTyped := strings.Replace(editForm,
		`value="La invención"`, `value="La invención de Mor"`, 1)
	if halfTyped == editForm {
		t.Fatalf("could not simulate half-typed text: %.900q", editForm)
	}
	_, fragment := uiSearchReference(t, client, srv, "libros", "autor", "Bio", autorID)

	// The fragment is the option block and NOTHING more: no title input, no
	// submit button, no search box (which is why the typed query survives too).
	if strings.Contains(fragment, `name="titulo"`) {
		t.Fatalf("the fragment carries the rest of the form and would overwrite it: %.400q", fragment)
	}
	if strings.Contains(fragment, `name="q-autor"`) {
		t.Fatalf("the fragment replaces the search box and would lose the typed text: %.400q", fragment)
	}
	if strings.Contains(fragment, "<form") || strings.Contains(fragment, "<button") {
		t.Fatalf("the fragment is not scoped to the options: %.400q", fragment)
	}

	swapped := replaceOptionsBlock(t, halfTyped, "autor", fragment)
	form := uiFormValues(t, swapped)
	if got := form.Get("titulo"); got != "La invención de Mor" {
		t.Fatalf("the half-typed title did not survive the search: %q", got)
	}
	if got := form.Get("q-autor"); got != "" && got != "Bio" {
		t.Errorf("unexpected search value after the swap: %q", got)
	}
	t.Log("OK: searching leaves the half-typed title and the search text alone")
}

// TestSearchEmptyIsTodaysBehaviour: T3 — an empty search is NOT an empty list.
func TestSearchEmptyIsTodaysBehaviour(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	relationFixture(t, db, client, srv, "Walsh")
	manyAuthors(t, client, srv, 105)

	_, page := getBody(t, client, srv.URL+"/admin/content/libros/new")
	pageOptions, ok := fragmentOptionValues(t, formCardRe.FindString(page))
	if !ok {
		t.Fatalf("no selector in the form: %.900q", page)
	}
	status, fragment := uiSearchReference(t, client, srv, "libros", "autor", "", "")
	if status != http.StatusOK {
		t.Fatalf("empty search status = %d", status)
	}
	fragmentOptions, ok := fragmentOptionValues(t, fragment)
	if !ok {
		t.Fatalf("no selector in the fragment: %.400q", fragment)
	}
	if len(fragmentOptions) != len(pageOptions) {
		t.Fatalf("empty search offers %d options, the form offers %d — they must be the same page",
			len(fragmentOptions), len(pageOptions))
	}
	for i := range pageOptions {
		if pageOptions[i] != fragmentOptions[i] {
			t.Fatalf("option %d differs: form %q, empty search %q", i, pageOptions[i], fragmentOptions[i])
		}
	}
	if !strings.Contains(fragment, `id="reference-truncated-autor"`) {
		t.Errorf("the empty search stopped saying the list is cut: %.400q", fragment)
	}
	t.Log("OK: an empty search is exactly the 100 most recent, the behaviour of today")
}

// TestSearchWithNoMatchesStaysUsable: T3 — it says so, and the control still
// works. The empty option is still there, so «sin relación» stays expressible.
func TestSearchWithNoMatchesStaysUsable(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	relationFixture(t, db, client, srv, "Arlt")
	_, fragment := uiSearchReference(t, client, srv, "libros", "autor", "no-existe-nadie-asi", "")
	values, ok := fragmentOptionValues(t, fragment)
	if !ok {
		t.Fatalf("a fruitless search left no control at all: %.400q", fragment)
	}
	if len(values) != 1 || values[0] != "" {
		t.Fatalf("a fruitless search offers %v, want only the empty option", values)
	}
	if !strings.Contains(fragment, `id="reference-no-matches-autor"`) {
		t.Fatalf("a fruitless search does not say so: %.400q", fragment)
	}
	if strings.Contains(fragment, `id="no-reference-rows-autor"`) {
		t.Errorf("no matches was reported as «no hay filas», which is a different thing: %.400q", fragment)
	}
}

// TestSearchIsNotAValidation: T3 — checkReferenceTargets and bindReference stay
// the only authority. A crafted POST naming a row the search never offered is
// still refused with the same 400, and an unknown relation name is a 404 rather
// than a query built from it.
func TestSearchIsNotAValidation(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	relationFixture(t, db, client, srv, "Saer")

	status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Inventado"}, "autor": {nonexistentID}})
	if status != http.StatusBadRequest {
		t.Fatalf("a crafted POST with an unknown target = %d, want 400: %.300q", status, body)
	}

	// The fragment itself refuses to be pointed at something that is not a
	// declared relation of this type.
	if status, _ := uiSearchReference(t, client, srv, "libros", "titulo", "x", ""); status != http.StatusNotFound {
		t.Errorf("the fragment for a FIELD name = %d, want 404", status)
	}
	if status, _ := uiSearchReference(t, client, srv, "libros", "no_existe", "x", ""); status != http.StatusNotFound {
		t.Errorf("the fragment for an unknown relation = %d, want 404", status)
	}
	if status, _ := getBody(t, client, srv.URL+"/admin/content/libros/reference"); status != http.StatusNotFound {
		t.Errorf("the fragment with no relation name = %d, want 404", status)
	}
	if status, _ := uiSearchReference(t, client, srv, "no_existe", "autor", "x", ""); status != http.StatusNotFound {
		t.Errorf("the fragment for an unknown type = %d, want 404", status)
	}
}

// TestSearchWildcardsAreNotWildcards is the red-team question about `%` and `_`:
// a person typing them means those characters, not "match anything".
//
// CONTRACT-33 made this cheaper to guarantee, not harder. Under `like` the bound
// pattern had to widen a typed `%` into `_` so it could not match everything, and
// this test proved the widening did not leak. `contains` has NO wildcards and NO
// escape character, so `%`, `_` and `\` are three ordinary characters that go
// into the bound needle exactly as typed. The assertions are unchanged on
// purpose: the same questions, answered by a simpler mechanism.
func TestSearchWildcardsAreNotWildcards(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	relationFixture(t, db, client, srv, "Borges")
	for _, name := range []string{"des%cuento", "des_cuento", "descuento", `barra\invertida`, "apo'strofe"} {
		if status, body := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {name}}); status != http.StatusOK {
			t.Fatalf("create autor %q: %d %.300q", name, status, body)
		}
	}
	cases := []struct {
		query string
		want  []string // labels that must be offered, in any order
		deny  []string // labels that must NOT be offered
	}{
		{"%", []string{"des%cuento"}, []string{"descuento", "des_cuento", "Borges"}},
		{"_", []string{"des_cuento"}, []string{"descuento", "des%cuento", "Borges"}},
		{"des%cuento", []string{"des%cuento"}, []string{"descuento", "des_cuento"}},
		{"des_cuento", []string{"des_cuento"}, []string{"descuento", "des%cuento"}},
		{`barra\inv`, []string{`barra\invertida`}, []string{"descuento", "Borges"}},
		{"apo'st", []string{"apo'strofe"}, []string{"descuento", "Borges"}},
		{"descuento", []string{"descuento"}, []string{"des%cuento", "des_cuento"}},
	}
	for _, c := range cases {
		status, fragment := uiSearchReference(t, client, srv, "libros", "autor", c.query, "")
		if status != http.StatusOK {
			t.Fatalf("search %q: status %d", c.query, status)
		}
		for _, want := range c.want {
			if !strings.Contains(fragment, htmlEscapeForTest(want)) {
				t.Errorf("search %q does not offer %q: %.500q", c.query, want, fragment)
			}
		}
		for _, deny := range c.deny {
			if strings.Contains(fragment, htmlEscapeForTest(deny)) {
				t.Errorf("search %q wrongly offers %q — the wildcard leaked: %.500q", c.query, deny, fragment)
			}
		}
	}
}

// htmlEscapeForTest mirrors what html/template does to a label inside an
// <option>, so a test can look for the text a person would read.
func htmlEscapeForTest(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}

// TestSearchIsCaseSensitiveIncludingAccents is the REWRITTEN twin of
// TestSearchIsCaseInsensitiveIncludingAccents, and it is rewritten rather than
// deleted on purpose: a test dropped during a migration is a property that stops
// being true with nobody recording that it did.
//
// WHAT IT USED TO ASSERT (CONTRACT-32): `ñandú` finds `ÑANDÚ`. The `like` WHERE
// folded at least ASCII on both engines, so the true matches always reached Go
// and referenceSearchMatches — case-insensitive over the whole Unicode range —
// could level the two engines UP to the better behaviour.
//
// WHAT IT ASSERTS NOW (CONTRACT-33): `ñandú` does NOT find `ÑANDÚ`. compat v0.7.0
// refuses `like`, and the portable replacement `contains` compiles to
// instr/strpos, which do not fold. The row is discarded by the WHERE, so Go never
// sees it and no amount of folding in Go can bring it back. The regression is
// real and visible in the panel; it is registered as a gap in docs/PENDIENTES.md
// with its way out, and the form states it next to the search box.
//
// The accented cases are kept exactly because that is where the old divergence
// lived: what changed is the ANSWER, not the questions asked.
func TestSearchIsCaseSensitiveIncludingAccents(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	relationFixture(t, db, client, srv, "ÑANDÚ")
	for _, name := range []string{"ñandú menor", "Ábaco", "abaco sin tilde"} {
		if status, body := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {name}}); status != http.StatusOK {
			t.Fatalf("create autor %q: %d %.300q", name, status, body)
		}
	}
	cases := []struct {
		query string
		want  []string
		deny  []string
	}{
		// The case barrier, in both directions. Each query finds the row spelled
		// its way and NOT the one spelled the other way.
		{"ñandú", []string{"ñandú menor"}, []string{"ÑANDÚ", "Ábaco", "abaco sin tilde"}},
		{"ÑANDÚ", []string{"ÑANDÚ"}, []string{"ñandú menor", "Ábaco"}},
		{"Ábaco", []string{"Ábaco"}, []string{"abaco sin tilde", "ÑANDÚ"}},
		{"abaco", []string{"abaco sin tilde"}, []string{"Ábaco", "ÑANDÚ"}},
		// And the accent itself is NOT the barrier: `baco` — a pure-ASCII needle
		// whose match runs right after a non-ASCII letter — finds both rows. So
		// what `contains` lost is case folding specifically, not the ability to
		// match text with accents in it.
		{"baco", []string{"Ábaco", "abaco sin tilde"}, []string{"ÑANDÚ", "ñandú menor"}},
	}
	for _, c := range cases {
		status, fragment := uiSearchReference(t, client, srv, "libros", "autor", c.query, "")
		if status != http.StatusOK {
			t.Fatalf("search %q: status %d", c.query, status)
		}
		for _, want := range c.want {
			if !strings.Contains(fragment, htmlEscapeForTest(want)) {
				t.Errorf("search %q does not offer %q: %.500q", c.query, want, fragment)
			}
		}
		for _, deny := range c.deny {
			if strings.Contains(fragment, htmlEscapeForTest(deny)) {
				t.Errorf("search %q wrongly offers %q: %.500q", c.query, deny, fragment)
			}
		}
	}

	// THE REGRESSION, ASSERTED AS A WHOLE and not only as an absent label: a query
	// that differs from the row ONLY in case answers «ninguna coincide». That is
	// exactly what the admin sees, and it is why the form has to say the search
	// distinguishes case — otherwise this message reads as "that row is not here".
	_, fragment := uiSearchReference(t, client, srv, "libros", "autor", "ábaco", "")
	if !strings.Contains(fragment, `id="reference-no-matches-autor"`) {
		t.Errorf("searching «ábaco» against the row «Ábaco» found something — case folding came back without the docs: %.500q", fragment)
	}

	// And the form WARNS about it, where the searching happens.
	_, page := getBody(t, client, srv.URL+"/admin/content/libros/new")
	if !strings.Contains(page, `id="reference-search-case-autor"`) {
		t.Fatalf("the form does not warn that the search distinguishes case: %.1200q", page)
	}
	if !strings.Contains(page, "Distingue mayúsculas") {
		t.Errorf("the warning does not say what it warns about: %.1200q", page)
	}
}

// TestSearchAbsentForAFieldlessTarget: T3 — a target whose label IS the id has
// nothing to search by. The form must not break, must not offer a control that
// cannot work, and must SAY why.
func TestSearchAbsentForAFieldlessTarget(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	// `sin_campos` declares no field at all; `numerico` declares a non-text first
	// field, which PostgreSQL has no LIKE operator for.
	uiCreateContentType(t, client, srv, "sin_campos")
	uiCreateContentType(t, client, srv, "numerico", [2]string{"numero", "integer"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "cosas",
		[][2]string{{"titulo", "text"}},
		[][2]string{{"a", "sin_campos"}, {"b", "numerico"}}); status != http.StatusOK {
		t.Fatalf("create cosas: %d %.400q", status, body)
	}
	if status, _ := uiCreateRow(t, client, srv, "sin_campos", url.Values{}); status != http.StatusOK {
		t.Fatalf("create sin_campos row: %d", status)
	}
	if status, _ := uiCreateRow(t, client, srv, "numerico", url.Values{"numero": {"7"}}); status != http.StatusOK {
		t.Fatalf("create numerico row: %d", status)
	}

	_, page := getBody(t, client, srv.URL+"/admin/content/cosas/new")
	if strings.Contains(page, `name="q-a"`) || strings.Contains(page, `name="q-b"`) {
		t.Fatalf("a search control was offered for a target that cannot be searched: %.900q", page)
	}
	if !strings.Contains(page, `<select name="a"`) || !strings.Contains(page, `<select name="b"`) {
		t.Fatalf("the selectors themselves disappeared: %.900q", page)
	}
	// And the fragment, reached by hand, answers the unfiltered page instead of
	// failing or claiming there is nothing.
	for _, relation := range []string{"a", "b"} {
		status, fragment := uiSearchReference(t, client, srv, "cosas", relation, "cualquier cosa", "")
		if status != http.StatusOK {
			t.Fatalf("hand-made search on %q: status %d: %.300q", relation, status, fragment)
		}
		values, ok := fragmentOptionValues(t, fragment)
		if !ok || len(values) != 2 {
			t.Fatalf("hand-made search on %q returned %v (ok=%v), want the unfiltered page", relation, values, ok)
		}
	}
	// Creating with both relations still works: nothing about the missing search
	// weakened the control.
	sinCamposID := firstID(t, db, "cpt_sin_campos")
	numericoID := firstID(t, db, "cpt_numerico")
	if status, body := uiCreateRow(t, client, srv, "cosas", url.Values{
		"titulo": {"Con dos"}, "a": {sinCamposID}, "b": {numericoID},
	}); status != http.StatusOK {
		t.Fatalf("create cosas row: %d %.400q", status, body)
	}
}

// TestSearchTruncationIsStated: T1 says the cap does not move. A search that
// matches more rows than the selector offers still offers exactly the cap, and
// says the list is partial — the same honesty the unfiltered cut already owed.
func TestSearchTruncationIsStated(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	relationFixture(t, db, client, srv, "Borges")
	manyAuthors(t, client, srv, 120) // 120 rows whose name contains "relleno"

	status, fragment := uiSearchReference(t, client, srv, "libros", "autor", "relleno", "")
	if status != http.StatusOK {
		t.Fatalf("search status = %d", status)
	}
	values, ok := fragmentOptionValues(t, fragment)
	if !ok {
		t.Fatalf("no selector: %.400q", fragment)
	}
	if len(values) != 101 { // 100 rows + the empty option
		t.Fatalf("a broad search offers %d options, want 101 — the cap must not move", len(values))
	}
	if !strings.Contains(fragment, `id="reference-truncated-autor"`) {
		t.Fatalf("a truncated search does not say so: %.400q", fragment)
	}
	if !strings.Contains(fragment, "se muestran solo las primeras 100") {
		t.Errorf("the truncation notice does not state the number: %.400q", fragment)
	}
}

// TestSearchLabelsMatchTheListing: T3 — one label rule, or the same row reads
// differently on two screens. The label the search returns must be exactly the
// one the listing of the target shows.
func TestSearchLabelsMatchTheListing(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	autorID := relationFixture(t, db, client, srv, "Piglia")
	if status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Respiración"}, "autor": {autorID}}); status != http.StatusOK {
		t.Fatalf("create libro: %d %.400q", status, body)
	}
	// The listing of `libros` translates the relation with referenceOptionLabel.
	_, listing := getBody(t, client, srv.URL+"/admin/content/libros")
	wantLabel := "Piglia · " + autorID[:8]
	if !strings.Contains(listing, wantLabel) {
		t.Fatalf("the listing does not show the shared label %q: %.900q", wantLabel, listing)
	}
	_, fragment := uiSearchReference(t, client, srv, "libros", "autor", "Piglia", "")
	if !strings.Contains(fragment, `<option value="`+autorID+`">`+wantLabel+`</option>`) {
		t.Fatalf("the search label differs from the listing's: %.400q", fragment)
	}
}

// TestSearchDoesNotNeedAWritePermission: the fragment is a read of what the same
// session can already open. It must not have grown a permission of its own, and
// it must still refuse an anonymous caller.
func TestSearchDoesNotNeedAWritePermission(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")
	relationFixture(t, db, client, srv, "Borges")

	// A session with NO content permission at all can still search: `contributor`
	// is seeded with no grant of its own in this fixture.
	reader := loginUI(t, db, srv, "lector@example.com", "pw", "contributor")
	if status, _ := uiSearchReference(t, reader, srv, "libros", "autor", "Bor", ""); status != http.StatusOK {
		t.Errorf("a plain session cannot use the search: %d", status)
	}

	// No session at all is a redirect to the login, like every other admin read.
	anon := &http.Client{
		Transport: client.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := anon.Get(srv.URL + "/admin/content/libros/reference?name=autor")
	if err != nil {
		t.Fatalf("anonymous GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("anonymous search = %d, want 302 to /login", resp.StatusCode)
	}
}

// TestSearchCostDoesNotGrowWithTheTarget is the measurement CONTRACT-31's bound
// demands of anything added on top of it. The contract's other road
// (compat.Store.SearchText) reads the WHOLE target table on every keystroke; this
// one pushes the filter into the database, so the statement count of a search
// must not move when the target grows.
//
// It reuses CONTRACT-31's instrumented pool (openCountingUITLS/measureQueries):
// the driver counts every statement PREPARED, which is the only witness that
// cannot be argued with.
func TestSearchCostDoesNotGrowWithTheTarget(t *testing.T) {
	db, srv, cleanup := openCountingUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	relationFixture(t, db, client, srv, "Borges")
	manyAuthors(t, client, srv, 20)

	params := url.Values{"name": {"autor"}, "q-autor": {"relleno"}}
	url20, _ := measureQueries(t, client, srv.URL+"/admin/content/libros/reference?"+params.Encode())

	manyAuthors(t, client, srv, 200)
	url220, _ := measureQueries(t, client, srv.URL+"/admin/content/libros/reference?"+params.Encode())

	t.Logf("statements for one search: 21 target rows = %d, 221 target rows = %d", url20, url220)
	if url20 != url220 {
		t.Errorf("the cost of a search moved with the target size: %d → %d", url20, url220)
	}
}

// TestSearchRedTeamHostileAndDoubled covers the two remaining red-team items:
// a hand-made `current` that is not a uuid (or is markup), and a type with TWO
// relations to the SAME target, whose two search boxes must be independent.
func TestSearchRedTeamHostileAndDoubled(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "libros",
		[][2]string{{"titulo", "text"}},
		[][2]string{{"autor", "autores"}, {"prologuista", "autores"}}); status != http.StatusOK {
		t.Fatalf("create libros: %d %.400q", status, body)
	}
	if status, _ := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {"Borges"}}); status != http.StatusOK {
		t.Fatalf("create autor")
	}

	// TWO relations to the same target: two independent controls, two names, two
	// swap targets. A shared id would make one search rewrite the other's options.
	_, page := getBody(t, client, srv.URL+"/admin/content/libros/new")
	for _, needle := range []string{
		`name="q-autor"`, `name="q-prologuista"`,
		`id="reference-options-autor"`, `id="reference-options-prologuista"`,
		`hx-target="#reference-options-autor"`, `hx-target="#reference-options-prologuista"`,
		`hx-include="#reference-select-autor"`, `hx-include="#reference-select-prologuista"`,
	} {
		if !strings.Contains(page, needle) {
			t.Errorf("the doubled relation is missing %s: %.1200q", needle, page)
		}
	}

	// A hostile `current`: not a uuid, and markup. It must come back as an option
	// (so the admin sees what was submitted, and checkReferenceTargets refuses it
	// on save) and it must come back ESCAPED, never as live markup.
	hostile := `<script>alert(1)</script>`
	params := url.Values{"name": {"autor"}, "q-autor": {"zzz"}, "autor": {hostile}}
	status, fragment := getBody(t, client, srv.URL+"/admin/content/libros/reference?"+params.Encode())
	if status != http.StatusOK {
		t.Fatalf("hostile current: status %d: %.300q", status, fragment)
	}
	if strings.Contains(fragment, "<script") {
		t.Fatalf("the hostile current value was rendered as markup: %.400q", fragment)
	}
	if !strings.Contains(fragment, "&lt;script&gt;") {
		t.Errorf("the hostile current value was not echoed back escaped: %.400q", fragment)
	}
	// And submitting it is still a 400 from the existing validator, not a 500.
	if s, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"x"}, "autor": {hostile}}); s != http.StatusBadRequest {
		t.Errorf("a hostile relation id = %d, want 400: %.300q", s, body)
	}
}
