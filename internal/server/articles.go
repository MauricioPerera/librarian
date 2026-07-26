package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
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
	// Terms are the taxonomy terms assigned to this article (CONTRACT-12 T3).
	// Populated only on the single-article GET path (fetchArticle), not in the
	// list, so a list response is unchanged; omitempty means an article with no
	// assigned terms omits the field.
	Terms []assignedTerm `json:"terms,omitempty"`
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
	//
	// CONTRACT-23 T2: on an installation without the vector capability the field
	// is REFUSED before anything else looks at it — there is no column to write
	// it to, and ignoring it would return a success for a write that stored
	// nothing.
	if h.refuseEmbeddingIfDisabled(w, req.Embedding) {
		return
	}
	embCanonical, embPresent, _, err := validateEmbedding(req.Embedding, schema.EmbeddingDimension)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hasEmbedding := embPresent && embCanonical != ""
	// CONTRACT-20 collapsed the four hand-branched INSERT variants into ONE
	// statement built from the optional columns actually present. The contract
	// warns against multiplying them into four near-identical routines, and the
	// warning generalizes: four variants of the same INSERT were already one
	// variant too many. metadata and embedding are appended only when set, so the
	// omitted case still leaves the column at its NULL default — byte-identical
	// behavior, one statement to keep dual-engine instead of four.
	optional := make([]string, 0, 2)
	extra := make([]any, 0, 2)
	if hasMeta {
		optional = append(optional, "metadata")
		extra = append(extra, string(req.Metadata))
	}
	if hasEmbedding {
		optional = append(optional, "embedding")
		extra = append(extra, embCanonical)
	}
	articleID, err := h.insertArticle(r.Context(), id.UserID, req.Title, req.Body, optional, extra)
	if err != nil {
		h.writeOperationFailure(w, r, err, "could not create article")
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
		h.writeOperationFailure(w, r, err, "could not list articles")
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
		h.writeOperationFailure(w, r, err, "could not read article")
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
	//
	// CONTRACT-23 T2: refused outright when the installation has no vector
	// capability, including the explicit-null "clear it" form — see
	// refuseEmbeddingIfDisabled.
	if h.refuseEmbeddingIfDisabled(w, req.Embedding) {
		return
	}
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
		h.writeOperationFailure(w, r, err, "could not update article")
		return
	}
	if !present {
		writeError(w, http.StatusNotFound, "article not found")
		return
	}

	// CONTRACT-20: the same collapse as create. The three update variants were
	// "set the embedding", "clear it" and "leave it alone"; the first two are the
	// same statement with a different bound value (the canonical carrier, or SQL
	// NULL), and the third simply omits the assignment. embedding is bound as a
	// plain parameter in BOTH cases — see insertArticle for why no cast is needed
	// even against PostgreSQL's native vector(1536) column.
	var res sql.Result
	if embPresent {
		var value any
		if !embIsNull {
			value = embCanonical
		}
		res, err = h.updateArticleWithEmbedding(r.Context(), id, req.Title, req.Body, value)
	} else {
		res, err = h.updateArticleTitleBody(r.Context(), id, req.Title, req.Body)
	}
	if err != nil {
		h.writeOperationFailure(w, r, err, "could not update article")
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		h.writeOperationFailure(w, r, err, "could not update article")
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

// handlePublishArticle sets published_at to the current instant if it was NULL
// (idempotent — a no-op when already published). Requires content.publish.
// 404 when the id does not exist; 200 on both first publish and repeat.
func (h *handlers) handlePublishArticle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pub, found, err := h.publishArticleByID(r.Context(), id)
	if err != nil {
		h.writeOperationFailure(w, r, err, "could not publish article")
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
		h.writeOperationFailure(w, r, err, "could not delete article")
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
	row, found, err := h.queryOne(r.Context(), schema.RoutineArticleByID,
		map[string]compat.Value{"article_id": dual.UUIDValue(id)})
	if err != nil || !found {
		return article{}, false, err
	}
	a, err := articleFromRow(row)
	if err != nil {
		return article{}, false, err
	}
	// CONTRACT-12 T3: attach the assigned terms so GET /articles/{id} includes
	// them. Loaded here (single-row path) rather than in listArticles so the list
	// response shape is untouched.
	terms, err := h.assignedTermsFor(r.Context(), "article_terms", "article_id", a.ID)
	if err != nil {
		return article{}, false, err
	}
	a.Terms = terms
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
	return h.insertArticle(ctx, authorID, title, body, nil, nil)
}

// insertArticle inserts one article with the always-present columns plus any
// optional ones the caller supplies (metadata, embedding), and returns the
// generated id.
//
// THE VECTOR, MEASURED — this is the risk CONTRACT-20 says to settle first.
// articles.embedding is TEXT on SQLite but a NATIVE vector(1536) column on
// PostgreSQL (pgvector). The question was whether binding the canonical carrier
// text '[c1,c2,...]' as an ordinary parameter is enough, or whether PostgreSQL
// needs an explicit conversion. It was measured against a real PostgreSQL 17
// with pgvector before any of this was written, and the answer is that a PLAIN
// BIND WORKS: in `INSERT INTO articles (..., embedding) VALUES (..., $4)` the
// parameter's type is inferred from the target column, so pgvector's own text
// input function parses it. No cast is emitted, because emitting one would be
// PostgreSQL-only syntax in a statement that must also run on SQLite — i.e. the
// exact divergence this contract removes. The evidence, and the ONE real
// difference that binding cannot fix (pgvector stores float4, so a component
// needing more than single precision is truncated on PostgreSQL and kept in full
// on SQLite), are in docs/reports/CONTRACT-20-REPORT.md.
//
// The id and both timestamps are written by the application: created_at is the
// primary sort key of listArticles and the two engines render CURRENT_TIMESTAMP
// differently, so leaving it to the DEFAULT would make the LIST ORDER diverge.
func (h *handlers) insertArticle(ctx context.Context, authorID, title, body string, optional []string, extra []any) (string, error) {
	id, err := dual.NewUUID()
	if err != nil {
		return "", err
	}
	engine := h.engine()
	stamp := nowCanonical()
	columns := []string{"id", "author_id", "title", "body", "created_at", "updated_at"}
	args := []any{id, authorID, title, body, stamp, stamp}
	columns = append(columns, optional...)
	args = append(args, extra...)
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = quote(c)
	}
	statement := `INSERT INTO ` + quote("articles") + ` (` + strings.Join(quoted, ", ") +
		`) VALUES (` + bindList(engine, 1, len(args)) + `)`
	if _, err := h.db.ExecContext(ctx, statement, args...); err != nil {
		return "", err
	}
	return id, nil
}

