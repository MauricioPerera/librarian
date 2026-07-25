package server

// terms.go implements CONTRACT-12 T2/T3: the JSON API for taxonomy terms
// (categories/tags) and the assignment of terms to content.
//
// Terms are admin-managed reference data (created at runtime, like users), NOT a
// content type: they have no author and no publish workflow. CRUD of terms and
// the (re)assignment of terms to content are BOTH gated by the new terms.manage
// permission (see schema.Permissions). Listing/reading is not permission-gated
// (only a valid identity), like the rest of the project.
//
// The taxonomy a term belongs to is addressed by NAME ("category"/"tag") in the
// API, never by its internal id — taxonomies are a code-fixed catalog, so the
// name is the stable public handle. An unknown taxonomy name → 400.
//
// A duplicate slug WITHIN a taxonomy is rejected with a clear 400, translated
// from the schema-level UNIQUE(taxonomy_id, slug) — the exact same pattern as
// products.sku (isUniqueSlugViolation below), never a raw SQL 500. The SAME slug
// under a DIFFERENT taxonomy is allowed (the composite unique is the point).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// term is the JSON view of a terms row. ParentID is nullable (a top-level term
// has no parent); Taxonomy is the taxonomy NAME resolved through taxonomy_id.
type term struct {
	ID       string  `json:"id"`
	Taxonomy string  `json:"taxonomy"`
	Name     string  `json:"name"`
	Slug     string  `json:"slug"`
	ParentID *string `json:"parent_id"`
}

// assignedTerm is the compact view of a term as it appears embedded in the GET
// response of a piece of content (CONTRACT-12 T3). It is deliberately smaller
// than term (no parent_id) — a caller listing an article's terms wants the
// label/slug/taxonomy, not the term's own hierarchy.
type assignedTerm struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Taxonomy string `json:"taxonomy"`
}

// termBody is the request body for POST/PUT /terms. Taxonomy is the taxonomy
// NAME. ParentID is an optional pointer so absent and explicit-null are both
// "no parent"; a non-empty value must reference an existing term.
type termBody struct {
	Taxonomy string  `json:"taxonomy"`
	Name     string  `json:"name"`
	Slug     string  `json:"slug"`
	ParentID *string `json:"parent_id"`
}

// contentTermsBody is the request body for PUT /articles|products/{id}/terms:
// the COMPLETE new set of term ids (atomic replace, like auth.SetUserRoles — not
// individual add/remove). An empty array clears all assignments.
type contentTermsBody struct {
	TermIDs []string `json:"term_ids"`
}

// Sentinels the term data helpers return so handlers map them to the right
// status without ever leaking raw SQL text.
var (
	errDuplicateSlug   = errors.New("slug already exists in taxonomy")
	errUnknownTaxonomy = errors.New("unknown taxonomy")
	errUnknownTerm     = errors.New("unknown term")
	errParentIsSelf    = errors.New("a term cannot be its own parent")
	errTermNotFound    = errors.New("term not found")
)

// isUniqueSlugViolation reports whether err is the UNIQUE(taxonomy_id, slug)
// constraint failure on terms.
//
// CONTRACT-20 REPLACED THE MECHANISM, and this is one of the two places in the
// package where the old code was silently single-engine. It used to match the
// TEXT "UNIQUE constraint failed" — which is SQLite's wording. PostgreSQL says
// "duplicate key value violates unique constraint", so on PostgreSQL the match
// would simply never fire: the duplicate would stop being a clean 400 and become
// a 500, with no compiler signal, until a user reused a slug in production.
// compat.Store.IsUniqueViolation classifies by the DRIVER'S STRUCTURED CODE
// (SQLITE_CONSTRAINT_UNIQUE/PRIMARYKEY, SQLSTATE 23505) through errors.As, so it
// gives the same answer on both engines and survives wrapping.
//
// The documented limitation of that primitive (it cannot say WHICH constraint)
// is not a problem here: terms has exactly two unique keys, the primary key and
// (taxonomy_id, slug), and the primary key is a v4 UUID this package generates
// per insert. The slug is the only one a caller can collide with. The schema
// constraint — not this check — remains the real guarantee; this only turns the
// resulting DB error into a clean 400.
func (h *handlers) isUniqueSlugViolation(err error) bool {
	return h.store.IsUniqueViolation(err)
}

// --- CRUD handlers (T2) ------------------------------------------------------

