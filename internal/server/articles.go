package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/MauricioPerera/librarian/internal/schema"
)

// articles.go implements CONTRACT-03 T3: a REST CRUD surface over the
// articles content type (the example type from CONTRACT-01), using database/sql
// directly against the shared compat.Store.DB — the same parameterized-SQL
// pattern as CONTRACT-02, no ORM. All routes are wired in server.NewMux through
// the authorization middleware from authz.go.
//
// Authorship (design decision the contract left open): articles.author_id is
// NOT NULL with an FK to users(id) (see schema.ContentType), so an article MUST
// have a human author. An API key has no user behind it, so POST /articles by
// an API key is rejected with 403 + a clear message rather than silently
// inserting a NULL author (which the schema forbids anyway). See the contract
// report for the rationale.

// article is the row view returned by the read handlers. PublishedAt is
// nullable; on SQLite the timestamp driver returns it as a string, so it is
// scanned into NullString (the same approach apikey.go uses for revoked_at).
// Embedding (CONTRACT-05 T2) is the parsed vector: a nil slice (omitempty)
// represents a NULL column; a non-nil slice serializes as a JSON array of
// numbers — never the raw carrier text '[c1,c2,...]'.
type article struct {
	ID          string    `json:"id"`
	AuthorID    string    `json:"author_id"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	PublishedAt *string   `json:"published_at,omitempty"`
	Embedding   []float64 `json:"embedding,omitempty"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
}

// articleBody is the request body for POST and PUT. Metadata is an optional
// JSON value (CONTRACT-04 T2): when present and non-null it is stored verbatim
// in the articles.metadata JSON escape column, so the export path exercises a
// real JSON value produced by the app end-to-end. It is omitempty and optional
// — existing callers that omit it keep the original NULL-default behavior, so
// the CONTRACT-03 surface is unchanged for them.
//
// Embedding (CONTRACT-05 T2) is an optional JSON array of N numbers (N =
// schema.EmbeddingDimension). It is a json.RawMessage so the handler can
// distinguish absent (leave the column NULL/unchanged) from explicit null
// (clear on update) from an array (validate dimension + canonicalize). A
// wrong dimension or a non-numeric component is rejected with 400 — never
// 500, never silent truncation. Omitting it is backward compatible with
// CONTRACT-03/04.
type articleBody struct {
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Embedding json.RawMessage `json:"embedding,omitempty"`
}

// handleCreateArticle creates a draft (published_at NULL). Requires
// content.create. The author is the authenticated user; an API-key identity is
// rejected (no human author).
func (h *handlers) handleCreateArticle(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromContext(r.Context())
	if !ok {
		// Should not happen — the middleware always sets the identity — but
		// fail closed rather than nil-deref.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if id.Kind != "jwt" {
		writeError(w, http.StatusForbidden, "creating an article requires a user identity (API keys have no author)")
		return
	}
	var req articleBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.Body == "" {
		writeError(w, http.StatusBadRequest, "title and body are required")
		return
	}
	// metadata is an optional JSON value. A literal JSON null, or an absent
	// field, leaves the column at its NULL default; any other JSON value is
	// stored verbatim. The metadata column is TEXT on both engines (compat maps
	// JSONType to TEXT to preserve the payload byte-for-byte), so the raw JSON
	// text is bound as a parameter just like title/body — no string interpolation.
	hasMeta := len(req.Metadata) > 0 && string(req.Metadata) != "null"
	// embedding is an optional vector(N). An absent field or explicit null
	// leaves the column NULL (create default); a present array is validated
	// against the exact declared dimension and canonicalized to '[c1,c2,...]'.
	// Validation failures (wrong dimension, non-numeric component) are 400 —
	// they surface here, before any SQL, so they never become a 500.
	embCanonical, embPresent, _, err := validateEmbedding(req.Embedding, schema.EmbeddingDimension)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hasEmbedding := embPresent && embCanonical != ""
	var articleID string
	switch {
	case hasMeta && hasEmbedding:
		err = h.db.QueryRowContext(r.Context(),
			`INSERT INTO articles (author_id, title, body, metadata, embedding) VALUES (?, ?, ?, ?, ?) RETURNING id`,
			id.UserID, req.Title, req.Body, string(req.Metadata), embCanonical,
		).Scan(&articleID)
	case hasMeta:
		err = h.db.QueryRowContext(r.Context(),
			`INSERT INTO articles (author_id, title, body, metadata) VALUES (?, ?, ?, ?) RETURNING id`,
			id.UserID, req.Title, req.Body, string(req.Metadata),
		).Scan(&articleID)
	case hasEmbedding:
		err = h.db.QueryRowContext(r.Context(),
			`INSERT INTO articles (author_id, title, body, embedding) VALUES (?, ?, ?, ?) RETURNING id`,
			id.UserID, req.Title, req.Body, embCanonical,
		).Scan(&articleID)
	default:
		articleID, err = h.insertArticleBasic(r.Context(), id.UserID, req.Title, req.Body)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create article")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        articleID,
		"author_id": id.UserID,
		"title":     req.Title,
		"body":      req.Body,
	})
}

