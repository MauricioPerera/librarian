//go:build dualengine

package server

// CONTRACT-23 T4 — THE BATTERY THAT CLOSES THE HUECO, and the only one that can:
// a librarian installation running on a PostgreSQL 17 that does NOT have (and
// cannot have) the pgvector extension.
//
// It needs a SECOND PostgreSQL, different from the one every other dual-engine
// battery uses, and the difference between the two IS the contract:
//
//	COMPAT_POSTGRES_DSN='postgres://…:***@host:5445/postgres?sslmode=disable'  # WITH pgvector
//	LIBRARIAN_PG_NO_VECTOR_DSN='postgres://…:***@host:5446/postgres?sslmode=disable'  # WITHOUT
//
//	LIBRARIAN_PG_NO_VECTOR_DSN=… go test -tags dualengine -run TestVectorOptional -count=1 -v ./internal/server
//
// Without the variable the tests SKIP rather than passing vacuously. The battery
// asserts FIRST that the target genuinely cannot resolve the `vector` type, so
// it can never pass by accident against a server that happens to have pgvector.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/pgtestdb"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

const pgNoVectorDSNEnv = "LIBRARIAN_PG_NO_VECTOR_DSN"

var (
	capsOn  = schema.Capabilities{}
	capsOff = schema.Capabilities{VectorDisabled: true}
)

// noVectorDSN returns the DSN of the PostgreSQL WITHOUT pgvector, or skips.
func noVectorDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(pgNoVectorDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a PostgreSQL that lacks pgvector", pgNoVectorDSNEnv)
	}
	return dsn
}

// openScopedPostgres opens a connection to a fresh, private database and returns
// it with a dropper. It does NOT apply any schema: what each test does next is
// the thing under test.
//
// CONTRACT-29 UPDATE: this used to be a schema per run reached with
// `search_path=<schema>,public`. It is now a DATABASE per run
// (internal/pgtestdb). The distinction this battery lives on survives intact:
// pgtestdb installs pgvector only when the SERVER offers it, so against
// LIBRARIAN_PG_NO_VECTOR_DSN the `vector` type stays unresolvable — which
// requireNoPgvector asserts immediately afterwards.
func openScopedPostgres(t *testing.T, dsn, prefix string) (*compat.Store, func()) {
	t.Helper()
	return pgtestdb.Provision(t, dsn, prefix)
}

// requireNoPgvector proves the target is the RIGHT target. Without this the
// whole battery could pass against a server that has the extension.
func requireNoPgvector(t *testing.T, st *compat.Store) {
	t.Helper()
	var resolvable bool
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT to_regtype('vector') IS NOT NULL`).Scan(&resolvable); err != nil {
		t.Fatalf("probe to_regtype('vector'): %v", err)
	}
	if resolvable {
		t.Fatalf("%s points at a PostgreSQL that CAN resolve the vector type — this battery must run against one that cannot", pgNoVectorDSNEnv)
	}
	var version string
	if err := st.DB.QueryRowContext(context.Background(), `SELECT version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("target: %s", strings.SplitN(version, " on ", 2)[0])
	t.Log("to_regtype('vector') IS NULL — pgvector is NOT available here")
}

