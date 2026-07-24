package server_test

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// server_vector_test.go implements CONTRACT-05 T2 acceptance + the red-team
// convergence check. It runs in the DEFAULT suite (green twice, no PG, no
// build tag). The end-to-end export fixture with a real embedding lives in the
// tagged export_fixture_test.go (CONTRACT-04 pattern).

// edgeVec returns a vector of the declared dimension whose FIRST components
// are the red-team edge cases ('2.0', '2', '1.5', '0.1', '-0.4', '1e-05',
// '100000', '0', '-3', '0.5') and the rest are a stable fractional fill. The
// edge cases are placed at the front so the stored canonical text can be
// inspected component-by-component without scanning 1536 commas by hand.
func edgeVec(n int) []float64 {
	edges := []float64{2.0, 2, 1.5, 0.1, -0.4, 1e-05, 100000, 0, -3, 0.5}
	v := make([]float64, n)
	for i := 0; i < n; i++ {
		if i < len(edges) {
			v[i] = edges[i]
		} else {
			v[i] = float64(i%5)/4.0 - 0.5
		}
	}
	return v
}

// dimVec returns a deterministic, non-trivial vector of the declared dimension.
func dimVec(n int) []float64 {
	v := make([]float64, n)
	for i := 0; i < n; i++ {
		v[i] = float64(i%7)/3.0 - 1.0
	}
	return v
}

// TestCreateArticleWithEmbedding covers the create criterion: a valid embedding
// array of the exact declared dimension → 201 + stored as the canonical carrier
// text; GET returns it as a JSON array of numbers (not the raw text).
func TestCreateArticleWithEmbedding(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	emb := dimVec(schema.EmbeddingDimension)
	status, body := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "With Embedding", "body": "B", "embedding": emb},
		authHeader(editorToken))
	if status != http.StatusCreated {
		t.Fatalf("with-embedding status=%d body=%v, want 201", status, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("no id returned: %v", body)
	}

	// Stored as the canonical carrier text '[c1,c2,...]' (no spaces), exactly
	// what compat's canonicalVectorValue produces — proven by the convergence
	// test below and by the export round-trip in the tagged fixture.
	var stored sql.NullString
	if err := db.QueryRow(`SELECT embedding FROM articles WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("select embedding: %v", err)
	}
	want := canonicalCarrierText(emb)
	if !stored.Valid || stored.String != want {
		t.Fatalf("stored embedding = %q (valid=%v), want canonical %q", stored.String, stored.Valid, want)
	}
	if strings.Contains(stored.String, " ") {
		t.Fatalf("canonical embedding must have no spaces: %q", stored.String)
	}

	// GET returns the embedding as a JSON array of numbers, not the raw text.
	gstatus, gbody := doJSON(t, srv, http.MethodGet, "/articles/"+id, nil, authHeader(editorToken))
	if gstatus != http.StatusOK {
		t.Fatalf("get status=%d body=%v, want 200", gstatus, gbody)
	}
	gotEmb, ok := gbody["embedding"].([]any)
	if !ok {
		t.Fatalf("GET did not return embedding array: %v", gbody)
	}
	if len(gotEmb) != schema.EmbeddingDimension {
		t.Fatalf("GET embedding len=%d, want %d", len(gotEmb), schema.EmbeddingDimension)
	}
	for i, el := range gotEmb {
		f, ok := el.(float64)
		if !ok {
			t.Fatalf("GET embedding[%d] not a number: %v", i, el)
		}
		if f != emb[i] {
			t.Fatalf("GET embedding[%d]=%v, want %v", i, f, emb[i])
		}
	}
}

// TestCreateArticleEmbeddingOmittedIsNull confirms backward compatibility:
// omitting the embedding field on create leaves the column NULL, identical to
// CONTRACT-03/04 behavior.
func TestCreateArticleEmbeddingOmittedIsNull(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	status, body := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "No Embedding", "body": "B"}, authHeader(editorToken))
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%v, want 201", status, body)
	}
	id, _ := body["id"].(string)
	var stored sql.NullString
	if err := db.QueryRow(`SELECT embedding FROM articles WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("select: %v", err)
	}
	if stored.Valid {
		t.Fatalf("omitted embedding should be NULL, got %q", stored.String)
	}
	// GET omits the field entirely (nil slice + omitempty).
	gstatus, gbody := doJSON(t, srv, http.MethodGet, "/articles/"+id, nil, authHeader(editorToken))
	if gstatus != http.StatusOK {
		t.Fatalf("get status=%d, want 200", gstatus)
	}
	if _, present := gbody["embedding"]; present {
		t.Fatalf("GET should omit embedding when NULL: %v", gbody)
	}
}

