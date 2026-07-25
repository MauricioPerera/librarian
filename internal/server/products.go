package server

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

// products.go implements CONTRACT-11 T2: a REST CRUD surface over the products
// content type, mirroring articles.go (CONTRACT-03) exactly — the same
// parameterized-SQL pattern against the shared compat.Store.DB, the same status
// codes (404 never 500 for a missing/malformed id, 400 on validation), and the
// same permission gating with the SAME generic content.* permissions (products
// is not a special case: content.create/update/delete are domain permissions,
// not article-specific). Routes are wired in server.NewMux.
//
// Differences from articles (by contract): products has no published_at, so
// there is NO publish route and content.publish is not used here — products is
// simple CRUD. It has two own columns beyond title/body: price (a decimal stored
// as canonical text, validated numeric) and sku (UNIQUE at the schema level; a
// duplicate is translated to a clear 400, never a raw SQL 500).
//
// Authorship: like articles, products.author_id is NOT NULL with an FK to
// users(id), so a product MUST have a human author — POST /products by an API
// key is rejected with 403 rather than inserting a NULL author.

// product is the row view returned by the read handlers. Price is the canonical
// decimal text stored on SQLite (compat maps DecimalType to TEXT there), surfaced
// verbatim as a JSON string so no float rounding is ever introduced.
type product struct {
	ID        string `json:"id"`
	AuthorID  string `json:"author_id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Price     string `json:"price"`
	SKU       string `json:"sku"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// Terms are the taxonomy terms assigned to this product (CONTRACT-12 T3).
	// Populated only on the single-product GET path (fetchProduct), not in the
	// list; omitempty means a product with no assigned terms omits the field.
	Terms []assignedTerm `json:"terms,omitempty"`
}

// productBody is the request body for POST and PUT. Price is a json.RawMessage so
// the handler accepts either a JSON number (9.99) or a JSON string ("9.99") and
// validates it as a decimal before storing — a non-numeric price is rejected with
// a clear 400, never a 500 and never a corrupt stored value (red-team). Keeping
// the raw token (rather than decoding a number into a float64) also preserves the
// exact decimal text, so an arbitrary-precision value is stored byte-for-byte.
type productBody struct {
	Title string          `json:"title"`
	Body  string          `json:"body"`
	Price json.RawMessage `json:"price"`
	SKU   string          `json:"sku"`
}

// errDuplicateSKU is the sentinel the data-access helpers return when an INSERT/
// UPDATE violates the schema-level UNIQUE(sku). The handlers translate it to a
// 400 with a human message; the raw SQL error text is never sent to the client.
var errDuplicateSKU = errors.New("sku already exists")

// isUniqueSKUViolation reports whether err is the UNIQUE(sku) constraint
// failure on products.
//
// CONTRACT-20 REPLACED THE MECHANISM, and this is the case the contract's
// red-team section names explicitly. The old check matched the TEXT "UNIQUE
// constraint failed", which is SQLite's wording; PostgreSQL says "duplicate key
// value violates unique constraint". On PostgreSQL the match would never fire,
// so a duplicate sku would stop being a clean 400 and become a 500 — silently,
// with nothing failing to compile, until a user reused a sku in production.
// compat.Store.IsUniqueViolation classifies by the DRIVER'S STRUCTURED CODE
// (SQLITE_CONSTRAINT_UNIQUE / SQLITE_CONSTRAINT_PRIMARYKEY on SQLite, SQLSTATE
// 23505 on PostgreSQL) through errors.As, so both engines answer the same and a
// wrapped error is still classified.
//
// That primitive cannot say WHICH constraint was violated, which is fine here:
// products has two unique keys, the primary key and sku, and the primary key is
// a v4 UUID this package generates per insert. sku is the only one a caller can
// collide with. The schema constraint — not this check — remains the real
// guarantee (it cannot lose a concurrent race); this only turns the resulting DB
// error into a clean 400.
func (h *handlers) isUniqueSKUViolation(err error) bool {
	return h.store.IsUniqueViolation(err)
}

// handleCreateProduct creates a product. Requires content.create. The author is
// the authenticated user; an API-key identity is rejected (no human author). A
// duplicate sku → 400 (not 500); a non-numeric price → 400.
func (h *handlers) handleCreateProduct(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if id.Kind != "jwt" {
		writeError(w, http.StatusForbidden, "creating a product requires a user identity (API keys have no author)")
		return
	}
	req, price, ok := decodeProductBody(w, r)
	if !ok {
		return
	}
	newID, err := h.insertProduct(r.Context(), id.UserID, req.Title, req.Body, price, req.SKU)
	if errors.Is(err, errDuplicateSKU) {
		writeError(w, http.StatusBadRequest, "a product with this sku already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create product")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        newID,
		"author_id": id.UserID,
		"title":     req.Title,
		"body":      req.Body,
		"price":     price,
		"sku":       req.SKU,
	})
}

