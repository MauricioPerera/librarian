package server_test

// CONTRACT-23 T2 over the real HTTP surface, on SQLite, in the DEFAULT suite (no
// build tag, no PostgreSQL): an installation WITHOUT the vector capability
// serves articles normally and REFUSES, with an explanation, any request that
// carries an `embedding`.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// openNoVectorMux is openAuthMux for an installation created WITHOUT the vector
// capability: the schema is applied for those capabilities and the mux is built
// with the same declaration, exactly as cmd/librarian does.
func openNoVectorMux(t *testing.T) (*sql.DB, *httptest.Server, func()) {
	t.Helper()
	caps := schema.Capabilities{VectorDisabled: true}
	dbPath := filepath.Join(t.TempDir(), "novector.db")
	sdb, err := store.Open(compat.SQLite, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if err := store.EnsureSchemaFor(ctx, sdb, caps); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := store.SeedCatalogs(ctx, sdb); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux, err := server.NewMux(server.Deps{Store: sdb, JWTSecret: testSecret, Capabilities: caps})
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	return sdb.DB, srv, func() {
		srv.Close()
		_ = sdb.Close()
	}
}

// TestArticlesCRUDWithoutTheVectorCapability is the API half of the hueco this
// contract closes: everything an installation that only administers content
// needs still works when the column is not there.
func TestArticlesCRUDWithoutTheVectorCapability(t *testing.T) {
	db, srv, cleanup := openNoVectorMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	grant(t, db, "editor", "content.update")
	grant(t, db, "editor", "content.publish")
	grant(t, db, "editor", "content.delete")
	token := jwtFor(t, db, "ed@example.com", "pw", "editor")
	auth := authHeader(token)

	status, body := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "Sin vector", "body": "B"}, auth)
	if status != http.StatusCreated {
		t.Fatalf("create status=%d body=%v, want 201", status, body)
	}
	id, _ := body["id"].(string)
	t.Logf("POST /articles -> 201 id issued: %v", id != "")

	status, body = doJSON(t, srv, http.MethodGet, "/articles/"+id, nil, auth)
	if status != http.StatusOK {
		t.Fatalf("get status=%d body=%v", status, body)
	}
	// The read simply does not carry the field — it is `omitempty` and the
	// routine does not declare the column.
	if _, present := body["embedding"]; present {
		t.Fatalf("GET carries an embedding field on an installation without the capability: %v", body)
	}
	t.Logf("GET /articles/{id} -> 200 title=%v, embedding field present: false", body["title"])

	status, _ = doJSON(t, srv, http.MethodGet, "/articles", nil, auth)
	if status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	status, body = doJSON(t, srv, http.MethodPut, "/articles/"+id,
		map[string]any{"title": "Editado", "body": "B2"}, auth)
	if status != http.StatusOK {
		t.Fatalf("update status=%d body=%v", status, body)
	}
	status, _ = doJSON(t, srv, http.MethodPost, "/articles/"+id+"/publish", nil, auth)
	if status != http.StatusOK {
		t.Fatalf("publish status=%d", status)
	}
	status, _ = doJSON(t, srv, http.MethodDelete, "/articles/"+id, nil, auth)
	if status != http.StatusNoContent {
		t.Fatalf("delete status=%d, want 204", status)
	}
	status, _ = doJSON(t, srv, http.MethodDelete, "/articles/"+id, nil, auth)
	if status != http.StatusNotFound {
		t.Fatalf("delete twice status=%d, want 404", status)
	}
	t.Log("GET list / PUT / publish / DELETE / DELETE again -> 200 200 200 204 404")
}

// TestEmbeddingIsRefusedNotIgnored is T2 itself. Every shape of a PRESENT
// embedding is refused with the explanation — including the explicit null, which
// is a statement about a column that does not exist.
func TestEmbeddingIsRefusedNotIgnored(t *testing.T) {
	db, srv, cleanup := openNoVectorMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	grant(t, db, "editor", "content.update")
	token := jwtFor(t, db, "ed@example.com", "pw", "editor")
	auth := authHeader(token)

	// A row to update, created without an embedding.
	_, created := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "base", "body": "B"}, auth)
	id, _ := created["id"].(string)

	cases := []struct {
		name  string
		value any
	}{
		{name: "a full-dimension array", value: dimVec(schema.EmbeddingDimension)},
		{name: "a wrong-dimension array", value: []float64{1, 2, 3}},
		{name: "an empty array", value: []float64{}},
		{name: "an explicit null", value: nil},
		{name: "a non-array", value: "nope"},
	}
	for _, tc := range cases {
		for _, call := range []struct {
			method string
			path   string
		}{
			{http.MethodPost, "/articles"},
			{http.MethodPut, "/articles/" + id},
		} {
			payload := map[string]any{"title": "T", "body": "B", "embedding": tc.value}
			status, body := doJSON(t, srv, call.method, call.path, payload, auth)
			if status != http.StatusBadRequest {
				t.Fatalf("%s %s with %s: status=%d body=%v, want 400", call.method, call.path, tc.name, status, body)
			}
			msg, _ := body["error"].(string)
			for _, phrase := range []string{"does not have the vector capability", "refused rather than ignored", "LIBRARIAN_VECTOR", "irreversible"} {
				if !strings.Contains(msg, phrase) {
					t.Fatalf("%s %s with %s: the refusal does not mention %q: %s", call.method, call.path, tc.name, phrase, msg)
				}
			}
		}
		t.Logf("POST and PUT /articles with %s -> 400, explained", tc.name)
	}

	// NOTHING was written or changed by any of those refusals.
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM articles`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("articles rows = %d after the refusals, want 1 (only the one created without an embedding)", count)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM articles WHERE id = ?`, id).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "base" {
		t.Fatalf("the refused PUT still updated the row: title=%q", title)
	}
	t.Logf("after every refusal: rows=%d, the row is untouched (title=%q)", count, title)
}

// TestAbsentEmbeddingIsNotARefusal pins the boundary: a client that never used
// the capability is not affected. An ABSENT field is not a statement about the
// embedding, so it passes — which is what keeps every existing caller working.
func TestAbsentEmbeddingIsNotARefusal(t *testing.T) {
	db, srv, cleanup := openNoVectorMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	token := jwtFor(t, db, "ed@example.com", "pw", "editor")

	status, body := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "T", "body": "B"}, authHeader(token))
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%v, want 201", status, body)
	}
	t.Log("a request with NO embedding field -> 201, unchanged")
}

// TestEmbeddingStillWorksWhenEnabled is the acceptance criterion "with the
// capability enabled nothing changes", checked at the same surface: the ordinary
// mux (built with the zero-value capabilities, like every pre-existing caller)
// still stores and returns a 1536-component embedding.
func TestEmbeddingStillWorksWhenEnabled(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()
	grant(t, db, "editor", "content.create")
	token := jwtFor(t, db, "ed@example.com", "pw", "editor")

	emb := dimVec(schema.EmbeddingDimension)
	status, body := doJSON(t, srv, http.MethodPost, "/articles",
		map[string]any{"title": "T", "body": "B", "embedding": emb}, authHeader(token))
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%v, want 201", status, body)
	}
	id, _ := body["id"].(string)
	_, got := doJSON(t, srv, http.MethodGet, "/articles/"+id, nil, authHeader(token))
	components, _ := got["embedding"].([]any)
	if len(components) != schema.EmbeddingDimension {
		t.Fatalf("read back %d components, want %d", len(components), schema.EmbeddingDimension)
	}
	t.Logf("capability enabled: %d components written and read back", len(components))
}
