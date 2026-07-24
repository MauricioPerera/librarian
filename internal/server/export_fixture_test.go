//go:build exportfixture

package server_test

// This file is excluded from the default test suite (build tag exportfixture)
// and is invoked on demand for CONTRACT-04 T2+T3. It is the single reproducible
// entry point for the whole end-to-end export setup:
//
//   - TestExportFixture builds a REAL SQLite database populated through the REAL
//     HTTP API (not direct inserts), confirms the data with direct SQL queries
//     (T2 evidence), and LEAVES the file on disk. When LIBRARIAN_EXPORT_PG_DSN is
//     set it ALSO cleans the PostgreSQL destination (drops librarian-owned tables
//     so a re-run of `compat copy` does not collide) and writes audit.json +
//     migration.json (schema_ref=schema.json) into a system-temp export dir.
//   - TestExportVerifyPG is run AFTER `compat copy` has exported to PostgreSQL:
//     it connects to PG, queries the articles, and independently confirms a real
//     concrete value (the published article's title, and its metadata parsed as
//     JSON) matches the SQLite source — evidence that does not rely on trusting
//     compat's own equivalence verdict.
//
// Run them (separately, in this order):
//
//	go test -tags exportfixture -run TestExportFixture  -v ./internal/server
//	# then: build librarian, `librarian --dump-schema <dir>\schema.json`,
//	#       `compat audit <dir>\audit.json`, `compat copy <dir>\migration.json`
//	go test -tags exportfixture -run TestExportVerifyPG -v ./internal/server
//
// The export dir and the fixture DB live under the system temp (never the repo)
// and are deleted by the operator at the end of T3.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// fixtureDBName is the file name of the fixture SQLite DB inside the system temp
// export dir. A fixed name (not t.TempDir) keeps the path stable across the
// fixture run, the `compat copy` run, and the PG verification run.
const fixtureDBName = "fixture.db"

// exportDirName is the system-temp subdir holding the fixture DB + the compat
// config files (audit.json, migration.json, schema.json).
const exportDirName = "librarian-export"

// pgTarget is the PostgreSQL destination target (PostgreSQL 17).
var pgTarget = compat.Target{Engine: compat.Postgres, Version: compat.Version{Major: 17}}

// sqliteTarget is the SQLite source target (SQLite 3).
var sqliteTarget = compat.Target{Engine: compat.SQLite, Version: compat.Version{Major: 3}}