// handleListArticles lists articles with simple ?limit=&offset= paging
// (default limit 20). Requires only a valid identity — reading is not
// permission-gated in v1.
func (h *handlers) handleListArticles(w http.ResponseWriter, r *http.Request) {
	limit := queryIntDefault(r, "limit", 20)
	offset := queryIntDefault(r, "offset", 0)
	out, err := h.listArticles(r.Context(), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list articles")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"articles": out})
}

// handleGetArticle returns one article by id. Requires only a valid identity.
// 404 when the id does not exist — never 500/panic for a missing or
// malformed id (a non-UUID string simply matches no row).
func (h *handlers) handleGetArticle(w http.ResponseWriter, r *http.Request) {
	a, ok, err := h.fetchArticle(r, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read article")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "article not found")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// handleUpdateArticle updates title/body (NOT published_at — that is the
// publish route). Requires content.update. 404 when the id does not exist.
func (h *handlers) handleUpdateArticle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req articleBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.Body == "" {
		writeError(w, http.StatusBadRequest, "title and body are required")
		return
	}
	// embedding on update: absent (raw empty) leaves the column untouched
	// (backward compatible with CONTRACT-03/04, which never touched it);
	// explicit null clears it to NULL; a present array is validated against
	// the exact declared dimension and canonicalized. Validation failures are
	// 400, surfaced before any SQL — never 500, never silent.
	embCanonical, embPresent, embIsNull, err := validateEmbedding(req.Embedding, schema.EmbeddingDimension)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Existence check first so a dimension-validated but missing id still
	// returns 404 (not a silent no-op), and so the update below can rely on
	// RowsAffected == 0 ⇒ not found.
	present, err := h.articleExists(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update article")
		return
	}
	if !present {
		writeError(w, http.StatusNotFound, "article not found")
		return
	}

	setEmbedding := embPresent && !embIsNull
	clearEmbedding := embPresent && embIsNull
	var res sql.Result
	switch {
	case setEmbedding:
		res, err = h.db.ExecContext(r.Context(),
			`UPDATE articles SET title = ?, body = ?, embedding = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			req.Title, req.Body, embCanonical, id,
		)
	case clearEmbedding:
		res, err = h.db.ExecContext(r.Context(),
			`UPDATE articles SET title = ?, body = ?, embedding = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			req.Title, req.Body, id,
		)
	default:
		res, err = h.updateArticleTitleBody(r.Context(), id, req.Title, req.Body)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update article")
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update article")
		return
	}
	if n == 0 {
		// No row matched — either the id does not exist or is malformed. Both
		// surface as 404, never a raw SQL error to the client.
		writeError(w, http.StatusNotFound, "article not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    id,
		"title": req.Title,
		"body":  req.Body,
	})
}

// handlePublishArticle sets published_at = CURRENT_TIMESTAMP if it was NULL
// (idempotent — a no-op when already published). Requires content.publish.
// 404 when the id does not exist; 200 on both first publish and repeat.
func (h *handlers) handlePublishArticle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pub, found, err := h.publishArticleByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not publish article")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "article not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           id,
		"published_at": pub,
	})
}

// handleDeleteArticle deletes one article. Requires content.delete. 404 when
// the id does not exist. 204 on success.
func (h *handlers) handleDeleteArticle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := h.deleteArticleByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete article")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "article not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fetchArticle loads one article by id. ok is false when no row matches
// (missing or malformed id); err is non-nil only on a real DB failure. The
// caller maps ok=false to 404 and err!=nil to 500, so a malformed id never
// surfaces as a raw SQL error.
func (h *handlers) fetchArticle(r *http.Request, id string) (article, bool, error) {
	row := h.db.QueryRowContext(r.Context(),
		`SELECT id, author_id, title, body, published_at, embedding, created_at, updated_at
		   FROM articles
		  WHERE id = ?`,
		id,
	)
	a, err := scanArticle(row)
	if errors.Is(err, sql.ErrNoRows) {
		return article{}, false, nil
	}
	if err != nil {
		return article{}, false, err
	}
	return a, true, nil
}