// TestEmbeddingInvalidDimension is the red-team check: a wrong-dimension
// embedding is rejected with 400 (clear), never 500 nor silent truncation.
func TestEmbeddingInvalidDimension(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.update")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	wrong := dimVec(schema.EmbeddingDimension - 1) // off by one
	// POST: wrong dimension → 400, no row created.
	status, body := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "T", "body": "B", "embedding": wrong}, authHeader(editorToken))
	if status != http.StatusBadRequest {
		t.Fatalf("POST wrong-dim status=%d body=%v, want 400", status, body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Fatalf("POST wrong-dim: expected clear error, got %v", body)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM articles`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("wrong-dim POST should not have created a row, got %d", n)
	}
	// PUT: wrong dimension → 400 (not 500, not a silent update).
	status, body = doJSON(t, srv, http.MethodPut, "/articles/"+nonexistentID,
		map[string]any{"title": "T", "body": "B", "embedding": wrong}, authHeader(editorToken))
	if status != http.StatusBadRequest {
		t.Fatalf("PUT wrong-dim status=%d body=%v, want 400", status, body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Fatalf("PUT wrong-dim: expected clear error, got %v", body)
	}
}

// TestEmbeddingNonNumericComponent is the red-team check: a non-numeric
// component in the array is rejected with 400 (clear), never 500.
func TestEmbeddingNonNumericComponent(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	// An array of the right length but with a string mixed in.
	arr := make([]any, schema.EmbeddingDimension)
	for i := range arr {
		arr[i] = 0.1
	}
	arr[0] = "not-a-number"
	status, body := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "T", "body": "B", "embedding": arr}, authHeader(editorToken))
	if status != http.StatusBadRequest {
		t.Fatalf("string-component status=%d body=%v, want 400", status, body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Fatalf("string-component: expected clear error, got %v", body)
	}

	// A bool is also non-numeric (encoding/json decodes it to bool, not float64).
	arr[0] = true
	status, body = doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "T", "body": "B", "embedding": arr}, authHeader(editorToken))
	if status != http.StatusBadRequest {
		t.Fatalf("bool-component status=%d body=%v, want 400", status, body)
	}

	// A null inside the array is non-numeric too.
	arr[0] = nil
	status, body = doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "T", "body": "B", "embedding": arr}, authHeader(editorToken))
	if status != http.StatusBadRequest {
		t.Fatalf("null-component status=%d body=%v, want 400", status, body)
	}
}

// TestEmbeddingNotArray rejects a non-array embedding value with 400.
func TestEmbeddingNotArray(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	status, _ := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "T", "body": "B", "embedding": "not-an-array"}, authHeader(editorToken))
	if status != http.StatusBadRequest {
		t.Fatalf("non-array status=%d, want 400", status)
	}
}

// TestUpdateArticleEmbedding covers the update criterion: PUT with a valid
// embedding → 200 + stored; omitting it leaves the column untouched (backward
// compatible); explicit null clears it.
func TestUpdateArticleEmbedding(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create", "content.update")
	editorToken := jwtFor(t, db, "ed@example.com", "pw", "editor")

	id := createArticle(t, srv, editorToken, "Old", "OldBody")

	// PUT with embedding → 200 + stored canonical.
	emb := dimVec(schema.EmbeddingDimension)
	status, _ := doJSON(t, srv, http.MethodPut, "/articles/"+id,
		map[string]any{"title": "New", "body": "NewBody", "embedding": emb}, authHeader(editorToken))
	if status != http.StatusOK {
		t.Fatalf("update-with-embedding status=%d, want 200", status)
	}
	var stored sql.NullString
	if err := db.QueryRow(`SELECT embedding FROM articles WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	want := canonicalCarrierText(emb)
	if !stored.Valid || stored.String != want {
		t.Fatalf("after update, embedding = %q, want %q", stored.String, want)
	}

	// PUT omitting embedding leaves it untouched (backward compatible).
	status, _ = doJSON(t, srv, http.MethodPut, "/articles/"+id,
		map[string]any{"title": "Newer", "body": "NewerBody"}, authHeader(editorToken))
	if status != http.StatusOK {
		t.Fatalf("update-omit status=%d, want 200", status)
	}
	if err := db.QueryRow(`SELECT embedding FROM articles WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Valid || stored.String != want {
		t.Fatalf("omitted-embedding update changed it: %q, want %q", stored.String, want)
	}

	// PUT with explicit null clears the embedding.
	status, _ = doJSON(t, srv, http.MethodPut, "/articles/"+id,
		map[string]any{"title": "Cleared", "body": "ClearedBody", "embedding": nil}, authHeader(editorToken))
	if status != http.StatusOK {
		t.Fatalf("update-null status=%d, want 200", status)
	}
	if err := db.QueryRow(`SELECT embedding FROM articles WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Valid {
		t.Fatalf("explicit null should clear embedding, got %q", stored.String)
	}
}

// TestVectorFormatConvergesWithCompat is the explicit red-team check the
// contract's checklist demands: the hand-written vector text formatter in
// librarian MUST converge with compat's canonicalVectorValue on edge cases
// like '2.0' vs '2', scientific notation, and negatives — otherwise the SQLite
// source text and the exported snapshot would diverge and compat copy would
// fail ERR_VERIFY_DIVERGED.
//
// It proves convergence through the REAL code path (not a self-computed
// reference): the embedding goes in via the real POST /articles handler (which
// uses server.FormatVector, the production formatter), is read back as raw
// stored text, and that text is then re-canonicalized by compat's REAL
// ExportSnapshot (which calls canonicalVectorValue internally). The snapshot
// value must equal the stored text byte-for-byte. The independent reference
// (canonicalCarrierText, using strconv.FormatFloat(...,'g',-1,64) — the exact
// algorithm of compat/store.go normalizeFloat) cross-checks the stored text,
// so the equality is not assumed from the formatter under test.
func TestVectorFormatConvergesWithCompat(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	editorToken := jwtFor(t, db, "conv@example.com", "pw", "editor")

	emb := edgeVec(schema.EmbeddingDimension)
	status, body := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "Convergence", "body": "B", "embedding": emb}, authHeader(editorToken))
	if status != http.StatusCreated {
		t.Fatalf("create status=%d body=%v, want 201", status, body)
	}
	id, _ := body["id"].(string)

	// Raw stored text produced by the REAL server formatter.
	var stored sql.NullString
	if err := db.QueryRow(`SELECT embedding FROM articles WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Valid {
		t.Fatal("embedding not stored")
	}

	// (1) The stored text matches the independent normalizeFloat reference for
	// every edge-case component at the front — proving the real formatter ==
	// compat's normalizeFloat algorithm. The '2.0' vs '2' convergence the
	// checklist names explicitly is the first two components.
	wantRef := canonicalCarrierText(emb)
	if stored.String != wantRef {
		t.Fatalf("real formatter diverges from reference:\n got=%q\nwant=%q", stored.String, wantRef)
	}
	edges := []string{"2", "2", "1.5", "0.1", "-0.4", "1e-05", "100000", "0", "-3", "0.5"}
	gotParts := strings.Split(stored.String[1:len(stored.String)-1], ",")
	if len(gotParts) < len(edges) {
		t.Fatalf("stored vector has too few components: %d", len(gotParts))
	}
	for i, want := range edges {
		if gotParts[i] != want {
			t.Fatalf("edge component %d: got %q, want %q ('2.0'/'2' convergence)", i, gotParts[i], want)
		}
	}
	t.Logf("edge components: %v", gotParts[:len(edges)])
	t.Logf("'2.0' and '2' both → %q (converge)", gotParts[0])

	// (2) End-to-end through compat's REAL canonicalization: store the
	// librarian-produced text and re-export it; compat must agree byte-for-byte.
	ctx := context.Background()
	vschema := compat.Schema{Tables: []compat.Table{{
		Name: "vecs",
		Columns: []compat.Column{
			{Name: "id", Type: compat.Type{Family: compat.IntegerType}},
			{Name: "v", Type: compat.Type{Family: compat.VectorType, Arguments: []int{schema.EmbeddingDimension}}, Nullable: true},
		},
		Constraints: []compat.Constraint{{Kind: compat.PrimaryKey, Columns: []string{"id"}}},
	}}}
	vstore, err := compat.OpenSQLite(compat.Version{Major: 3}, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer vstore.Close()
	if err := vstore.ApplySchema(ctx, vschema); err != nil {
		t.Fatal(err)
	}
	if _, err := vstore.DB.Exec(`INSERT INTO vecs (id, v) VALUES (1, ?)`, stored.String); err != nil {
		t.Fatalf("insert librarian canonical text into compat store: %v", err)
	}
	snap, err := vstore.ExportSnapshot(ctx, vschema)
	if err != nil {
		t.Fatalf("compat ExportSnapshot rejected librarian text: %v", err)
	}
	got := snap.Rows["vecs"][0]["v"]
	if got.Kind != compat.VectorValue || got.Value != stored.String {
		t.Fatalf("compat did not converge on librarian canonical: kind=%v value=%q want=%q", got.Kind, got.Value, stored.String)
	}
	t.Logf("compat re-canonicalized == librarian canonical == %q (MATCH, %d dims)", got.Value, schema.EmbeddingDimension)
}

// canonicalCarrierText is the expected canonical text for a slice, computed
// with the same expression compat's normalizeFloat uses — the reference the
// storage assertions compare against. It is computed INDEPENDENTLY of
// server.FormatVector (its own implementation of the same algorithm) so the
// equality is a cross-check, not self-fulfilling.
func canonicalCarrierText(components []float64) string {
	parts := make([]string, len(components))
	for i, c := range components {
		parts[i] = strconv.FormatFloat(c, 'g', -1, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}