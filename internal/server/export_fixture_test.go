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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	//   - article A: with metadata, then PUBLISHED via POST /articles/{id}/publish
	//   - article B: with metadata, left as draft (no publish)
	//   - article C: no metadata, left as draft
	// This exercises gen_random_uuid (ids), CURRENT_TIMESTAMP (created_at /
	// updated_at / published_at), the author_id FK, and the metadata JSON column.
	postArticle := func(title, body string, meta any) string {
		t.Helper()
		payload := map[string]any{"title": title, "body": body}
		if meta != nil {
			payload["metadata"] = meta
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

	idA := postArticle("Published With Meta", "body-A", map[string]any{"tags": []string{"export", "pg"}, "lang": "es"})
	_ = postArticle("Draft With Meta", "body-B", map[string]any{"reviewer": "carol", "n": 42})
	_ = postArticle("Draft No Meta", "body-C", nil)

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
	)
	if err := sdb.DB.QueryRow(`SELECT count(*) FROM articles`).Scan(&sqlCount); err != nil {
		t.Fatalf("sqlite count: %v", err)
	}
	if err := sdb.DB.QueryRow(`SELECT title, metadata, published_at FROM articles WHERE title = 'Published With Meta'`).
		Scan(&sqlTitle, &sqlMeta, &sqlPublished); err != nil {
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
	)
	if err := pg.DB.QueryRowContext(ctx,
		`SELECT title, metadata, published_at FROM articles WHERE title = 'Published With Meta'`).
		Scan(&pgTitle, &pgMeta, &pgPublished); err != nil {
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
	t.Logf("EXPORT_VERIFY_DONE: PG count=%d, published title MATCH, metadata JSON MATCH.", pgCount)
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