// handleCreateTerm creates a term (terms.manage). A missing field, unknown
// taxonomy, unknown/self parent, or duplicate slug within the taxonomy → 400
// (never 500, no row created).
func (h *handlers) handleCreateTerm(w http.ResponseWriter, r *http.Request) {
	var req termBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)
	if req.Name == "" || req.Slug == "" || req.Taxonomy == "" {
		writeError(w, http.StatusBadRequest, "taxonomy, name and slug are required")
		return
	}
	created, err := h.insertTerm(r.Context(), req)
	switch {
	case errors.Is(err, errUnknownTaxonomy):
		writeError(w, http.StatusBadRequest, "unknown taxonomy (must be one of the fixed catalog)")
		return
	case errors.Is(err, errUnknownTerm):
		writeError(w, http.StatusBadRequest, "parent_id does not reference an existing term")
		return
	case errors.Is(err, errParentIsSelf):
		writeError(w, http.StatusBadRequest, "a term cannot be its own parent")
		return
	case errors.Is(err, errDuplicateSlug):
		writeError(w, http.StatusBadRequest, "a term with this slug already exists in this taxonomy")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not create term")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// handleListTerms lists every term (with its taxonomy name), ordered by taxonomy
// then name. Requires only a valid identity — reading is not permission-gated.
func (h *handlers) handleListTerms(w http.ResponseWriter, r *http.Request) {
	out, err := h.listTerms(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list terms")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"terms": out})
}

// handleGetTerm returns one term by id. 404 when the id does not exist (or is
// malformed) — never 500.
func (h *handlers) handleGetTerm(w http.ResponseWriter, r *http.Request) {
	t, ok, err := h.fetchTerm(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read term")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "term not found")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleUpdateTerm updates a term's taxonomy/name/slug/parent (terms.manage). A
// missing id → 404; unknown taxonomy/parent, self-parent, or duplicate slug →
// 400.
func (h *handlers) handleUpdateTerm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req termBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)
	if req.Name == "" || req.Slug == "" || req.Taxonomy == "" {
		writeError(w, http.StatusBadRequest, "taxonomy, name and slug are required")
		return
	}
	updated, err := h.updateTerm(r.Context(), id, req)
	switch {
	case errors.Is(err, errTermNotFound):
		writeError(w, http.StatusNotFound, "term not found")
		return
	case errors.Is(err, errUnknownTaxonomy):
		writeError(w, http.StatusBadRequest, "unknown taxonomy (must be one of the fixed catalog)")
		return
	case errors.Is(err, errUnknownTerm):
		writeError(w, http.StatusBadRequest, "parent_id does not reference an existing term")
		return
	case errors.Is(err, errParentIsSelf):
		writeError(w, http.StatusBadRequest, "a term cannot be its own parent")
		return
	case errors.Is(err, errDuplicateSlug):
		writeError(w, http.StatusBadRequest, "a term with this slug already exists in this taxonomy")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not update term")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteTerm deletes one term (terms.manage). 404 when the id does not
// exist; 204 on success. Rows in article_terms/product_terms that referenced the
// term are removed by the schema-level ON DELETE CASCADE (the content keeps
// existing, it just loses that term); any child terms keep existing with
// parent_id set to NULL by ON DELETE SET NULL.
func (h *handlers) handleDeleteTerm(w http.ResponseWriter, r *http.Request) {
	n, err := h.deleteTermByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete term")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "term not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- assignment handlers (T3) ------------------------------------------------

// handleSetArticleTerms replaces an article's complete term set (terms.manage).
func (h *handlers) handleSetArticleTerms(w http.ResponseWriter, r *http.Request) {
	h.handleSetContentTerms(w, r, "articles", "article_terms", "article_id")
}

// handleSetProductTerms replaces a product's complete term set (terms.manage).
func (h *handlers) handleSetProductTerms(w http.ResponseWriter, r *http.Request) {
	h.handleSetContentTerms(w, r, "products", "product_terms", "product_id")
}

// handleSetContentTerms is the shared body of the two assignment routes: decode
// the id array, replace the set atomically (nonexistent content → 404; any
// unknown term id → 400 with nothing changed), and echo the resulting set.
func (h *handlers) handleSetContentTerms(w http.ResponseWriter, r *http.Request, contentTable, junction, idCol string) {
	id := r.PathValue("id")
	var req contentTermsBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	err := h.setContentTerms(r.Context(), contentTable, junction, idCol, id, req.TermIDs)
	switch {
	case errors.Is(err, errContentNotFound):
		writeError(w, http.StatusNotFound, contentTable+" not found")
		return
	case errors.Is(err, errUnknownTerm):
		writeError(w, http.StatusBadRequest, "one or more term ids do not reference an existing term")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not assign terms")
		return
	}
	out, err := h.assignedTermsFor(r.Context(), junction, idCol, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read assigned terms")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"terms": out})
}

// errContentNotFound is returned by setContentTerms when the target article/
// product id does not exist (→ 404).
var errContentNotFound = errors.New("content not found")

// --- data-access helpers -----------------------------------------------------

// taxonomyIDForName resolves a taxonomy name to its catalog id inside q (the
// caller's transaction). Returns errUnknownTaxonomy when the name is not in the
// catalog.
//
// CONTRACT-20: this stays RAW SQL rather than becoming a read routine, for the
// same structural reason CONTRACT-19 kept auth's roleIDsForNames raw:
// QueryRoutine opens its OWN transaction, so calling it from here would resolve
// the catalog OUTSIDE the transaction that is about to insert against it, and
// the "resolve before mutating" guarantee this file depends on would be read
// from a different snapshot than the one it writes into. It is composed with
// compat.Placeholder, so it is dual-engine.
func taxonomyIDForName(ctx context.Context, q queryer, engine compat.Engine, name string) (string, error) {
	statement := `SELECT ` + quote("id") + ` FROM ` + quote("taxonomies") +
		` WHERE ` + quote("name") + ` = ` + dual.Bind(engine, 1)
	var id string
	err := q.QueryRowContext(ctx, statement, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errUnknownTaxonomy
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// queryer is the tiny shared surface of *sql.DB and *sql.Tx used by the term
// helpers so the same resolver works inside or outside a transaction.
type queryer = dual.TxQuerier

// termExists reports whether a term id is present, inside the caller's
// transaction. Raw SQL for the same reason taxonomyIDForName is.
func termExists(ctx context.Context, q queryer, engine compat.Engine, id string) (bool, error) {
	statement := `SELECT ` + quote("id") + ` FROM ` + quote("terms") +
		` WHERE ` + quote("id") + ` = ` + dual.Bind(engine, 1)
	var found string
	err := q.QueryRowContext(ctx, statement, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// insertTerm resolves the taxonomy + optional parent and inserts the term,
// returning its JSON view. Runs in a transaction so a mid-way failure leaves no
// partial state. A UNIQUE(taxonomy_id, slug) violation → errDuplicateSlug.
func (h *handlers) insertTerm(ctx context.Context, req termBody) (term, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return term{}, err
	}
	defer tx.Rollback()

	engine := h.engine()
	taxID, err := taxonomyIDForName(ctx, tx, engine, req.Taxonomy)
	if err != nil {
		return term{}, err
	}
	parent, err := resolveParent(ctx, tx, engine, "", req.ParentID)
	if err != nil {
		return term{}, err
	}
	// CONTRACT-20: the id is generated here instead of being read back with
	// RETURNING (compat deliberately has no RETURNING; the column DEFAULT
	// gen_random_uuid() stays as a safety net for writes not coming from the app).
	id, err := dual.NewUUID()
	if err != nil {
		return term{}, err
	}
	statement := `INSERT INTO ` + quote("terms") + ` (` +
		quote("id") + `, ` + quote("taxonomy_id") + `, ` + quote("name") + `, ` +
		quote("slug") + `, ` + quote("parent_id") + `) VALUES (` +
		bindList(engine, 1, 5) + `)`
	// parent is a *string so a nil parent binds as SQL NULL on both engines.
	_, err = tx.ExecContext(ctx, statement, id, taxID, req.Name, req.Slug, parent)
	if h.isUniqueSlugViolation(err) {
		return term{}, errDuplicateSlug
	}
	if err != nil {
		return term{}, err
	}
	if err := tx.Commit(); err != nil {
		return term{}, err
	}
	return term{ID: id, Taxonomy: req.Taxonomy, Name: req.Name, Slug: req.Slug, ParentID: parent}, nil
}

// updateTerm updates an existing term. Verifies existence first (→ errTermNotFound),
// resolves taxonomy + parent (rejecting a self-parent and an unknown parent), and
// applies the update. A UNIQUE(taxonomy_id, slug) violation → errDuplicateSlug.
func (h *handlers) updateTerm(ctx context.Context, id string, req termBody) (term, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return term{}, err
	}
	defer tx.Rollback()

	engine := h.engine()
	present, err := termExists(ctx, tx, engine, id)
	if err != nil {
		return term{}, err
	}
	if !present {
		return term{}, errTermNotFound
	}
	taxID, err := taxonomyIDForName(ctx, tx, engine, req.Taxonomy)
	if err != nil {
		return term{}, err
	}
	parent, err := resolveParent(ctx, tx, engine, id, req.ParentID)
	if err != nil {
		return term{}, err
	}
	statement := `UPDATE ` + quote("terms") + ` SET ` +
		quote("taxonomy_id") + ` = ` + dual.Bind(engine, 1) + `, ` +
		quote("name") + ` = ` + dual.Bind(engine, 2) + `, ` +
		quote("slug") + ` = ` + dual.Bind(engine, 3) + `, ` +
		quote("parent_id") + ` = ` + dual.Bind(engine, 4) +
		` WHERE ` + quote("id") + ` = ` + dual.Bind(engine, 5)
	_, err = tx.ExecContext(ctx, statement, taxID, req.Name, req.Slug, parent, id)
	if h.isUniqueSlugViolation(err) {
		return term{}, errDuplicateSlug
	}
	if err != nil {
		return term{}, err
	}
	if err := tx.Commit(); err != nil {
		return term{}, err
	}
	return term{ID: id, Taxonomy: req.Taxonomy, Name: req.Name, Slug: req.Slug, ParentID: parent}, nil
}

// resolveParent validates an optional parent_id for a term whose own id is
// selfID (empty on create). It returns nil when no parent is set, errParentIsSelf
// when the parent equals the term itself (a trivial 1-cycle — the only cycle the
// application prevents; see the CONTRACT-12 report for deeper-cycle behavior),
// and errUnknownTerm when the parent id does not reference an existing term.
func resolveParent(ctx context.Context, q queryer, engine compat.Engine, selfID string, parentID *string) (*string, error) {
	if parentID == nil {
		return nil, nil
	}
	p := strings.TrimSpace(*parentID)
	if p == "" {
		return nil, nil
	}
	if p == selfID {
		return nil, errParentIsSelf
	}
	present, err := termExists(ctx, q, engine, p)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, errUnknownTerm
	}
	return &p, nil
}

// listTerms returns every term with its taxonomy name.
//
// CONTRACT-20: the JOIN moved into the canonical view schema.ViewTermRecords and
// the read is schema.RoutineListTerms. The declared order gained a third key:
// it was (taxonomy, name), which is NOT total — two terms with the same name in
// the same taxonomy tie, and each engine settles a tie its own way. slug is
// UNIQUE within a taxonomy, so (taxonomy, name, slug) is total. The sequence of
// any set that had no tie is unchanged, which is why no existing assertion moves.
func (h *handlers) listTerms(ctx context.Context) ([]term, error) {
	rows, err := h.queryRoutine(ctx, schema.RoutineListTerms, nil)
	if err != nil {
		return nil, err
	}
	out := make([]term, 0, len(rows))
	for _, row := range rows {
		out = append(out, termFromRow(row))
	}
	// The final order is imposed HERE, not by the engine. taxonomy/name/slug are
	// arbitrary user text, and PostgreSQL orders text by the database collation
	// while SQLite orders it by bytes. See the COLLATION note in dual.go.
	dual.SortByKeys(out, func(t term) []dual.Key { return dual.Ascending(t.Taxonomy, t.Name, t.Slug) })
	return out, nil
}

// fetchTerm loads one term by id. ok is false when no row matches (missing or
// malformed id) — never a raw SQL error, because the id is a bound value and a
// non-UUID simply matches nothing.
func (h *handlers) fetchTerm(ctx context.Context, id string) (term, bool, error) {
	row, found, err := h.queryOne(ctx, schema.RoutineTermByID,
		map[string]compat.Value{"term_id": dual.UUIDValue(id)})
	if err != nil || !found {
		return term{}, false, err
	}
	return termFromRow(row), true, nil
}

// deleteTermByID deletes one term and returns RowsAffected (0 ⇒ 404). It stays
// raw SQL because CallRoutine returns only error: the row count IS the answer
// here, and simulating it with a prior read would add a round trip and a race.
func (h *handlers) deleteTermByID(ctx context.Context, id string) (int64, error) {
	engine := h.engine()
	statement := `DELETE FROM ` + quote("terms") + ` WHERE ` + quote("id") + ` = ` + dual.Bind(engine, 1)
	res, err := h.db.ExecContext(ctx, statement, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// termFromRow maps one canonicalized routine row to the JSON view. parent_id is
// nullable and its NULL-ness is read from the canonical KIND, not from an empty
// string — so a term with no parent and a term with an empty parent text (which
// the schema forbids anyway) can never be confused.
func termFromRow(row compat.Row) term {
	return term{
		ID:       dual.RowText(row, "id"),
		Taxonomy: dual.RowText(row, "taxonomy"),
		Name:     dual.RowText(row, "name"),
		Slug:     dual.RowText(row, "slug"),
		ParentID: rowTextPointer(row, "parent_id"),
	}
}

// setContentTerms replaces the complete term set assigned to a piece of content,
// atomically. It verifies the content exists (→ errContentNotFound), resolves
// every term id against the terms table BEFORE mutating (any unknown id →
// errUnknownTerm with nothing changed — the same abort-before-mutate contract as
// auth.SetUserRoles), then deletes the current junction rows and inserts the new
// set. contentTable/junction/idCol are fixed internal constants (never user
// input), so they are interpolated; all values are bound as parameters.
func (h *handlers) setContentTerms(ctx context.Context, contentTable, junction, idCol, contentID string, termIDs []string) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	engine := h.engine()
	var exists string
	existsStatement := `SELECT ` + quote("id") + ` FROM ` + quote(contentTable) +
		` WHERE ` + quote("id") + ` = ` + dual.Bind(engine, 1)
	if err := tx.QueryRowContext(ctx, existsStatement, contentID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errContentNotFound
		}
		return err
	}

	// Resolve BEFORE mutating so an unknown term id aborts with nothing changed.
	for _, tid := range termIDs {
		present, err := termExists(ctx, tx, engine, tid)
		if err != nil {
			return err
		}
		if !present {
			return errUnknownTerm
		}
	}

	deleteStatement := `DELETE FROM ` + quote(junction) +
		` WHERE ` + quote(idCol) + ` = ` + dual.Bind(engine, 1)
	if _, err := tx.ExecContext(ctx, deleteStatement, contentID); err != nil {
		return err
	}
	// CONTRACT-20 removed the `ON CONFLICT DO NOTHING` these inserts carried, for
	// the same reason and with the same method CONTRACT-19 used in auth: the
	// DELETE above just emptied the target set, so the ONLY way to hit the
	// composite primary key is a term id repeated in the CALLER's list.
	// Deduplicating in Go makes the conflict IMPOSSIBLE rather than tolerated,
	// and removes a dependency on an upsert clause whose accepted spelling
	// differs between engines and versions. Observable behavior is identical: a
	// repeated id produced one row before and produces one row now.
	insertStatement := `INSERT INTO ` + quote(junction) + ` (` +
		quote(idCol) + `, ` + quote("term_id") + `) VALUES (` + bindList(engine, 1, 2) + `)`
	for _, tid := range dual.Dedupe(termIDs) {
		if _, err := tx.ExecContext(ctx, insertStatement, contentID, tid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// assignedTermsFor returns the terms assigned to a piece of content.
//
// CONTRACT-20: the two-JOIN query moved into a canonical view per junction
// (schema.ViewArticleAssignedTerms / ViewProductAssignedTerms) read by a routine
// each. Two views rather than one is not duplication for its own sake: a read
// action's relation is a DECLARED name, never caller-supplied, which is exactly
// the property that used to be guaranteed only by a hand-audited "junction and
// idCol are internal constants" comment around a string concatenation.
//
// The declared order gained a third key for the same reason listTerms did:
// (taxonomy, term_name) is not total, (taxonomy, term_name, term_slug) is.
func (h *handlers) assignedTermsFor(ctx context.Context, junction, idCol, contentID string) ([]assignedTerm, error) {
	routine := schema.RoutineArticleAssignedTerm
	if junction == "product_terms" {
		routine = schema.RoutineProductAssignedTerm
	}
	rows, err := h.queryRoutine(ctx, routine,
		map[string]compat.Value{"content_id": dual.UUIDValue(contentID)})
	if err != nil {
		return nil, err
	}
	out := make([]assignedTerm, 0, len(rows))
	for _, row := range rows {
		out = append(out, assignedTerm{
			ID:       dual.RowText(row, "term_id"),
			Name:     dual.RowText(row, "term_name"),
			Slug:     dual.RowText(row, "term_slug"),
			Taxonomy: dual.RowText(row, "taxonomy"),
		})
	}
	dual.SortByKeys(out, func(a assignedTerm) []dual.Key { return dual.Ascending(a.Taxonomy, a.Name, a.Slug) })
	return out, nil
}