// --- Shared data-access helpers (CONTRACT-07 T1) ----------------------------
//
// These wrap the parameterized SQL for the articles table so BOTH the JSON API
// handlers (CONTRACT-03) and the new admin UI handlers (CONTRACT-07) reuse the
// exact same queries instead of duplicating them. Extracting them here does not
// change the JSON contract: each JSON handler now calls a helper that runs the
// identical SQL it ran inline before.

// insertArticleBasic inserts a draft with only author/title/body (the common
// case shared by the JSON default branch and the admin UI create form) and
// returns the generated id.
func (h *handlers) insertArticleBasic(ctx context.Context, authorID, title, body string) (string, error) {
	var id string
	err := h.db.QueryRowContext(ctx,
		`INSERT INTO articles (author_id, title, body) VALUES (?, ?, ?) RETURNING id`,
		authorID, title, body,
	).Scan(&id)
	return id, err
}

// listArticles returns a page of articles ordered by created_at DESC. Shared by
// the JSON list route and the admin UI list page.
func (h *handlers) listArticles(ctx context.Context, limit, offset int) ([]article, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, author_id, title, body, published_at, embedding, created_at, updated_at
		   FROM articles
		  ORDER BY created_at DESC
		  LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]article, 0)
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// articleExists reports whether a row with the given id is present. A missing or
// malformed id yields (false, nil) — never a raw SQL error — so callers map it
// to 404 rather than 500.
func (h *handlers) articleExists(ctx context.Context, id string) (bool, error) {
	var x int
	err := h.db.QueryRowContext(ctx, `SELECT 1 FROM articles WHERE id = ?`, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// updateArticleTitleBody updates only title/body (not published_at). It returns
// the sql.Result so the caller can inspect RowsAffected. Shared by the JSON
// default update branch and the admin UI edit form.
func (h *handlers) updateArticleTitleBody(ctx context.Context, id, title, body string) (sql.Result, error) {
	return h.db.ExecContext(ctx,
		`UPDATE articles SET title = ?, body = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		title, body, id,
	)
}

// publishArticleByID sets published_at when still NULL (idempotent). found is
// false when the id does not exist (→ 404). On success it returns the current
// published_at so callers can confirm idempotency. Shared by the JSON publish
// route and the admin UI publish button.
func (h *handlers) publishArticleByID(ctx context.Context, id string) (publishedAt *string, found bool, err error) {
	present, err := h.articleExists(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if !present {
		return nil, false, nil
	}
	if _, err := h.db.ExecContext(ctx,
		`UPDATE articles SET published_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND published_at IS NULL`,
		id,
	); err != nil {
		return nil, true, err
	}
	var published sql.NullString
	if err := h.db.QueryRowContext(ctx,
		`SELECT published_at FROM articles WHERE id = ?`, id,
	).Scan(&published); err != nil {
		return nil, true, err
	}
	var pub *string
	if published.Valid && published.String != "" {
		s := published.String
		pub = &s
	}
	return pub, true, nil
}

// deleteArticleByID deletes one row and returns RowsAffected (0 ⇒ 404). Shared
// by the JSON delete route and the admin UI delete button.
func (h *handlers) deleteArticleByID(ctx context.Context, id string) (int64, error) {
	res, err := h.db.ExecContext(ctx, `DELETE FROM articles WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// scanArticle scans an article row from either a *sql.Rows or a *sql.Row (both
// implement the same Scan method via the shared sql.Scanner-like interface —
// here we accept a scanner with Scan(...) ).
func scanArticle(s interface {
	Scan(dest ...any) error
}) (article, error) {
	var (
		a         article
		published sql.NullString
		embedding sql.NullString
	)
	if err := s.Scan(&a.ID, &a.AuthorID, &a.Title, &a.Body, &published, &embedding, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return article{}, err
	}
	if published.Valid && published.String != "" {
		s := published.String
		a.PublishedAt = &s
	}
	// embedding is stored as the canonical carrier text '[c1,c2,...]' on both
	// engines. GET returns it as a JSON array of numbers, never the raw text;
	// a NULL column stays a nil slice (omitempty) — backward compatible with
	// CONTRACT-03/04 which had no embedding field.
	if embedding.Valid && embedding.String != "" {
		components, err := ParseVector(embedding.String)
		if err != nil {
			return article{}, err
		}
		a.Embedding = components
	}
	return a, nil
}

// queryIntDefault parses an int query param, returning def when it is absent
// or non-positive (for limit) / negative (for offset). A non-numeric value
// yields the default rather than a 500 — bad paging input is treated leniently.
func queryIntDefault(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < 0 {
		return def
	}
	return n
}
