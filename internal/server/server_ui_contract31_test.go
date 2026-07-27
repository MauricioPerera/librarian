package server_test

// CONTRACT-31 acceptance tests — the relations, readable in the listing and
// BOUNDED in cost.
//
// THE COST IS THE CRITERION, SO THE COST IS MEASURED. The contract's T1 is not
// "the listing looks right", it is "the number of queries a listing issues does
// not grow with the number of rows it shows". An argument about the code cannot
// establish that; only counting can. So this file registers an INSTRUMENTED
// database/sql driver that wraps the very driver store.Open would have used and
// counts every statement prepared through it.
//
// WHY COUNTING PREPARES IS COUNTING STATEMENTS. database/sql runs a query through
// the connection's Queryer/Execer fast path only when the driver's *conn*
// implements it; countingConn deliberately implements neither, so every single
// Query/Exec goes through the prepare → run → close path, i.e. exactly one
// Prepare per statement executed. Nothing in librarian or in compat ever calls
// Prepare on its own (measured: no `.Prepare` call site exists in either), so no
// prepared statement is reused and the count is not inflated by bookkeeping.
//
// The instrumented pool is substituted INTO the store store.Open returned rather
// than composing a compat.Store by hand: the engine/target pairing keeps coming
// from the one place that is allowed to decide it, and only the pool underneath
// is swapped for the same file opened with the same DSN.
//
// Reuses grant, getBody, uiDo, uiCreateContentType, uiCreateTypeWithReference,
// uiCreateRow and strconvItoa from the sibling test files.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// --- the instrument ---------------------------------------------------------

const countingDriverName = "sqlite-contract31-counting"

// preparedStatements is the process-wide counter the instrumented driver feeds.
// It is package state because a database/sql driver is registered by NAME and
// therefore process-wide; no other test in this package opens that driver, so
// nothing else can move the number.
var preparedStatements atomic.Int64

var registerCounting sync.Once

type countingDriver struct{ inner driver.Driver }

func (d countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return countingConn{inner: c}, nil
}

// countingConn implements ONLY driver.Conn — no Queryer, no Execer, no
// ConnBeginTx — which is what forces database/sql to prepare every statement and
// makes the count exact. The prepared statement itself is the driver's own, so
// every interface the real one implements survives untouched.
type countingConn struct{ inner driver.Conn }

func (c countingConn) Prepare(query string) (driver.Stmt, error) {
	preparedStatements.Add(1)
	return c.inner.Prepare(query)
}

