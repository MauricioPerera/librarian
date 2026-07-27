package server_test

// CONTRACT-34 acceptance tests — giving the relation search back its
// case-insensitivity, WITHOUT giving up the reachability hueco 9 bought and
// WITHOUT asking the database to fold anything.
//
// The mechanism under test is one column and one function: every dynamic type
// created from this contract on carries the system column `_search_fold`, the
// application writes into it the lowercased copy of the field the search filters
// on (schema.FoldSearchText, in Go), and the search binds the needle through the
// SAME function. The database keeps doing the only thing it does identically on
// both engines: an exact substring test.
//
// Two of these tests exist because the answer is NOT uniform across an
// installation, and the contract's hardest requirement is that the difference is
// never silent: a type created BEFORE this contract has no such column (the
// startup path creates missing tables and never alters an existing one), so its
// search is still case-sensitive — and the form has to say which of the two modes
// applies. legacyUnfoldType below builds exactly such a table so the old mode is
// asserted against a real one instead of being described.
//
// Reuses grant, getBody, uiDo, openUITLS, loginUI, uiCreateContentType,
// uiCreateTypeWithReference, uiCreateRow, uiFormValues, uiSearchReference,
// fragmentOptionValues, manyAuthors, relationFixture, htmlEscapeForTest,
// uiFieldIDs, postEditForm, dynamicColumns, tableColumns and strconvItoa from the
// sibling test files.

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// legacyUnfoldType turns an already-created type into what a type created BEFORE
// CONTRACT-34 looks like: no `_search_fold` column and no row in the fold
// register. It is the only way to test the pre-existing half without carrying a
// database fixture around, and it is honest about what it simulates — the two
// statements are exactly the two things this contract adds, undone.
//
// It is a TEST-ONLY surgery on purpose: no production path removes the column,
// because no production path may (the register and the column are written by one
// transaction and read back together).
func legacyUnfoldType(t *testing.T, db *sql.DB, typeName string) {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE "cpt_` + typeName + `" DROP COLUMN "_search_fold"`); err != nil {
		t.Fatalf("drop the folded column of %q: %v", typeName, err)
	}
	if _, err := db.Exec(
		`DELETE FROM content_type_folds WHERE content_type_id = (SELECT id FROM content_types WHERE name = ?)`,
		typeName); err != nil {
		t.Fatalf("delete the fold marker of %q: %v", typeName, err)
	}
}

// TestSearchFindsTheOtherCaseBeyondTheCap is THE proof that gives this contract
// its reason to exist, and it is deliberately the CONTRACT-32 reachability test
// with the case flipped: the row is pushed past the offered page of 100 first, so
// a pass cannot be explained by the row having been in the page all along.
//
//	`borges` finds `BORGES`   — ASCII, the case everybody types.
//	`ñandú`  finds `ÑANDÚ`    — non-ASCII, exactly where SQLite's LIKE and
//	                            PostgreSQL's ILIKE disagreed, which is the reason
//	                            CONTRACT-33 had to give up folding in the database.
func TestSearchFindsTheOtherCaseBeyondTheCap(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	borgesID := relationFixture(t, db, client, srv, "BORGES")
	if status, body := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {"ÑANDÚ"}}); status != http.StatusOK {
		t.Fatalf("create ÑANDÚ: %d %.300q", status, body)
	}
	var nanduID string
	if err := db.QueryRow(`SELECT id FROM cpt_autores WHERE nombre = ?`, "ÑANDÚ").Scan(&nanduID); err != nil {
		t.Fatalf("id of ÑANDÚ: %v", err)
	}
	// 105 more rows, so BOTH targets are past the cap of 100 the form offers.
	manyAuthors(t, client, srv, 105)

	_, page := getBody(t, client, srv.URL+"/admin/content/libros/new")
	for _, id := range []string{borgesID, nanduID} {
		if strings.Contains(page, `<option value="`+id+`"`) {
			t.Fatalf("the row %q is still inside the offered page — the fixture does not reproduce hueco 9's case", id)
		}
	}
	if !strings.Contains(page, `id="reference-truncated-autor"`) {
		t.Fatalf("the form does not say the list is truncated: %.900q", page)
	}

	for _, c := range []struct{ query, want, id string }{
		{"borges", "BORGES", borgesID},
		{"ñandú", "ÑANDÚ", nanduID},
		// The mirror direction too: an admin who types the row the way it is
		// written must not be punished for it either.
		{"BORGES", "BORGES", borgesID},
		{"ÑANDÚ", "ÑANDÚ", nanduID},
	} {
		status, fragment := uiSearchReference(t, client, srv, "libros", "autor", c.query, "")
		if status != http.StatusOK {
			t.Fatalf("search %q: status %d", c.query, status)
		}
		values, ok := fragmentOptionValues(t, fragment)
		if !ok {
			t.Fatalf("the fragment has no selector: %.400q", fragment)
		}
		if len(values) != 2 || values[1] != c.id {
			t.Fatalf("searching %q offers %v, want the empty option plus %q (%s)", c.query, values, c.id, c.want)
		}
		t.Logf("THE EXACT BOX for %q (the row is beyond the cap of 100):\n%s", c.query, strings.TrimSpace(fragment))
	}

	// Reachability is not just "it appears": the row can be CHOSEN and STORED,
	// which is the whole point of hueco 9 and must survive this contract.
	if status, body := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"El Aleph"}, "autor": {borgesID}}); status != http.StatusOK {
		t.Fatalf("create with the searched author: %d %.400q", status, body)
	}
	var libroID string
	if err := db.QueryRow(`SELECT id FROM cpt_libros WHERE titulo = ?`, "El Aleph").Scan(&libroID); err != nil {
		t.Fatalf("id of the book: %v", err)
	}
	if got := storedRelation(t, db, "cpt_libros", "autor", libroID); !got.Valid || got.String != borgesID {
		t.Fatalf("autor = %v, want the row reached by searching in the other case (%q)", got, borgesID)
	}
}

// TestFoldFollowsTheValue is T3 as a PROPERTY, not as three separate happy
// paths: after every operation that can change what a row's searchable text is,
// the search must find it by the CURRENT text and must NOT find it by the
// previous one. A fold that lags is worse than no fold — the panel would offer a
// row under a name it no longer has, and the admin would pick it believing the
// listing.
//
// The three operations are the three writers of the column: the generic CRUD
// insert, the generic CRUD update, and the CONTRACT-18 table rebuild (where the
// searched field can even change its NAME under the same values).
func TestFoldFollowsTheValue(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	relationFixture(t, db, client, srv, "PRIMERO")
	var autorID string
	if err := db.QueryRow(`SELECT id FROM cpt_autores WHERE nombre = ?`, "PRIMERO").Scan(&autorID); err != nil {
		t.Fatalf("id of the author: %v", err)
	}

	// 1 — WRITE. The fold exists from the very first insert.
	assertSearchFinds(t, client, srv, "primero", autorID, true)

	// 2 — EDIT the row through the panel, as a browser would.
	_, editForm := getBody(t, client, srv.URL+"/admin/content/autores/"+autorID+"/edit")
	form := uiFormValues(t, editForm)
	form.Set("nombre", "SEGUNDO")
	resp := uiDo(t, client, http.MethodPut, srv.URL+"/admin/content/autores/"+autorID, form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update the row: status %d", resp.StatusCode)
	}
	assertSearchFinds(t, client, srv, "segundo", autorID, true)
	assertSearchFinds(t, client, srv, "primero", autorID, false)

	// And the stored fold is the fold of the CURRENT value, byte for byte —
	// asserted against the database rather than inferred from the search, so a
	// search that happened to work for another reason cannot hide a stale value.
	var fold sql.NullString
	if err := db.QueryRow(`SELECT "_search_fold" FROM cpt_autores WHERE id = ?`, autorID).Scan(&fold); err != nil {
		t.Fatalf("read the fold: %v", err)
	}
	if !fold.Valid || fold.String != "segundo" {
		t.Fatalf("_search_fold = %v, want %q", fold, "segundo")
	}

	// The THIRD writer of the column — the CONTRACT-18 rebuild, where the searched
	// field can be renamed, removed or displaced — cannot be exercised on THIS type
	// at all, and that limit is measured in TestRebuildIsRefusedForARelationTarget
	// below. It is covered instead in internal/store (TestRebuildRecomputesTheFold),
	// on a type nothing references.
}

// TestRebuildIsRefusedForARelationTarget is the MEASUREMENT behind this
// contract's T2 answer, and it is a test rather than a paragraph because the
// conclusion it supports is "less was delivered".
//
// T2 proposed migrating the types that already exist by reusing
// store.EditContentType, which rebuilds a type's table in one transaction. That
// rebuild DROPS the real table and re-creates it under the same name (compat has
// no ALTER TABLE and no RENAME), so CONTRACT-27's guard refuses it for any type
// something else references — a foreign key forbids exactly that drop, and this
// project never emits CASCADE.
//
// The set that guard excludes is not a corner: the relation search only ever runs
// over the TARGET of a relation, so "types the search exists for" and "types the
// rebuild refuses" are the SAME SET. A migration built on the rebuild could
// therefore never reach a single type whose search is the thing being fixed. That
// is why no migration is shipped and why the form states the mode instead.
func TestRebuildIsRefusedForARelationTarget(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	relationFixture(t, db, client, srv, "Borges")

	// `autores` is the TARGET of `libros.autor`, i.e. precisely a type the panel
	// searches. Its field editor is not merely hard to use: it is not offered.
	status, page := getBody(t, client, srv.URL+"/admin/content-types/autores/edit")
	if status != http.StatusOK {
		t.Fatalf("edit page of the target: status %d", status)
	}
	if !strings.Contains(page, "referenced-block") {
		t.Fatalf("the edit page of a relation target is NOT blocked — the T2 measurement is stale: %.1200q", page)
	}
	if len(uiFieldIDs(t, client, srv, "autores")) != 0 {
		t.Fatalf("the blocked page still renders an editable field list")
	}
	// And the write path refuses it too, not only the page.
	form := url.Values{}
	form.Add("field_id", "")
	form.Add("field_name", "apodo")
	form.Add("field_type", "text")
	if status, body := postEditForm(t, client, srv, "autores", form); status == http.StatusSeeOther {
		t.Fatalf("the rebuild of a referenced target was ACCEPTED: %.500q", body)
	}
	t.Log("MEASURED: the CONTRACT-18 rebuild is unavailable for a relation target, which is the whole set the search serves")
}

// assertSearchFinds asserts that searching `query` over the `autores` target does
// (or does not) offer the given row id.
func assertSearchFinds(t *testing.T, client *http.Client, srv *httptest.Server, query, id string, want bool) {
	t.Helper()
	status, fragment := uiSearchReference(t, client, srv, "libros", "autor", query, "")
	if status != http.StatusOK {
		t.Fatalf("search %q: status %d", query, status)
	}
	values, ok := fragmentOptionValues(t, fragment)
	if !ok {
		t.Fatalf("the fragment has no selector: %.400q", fragment)
	}
	found := false
	for _, v := range values {
		if v == id {
			found = true
		}
	}
	if found != want {
		t.Fatalf("searching %q: row offered = %v, want %v (options %v)", query, found, want, values)
	}
}

// TestUnfoldedTypeStaysCaseSensitiveAndTheFormSaysSo is the requirement the
// contract calls non-negotiable: two types that behave differently may not look
// the same. A type without the column keeps the CONTRACT-33 behaviour exactly —
// nothing about it silently changed — and the form states which mode is in force
// next to the box where the searching happens, in both the folded and the
// unfolded case.
func TestUnfoldedTypeStaysCaseSensitiveAndTheFormSaysSo(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	borgesID := relationFixture(t, db, client, srv, "BORGES")

	// Folded (as created): the form states the insensitive mode.
	_, page := getBody(t, client, srv.URL+"/admin/content/libros/new")
	if !strings.Contains(page, `id="reference-search-fold-autor"`) || !strings.Contains(page, "No distingue mayúsculas") {
		t.Fatalf("a folded target does not state its mode: %.1500q", page)
	}

	legacyUnfoldType(t, db, "autores")

	// The shape is now the shape of a type created before this contract.
	if got := dynamicColumns(t, db, "autores"); got != "id,author_id,nombre,created_at,updated_at,metadata" {
		t.Fatalf("the simulated legacy table has the wrong shape: %s", got)
	}
	// The search still WORKS (nothing broke), and it works the old way.
	assertSearchFinds(t, client, srv, "BORGES", borgesID, true)
	assertSearchFinds(t, client, srv, "borges", borgesID, false)

	// And the form says THAT, not the other sentence.
	_, page = getBody(t, client, srv.URL+"/admin/content/libros/new")
	if !strings.Contains(page, `id="reference-search-case-autor"`) {
		t.Fatalf("an unfolded target does not warn that its search distinguishes case: %.1500q", page)
	}
	if strings.Contains(page, `id="reference-search-fold-autor"`) {
		t.Fatalf("an unfolded target claims to ignore case: %.1500q", page)
	}
	if !strings.Contains(page, "Distingue mayúsculas") {
		t.Errorf("the warning does not say what it warns about: %.1500q", page)
	}
}

// TestFoldColumnIsInvisibleToTheAdmin is T1's other half: the column is a system
// column like id or created_at. It is not a field, so it must not appear as a
// control in the form, as a cell in the listing, or as a key of the JSON a
// content read returns — an admin has no reason to know it exists.
func TestFoldColumnIsInvisibleToTheAdmin(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	if status, body := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {"Borges"}}); status != http.StatusOK {
		t.Fatalf("create row: %d %.300q", status, body)
	}
	var rowID string
	if err := db.QueryRow(`SELECT id FROM cpt_autores WHERE nombre = ?`, "Borges").Scan(&rowID); err != nil {
		t.Fatalf("row id: %v", err)
	}
	// It IS in the table — otherwise this test would prove nothing.
	if got := dynamicColumns(t, db, "autores"); !strings.Contains(got, "_search_fold") {
		t.Fatalf("the folded column was not created: %s", got)
	}
	for _, u := range []string{
		"/admin/content/autores",
		"/admin/content/autores/new",
		"/admin/content/autores/" + rowID + "/edit",
		"/admin/content-types",
		"/admin/content-types/autores/edit",
	} {
		if _, body := getBody(t, client, srv.URL+u); strings.Contains(body, "_search_fold") {
			t.Errorf("%s leaks the system column to the admin: %.600q", u, body)
		}
	}
	if _, body := getBody(t, client, srv.URL+"/content/autores/"+rowID); strings.Contains(body, "_search_fold") {
		t.Errorf("the JSON read leaks the system column: %.600q", body)
	}
}

// TestFoldColumnNameIsUnreachableForADeclaredField is why the name starts with an
// underscore. It is not a reservation to be remembered: the identifier gate
// requires a LETTER first, so there is no input — through the JSON API or through
// the panel — that produces the name. Asserting the refusal is what turns the
// argument into a guarantee.
func TestFoldColumnNameIsUnreachableForADeclaredField(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")

	form := url.Values{"name": {"cosas"}}
	form.Add("field_name", "_search_fold")
	form.Add("field_type", "text")
	resp := uiDo(t, client, http.MethodPost, srv.URL+"/admin/content-types", form)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatalf("a field named like the system column was ACCEPTED")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM content_types WHERE name = 'cosas'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("the rejected definition was persisted anyway")
	}
}