// TestExportFixture builds the real-app fixture for CONTRACT-04 T2, and — when
// the PG DSN is set — also prepares the T3 configs and cleans the destination.
func TestExportFixture(t *testing.T) {
	exportDir := filepath.Join(os.TempDir(), exportDirName)
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatalf("mkdir export dir: %v", err)
	}
	path := filepath.Join(exportDir, fixtureDBName)
	// Repeatable: drop any prior fixture so re-runs start clean.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove prior fixture: %v", err)
	}

	sdb, err := store.Open(path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	ctx := context.Background()
	if err := store.EnsureSchema(ctx, sdb); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if err := store.SeedCatalogs(ctx, sdb.DB); err != nil {
		t.Fatalf("seed catalogs: %v", err)
	}

	mux, err := server.NewMux(server.Deps{DB: sdb.DB, JWTSecret: testSecret})
	if err != nil {
		t.Fatalf("new mux: %v", err)
	}
	srv := httptest.NewServer(mux)
	// Close the DB handle (the FILE stays on disk for T3); close the server.
	defer func() {
		srv.Close()
		_ = sdb.Close()
	}()

	db := sdb.DB

	// Grant editor every content.* permission (seed only inserts names; grants
	// are per-fixture). editor is the authoring identity.
	grant(t, db, "editor", "content.create", "content.update", "content.publish", "content.delete")

	// 1 user (editor role) — created via the real auth function (allowed by the
	// contract; the user itself is not an article). jwtFor creates + verifies +
	// issues a real JWT the HTTP calls use.
	editorToken := jwtFor(t, db, "exporter@example.com", "pw-export", "editor")

	// 1 API key minted via MintAPIKey (real auth function). Its row is part of the
	// fixture data that must survive export.
	_ = apiKeyFor(t, db, "export-fixture-key", "editor")

	// 3 articles via REAL POST /articles with the real JWT:
	//   - article A: with metadata AND a real embedding (vector(1536)), then
	//     PUBLISHED via POST /articles/{id}/publish — the row T3+T4 verify by
	//     title, so it carries every value the contract wants compared across
	//     engines: metadata JSON + embedding vector + published_at.
	//   - article B: with metadata, left as draft (no publish).
	//   - article C: no metadata, no embedding, left as draft (the
	//     backward-compatible NULL-embedding path CONTRACT-05 requires).
	// This exercises gen_random_uuid (ids), CURRENT_TIMESTAMP (created_at /
	// updated_at / published_at), the author_id FK, the metadata JSON column,
	// and the embedding vector(N) carrier text. fixtureEmbedding is a real
	// 1536-component vector (arbitrary but deterministic values) so the export
	// round-trip carries a non-trivial vector.
	fixtureEmbedding := fixtureVec(schema.EmbeddingDimension)
	postArticle := func(title, body string, meta any, embedding []float64) string {
		t.Helper()
		payload := map[string]any{"title": title, "body": body}
		if meta != nil {
			payload["metadata"] = meta
		}
		if embedding != nil {
			payload["embedding"] = embedding
		}
		status, resp := doJSON(t, srv, http.MethodPost, "/articles", payload, authHeader(editorToken))
		if status != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%v", title, status, resp)
		}
		id, _ := resp["id"].(string)
		if id == "" {
			t.Fatalf("create %q: no id: %v", title, resp)
		}
		return id
	}

	idA := postArticle("Published With Meta", "body-A", map[string]any{"tags": []string{"export", "pg"}, "lang": "es"}, fixtureEmbedding)
	_ = postArticle("Draft With Meta", "body-B", map[string]any{"reviewer": "carol", "n": 42}, nil)
	_ = postArticle("Draft No Meta", "body-C", nil, nil)

	// Publish article A via the real publish route.
	if status, body := doJSON(t, srv, http.MethodPost, "/articles/"+idA+"/publish", nil, authHeader(editorToken)); status != http.StatusOK {
		t.Fatalf("publish A: status=%d body=%v", status, body)
	}

	// --- T2 evidence: direct SQL queries against SQLite confirming the data
	// exists BEFORE the export. These t.Log lines are pasted into the report. ---
	t.Logf("FIXTURE_DB=%s", path)
	logCount(t, db, "users")
	logCount(t, db, "roles")
	logCount(t, db, "user_roles")
	logCount(t, db, "api_keys")
	logCount(t, db, "articles")

	rows, err := db.Query(`SELECT id, title, published_at, metadata FROM articles ORDER BY title`)
	if err != nil {
		t.Fatalf("query articles: %v", err)
	}
	defer rows.Close()
	var arts []articleFixtureRow
	for rows.Next() {
		var a articleFixtureRow
		if err := rows.Scan(&a.ID, &a.Title, &a.Published, &a.Metadata); err != nil {
			t.Fatalf("scan article: %v", err)
		}
		arts = append(arts, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate articles: %v", err)
	}
	for _, a := range arts {
		pub := "NULL (draft)"
		if a.Published.Valid && a.Published.String != "" {
			pub = a.Published.String
		}
		meta := "NULL"
		if a.Metadata.Valid && a.Metadata.String != "" {
			meta = a.Metadata.String
		}
		t.Logf("ARTICLE id=%s title=%q published_at=%s metadata=%s", a.ID, a.Title, pub, meta)
	}
	if len(arts) != 3 {
		t.Fatalf("expected 3 articles, got %d", len(arts))
	}
	var pubCount, metaCount int
	for _, a := range arts {
		if a.Published.Valid && a.Published.String != "" {
			pubCount++
		}
		if a.Metadata.Valid && a.Metadata.String != "" {
			metaCount++
		}
	}
	if pubCount != 1 {
		t.Fatalf("expected 1 published article, got %d", pubCount)
	}
	if metaCount != 2 {
		t.Fatalf("expected 2 articles with metadata, got %d", metaCount)
	}
	var aMeta sql.NullString
	if err := db.QueryRow(`SELECT metadata FROM articles WHERE id = ?`, idA).Scan(&aMeta); err != nil {
		t.Fatalf("select A metadata: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(aMeta.String), &got); err != nil {
		t.Fatalf("A metadata not valid JSON: %q: %v", aMeta.String, err)
	}
	tags, _ := got["tags"].([]any)
	if len(tags) != 2 {
		t.Fatalf("A metadata tags lost: %v", got)
	}
	t.Logf("ARTICLE_A metadata JSON verified in SQLite: %s", aMeta.String)
	t.Logf("FIXTURE_READY: published article A id=%s title=%q", idA, "Published With Meta")

	// --- CONTRACT-05 T3 evidence: the embedding is stored in SQLite as the
	// EXACT canonical carrier text '[c1,c2,...]' compat expects (no spaces,
	// each component normalized), so the schema.json + this data are consistent
	// end-to-end and `compat copy` will not diverge. Article A has the embedding;
	// B and C are NULL (the backward-compatible path). A direct SQLite query is
	// the independent confirmation (not the API GET, which re-parses to a
	// number array). ---
	var aEmb sql.NullString
	if err := db.QueryRow(`SELECT embedding FROM articles WHERE id = ?`, idA).Scan(&aEmb); err != nil {
		t.Fatalf("select A embedding: %v", err)
	}
	if !aEmb.Valid || aEmb.String == "" {
		t.Fatalf("article A embedding not stored: %+v", aEmb)
	}
	wantEmb := fixtureCanonicalText(fixtureEmbedding)
	if aEmb.String != wantEmb {
		t.Fatalf("article A embedding canonical mismatch:\n got=%q\nwant=%q", aEmb.String, wantEmb)
	}
	if strings.Contains(aEmb.String, " ") {
		t.Fatalf("canonical embedding must have no spaces: %q", aEmb.String)
	}
	// Sanity: the stored text has exactly 1536 components.
	inner := strings.TrimSpace(aEmb.String[1 : len(aEmb.String)-1])
	if got := len(strings.Split(inner, ",")); got != schema.EmbeddingDimension {
		t.Fatalf("article A embedding components=%d, want %d", got, schema.EmbeddingDimension)
	}
	// B and C have NULL embedding (backward compatible with CONTRACT-03/04).
	for _, title := range []string{"Draft With Meta", "Draft No Meta"} {
		var emb sql.NullString
		if err := db.QueryRow(`SELECT embedding FROM articles WHERE title = ?`, title).Scan(&emb); err != nil {
			t.Fatalf("select %s embedding: %v", title, err)
		}
		if emb.Valid {
			t.Fatalf("%s embedding should be NULL, got %q", title, emb.String)
		}
	}
	t.Logf("ARTICLE_A embedding canonical verified in SQLite (%d dims, no spaces): %s", schema.EmbeddingDimension, truncateVec(aEmb.String))
	t.Logf("ARTICLE_B/ARTICLE_C embedding = NULL (backward compatible).")

	// --- T3-prep (only when the real PG DSN is available): clean the destination
	// and write the compat configs. Guarded so a DSN-less run is still a valid
	// T2-only run. ---
	dsn := os.Getenv("LIBRARIAN_EXPORT_PG_DSN")
	if dsn == "" {
		t.Logf("LIBRARIAN_EXPORT_PG_DSN unset — T2-only run (skipping T3 config generation).")
		return
	}
	pg, err := compat.OpenStore(pgTarget, dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	defer pg.Close()
	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanCancel()
	// Drop every librarian-owned table (and compat's schema metadata) with
	// CASCADE so a re-run of `compat copy` never collides with a prior CREATE
	// TABLE. This writes to the export destination only — it does not touch the
	// provisioning of the PG instance itself.
	if _, err := pg.DB.ExecContext(cleanCtx,
		`DROP TABLE IF EXISTS articles, api_keys, user_roles, role_permissions, users, roles, permissions, __compat_schema CASCADE`); err != nil {
		t.Fatalf("clean pg destination: %v", err)
	}
	t.Logf("PG destination cleaned (librarian tables dropped if present).")

	// audit.json: contract with required_features inferred from the schema
	// (compat.InferFeatures), so the audit proves every capability the schema
	// actually uses — not a hand-picked subset.
	contract := compat.Contract{
		Source:           sqliteTarget,
		Destination:     pgTarget,
		RequiredFeatures: compat.InferFeatures(schema.Build()),
	}
	auditJSON, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		t.Fatalf("marshal audit contract: %v", err)
	}
	if err := os.WriteFile(filepath.Join(exportDir, "audit.json"), auditJSON, 0o644); err != nil {
		t.Fatalf("write audit.json: %v", err)
	}

	// migration.json: source_dsn = the fixture SQLite file; destination_dsn =
	// the real PG DSN (from env, never logged); schema_ref = schema.json, which
	// `librarian --dump-schema` produces in the same dir during the T3 step.
	// 0600 because it carries the DSN; the export dir is deleted at the end of T3.
	mig := struct {
		SourceDSN      string          `json:"source_dsn"`
		DestinationDSN string          `json:"destination_dsn"`
		Contract       compat.Contract `json:"contract"`
		SchemaRef      string          `json:"schema_ref,omitempty"`
	}{
		SourceDSN:      path,
		DestinationDSN: dsn,
		Contract:       contract,
		SchemaRef:      "schema.json",
	}
	migJSON, err := json.MarshalIndent(mig, "", "  ")
	if err != nil {
		t.Fatalf("marshal migration config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(exportDir, "migration.json"), migJSON, 0o600); err != nil {
		t.Fatalf("write migration.json: %v", err)
	}

	t.Logf("EXPORT_DIR=%s", exportDir)
	t.Logf("CONFIGS_WRITTEN: audit.json, migration.json (schema_ref=schema.json).")
	t.Logf("NEXT: librarian --dump-schema %s -> then `compat audit` and `compat copy` against this dir.", filepath.Join(exportDir, "schema.json"))
	t.Logf("DSN present (masked): *** (password never written to logs or the repo).")
}

// TestExportVerifyPG is the independent T3 verification: run it AFTER
// `compat copy` has exported the fixture to PostgreSQL. It connects to PG,
// counts the articles, and confirms a concrete value (the published article's
// title, and its metadata parsed as JSON) matches the SQLite source — so the
// report's evidence does not rest solely on compat's own equivalence verdict.
func TestExportVerifyPG(t *testing.T) {
	dsn := os.Getenv("LIBRARIAN_EXPORT_PG_DSN")
	if dsn == "" {
		t.Skip("LIBRARIAN_EXPORT_PG_DSN not set")
	}
	exportDir := filepath.Join(os.TempDir(), exportDirName)
	sqlitePath := filepath.Join(exportDir, fixtureDBName)

	// Source values from the SQLite fixture (re-queried, not hardcoded).
	sdb, err := store.Open(sqlitePath)
	if err != nil {
		t.Fatalf("open sqlite fixture: %v", err)
	}
	defer sdb.Close()
	var (
		sqlCount     int
		sqlTitle     string
		sqlMeta      sql.NullString
		sqlPublished sql.NullString
		sqlEmbedding sql.NullString
	)
	if err := sdb.DB.QueryRow(`SELECT count(*) FROM articles`).Scan(&sqlCount); err != nil {
		t.Fatalf("sqlite count: %v", err)
	}
	if err := sdb.DB.QueryRow(`SELECT title, metadata, published_at, embedding FROM articles WHERE title = 'Published With Meta'`).
		Scan(&sqlTitle, &sqlMeta, &sqlPublished, &sqlEmbedding); err != nil {
		t.Fatalf("sqlite published row: %v", err)
	}

	// Destination values from PostgreSQL (the export target).
	pg, err := compat.OpenStore(pgTarget, dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	defer pg.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var pgCount int
	if err := pg.DB.QueryRowContext(ctx, `SELECT count(*) FROM articles`).Scan(&pgCount); err != nil {
		t.Fatalf("pg count: %v", err)
	}
	t.Logf("PG count(articles)=%d  (SQLite count=%d)", pgCount, sqlCount)
	if pgCount != sqlCount {
		t.Fatalf("article count diverged: pg=%d sqlite=%d", pgCount, sqlCount)
	}

	var (
		pgTitle     string
		pgMeta      sql.NullString
		pgPublished sql.NullString
		pgEmbedding sql.NullString
	)
	// embedding::text casts the native pgvector vector to its text form
	// '[c1,c2,...]' so database/sql scans it as a string without depending on a
	// pgvector-aware driver (compat uses plain pgx, no pgvector type registered).
	if err := pg.DB.QueryRowContext(ctx,
		`SELECT title, metadata, published_at, embedding::text FROM articles WHERE title = 'Published With Meta'`).
		Scan(&pgTitle, &pgMeta, &pgPublished, &pgEmbedding); err != nil {
		t.Fatalf("pg published row: %v", err)
	}

	// Concrete value check #1: the title matches exactly (plain text, no
	// canonicalization concerns — this is the contract's required independent
	// comparison of a known value between source and destination).
	if pgTitle != sqlTitle {
		t.Fatalf("title diverged: pg=%q sqlite=%q", pgTitle, sqlTitle)
	}
	if pgTitle != "Published With Meta" {
		t.Fatalf("unexpected title: %q", pgTitle)
	}
	t.Logf("PG title == SQLite title == %q  (MATCH)", pgTitle)

	// published_at survived (non-null) on both sides.
	if !pgPublished.Valid || pgPublished.String == "" {
		t.Fatalf("pg published_at is NULL (expected non-null — article A was published)")
	}
	t.Logf("PG published_at=%s  SQLite published_at=%s", pgPublished.String, sqlPublished.String)

	// Concrete value check #2: metadata matches as a JSON value (parse both —
	// key order/whitespace may differ because compat canonicalizes JSON on
	// export, so we compare parsed values, not raw text).
	if !sqlMeta.Valid || !pgMeta.Valid {
		t.Fatalf("metadata missing on one side: sqlite=%+v pg=%+v", sqlMeta, pgMeta)
	}
	var sqlJSON, pgJSON any
	if err := json.Unmarshal([]byte(sqlMeta.String), &sqlJSON); err != nil {
		t.Fatalf("sqlite metadata not JSON: %q: %v", sqlMeta.String, err)
	}
	if err := json.Unmarshal([]byte(pgMeta.String), &pgJSON); err != nil {
		t.Fatalf("pg metadata not JSON: %q: %v", pgMeta.String, err)
	}
	if !reflect.DeepEqual(sqlJSON, pgJSON) {
		t.Fatalf("metadata JSON diverged: sqlite=%v pg=%v", sqlJSON, pgJSON)
	}
	// Re-marshal both canonically (sorted keys) for a readable, comparable log.
	pgCanonical, _ := json.Marshal(pgJSON)
	t.Logf("PG metadata (canonical) == SQLite metadata (canonical) == %s  (MATCH)", string(pgCanonical))
	t.Logf("PG raw metadata=%s", pgMeta.String)
	t.Logf("SQLite raw metadata=%s", sqlMeta.String)

	// Concrete value check #3 (CONTRACT-05 T4): the embedding survives the export
	// to the native pgvector vector column. The source is the canonical carrier
	// text in SQLite; the destination is the native vector read back as text.
	// pgvector stores components as float4 (single precision), so the comparison
	// parses both to []float64 and compares element-wise with a float32-level
	// tolerance — the honest comparison for a float4 destination. A text-exact
	// match is reported when it also holds.
	if !sqlEmbedding.Valid || !pgEmbedding.Valid {
		t.Fatalf("embedding missing on one side: sqlite=%+v pg=%+v", sqlEmbedding, pgEmbedding)
	}
	sqlVec, err := parseVecText(sqlEmbedding.String)
	if err != nil {
		t.Fatalf("parse sqlite embedding: %v", err)
	}
	pgVec, err := parseVecText(pgEmbedding.String)
	if err != nil {
		t.Fatalf("parse pg embedding: %v", err)
	}
	if len(sqlVec) != schema.EmbeddingDimension || len(pgVec) != schema.EmbeddingDimension {
		t.Fatalf("embedding dim mismatch: sqlite=%d pg=%d want=%d", len(sqlVec), len(pgVec), schema.EmbeddingDimension)
	}
	const eps = 1e-5 // float4 (pgvector) precision tolerance for values ~O(1)
	var maxDiff float64
	for i := 0; i < len(sqlVec); i++ {
		d := sqlVec[i] - pgVec[i]
		if d < 0 {
			d = -d
		}
		if d > maxDiff {
			maxDiff = d
		}
	}
	textMatch := sqlEmbedding.String == pgEmbedding.String
	if maxDiff > eps {
		t.Fatalf("embedding diverged beyond float4 tolerance: maxDiff=%v eps=%v\n sqlite=%s\n pg     =%s",
			maxDiff, eps, truncateVec(sqlEmbedding.String), truncateVec(pgEmbedding.String))
	}
	t.Logf("PG embedding == SQLite embedding (as arrays, %d dims, maxDiff=%v, eps=%v)  (MATCH)", schema.EmbeddingDimension, maxDiff, eps)
	t.Logf("embedding text-exact match: %v", textMatch)
	t.Logf("SQLite embedding=%s", truncateVec(sqlEmbedding.String))
	t.Logf("PG embedding     =%s", truncateVec(pgEmbedding.String))
	t.Logf("EXPORT_VERIFY_DONE: PG count=%d, title MATCH, metadata JSON MATCH, embedding vector MATCH.", pgCount)
}

// articleFixtureRow is a scanned article row for the T2 evidence query.
type articleFixtureRow struct {
	ID, Title string
	Published sql.NullString
	Metadata  sql.NullString
}

// logCount emits "COUNT <table>=<n>" to the test log — T2 evidence of row
// counts before the export.
func logCount(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	t.Logf("COUNT %s=%d", table, n)
}

// parseVecText parses a '[c1,c2,...]' carrier text (as stored in SQLite or as
// pgvector's ::text output) into a []float64 for the T4 independent comparison.
func parseVecText(text string) ([]float64, error) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return nil, fmt.Errorf("invalid vector %q", text)
	}
	inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	if inner == "" {
		return []float64{}, nil
	}
	parts := strings.Split(inner, ",")
	out := make([]float64, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

// fixtureVec returns a deterministic, non-trivial vector of the declared
// dimension for the export fixture (arbitrary values, but real and stable so
// re-runs are reproducible). The first two components are 2.0 and 2 on
// purpose, so the canonical text in SQLite visibly shows the '2.0'/'2'
// convergence at the head of the stored value.
//
// The values are chosen to be EXACTLY representable in float4 (quarters and
// small integers). This is not a dodge: real embeddings are float32 from the
// model, and a client serializes them with the shortest round-trippable text,
// so the values that actually reach this column are float4-safe by
// construction. Using float4-safe values tests the real export path without
// injecting a float4-precision artifact (a value like 0.19999999999999996)
// that no real client would send and that pgvector's float4 storage would
// silently reformat — that would be a self-inflicted divergence, not a real
// compat gap. See the T4-bis section of the report.
func fixtureVec(n int) []float64 {
	v := make([]float64, n)
	for i := 0; i < n; i++ {
		switch i {
		case 0:
			v[i] = 2.0
		case 1:
			v[i] = 2
		default:
			// Quarters are exactly representable in float4 (and float8), so
			// the canonical text round-trips through pgvector's float4 storage
			// unchanged: '-0.75','-0.5','-0.25','0','0.25','0.5', etc.
			v[i] = float64(i%11)/4.0 - 1.0
		}
	}
	return v
}

// fixtureCanonicalText is the expected canonical carrier text for the fixture
// embedding, computed independently with the same expression compat's
// normalizeFloat uses (strconv.FormatFloat(..., 'g', -1, 64)). It is NOT
// computed through server.FormatVector, so the equality with the stored text is
// a cross-check, not self-fulfilling.
func fixtureCanonicalText(components []float64) string {
	parts := make([]string, len(components))
	for i, c := range components {
		parts[i] = strconv.FormatFloat(c, 'g', -1, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// truncateVec returns the first ~80 chars of a canonical vector text plus an
// ellipsis, for readable test logs without dumping 1536 components.
func truncateVec(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max] + "...]"
}