func (c countingConn) Close() error              { return c.inner.Close() }
func (c countingConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

// openCountingUITLS is openUITLS with the instrumented pool underneath.
func openCountingUITLS(t *testing.T) (*sql.DB, *httptest.Server, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "contract31.db")
	sdb, err := store.Open(compat.SQLite, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	registerCounting.Do(func() { sql.Register(countingDriverName, countingDriver{inner: sdb.DB.Driver()}) })
	if err := sdb.DB.Close(); err != nil {
		t.Fatalf("close the uninstrumented pool: %v", err)
	}
	// The same DSN compat composes for SQLite, including the connection-scoped
	// foreign-key pragma and the single connection that makes it hold.
	counted, err := sql.Open(countingDriverName, dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open the instrumented pool: %v", err)
	}
	counted.SetMaxOpenConns(1)
	sdb.DB = counted

	ctx := context.Background()
	if err := store.EnsureSchema(ctx, sdb); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.SeedCatalogs(ctx, sdb); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux, err := server.NewMux(server.Deps{Store: sdb, JWTSecret: testSecret})
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewTLSServer(mux)
	return sdb.DB, srv, func() {
		srv.Close()
		_ = sdb.Close()
	}
}

// measureQueries runs one request and returns how many statements it executed.
func measureQueries(t *testing.T, client *http.Client, u string) (int64, string) {
	t.Helper()
	before := preparedStatements.Load()
	_, body := getBody(t, client, u)
	return preparedStatements.Load() - before, body
}

// --- reading the rendered listing -------------------------------------------

var (
	listRowRe  = regexp.MustCompile(`(?s)<tr id="content-([0-9a-f-]+)">(.*?)</tr>`)
	listCellRe = regexp.MustCompile(`<td class="cell-value">(.*?)</td>`)
	listHeadRe = regexp.MustCompile(`(?s)<thead>.*?<tr>(.*?)</tr>`)
	thRe       = regexp.MustCompile(`<th>(.*?)</th>`)
)

// listColumns returns the listing's header names.
func listColumns(t *testing.T, page string) []string {
	t.Helper()
	head := listHeadRe.FindStringSubmatch(page)
	if head == nil {
		t.Fatalf("no table header in the listing: %.500q", page)
	}
	out := []string{}
	for _, m := range thRe.FindAllStringSubmatch(head[1], -1) {
		out = append(out, m[1])
	}
	return out
}

// listCells returns the value cells of the row with the given id.
func listCells(t *testing.T, page, id string) []string {
	t.Helper()
	for _, row := range listRowRe.FindAllStringSubmatch(page, -1) {
		if row[1] != id {
			continue
		}
		out := []string{}
		for _, m := range listCellRe.FindAllStringSubmatch(row[2], -1) {
			out = append(out, m[1])
		}
		return out
	}
	t.Fatalf("row %s is not in the listing: %.900q", id, page)
	return nil
}

func listRowCount(page string) int { return len(listRowRe.FindAllStringSubmatch(page, -1)) }

// --- T1: the measured bound --------------------------------------------------

// TestListingRelationCostDoesNotGrowWithRows is THE acceptance measurement of
// this contract: the same listing, first with 3 rows and then with 100, each row
// pointing at a DIFFERENT row of the same target type. If the translation cost
// anything per row, the second number would be about a hundred times the first.
func TestListingRelationCostDoesNotGrowWithRows(t *testing.T) {
	db, srv, cleanup := openCountingUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "libros",
		[][2]string{{"titulo", "text"}}, [][2]string{{"autor", "autores"}}); status != http.StatusOK {
		t.Fatalf("create libros: %d %.400q", status, body)
	}

	// A hundred authors — exactly the cap, so every one of them is translatable
	// and the measurement is not quietly measuring unresolved cells.
	for i := 0; i < 100; i++ {
		if status, body := uiCreateRow(t, client, srv, "autores",
			url.Values{"nombre": {"autor_" + strconvItoa(i)}}); status != http.StatusOK {
			t.Fatalf("create autor %d: %d %.300q", i, status, body)
		}
	}
	ids := []string{}
	rows, err := db.Query(`SELECT id FROM cpt_autores ORDER BY nombre`)
	if err != nil {
		t.Fatalf("read author ids: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 100 {
		t.Fatalf("%d authors, want 100", len(ids))
	}

	book := func(i int) {
		if status, body := uiCreateRow(t, client, srv, "libros",
			url.Values{"titulo": {"libro_" + strconvItoa(i)}, "autor": {ids[i]}}); status != http.StatusOK {
			t.Fatalf("create libro %d: %d %.300q", i, status, body)
		}
	}
	for i := 0; i < 3; i++ {
		book(i)
	}
	withThree, page := measureQueries(t, client, srv.URL+"/admin/content/libros")
	if got := listRowCount(page); got != 3 {
		t.Fatalf("the listing shows %d rows, want 3", got)
	}
	if strings.Contains(page, "(sin resolver)") {
		t.Fatalf("a relation was not translated with only 3 rows: %.900q", page)
	}

	for i := 3; i < 100; i++ {
		book(i)
	}
	withHundred, page := measureQueries(t, client, srv.URL+"/admin/content/libros")
	if got := listRowCount(page); got != 100 {
		t.Fatalf("the listing shows %d rows, want 100", got)
	}
	if strings.Contains(page, "(sin resolver)") {
		t.Fatalf("a relation was not translated with 100 rows: %.900q", page)
	}
	// Every one of the hundred rows points at a different author, and every one
	// of them is READ, not shown as a uuid.
	for i := 0; i < 100; i++ {
		if !strings.Contains(page, "autor_"+strconvItoa(i)+" · ") {
			t.Fatalf("the label of autor_%d is missing from the listing", i)
		}
	}

	t.Logf("MEDICIÓN T1 — consultas de GET /admin/content/libros: 3 filas = %d, 100 filas = %d",
		withThree, withHundred)
	if withThree != withHundred {
		t.Fatalf("the cost grew with the rows: 3 rows = %d queries, 100 rows = %d", withThree, withHundred)
	}
}

// TestListingWithoutRelationsCostsNothingExtra is the other half of the bound,
// and covers two of T3's cases at once: a type with NO relations must issue
// exactly the queries it issued before this contract, and a relation that is
// NULL in every row must not make the panel read the target at all.
func TestListingWithoutRelationsCostsNothingExtra(t *testing.T) {
	db, srv, cleanup := openCountingUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create", "content.update")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	uiCreateContentType(t, client, srv, "notas", [2]string{"texto", "text"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "libros",
		[][2]string{{"titulo", "text"}}, [][2]string{{"autor", "autores"}}); status != http.StatusOK {
		t.Fatalf("create libros: %d %.400q", status, body)
	}
	if status, _ := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {"Borges"}}); status != http.StatusOK {
		t.Fatalf("create autor")
	}
	autorID := firstID(t, db, "cpt_autores")
	for i := 0; i < 3; i++ {
		if status, _ := uiCreateRow(t, client, srv, "notas", url.Values{"texto": {"n" + strconvItoa(i)}}); status != http.StatusOK {
			t.Fatalf("create nota %d", i)
		}
		// Every book starts WITHOUT a relation.
		if status, _ := uiCreateRow(t, client, srv, "libros",
			url.Values{"titulo": {"l" + strconvItoa(i)}, "autor": {""}}); status != http.StatusOK {
			t.Fatalf("create libro %d", i)
		}
	}

	noRelations, _ := measureQueries(t, client, srv.URL+"/admin/content/notas")
	allNull, page := measureQueries(t, client, srv.URL+"/admin/content/libros")

	// A NULL relation is the same em dash every other NULL of this listing is.
	for _, row := range listRowRe.FindAllStringSubmatch(page, -1) {
		cells := listCells(t, page, row[1])
		if got := cells[len(cells)-1]; got != "—" {
			t.Fatalf("a NULL relation renders as %q, want the em dash", got)
		}
	}

	// Now set ONE of them and measure again: this is what a target actually costs.
	libroID := listRowRe.FindAllStringSubmatch(page, -1)[0][1]
	resp := uiDo(t, client, http.MethodPut, srv.URL+"/admin/content/libros/"+libroID,
		url.Values{"titulo": {"con autor"}, "autor": {autorID}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set the relation: %d", resp.StatusCode)
	}
	withTarget, _ := measureQueries(t, client, srv.URL+"/admin/content/libros")

	t.Logf("MEDICIÓN T3 — consultas del listado: tipo sin relaciones = %d, relación toda en NULL = %d, un destino resuelto = %d",
		noRelations, allNull, withTarget)
	if allNull != noRelations {
		t.Fatalf("a listing whose relation is always NULL cost %d queries, a listing with no relations at all cost %d — the NULL is being paid for",
			allNull, noRelations)
	}
	if withTarget <= allNull {
		t.Fatalf("resolving a target cost %d queries and resolving nothing cost %d — the measurement is not measuring anything",
			withTarget, allNull)
	}
}