// listArticles returns a page of articles ordered by created_at DESC. Shared by
// the JSON list route and the admin UI list page.
//
// CONTRACT-20: schema.RoutineListArticles, and going through a routine here is
// not interchangeable with raw SQL. A raw read of the embedding column returns
// whatever the driver decided — SQLite hands back the stored carrier text,
// PostgreSQL hands back pgvector's own rendering — so the two engines would give
// this function different text for the same logical value. The routine declares
// the column as vector(EmbeddingDimension), so compat canonicalizes both sides
// to the identical '[c1,c2,...]' form before it reaches ParseVector.
//
// The declared order is (created_at DESC, id ASC). created_at alone is not
// total, and this read is PAGINATED: without a total order the same
// LIMIT/OFFSET can select different rows on each engine.
func (h *handlers) listArticles(ctx context.Context, limit, offset int) ([]article, error) {
	rows, err := h.queryRoutine(ctx, schema.RoutineListArticles, map[string]compat.Value{
		"page_limit":  integerValue(limit),
		"page_offset": integerValue(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]article, 0, len(rows))
	for _, row := range rows {
		a, err := articleFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// articleExists reports whether a row with the given id is present. A missing or
// malformed id yields (false, nil) — never a raw SQL error — so callers map it
// to 404 rather than 500.
//
// It has its own routine (selecting only id) rather than reusing the by-id read:
// this runs before every update and publish, and the full read would drag a
// 1536-component vector across the wire to answer a yes/no question. The old
// statement was `SELECT 1`; a read routine declares its outputs against the
// relation's own columns, so it selects `id`. The answer is identical.
func (h *handlers) articleExists(ctx context.Context, id string) (bool, error) {
	_, found, err := h.queryOne(ctx, schema.RoutineArticleExists,
		map[string]compat.Value{"article_id": dual.UUIDValue(id)})
	return found, err
}

// updateArticleTitleBody updates only title/body (not published_at, not
// embedding). It returns the sql.Result so the caller can inspect RowsAffected —
// which is exactly why every write in this file stays raw SQL: CallRoutine
// returns only error, and the row count is what decides 404 vs 200.
func (h *handlers) updateArticleTitleBody(ctx context.Context, id, title, body string) (sql.Result, error) {
	engine := h.engine()
	statement := `UPDATE ` + quote("articles") + ` SET ` +
		quote("title") + ` = ` + dual.Bind(engine, 1) + `, ` +
		quote("body") + ` = ` + dual.Bind(engine, 2) + `, ` +
		quote("updated_at") + ` = ` + dual.Bind(engine, 3) +
		` WHERE ` + quote("id") + ` = ` + dual.Bind(engine, 4)
	return h.db.ExecContext(ctx, statement, title, body, nowCanonical(), id)
}

// updateArticleWithEmbedding updates title/body AND the embedding column.
// embedding is a plain bound parameter: a non-nil value is the canonical carrier
// text, a nil value is SQL NULL (the explicit-clear case). Binding NULL rather
// than writing the literal `NULL` into the statement is what merges the old
// "set" and "clear" branches into one statement — and it is accepted by
// PostgreSQL's native vector column exactly as the carrier text is (measured;
// see insertArticle).
func (h *handlers) updateArticleWithEmbedding(ctx context.Context, id, title, body string, embedding any) (sql.Result, error) {
	engine := h.engine()
	statement := `UPDATE ` + quote("articles") + ` SET ` +
		quote("title") + ` = ` + dual.Bind(engine, 1) + `, ` +
		quote("body") + ` = ` + dual.Bind(engine, 2) + `, ` +
		quote("embedding") + ` = ` + dual.Bind(engine, 3) + `, ` +
		quote("updated_at") + ` = ` + dual.Bind(engine, 4) +
		` WHERE ` + quote("id") + ` = ` + dual.Bind(engine, 5)
	return h.db.ExecContext(ctx, statement, title, body, embedding, nowCanonical(), id)
}

// publishArticleByID sets published_at when still NULL (idempotent). found is
// false when the id does not exist (→ 404). On success it returns the current
// published_at so callers can confirm idempotency. Shared by the JSON publish
// route and the admin UI publish button.
//
// The instant is now written by the application in the canonical timestamp form
// instead of CURRENT_TIMESTAMP, for the same reason as everywhere else in this
// file; the idempotence still comes from the `AND published_at IS NULL` guard,
// which compat compiles identically on both engines.
func (h *handlers) publishArticleByID(ctx context.Context, id string) (publishedAt *string, found bool, err error) {
	present, err := h.articleExists(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if !present {
		return nil, false, nil
	}
	engine := h.engine()
	stamp := nowCanonical()
	statement := `UPDATE ` + quote("articles") + ` SET ` +
		quote("published_at") + ` = ` + dual.Bind(engine, 1) + `, ` +
		quote("updated_at") + ` = ` + dual.Bind(engine, 2) +
		` WHERE ` + quote("id") + ` = ` + dual.Bind(engine, 3) +
		` AND ` + quote("published_at") + ` IS NULL`
	if _, err := h.db.ExecContext(ctx, statement, stamp, stamp, id); err != nil {
		return nil, true, err
	}
	row, ok, err := h.queryOne(ctx, schema.RoutineArticlePublishedAt,
		map[string]compat.Value{"article_id": dual.UUIDValue(id)})
	if err != nil {
		return nil, true, err
	}
	if !ok {
		return nil, true, nil
	}
	return rowTextPointer(row, "published_at"), true, nil
}

// deleteArticleByID deletes one row and returns RowsAffected (0 ⇒ 404). Shared
// by the JSON delete route and the admin UI delete button.
func (h *handlers) deleteArticleByID(ctx context.Context, id string) (int64, error) {
	engine := h.engine()
	statement := `DELETE FROM ` + quote("articles") + ` WHERE ` + quote("id") + ` = ` + dual.Bind(engine, 1)
	res, err := h.db.ExecContext(ctx, statement, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// articleFromRow maps one canonicalized routine row to the JSON view.
//
// published_at and embedding are both nullable and their NULL-ness is read from
// the canonical KIND, never from an empty string — the distinction that decides
// whether the JSON carries the field at all (both are omitempty). embedding
// arrives as the canonical carrier '[c1,c2,...]' on BOTH engines because the
// routine declares the vector family; GET returns it as a JSON array of numbers,
// never the raw text, exactly as before.
func articleFromRow(row compat.Row) (article, error) {
	a := article{
		ID:          dual.RowText(row, "id"),
		AuthorID:    dual.RowText(row, "author_id"),
		Title:       dual.RowText(row, "title"),
		Body:        dual.RowText(row, "body"),
		PublishedAt: rowTextPointer(row, "published_at"),
		CreatedAt:   dual.RowText(row, "created_at"),
		UpdatedAt:   dual.RowText(row, "updated_at"),
	}
	// CONTRACT-23: on an installation without the vector capability the routine
	// does not DECLARE the column, so the row does not carry it. That is tested
	// explicitly rather than left to the zero value of a missing map entry: a
	// column that is absent from the result and one that is present and NULL are
	// different facts, and only the second is a nullable embedding. Reads simply
	// omit the field (it is `omitempty`), which is what the contract asks for —
	// the refusal is on the WRITE side, where a caller stated an intention.
	if _, declared := row["embedding"]; declared && !dual.RowIsNull(row, "embedding") {
		if text := dual.RowText(row, "embedding"); text != "" {
			components, err := ParseVector(text)
			if err != nil {
				return article{}, err
			}
			a.Embedding = components
		}
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