// handleListProducts lists products with simple ?limit=&offset= paging (default
// limit 20). Requires only a valid identity — reading is not permission-gated.
func (h *handlers) handleListProducts(w http.ResponseWriter, r *http.Request) {
	limit := queryIntDefault(r, "limit", 20)
	offset := queryIntDefault(r, "offset", 0)
	out, err := h.listProducts(r.Context(), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list products")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": out})
}

// handleGetProduct returns one product by id. Requires only a valid identity.
// 404 when the id does not exist — never 500/panic for a missing or malformed id.
func (h *handlers) handleGetProduct(w http.ResponseWriter, r *http.Request) {
	p, ok, err := h.fetchProduct(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read product")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "product not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleUpdateProduct updates title/body/price/sku. Requires content.update. 404
// when the id does not exist; a duplicate sku → 400; a non-numeric price → 400.
func (h *handlers) handleUpdateProduct(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req, price, ok := decodeProductBody(w, r)
	if !ok {
		return
	}
	present, err := h.productExists(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update product")
		return
	}
	if !present {
		writeError(w, http.StatusNotFound, "product not found")
		return
	}
	res, err := h.updateProductFields(r.Context(), id, req.Title, req.Body, price, req.SKU)
	if errors.Is(err, errDuplicateSKU) {
		writeError(w, http.StatusBadRequest, "a product with this sku already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update product")
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update product")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "product not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    id,
		"title": req.Title,
		"body":  req.Body,
		"price": price,
		"sku":   req.SKU,
	})
}

// handleDeleteProduct deletes one product. Requires content.delete. 404 when the
// id does not exist. 204 on success.
func (h *handlers) handleDeleteProduct(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := h.deleteProductByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete product")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "product not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeProductBody decodes + validates the JSON body shared by create/update. On
// any failure it writes the proper 400 and returns ok=false. On success it
// returns the decoded body and the validated canonical price string. The
// validation messages mirror articles' "required" style.
func decodeProductBody(w http.ResponseWriter, r *http.Request) (productBody, string, bool) {
	var req productBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return productBody{}, "", false
	}
	if req.Title == "" || req.Body == "" || req.SKU == "" {
		writeError(w, http.StatusBadRequest, "title, body, price and sku are required")
		return productBody{}, "", false
	}
	price, ok := canonicalDecimal(req.Price)
	if !ok {
		writeError(w, http.StatusBadRequest, "price must be a valid decimal number")
		return productBody{}, "", false
	}
	return req, price, true
}

// canonicalDecimal validates a price token from the JSON body and returns its
// canonical decimal text for storage. It accepts a JSON number (9.99) or a JSON
// string ("9.99"); anything else — a non-numeric string, an empty value, a null,
// a boolean, an object — is rejected (ok=false) so the caller returns a clean
// 400. The value is stored as its canonical text (compat maps DecimalType to TEXT
// on SQLite), never parsed to a float, so precision is preserved exactly.
func canonicalDecimal(raw json.RawMessage) (string, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", false
	}
	// A JSON string: unquote, then validate the inner text as a decimal.
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return "", false
		}
		s = strings.TrimSpace(str)
	}
	return validateDecimalText(s)
}

// validateDecimalText reports whether s is a plain decimal number (optional
// leading sign, digits, optional single fractional part) and returns s unchanged
// when valid. It deliberately rejects exponent notation, currency symbols,
// thousands separators, and whitespace-in-the-middle — the stored value must be a
// clean canonical decimal, not a float-ish approximation.
func validateDecimalText(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	i := 0
	if s[0] == '+' || s[0] == '-' {
		i++
	}
	digits, dot := 0, false
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' && !dot:
			dot = true
		default:
			return "", false
		}
	}
	if digits == 0 {
		return "", false
	}
	return s, true
}

// --- Shared data-access helpers (reused by the JSON API and the admin UI) -----

