package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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

// isUniqueSKUViolation reports whether err is the SQLite UNIQUE(sku) constraint
// failure. SQLite (modernc driver) surfaces it as an error whose message
// contains "UNIQUE constraint failed: products.sku". Matching the message keeps
// this dependency-free (no driver-type import) and is stable: SQLite always
// emits that exact phrasing for a UNIQUE violation. The schema constraint — not
// this check — is the real guarantee (it cannot lose a concurrent race); this
// only turns the resulting DB error into a clean 400.
func isUniqueSKUViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed") && strings.Contains(err.Error(), "sku")
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
func (h *handlers) insertProduct(ctx context.Context, authorID, title, body, price, sku string) (string, error) {
	var id string
	err := h.db.QueryRowContext(ctx,
		`INSERT INTO products (author_id, title, body, price, sku) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		authorID, title, body, price, sku,
	).Scan(&id)
	if isUniqueSKUViolation(err) {
		return "", errDuplicateSKU
	}
	return id, err
}

// listProducts returns a page of products ordered by created_at DESC. Shared by
// the JSON list route and the admin UI list page.
func (h *handlers) listProducts(ctx context.Context, limit, offset int) ([]product, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, author_id, title, body, price, sku, created_at, updated_at
		   FROM products
		  ORDER BY created_at DESC
		  LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]product, 0)
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// fetchProduct loads one product by id. ok is false when no row matches (missing
// or malformed id); err is non-nil only on a real DB failure.
func (h *handlers) fetchProduct(ctx context.Context, id string) (product, bool, error) {
	row := h.db.QueryRowContext(ctx,
		`SELECT id, author_id, title, body, price, sku, created_at, updated_at
		   FROM products
		  WHERE id = ?`,
		id,
	)
	p, err := scanProduct(row)
	if errors.Is(err, sql.ErrNoRows) {
		return product{}, false, nil
	}
	if err != nil {
		return product{}, false, err
	}
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
func (h *handlers) productExists(ctx context.Context, id string) (bool, error) {
	var x int
	err := h.db.QueryRowContext(ctx, `SELECT 1 FROM products WHERE id = ?`, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// updateProductFields updates title/body/price/sku. It returns the sql.Result so
// the caller can inspect RowsAffected. A UNIQUE(sku) violation is translated to
// errDuplicateSKU. Shared by the JSON update route and the admin UI edit form.
func (h *handlers) updateProductFields(ctx context.Context, id, title, body, price, sku string) (sql.Result, error) {
	res, err := h.db.ExecContext(ctx,
		`UPDATE products SET title = ?, body = ?, price = ?, sku = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		title, body, price, sku, id,
	)
	if isUniqueSKUViolation(err) {
		return nil, errDuplicateSKU
	}
	return res, err
}

// deleteProductByID deletes one row and returns RowsAffected (0 ⇒ 404). Shared by
// the JSON delete route and the admin UI delete button.
func (h *handlers) deleteProductByID(ctx context.Context, id string) (int64, error) {
	res, err := h.db.ExecContext(ctx, `DELETE FROM products WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// scanProduct scans a product row from either a *sql.Rows or a *sql.Row.
func scanProduct(s interface {
	Scan(dest ...any) error
}) (product, error) {
	var p product
	if err := s.Scan(&p.ID, &p.AuthorID, &p.Title, &p.Body, &p.Price, &p.SKU, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return product{}, err
	}
	return p, nil
}