// TestFormResolvesEachTargetOnce is T2, measured: a type with TWO relations to
// the SAME target must draw its form with the same number of queries as a type
// with one, because there is only one answer to fetch.
func TestFormResolvesEachTargetOnce(t *testing.T) {
	db, srv, cleanup := openCountingUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "uno",
		[][2]string{{"titulo", "text"}}, [][2]string{{"autor", "autores"}}); status != http.StatusOK {
		t.Fatalf("create uno: %d %.400q", status, body)
	}
	if status, body := uiCreateTypeWithReference(t, client, srv, "dos",
		[][2]string{{"titulo", "text"}},
		[][2]string{{"autor", "autores"}, {"prologuista", "autores"}}); status != http.StatusOK {
		t.Fatalf("create dos: %d %.400q", status, body)
	}
	if status, _ := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {"Borges"}}); status != http.StatusOK {
		t.Fatalf("create autor")
	}

	autorID := firstID(t, db, "cpt_autores")
	if status, _ := uiCreateRow(t, client, srv, "uno", url.Values{"titulo": {"a"}, "autor": {autorID}}); status != http.StatusOK {
		t.Fatalf("create uno row")
	}
	if status, _ := uiCreateRow(t, client, srv, "dos",
		url.Values{"titulo": {"a"}, "autor": {autorID}, "prologuista": {autorID}}); status != http.StatusOK {
		t.Fatalf("create dos row")
	}

	oneRelation, _ := measureQueries(t, client, srv.URL+"/admin/content/uno/new")
	twoRelations, page := measureQueries(t, client, srv.URL+"/admin/content/dos/new")

	// The LISTING shares the same cache, so the same must hold there.
	oneListed, _ := measureQueries(t, client, srv.URL+"/admin/content/uno")
	twoListed, _ := measureQueries(t, client, srv.URL+"/admin/content/dos")
	t.Logf("MEDICIÓN T2 — consultas del listado: 1 relación = %d, 2 relaciones al MISMO destino = %d",
		oneListed, twoListed)
	if twoListed != oneListed {
		t.Fatalf("listing two relations to the same target cost %d queries and one costs %d", twoListed, oneListed)
	}

	// Sharing the ANSWER is not sharing the CONTROL: two selectors, still.
	if !strings.Contains(page, `<select name="autor"`) || !strings.Contains(page, `<select name="prologuista"`) {
		t.Fatalf("the two relations are not two independent selectors: %.1200q", page)
	}
	t.Logf("MEDICIÓN T2 — consultas del formulario: 1 relación = %d, 2 relaciones al MISMO destino = %d",
		oneRelation, twoRelations)
	if twoRelations != oneRelation {
		t.Fatalf("two relations to the same target cost %d queries and one costs %d — the target is being resolved twice",
			twoRelations, oneRelation)
	}
}

