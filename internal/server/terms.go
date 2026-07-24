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

// isUniqueSlugViolation reports whether err is the SQLite UNIQUE(taxonomy_id,
// slug) constraint failure on terms. SQLite (modernc driver) phrases it as
// "UNIQUE constraint failed: terms.taxonomy_id, terms.slug". Matching the message
// keeps this dependency-free and is stable — the same technique as
// isUniqueSKUViolation for products. The schema constraint is the real guarantee
// (it cannot lose a race); this only turns the DB error into a clean 400.
func isUniqueSlugViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed") && strings.Contains(err.Error(), "terms.slug")
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

// taxonomyIDForName resolves a taxonomy name to its catalog id inside q (a *sql.DB
// or *sql.Tx). Returns errUnknownTaxonomy when the name is not in the catalog.
func taxonomyIDForName(ctx context.Context, q queryer, name string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM taxonomies WHERE name = ?`, name).Scan(&id)
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
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
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

	taxID, err := taxonomyIDForName(ctx, tx, req.Taxonomy)
	if err != nil {
		return term{}, err
	}
	parent, err := resolveParent(ctx, tx, "", req.ParentID)
	if err != nil {
		return term{}, err
	}
	var id string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO terms (taxonomy_id, name, slug, parent_id) VALUES (?, ?, ?, ?) RETURNING id`,
		taxID, req.Name, req.Slug, parent,
	).Scan(&id)
	if isUniqueSlugViolation(err) {
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

	var exists string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM terms WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return term{}, errTermNotFound
		}
		return term{}, err
	}
	taxID, err := taxonomyIDForName(ctx, tx, req.Taxonomy)
	if err != nil {
		return term{}, err
	}
	parent, err := resolveParent(ctx, tx, id, req.ParentID)
	if err != nil {
		return term{}, err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE terms SET taxonomy_id = ?, name = ?, slug = ?, parent_id = ? WHERE id = ?`,
		taxID, req.Name, req.Slug, parent, id,
	)
	if isUniqueSlugViolation(err) {
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
func resolveParent(ctx context.Context, q queryer, selfID string, parentID *string) (*string, error) {
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
	var found string
	err := q.QueryRowContext(ctx, `SELECT id FROM terms WHERE id = ?`, p).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errUnknownTerm
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// listTerms returns every term with its taxonomy name, ordered by taxonomy then
// name for a stable listing.
func (h *handlers) listTerms(ctx context.Context) ([]term, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT t.id, x.name, t.name, t.slug, t.parent_id
		   FROM terms t
		   JOIN taxonomies x ON x.id = t.taxonomy_id
		  ORDER BY x.name, t.name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]term, 0)
	for rows.Next() {
		t, err := scanTerm(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// fetchTerm loads one term by id. ok is false when no row matches (missing or
// malformed id) — never a raw SQL error.
func (h *handlers) fetchTerm(ctx context.Context, id string) (term, bool, error) {
	row := h.db.QueryRowContext(ctx,
		`SELECT t.id, x.name, t.name, t.slug, t.parent_id
		   FROM terms t
		   JOIN taxonomies x ON x.id = t.taxonomy_id
		  WHERE t.id = ?`,
		id,
	)
	t, err := scanTerm(row)
	if errors.Is(err, sql.ErrNoRows) {
		return term{}, false, nil
	}
	if err != nil {
		return term{}, false, err
	}
	return t, true, nil
}

// deleteTermByID deletes one term and returns RowsAffected (0 ⇒ 404).
func (h *handlers) deleteTermByID(ctx context.Context, id string) (int64, error) {
	res, err := h.db.ExecContext(ctx, `DELETE FROM terms WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// scanTerm scans a term row (id, taxonomy name, name, slug, parent_id) from a
// *sql.Rows or *sql.Row. parent_id is nullable.
func scanTerm(s interface {
	Scan(dest ...any) error
}) (term, error) {
	var (
		t      term
		parent sql.NullString
	)
	if err := s.Scan(&t.ID, &t.Taxonomy, &t.Name, &t.Slug, &parent); err != nil {
		return term{}, err
	}
	if parent.Valid && parent.String != "" {
		p := parent.String
		t.ParentID = &p
	}
	return t, nil
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

	var exists string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM `+contentTable+` WHERE id = ?`, contentID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errContentNotFound
		}
		return err
	}

	// Resolve BEFORE mutating so an unknown term id aborts with nothing changed.
	for _, tid := range termIDs {
		var found string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM terms WHERE id = ?`, tid).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errUnknownTerm
			}
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM `+junction+` WHERE `+idCol+` = ?`, contentID); err != nil {
		return err
	}
	for _, tid := range termIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+junction+` (`+idCol+`, term_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			contentID, tid,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// assignedTermsFor returns the terms assigned to a piece of content, joined
// through its junction table to terms + taxonomies. Ordered by taxonomy then
// name for a stable response. junction/idCol are fixed internal constants.
func (h *handlers) assignedTermsFor(ctx context.Context, junction, idCol, contentID string) ([]assignedTerm, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT t.id, t.name, t.slug, x.name
		   FROM `+junction+` j
		   JOIN terms t       ON t.id = j.term_id
		   JOIN taxonomies x  ON x.id = t.taxonomy_id
		  WHERE j.`+idCol+` = ?
		  ORDER BY x.name, t.name`,
		contentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]assignedTerm, 0)
	for rows.Next() {
		var a assignedTerm
		if err := rows.Scan(&a.ID, &a.Name, &a.Slug, &a.Taxonomy); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
