package server

import (
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
		err = h.db.QueryRowContext(r.Context(),
			`INSERT INTO articles (author_id, title, body) VALUES (?, ?, ?) RETURNING id`,
			id.UserID, req.Title, req.Body,
		).Scan(&articleID)
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
	rows, err := h.db.QueryContext(r.Context(),
		`SELECT id, author_id, title, body, published_at, embedding, created_at, updated_at
		   FROM articles
		  ORDER BY created_at DESC
		  LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list articles")
		return
	}
	defer rows.Close()
	out := make([]article, 0)
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read articles")
			return
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "could not read articles")
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
	var exists int
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT 1 FROM articles WHERE id = ?`, id,
	).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "article not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update article")
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
		res, err = h.db.ExecContext(r.Context(),
			`UPDATE articles SET title = ?, body = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			req.Title, req.Body, id,
		)
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
	// Existence check first: an already-published row must still return 200
	// (idempotent), so we cannot rely on UPDATE rows-affected to distinguish
	// "not found" from "already published".
	var exists int
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT 1 FROM articles WHERE id = ?`, id,
	).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "article not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "could not publish article")
		return
	}
	// Only set published_at when it is still NULL; an already-published row is
	// untouched (published_at and updated_at both unchanged) → idempotent.
	if _, err := h.db.ExecContext(r.Context(),
		`UPDATE articles SET published_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND published_at IS NULL`,
		id,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "could not publish article")
		return
	}
	// Return the current (post-publish) published_at so callers can confirm it
	// did not change on a repeat call.
	var published sql.NullString
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT published_at FROM articles WHERE id = ?`, id,
	).Scan(&published); err != nil {
		writeError(w, http.StatusInternalServerError, "could not publish article")
		return
	}
	var pub *string
	if published.Valid && published.String != "" {
		s := published.String
		pub = &s
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
	res, err := h.db.ExecContext(r.Context(),
		`DELETE FROM articles WHERE id = ?`, id,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete article")
		return
	}
	n, err := res.RowsAffected()
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