// --- T3: the cases that are easy to get wrong --------------------------------

// TestListingShowsOneColumnPerRelation covers the readable half of T1 plus T3's
// "two relations to the same target": two rows pointing at different targets
// stopped looking identical, each relation has its OWN column, and the label is
// the very one the selector shows.
func TestListingShowsOneColumnPerRelation(t *testing.T) {
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
	for _, n := range []string{"uno", "dos"} {
		if status, _ := uiCreateRow(t, client, srv, "autores", url.Values{"nombre": {n}}); status != http.StatusOK {
			t.Fatalf("create autor %q", n)
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

	// Two books with the SAME title and different relations: without the relation
	// columns these two rows are indistinguishable, which is the defect.
	if status, _ := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Igual"}, "autor": {unoID}, "prologuista": {dosID}}); status != http.StatusOK {
		t.Fatalf("create the first book")
	}
	if status, _ := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Igual"}, "autor": {dosID}, "prologuista": {""}}); status != http.StatusOK {
		t.Fatalf("create the second book")
	}

	_, page := getBody(t, client, srv.URL+"/admin/content/libros")
	if got, want := listColumns(t, page), []string{"titulo", "autor", "prologuista", "Creado", "Acciones"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("columns = %v, want %v", got, want)
	}

	// The label is THE selector's label — same function, so the same row reads
	// the same on both screens.
	_, form := getBody(t, client, srv.URL+"/admin/content/libros/new")
	labelOf := func(id string) string {
		m := regexp.MustCompile(`<option value="` + id + `"[^>]*>([^<]*)<`).FindStringSubmatch(form)
		if m == nil {
			t.Fatalf("the selector has no option for %s: %.900q", id, form)
		}
		return m[1]
	}
	unoLabel, dosLabel := labelOf(unoID), labelOf(dosID)

	seen := map[string]bool{}
	for _, row := range listRowRe.FindAllStringSubmatch(page, -1) {
		cells := listCells(t, page, row[1])
		if len(cells) != 3 {
			t.Fatalf("row %s has %d value cells, want 3 (titulo, autor, prologuista)", row[1], len(cells))
		}
		seen[strings.Join(cells, "|")] = true
	}
	for _, want := range []string{
		"Igual|" + unoLabel + "|" + dosLabel,
		"Igual|" + dosLabel + "|—",
	} {
		if !seen[want] {
			t.Fatalf("the listing does not contain the row %q; it has %v", want, seen)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("two rows with the same title collapsed into one rendering: %v", seen)
	}
}

// TestListingIsHonestWhenItCannotTranslate is T3's hardest case: the target row
// is not among the ones the listing translates. What must NOT happen is the two
// failures the contract names — an invented label, or a cell that looks exactly
// like a relation that was never set.
func TestListingIsHonestWhenItCannotTranslate(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	oldestID := relationFixture(t, db, client, srv, "autor_viejo")
	if status, _ := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Con autor viejo"}, "autor": {oldestID}}); status != http.StatusOK {
		t.Fatalf("create the book")
	}
	if status, _ := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Sin autor"}, "autor": {""}}); status != http.StatusOK {
		t.Fatalf("create the bookless book")
	}

	// While the author is still on the first page, it translates.
	_, page := getBody(t, client, srv.URL+"/admin/content/libros")
	if strings.Contains(page, "(sin resolver)") {
		t.Fatalf("the author was not translated while it was on the page: %.900q", page)
	}

	// Push it out of the 100 newest.
	for i := 1; i <= 105; i++ {
		if status, _ := uiCreateRow(t, client, srv, "autores",
			url.Values{"nombre": {"autor_" + strconvItoa(i)}}); status != http.StatusOK {
			t.Fatalf("create autor %d", i)
		}
	}

	_, page = getBody(t, client, srv.URL+"/admin/content/libros")
	withRelation, withoutRelation := "", ""
	for _, row := range listRowRe.FindAllStringSubmatch(page, -1) {
		cells := listCells(t, page, row[1])
		switch cells[0] {
		case "Con autor viejo":
			withRelation = cells[1]
		case "Sin autor":
			withoutRelation = cells[1]
		}
	}
	if withRelation == "" || withoutRelation == "" {
		t.Fatalf("both rows should be listed: %.900q", page)
	}
	if withoutRelation != "—" {
		t.Fatalf("the row WITHOUT a relation renders %q, want the em dash", withoutRelation)
	}
	if withRelation == withoutRelation {
		t.Fatalf("a relation that IS set renders exactly like one that is not (%q) — the failure this case exists to forbid", withRelation)
	}
	if !strings.Contains(withRelation, "(sin resolver)") || !strings.Contains(withRelation, oldestID[:8]) {
		t.Fatalf("the untranslated cell is not honest about what it is: %q (target id %s)", withRelation, oldestID)
	}
	if strings.Contains(withRelation, "autor_viejo") {
		t.Fatalf("a label was invented for a row that was never read: %q", withRelation)
	}
	t.Logf("OK: una relación puesta pero no traducida se ve %q, y una ausente se ve %q", withRelation, withoutRelation)
}