// insertProduct inserts a product and returns the generated id. A UNIQUE(sku)
// violation is translated to errDuplicateSKU so callers map it to 400.
// CONTRACT-20: raw SQL with compat.Placeholder. The id and both timestamps are
// written by the application (see dual.go newUUID / nowCanonical) instead of
// being left to RETURNING and the column DEFAULTs — created_at is the primary
// sort key of listProducts and the two engines render CURRENT_TIMESTAMP
// differently, so leaving it to the engine would make the LIST ORDER diverge.
func (h *handlers) insertProduct(ctx context.Context, authorID, title, body, price, sku string) (string, error) {
	id, err := dual.NewUUID()
	if err != nil {
		return "", err
	}
	engine := h.engine()
	stamp := nowCanonical()
	statement := `INSERT INTO ` + quote("products") + ` (` +
		quote("id") + `, ` + quote("author_id") + `, ` + quote("title") + `, ` +
		quote("body") + `, ` + quote("price") + `, ` + quote("sku") + `, ` +
		quote("created_at") + `, ` + quote("updated_at") + `) VALUES (` +
		bindList(engine, 1, 8) + `)`
	_, err = h.db.ExecContext(ctx, statement, id, authorID, title, body, price, sku, stamp, stamp)
	if h.isUniqueSKUViolation(err) {
		return "", errDuplicateSKU
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// listProducts returns a page of products ordered by created_at DESC. Shared by
// the JSON list route and the admin UI list page.
//
// CONTRACT-20: schema.RoutineListProducts. The declared order is
// (created_at DESC, id ASC): created_at alone is NOT total, so two products
// written in the same instant could come back in either sequence, differently
// per engine. price arrives canonicalized by its DECLARED decimal family, which
// is what makes SQLite's TEXT column and PostgreSQL's NUMERIC one read the same.
func (h *handlers) listProducts(ctx context.Context, limit, offset int) ([]product, error) {
	rows, err := h.queryRoutine(ctx, schema.RoutineListProducts, map[string]compat.Value{
		"page_limit":  integerValue(limit),
		"page_offset": integerValue(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]product, 0, len(rows))
	for _, row := range rows {
		out = append(out, productFromRow(row))
	}
	return out, nil
}

// fetchProduct loads one product by id. ok is false when no row matches (missing
// or malformed id); err is non-nil only on a real DB failure.
func (h *handlers) fetchProduct(ctx context.Context, id string) (product, bool, error) {
	row, found, err := h.queryOne(ctx, schema.RoutineProductByID,
		map[string]compat.Value{"product_id": dual.UUIDValue(id)})
	if err != nil || !found {
		return product{}, false, err
	}
	p := productFromRow(row)
	// CONTRACT-12 T3: attach the assigned terms so GET /products/{id} includes
	// them. Loaded on the single-row path only, keeping the list shape untouched.
	terms, err := h.assignedTermsFor(ctx, "product_terms", "product_id", p.ID)
	if err != nil {
		return product{}, false, err
	}
	p.Terms = terms
	return p, true, nil
}

// productExists reports whether a row with the given id is present. A missing or
// malformed id yields (false, nil) — never a raw SQL error.
//
// The old statement was `SELECT 1`; a read routine declares its output columns
// against the relation's own columns, so it selects `id` instead. The answer is
// identical (a row matched, or none did) and nothing observable changes.
func (h *handlers) productExists(ctx context.Context, id string) (bool, error) {
	_, found, err := h.queryOne(ctx, schema.RoutineProductExists,
		map[string]compat.Value{"product_id": dual.UUIDValue(id)})
	return found, err
}

// updateProductFields updates title/body/price/sku. It returns the sql.Result so
// the caller can inspect RowsAffected — which is exactly why it stays raw SQL:
// CallRoutine returns only error, and the row count is what decides 404 vs 200.
// A UNIQUE(sku) violation is translated to errDuplicateSKU.
func (h *handlers) updateProductFields(ctx context.Context, id, title, body, price, sku string) (sql.Result, error) {
	engine := h.engine()
	statement := `UPDATE ` + quote("products") + ` SET ` +
		quote("title") + ` = ` + dual.Bind(engine, 1) + `, ` +
		quote("body") + ` = ` + dual.Bind(engine, 2) + `, ` +
		quote("price") + ` = ` + dual.Bind(engine, 3) + `, ` +
		quote("sku") + ` = ` + dual.Bind(engine, 4) + `, ` +
		quote("updated_at") + ` = ` + dual.Bind(engine, 5) +
		` WHERE ` + quote("id") + ` = ` + dual.Bind(engine, 6)
	res, err := h.db.ExecContext(ctx, statement, title, body, price, sku, nowCanonical(), id)
	if h.isUniqueSKUViolation(err) {
		return nil, errDuplicateSKU
	}
	return res, err
}

// deleteProductByID deletes one row and returns RowsAffected (0 ⇒ 404). Raw SQL
// for the same reason updateProductFields is.
func (h *handlers) deleteProductByID(ctx context.Context, id string) (int64, error) {
	engine := h.engine()
	statement := `DELETE FROM ` + quote("products") + ` WHERE ` + quote("id") + ` = ` + dual.Bind(engine, 1)
	res, err := h.db.ExecContext(ctx, statement, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// productFromRow maps one canonicalized routine row to the JSON view.
func productFromRow(row compat.Row) product {
	return product{
		ID:        dual.RowText(row, "id"),
		AuthorID:  dual.RowText(row, "author_id"),
		Title:     dual.RowText(row, "title"),
		Body:      dual.RowText(row, "body"),
		Price:     dual.RowText(row, "price"),
		SKU:       dual.RowText(row, "sku"),
		CreatedAt: dual.RowText(row, "created_at"),
		UpdatedAt: dual.RowText(row, "updated_at"),
	}
}