// TestVectorOptionalPostgresWithoutPgvector is the acceptance criterion of the
// contract: a CLEAN installation with the capability disabled comes up on a
// PostgreSQL without pgvector, and serves — identity, authentication, and the
// full article CRUD over the real HTTP mux. It also records, in the same run,
// that the SAME database with the capability ENABLED still fails (the hueco,
// reproduced), so the difference is attributable to the capability and nothing
// else.
func TestVectorOptionalPostgresWithoutPgvector(t *testing.T) {
	dsn := noVectorDSN(t)

	// --- 1. The hueco, reproduced ------------------------------------------
	broken, dropBroken := openScopedPostgres(t, dsn, "librarian_c23_on")
	defer dropBroken()
	requireNoPgvector(t, broken)

	err := store.EnsureSchemaFor(context.Background(), broken, capsOn)
	if err == nil {
		t.Fatal("the schema WITH the vector capability was applied on a PostgreSQL without pgvector")
	}
	if !strings.Contains(err.Error(), "pgvector") {
		t.Fatalf("the refusal does not name pgvector: %v", err)
	}
	t.Logf("capability ENABLED  -> REFUSED as before: %v", err)

	// --- 2. The same database, the capability not declared ------------------
	live, dropLive := openScopedPostgres(t, dsn, "librarian_c23_off")
	defer dropLive()
	ctx := context.Background()
	if err := store.EnsureSchemaFor(ctx, live, capsOff); err != nil {
		t.Fatalf("capability DISABLED still failed on a PostgreSQL without pgvector: %v", err)
	}
	if err := store.SeedCatalogs(ctx, live); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A restart, because the metadata bug of CONTRACT-11 only shows on boot #2.
	if err := store.EnsureSchemaFor(ctx, live, capsOff); err != nil {
		t.Fatalf("boot #2: %v", err)
	}
	t.Log("capability DISABLED -> schema applied, twice, on a PostgreSQL WITHOUT pgvector")

	installed, exists, err := store.InstalledCapabilities(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || installed.Vector() {
		t.Fatalf("InstalledCapabilities = (%v, %v), want an installed, vector-less installation", installed, exists)
	}
	t.Log("InstalledCapabilities (read from information_schema, not from __compat_schema): vector disabled")

	// --- 3. It SERVES -------------------------------------------------------
	h := newCapsHarness(t, live, capsOff)

	status, body := h.doJSON(t, http.MethodGet, "/health", nil)
	t.Logf("GET /health -> %d %v", status, body)

	status, body = h.doJSON(t, http.MethodGet, "/whoami", nil)
	if status != http.StatusOK {
		t.Fatalf("whoami: %d %v", status, body)
	}
	t.Logf("GET /whoami -> %d roles=%v", status, body["roles"])

	status, body = h.doJSON(t, http.MethodPost, "/articles",
		map[string]any{"title": "sin pgvector", "body": "cuerpo"})
	if status != http.StatusCreated {
		t.Fatalf("create article: %d %v", status, body)
	}
	id, _ := body["id"].(string)
	t.Logf("POST /articles -> %d id issued: %v", status, id != "")

	status, body = h.doJSON(t, http.MethodGet, "/articles/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get article: %d %v", status, body)
	}
	if _, present := body["embedding"]; present {
		t.Fatalf("the read carries an embedding: %v", body)
	}
	t.Logf("GET /articles/{id} -> %d title=%v, embedding field: absent", status, body["title"])

	status, _ = h.doJSON(t, http.MethodGet, "/articles", nil)
	t.Logf("GET /articles -> %d", status)

	status, body = h.doJSON(t, http.MethodPut, "/articles/"+id,
		map[string]any{"title": "editado", "body": "cuerpo 2"})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, body)
	}
	t.Logf("PUT /articles/{id} -> %d title=%v", status, body["title"])

	status, body = h.doJSON(t, http.MethodPost, "/articles/"+id+"/publish", nil)
	if status != http.StatusOK {
		t.Fatalf("publish: %d %v", status, body)
	}
	t.Logf("POST /articles/{id}/publish -> %d published_at set: %v", status, body["published_at"] != nil)

	// --- 4. T2 over the real surface, on the real engine --------------------
	status, body = h.doJSON(t, http.MethodPost, "/articles", map[string]any{
		"title": "con embedding", "body": "b", "embedding": dimVec(schema.EmbeddingDimension),
	})
	if status != http.StatusBadRequest {
		t.Fatalf("an embedding was accepted: %d %v", status, body)
	}
	t.Logf("POST /articles with an embedding -> %d %q", status, body["error"])

	status, body = h.doJSON(t, http.MethodPut, "/articles/"+id, map[string]any{
		"title": "t", "body": "b", "embedding": nil,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("an explicit null embedding was accepted: %d %v", status, body)
	}
	t.Logf("PUT /articles/{id} with embedding:null -> %d (refused, not ignored)", status)

	status, _ = h.doJSON(t, http.MethodDelete, "/articles/"+id, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	status, _ = h.doJSON(t, http.MethodDelete, "/articles/"+id, nil)
	t.Logf("DELETE /articles/{id} -> 204, again -> %d", status)

	// --- 5. T3 on the real engine, both directions --------------------------
	if err := store.EnsureSchemaFor(ctx, live, capsOn); err == nil {
		t.Fatal("the guard did not fire when enabling the capability on an existing installation")
	} else {
		t.Logf("T3 (enable later)  -> REFUSED: %v", err)
	}

	withVector, dropWithVector := openScopedPostgres(t, os.Getenv(pgDSNEnv), "librarian_c23_flip")
	if os.Getenv(pgDSNEnv) == "" {
		t.Logf("T3 (disable later) -> skipped: %s is not set", pgDSNEnv)
		return
	}
	defer dropWithVector()
	if err := store.EnsureSchemaFor(ctx, withVector, capsOn); err != nil {
		t.Fatalf("apply the enabled schema on the pgvector server: %v", err)
	}
	if err := store.EnsureSchemaFor(ctx, withVector, capsOff); err == nil {
		t.Fatal("the guard did not fire when disabling the capability on an existing installation")
	} else {
		t.Logf("T3 (disable later) -> REFUSED: %v", err)
	}
}

// newCapsHarness is newHarness for a declared capability set: it builds the real
// mux the way cmd/librarian does, with the same capabilities the schema was
// applied with.
func newCapsHarness(t *testing.T, st *compat.Store, caps schema.Capabilities) *engineHarness {
	t.Helper()
	ctx := context.Background()
	if err := auth.SetRolePermissions(ctx, st, "administrator", schema.Permissions); err != nil {
		t.Fatalf("grant permissions: %v", err)
	}
	if _, err := auth.CreateUser(ctx, st, "admin@example.com", "correct-horse", []string{"administrator"}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h := &handlers{db: st.DB, store: st, jwtSecret: dualJWTSecret, now: time.Now, caps: caps}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	h.registerRoutes(mux)

	harness := &engineHarness{mux: mux, store: st}
	status, body := harness.do(t, http.MethodPost, "/auth/login", map[string]any{
		"email": "admin@example.com", "password": "correct-horse",
	})
	if status != http.StatusOK {
		t.Fatalf("login: status %d body %s", status, body)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &login); err != nil {
		t.Fatalf("login body: %v", err)
	}
	harness.admin = login.Token
	return harness
}

// dimVec is a deterministic vector of the declared dimension (the default suite
// has its own; this build tag excludes that file's package-level helpers from
// this one's compilation unit only when tags differ, so it is spelled here).
func dimVec(n int) []float64 {
	v := make([]float64, n)
	for i := 0; i < n; i++ {
		v[i] = float64(i%7)/3.0 - 1.0
	}
	return v
}