// TestListingWhenTheTargetRowVanished is the red-team item about the window
// between the two reads: the listing reads the rows, and only then reads the
// target page to translate them. If the target row disappears in between, the id
// is simply not on the page.
//
// It takes a second connection with foreign keys OFF to even produce the state,
// which is itself the answer to how likely it is: a relation's target is ON
// DELETE RESTRICT (CONTRACT-27), so through the application the row cannot be
// deleted while anything points at it. The listing still must not invent
// anything, and it does not — the cell is the same honest "(sin resolver)" the
// bounded translation produces, because from here the two are the same fact.
func TestListingWhenTheTargetRowVanished(t *testing.T) {
	db, srv, cleanup := openCountingUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	autorID := relationFixture(t, db, client, srv, "Fantasma")
	if status, _ := uiCreateRow(t, client, srv, "libros",
		url.Values{"titulo": {"Huérfano"}, "autor": {autorID}}); status != http.StatusOK {
		t.Fatalf("create the book")
	}

	// A pool WITHOUT the foreign-key pragma: SQLite enforces foreign keys per
	// connection, so this is the only way to reach the state the application
	// refuses to create.
	loose, err := sql.Open(countingDriverName, dbFileOf(t, db))
	if err != nil {
		t.Fatalf("open the unenforced pool: %v", err)
	}
	defer loose.Close()
	if _, err := loose.Exec(`DELETE FROM cpt_autores WHERE id = ?`, autorID); err != nil {
		t.Fatalf("delete the target row behind the foreign key: %v", err)
	}

	_, page := getBody(t, client, srv.URL+"/admin/content/libros")
	cells := listCells(t, page, firstID(t, db, "cpt_libros"))
	got := cells[len(cells)-1]
	if got == "—" {
		t.Fatalf("a dangling relation renders exactly like an absent one")
	}
	if !strings.Contains(got, "(sin resolver)") || !strings.Contains(got, autorID[:8]) {
		t.Fatalf("a dangling relation renders %q, want the honest untranslated cell", got)
	}
	t.Logf("OK: una relación cuyo destino desapareció se lista como %q (mismo hecho, misma celda)", got)
}

// dbFileOf reads the file the test database lives in, straight from SQLite.
func dbFileOf(t *testing.T, db *sql.DB) string {
	t.Helper()
	var seq int
	var name, file string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
		t.Fatalf("database_list: %v", err)
	}
	return file
}

// TestListingLabelsAFieldlessTarget is the red-team item about the label rule at
// its poorest: a target type with ZERO declared fields has nothing to read, so
// the label is the bare id. It must still be a legible cell and must still be
// different from a NULL.
func TestListingLabelsAFieldlessTarget(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "editor", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "ed@example.com", "pw", "editor")

	uiCreateContentType(t, client, srv, "marcadores")
	if status, body := uiCreateTypeWithReference(t, client, srv, "notas",
		[][2]string{{"texto", "text"}}, [][2]string{{"marcador", "marcadores"}}); status != http.StatusOK {
		t.Fatalf("create notas: %d %.400q", status, body)
	}
	if status, _ := uiCreateRow(t, client, srv, "marcadores", url.Values{}); status != http.StatusOK {
		t.Fatalf("create marcador")
	}
	marcadorID := firstID(t, db, "cpt_marcadores")
	if status, _ := uiCreateRow(t, client, srv, "notas",
		url.Values{"texto": {"apunte"}, "marcador": {marcadorID}}); status != http.StatusOK {
		t.Fatalf("create nota")
	}

	_, page := getBody(t, client, srv.URL+"/admin/content/notas")
	cells := listCells(t, page, firstID(t, db, "cpt_notas"))
	if got := cells[len(cells)-1]; got != marcadorID {
		t.Fatalf("a field-less target renders as %q, want the bare id %q", got, marcadorID)
	}
	t.Logf("OK: un destino sin campos declarados se lista como su id: %s", marcadorID)
}
