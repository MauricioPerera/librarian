//go:build dualengine

package server

// CONTRACT-20 T3 — the battery that gives the contract its meaning: the SAME
// HTTP surface of internal/server, exercised against a REAL SQLite file and a
// REAL PostgreSQL 17 server, must produce the SAME observable results.
//
// It continues the CONTRACT-19 pattern (internal/auth/dualengine_contract19_test.go)
// and is excluded from the default suite by the same build tag `dualengine`,
// because it needs a live PostgreSQL with pgvector. Run it with:
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineServer -count=1 -v ./internal/server
//
// Without COMPAT_POSTGRES_DSN the tests SKIP rather than passing vacuously.
//
// WHAT IS COMPARED, and why it is stronger than CONTRACT-19's. CONTRACT-19 drove
// the public Go functions of internal/auth. This battery drives the REAL HTTP
// MUX: every observation is a status code and a response body produced by the
// same handlers a browser or an API client reaches. Each engine runs the
// identical scenario, appending one line per OBSERVATION to a transcript, and
// the two transcripts must be byte-identical. Anything intrinsically per-run
// (uuids, timestamps, JWT secrets) is deliberately NOT recorded; what IS
// recorded is every status code, every error message, every field value a caller
// can see, and — critically — the ORDER of every listing.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

const pgDSNEnv = "COMPAT_POSTGRES_DSN"

// dynamicTypeName is the dynamic content type this battery CREATES during the
// run, so the generic CRUD layer is exercised over a type that did not exist
// when the process started — the contract asks for exactly that.
const dynamicTypeName = "reviews"

// dynamicTypeDef is the definition of that type. It uses ALL FIVE field types on
// purpose: text and date are TEXT on both engines, but integer, decimal and
// boolean are the three families that are PHYSICALLY DIFFERENT (INTEGER/BIGINT,
// TEXT/NUMERIC, INTEGER/BOOLEAN). Those three are the entire reason content.go's
// reads became routines, so the battery has to cover them.
var dynamicTypeDef = schema.ContentTypeDefinition{
	Name: dynamicTypeName,
	Fields: []schema.FieldDefinition{
		{Name: "headline", Type: schema.FieldText},
		{Name: "score", Type: schema.FieldInteger},
		{Name: "price_paid", Type: schema.FieldDecimal},
		{Name: "verified", Type: schema.FieldBoolean},
		{Name: "read_on", Type: schema.FieldDate},
	},
}

// TestDualEngineServer is the whole battery: it builds the librarian schema on
// both engines, runs the same HTTP scenario on each, and requires the
// transcripts to match line for line.
func TestDualEngineServer(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteStore, closeSQLite := openSQLiteEngine(t)
	defer closeSQLite()
	pgStore, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	sqliteTranscript := runServerScenario(t, "sqlite", sqliteStore)
	pgTranscript := runServerScenario(t, "postgres", pgStore)

	t.Logf("transcript (%d lines, identical on both engines):\n%s",
		len(sqliteTranscript), strings.Join(sqliteTranscript, "\n"))

	if len(sqliteTranscript) != len(pgTranscript) {
		t.Fatalf("transcript length differs: sqlite=%d postgres=%d", len(sqliteTranscript), len(pgTranscript))
	}
	diverged := 0
	for i := range sqliteTranscript {
		if sqliteTranscript[i] != pgTranscript[i] {
			diverged++
			t.Errorf("line %d diverges:\n  sqlite  : %s\n  postgres: %s", i+1, sqliteTranscript[i], pgTranscript[i])
		}
	}
	if diverged == 0 {
		t.Logf("OK: %d observations identical on SQLite and PostgreSQL 17", len(sqliteTranscript))
	}
}

// --- engine fixtures ---------------------------------------------------------

// openSQLiteEngine builds the SQLite side through the PRODUCTION path, so the
// battery also proves the real startup path creates the CONTRACT-20 views.
func openSQLiteEngine(t *testing.T) (*compat.Store, func()) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "dual20.db"))
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	ctx := context.Background()
	if err := store.EnsureSchema(ctx, st); err != nil {
		t.Fatalf("sqlite ensure schema: %v", err)
	}
	if err := store.SeedCatalogs(ctx, st.DB); err != nil {
		t.Fatalf("sqlite seed: %v", err)
	}
	return st, func() { _ = st.Close() }
}

// openPostgresEngine builds the PostgreSQL side in a FRESH, uniquely named
// schema so a run can never collide with leftovers, and drops it afterwards.
// Same approach as CONTRACT-19: compat's own ApplySchema over schema.Build(),
// because internal/store's metadata/seed statements are still SQLite-only
// (migrating them is CONTRACT-21 and is deliberately not anticipated).
func openPostgresEngine(t *testing.T, dsn string) (*compat.Store, func()) {
	t.Helper()
	ctx := context.Background()
	schemaName := fmt.Sprintf("librarian_c20_%d", time.Now().UnixNano())

	admin, err := compat.OpenPostgres(schema.PostgresVersion, dsn)
	if err != nil {
		t.Fatalf("postgres open (admin): %v", err)
	}
	if _, err := admin.DB.ExecContext(ctx, `CREATE SCHEMA "`+schemaName+`"`); err != nil {
		_ = admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	scoped, err := compat.OpenPostgres(schema.PostgresVersion, withSearchPath(dsn, schemaName))
	if err != nil {
		t.Fatalf("postgres open (scoped): %v", err)
	}
	var current string
	if err := scoped.DB.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	if current != schemaName {
		t.Fatalf("current_schema() = %q, want %q — the scoped connection is not isolated", current, schemaName)
	}

	if err := scoped.ApplySchema(ctx, schema.Build()); err != nil {
		t.Fatalf("postgres apply schema: %v", err)
	}
	seedPostgresCatalogs(t, scoped)

	return scoped, func() {
		_ = scoped.Close()
		if _, err := admin.DB.ExecContext(context.Background(), `DROP SCHEMA "`+schemaName+`" CASCADE`); err != nil {
			t.Logf("drop schema %s: %v", schemaName, err)
		}
		_ = admin.Close()
	}
}

// withSearchPath pins every connection of the pool to one schema. `public` stays
// as a SECOND entry only so the pgvector `vector` TYPE resolves; the run's own
// schema stays first and current_schema() is asserted right after connecting.
func withSearchPath(dsn, schemaName string) string {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "search_path=" + schemaName + ",public"
}

// seedPostgresCatalogs inserts the fixed catalogs on the PostgreSQL side.
func seedPostgresCatalogs(t *testing.T, st *compat.Store) {
	t.Helper()
	ctx := context.Background()
	insert := func(table string, names []string) {
		statement := `INSERT INTO "` + table + `" ("name") VALUES (` + compat.Placeholder(compat.Postgres, 1) + `)`
		for _, name := range names {
			if _, err := st.DB.ExecContext(ctx, statement, name); err != nil {
				t.Fatalf("seed %s %q: %v", table, name, err)
			}
		}
	}
	insert("roles", schema.Roles)
	insert("permissions", schema.Permissions)
	insert("taxonomies", schema.Taxonomies)
}

// createDynamicType creates the dynamic content type's REAL table and its
// registry rows, on whichever engine the store is bound to.
//
// It does NOT go through store.CreateContentType: that helper writes compat's
// __compat_schema metadata with statements internal/store has not migrated yet
// (CONTRACT-21). What this battery must prove is that the GENERIC CRUD LAYER —
// the part CONTRACT-20 migrated — works on both engines over a type created at
// runtime, so the type is created here with compat's own DDL compiler plus two
// parameterized inserts, and the routes are then driven normally.
func createDynamicType(t *testing.T, st *compat.Store, def schema.ContentTypeDefinition) {
	t.Helper()
	ctx := context.Background()
	engine := st.Target.Engine

	table, err := schema.DynamicTable(def)
	if err != nil {
		t.Fatalf("dynamic table: %v", err)
	}
	statements, err := compat.CompileDDL(st.Target, compat.Schema{Tables: []compat.Table{table}})
	if err != nil {
		t.Fatalf("compile dynamic table: %v", err)
	}
	for _, statement := range statements {
		if _, err := st.DB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create dynamic table: %v", err)
		}
	}

	typeID := mustUUID(t)
	insertType := `INSERT INTO "` + schema.ContentTypesTable + `" ("id", "name") VALUES (` +
		compat.Placeholder(engine, 1) + `, ` + compat.Placeholder(engine, 2) + `)`
	if _, err := st.DB.ExecContext(ctx, insertType, typeID, def.Name); err != nil {
		t.Fatalf("insert content type: %v", err)
	}
	insertField := `INSERT INTO "` + schema.ContentTypeFieldsTable +
		`" ("id", "content_type_id", "name", "field_type", "ordinal") VALUES (` +
		compat.Placeholder(engine, 1) + `, ` + compat.Placeholder(engine, 2) + `, ` +
		compat.Placeholder(engine, 3) + `, ` + compat.Placeholder(engine, 4) + `, ` +
		compat.Placeholder(engine, 5) + `)`
	for i, f := range def.Fields {
		if _, err := st.DB.ExecContext(ctx, insertField, mustUUID(t), typeID, f.Name, string(f.Type), i); err != nil {
			t.Fatalf("insert content type field %q: %v", f.Name, err)
		}
	}
}

func mustUUID(t *testing.T) string {
	t.Helper()
	id, err := dual.NewUUID()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id
}

// --- the HTTP harness --------------------------------------------------------

// engineHarness is one engine's full server: the mux built over that engine's
// store, plus the bearer token of an administrator who holds every permission.
type engineHarness struct {
	mux   *http.ServeMux
	admin string // bearer token
	store *compat.Store
}

const dualJWTSecret = "contract-20-dual-engine-secret"

// newHarness builds the real mux over the given store and logs an administrator
// in through the real POST /auth/login route.
func newHarness(t *testing.T, st *compat.Store) *engineHarness {
	t.Helper()
	ctx := context.Background()

	// The administrator role gets every permission in the catalog, so a 403 in
	// the transcript can only ever mean a genuine authorization decision.
	if err := auth.SetRolePermissions(ctx, st, "administrator", schema.Permissions); err != nil {
		t.Fatalf("grant permissions: %v", err)
	}
	if _, err := auth.CreateUser(ctx, st, "admin@example.com", "correct-horse", []string{"administrator"}); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	h := &handlers{db: st.DB, store: st, jwtSecret: dualJWTSecret, now: time.Now}
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

// do performs one request as the administrator and returns the status and the
// raw body. A nil payload sends no body.
func (e *engineHarness) do(t *testing.T, method, path string, payload any) (int, string) {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode payload: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	if e.admin != "" {
		req.Header.Set("Authorization", "Bearer "+e.admin)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

// doJSON performs a request and decodes the body as a generic JSON object.
func (e *engineHarness) doJSON(t *testing.T, method, path string, payload any) (int, map[string]any) {
	t.Helper()
	status, body := e.do(t, method, path, payload)
	out := map[string]any{}
	if body != "" {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s %s: decode body %q: %v", method, path, body, err)
		}
	}
	return status, out
}

// --- transcript --------------------------------------------------------------

type transcript struct{ lines []string }

func (tr *transcript) add(format string, args ...any) {
	tr.lines = append(tr.lines, fmt.Sprintf(format, args...))
}

// errorMessage extracts the {"error": "..."} envelope message, or "<none>".
func errorMessage(body map[string]any) string {
	if v, ok := body["error"].(string); ok {
		return v
	}
	return "<none>"
}

// str reads a string field, rendering absence as "<absent>" so a missing field
// and an empty one are distinguishable in the transcript.
func str(body map[string]any, key string) string {
	v, ok := body[key]
	if !ok || v == nil {
		return "<absent>"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// titles extracts the "title" of every element of a listing, IN ORDER. The order
// is the whole point: a listing that fell back to the engine's natural order
// could not produce the same sequence on both.
func titles(body map[string]any, key string) []string {
	items, _ := body[key].([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		out = append(out, fmt.Sprint(row["title"]))
	}
	return out
}

// field extracts one named field of every element of a listing, IN ORDER.
func field(body map[string]any, key, name string) []string {
	items, _ := body[key].([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		v, ok := row[name]
		if !ok || v == nil {
			out = append(out, "<null>")
			continue
		}
		out = append(out, fmt.Sprint(v))
	}
	return out
}

// embeddingOf extracts the parsed embedding array of a single-article body.
func embeddingOf(body map[string]any) []float64 {
	raw, ok := body["embedding"].([]any)
	if !ok {
		return nil
	}
	out := make([]float64, 0, len(raw))
	for _, v := range raw {
		f, _ := v.(float64)
		out = append(out, f)
	}
	return out
}

// --- the embedding fixture ---------------------------------------------------

// float32ExactEmbedding builds a vector(1536) whose every component is EXACTLY
// representable in binary32. That is deliberate and it is the single most
// important fixture decision of this battery: pgvector stores float4, so a
// component needing more than single precision is truncated on PostgreSQL and
// kept in full on SQLite. Using float32-exact components isolates what this
// contract actually controls — that the value is bound, stored, read back and
// canonicalized identically — from a storage property of pgvector that no amount
// of consumer code can change. The truncation itself is measured separately and
// on purpose by TestDualEngineVectorPrecision.
func float32ExactEmbedding(seed float64) []float64 {
	out := make([]float64, schema.EmbeddingDimension)
	// Powers of two and their exact sums are identical in binary32 and binary64.
	pattern := []float64{1, -2, 0.5, 0.25, 3.75, -0.125, 16, 0}
	for i := range out {
		out[i] = pattern[i%len(pattern)] * seed
	}
	return out
}

// componentReport summarises a read-back embedding by comparing it COMPONENT BY
// COMPONENT against what was written, which is what the contract asks for. It
// records the count, the first mismatching index (or none) and the first few
// components, so both a length change and a value change are visible.
func componentReport(written, read []float64) string {
	if len(written) != len(read) {
		return fmt.Sprintf("len-written=%d len-read=%d MISMATCHED-LENGTH", len(written), len(read))
	}
	firstBad := -1
	for i := range written {
		if written[i] != read[i] {
			firstBad = i
			break
		}
	}
	head := make([]string, 0, 6)
	for i := 0; i < 6 && i < len(read); i++ {
		head = append(head, formatVectorComponent(read[i]))
	}
	return fmt.Sprintf("len=%d all-components-identical=%t first-divergent-index=%d head=[%s]",
		len(read), firstBad == -1, firstBad, strings.Join(head, ","))
}

// --- the scenario ------------------------------------------------------------

// runServerScenario exercises every migrated surface of internal/server through
// the real HTTP mux, in a fixed order, and returns the transcript of what was
// observed.
func runServerScenario(t *testing.T, engine string, st *compat.Store) []string {
	t.Helper()
	ctx := context.Background()
	tr := &transcript{}
	createDynamicType(t, st, dynamicTypeDef)
	e := newHarness(t, st)

	const missingID = "11111111-1111-1111-1111-111111111111"
	const malformedID = "not-a-uuid"

	// ======================================================================
	// 1. authz.go — permission resolution, both identity kinds
	// ======================================================================
	h := &handlers{db: st.DB, store: st, jwtSecret: dualJWTSecret, now: time.Now}

	// The JWT branch: a variable-size list of role NAMES over the new view.
	perms, err := h.permissionsForRoles(ctx, []string{"administrator"})
	if err != nil {
		t.Fatalf("[%s] permissionsForRoles: %v", engine, err)
	}
	tr.add("authz jwt-branch roles=[administrator] order=%v", perms)

	// Two roles at once, one of them without grants: the DISTINCT and the union
	// must behave the same on both engines.
	if err := auth.SetRolePermissions(ctx, st, "editor", []string{"content.create", "content.update"}); err != nil {
		t.Fatalf("[%s] grant editor: %v", engine, err)
	}
	perms, _ = h.permissionsForRoles(ctx, []string{"editor", "author"})
	tr.add("authz jwt-branch roles=[editor author] order=%v", perms)
	perms, _ = h.permissionsForRoles(ctx, []string{"administrator", "editor"})
	tr.add("authz jwt-branch union-with-overlap order=%v", perms)
	perms, _ = h.permissionsForRoles(ctx, nil)
	tr.add("authz jwt-branch empty-roles perms=%v len=%d", perms, len(perms))
	perms, _ = h.permissionsForRoles(ctx, []string{"ghost-role"})
	tr.add("authz jwt-branch unknown-role perms=%v len=%d", perms, len(perms))

	// The API-key branch: one role id, through the routine over CONTRACT-19's view.
	editorID := roleIDOf(t, st, "editor")
	perms, err = h.permissionsForRoleID(ctx, editorID)
	if err != nil {
		t.Fatalf("[%s] permissionsForRoleID: %v", engine, err)
	}
	tr.add("authz apikey-branch role=editor order=%v", perms)
	perms, _ = h.permissionsForRoleID(ctx, missingID)
	tr.add("authz apikey-branch unknown-role-id perms=%v len=%d", perms, len(perms))

	// The three single-row catalog reads the ui_*.go files use.
	gotID, found, err := h.roleIDForName(ctx, "editor")
	tr.add("catalog roleIDForName editor found=%t err=%t matches=%t", found, err != nil, gotID == editorID)
	_, found, _ = h.roleIDForName(ctx, "superadmin")
	tr.add("catalog roleIDForName unknown found=%t", found)
	names, err := h.actorRoleNames(ctx, &Identity{Kind: "apikey", RoleID: editorID})
	tr.add("catalog actorRoleNames apikey=%v err=%t", names, err != nil)
	names, _ = h.actorRoleNames(ctx, &Identity{Kind: "apikey", RoleID: missingID})
	tr.add("catalog actorRoleNames vanished-role=%v len=%d", names, len(names))
	rolePerms, err := h.rolesWithPermissions(ctx)
	if err != nil {
		t.Fatalf("[%s] rolesWithPermissions: %v", engine, err)
	}
	roleOrder := make([]string, 0, len(rolePerms))
	for _, rp := range rolePerms {
		roleOrder = append(roleOrder, rp.Name+":"+strings.Join(rp.Permissions, "|"))
	}
	tr.add("catalog rolesWithPermissions order=%v", roleOrder)

	// ======================================================================
	// 2. articles — the vector surface
	// ======================================================================
	// Created oldest-first with titles whose alphabetical order is the REVERSE
	// of creation order, so a listing that silently fell back to natural order,
	// or that sorted a timestamp rendered differently per engine, could not
	// produce the same sequence on both.
	status, plain := e.doJSON(t, http.MethodPost, "/articles", map[string]any{
		"title": "z-first-created", "body": "body one",
	})
	tr.add("articles POST no-embedding status=%d title=%s", status, str(plain, "title"))
	plainID := str(plain, "id")

	time.Sleep(5 * time.Millisecond)
	written := float32ExactEmbedding(1)
	status, vector := e.doJSON(t, http.MethodPost, "/articles", map[string]any{
		"title": "m-second-created", "body": "body two",
		"embedding": written,
		"metadata":  map[string]any{"source": "battery", "n": 2},
	})
	tr.add("articles POST with-embedding status=%d title=%s", status, str(vector, "title"))
	vectorID := str(vector, "id")

	time.Sleep(5 * time.Millisecond)
	status, third := e.doJSON(t, http.MethodPost, "/articles", map[string]any{
		"title": "a-third-created", "body": "body three",
	})
	tr.add("articles POST third status=%d title=%s", status, str(third, "title"))
	thirdID := str(third, "id")

	// Validation failures must be 400 with the SAME message on both engines,
	// and must never reach SQL.
	status, bad := e.doJSON(t, http.MethodPost, "/articles", map[string]any{
		"title": "bad", "body": "bad", "embedding": []float64{1, 2, 3},
	})
	tr.add("articles POST wrong-dimension status=%d error=%q", status, errorMessage(bad))
	status, bad = e.doJSON(t, http.MethodPost, "/articles", map[string]any{
		"title": "bad", "body": "bad", "embedding": []any{1, "two", 3},
	})
	tr.add("articles POST non-numeric-component status=%d error=%q", status, errorMessage(bad))
	status, bad = e.doJSON(t, http.MethodPost, "/articles", map[string]any{"title": "", "body": ""})
	tr.add("articles POST missing-fields status=%d error=%q", status, errorMessage(bad))

	// GET one, with the embedding compared COMPONENT BY COMPONENT.
	status, got := e.doJSON(t, http.MethodGet, "/articles/"+vectorID, nil)
	tr.add("articles GET with-embedding status=%d title=%s published=%s",
		status, str(got, "title"), str(got, "published_at"))
	tr.add("articles GET embedding %s", componentReport(written, embeddingOf(got)))

	status, got = e.doJSON(t, http.MethodGet, "/articles/"+plainID, nil)
	tr.add("articles GET no-embedding status=%d title=%s embedding-absent=%t published-absent=%t",
		status, str(got, "title"), got["embedding"] == nil, got["published_at"] == nil)

	// THE ORDER of the listing.
	status, list := e.doJSON(t, http.MethodGet, "/articles", nil)
	tr.add("articles LIST status=%d order=%v", status, titles(list, "articles"))
	tr.add("articles LIST embedding-presence=%v", presence(list, "articles", "embedding"))

	// Paging: the same LIMIT/OFFSET must select the same rows on both engines,
	// which is only guaranteed because the declared order is TOTAL.
	status, page := e.doJSON(t, http.MethodGet, "/articles?limit=1&offset=1", nil)
	tr.add("articles LIST page limit=1 offset=1 status=%d order=%v", status, titles(page, "articles"))
	status, page = e.doJSON(t, http.MethodGet, "/articles?limit=2&offset=0", nil)
	tr.add("articles LIST page limit=2 offset=0 status=%d order=%v", status, titles(page, "articles"))
	status, page = e.doJSON(t, http.MethodGet, "/articles?limit=0&offset=0", nil)
	tr.add("articles LIST page limit=0 status=%d order=%v", status, titles(page, "articles"))

	// UPDATE: title/body only, then set an embedding, then clear it.
	status, upd := e.doJSON(t, http.MethodPut, "/articles/"+plainID, map[string]any{
		"title": "z-first-created", "body": "body one edited",
	})
	tr.add("articles PUT title-body status=%d body=%s", status, str(upd, "body"))

	replacement := float32ExactEmbedding(2)
	status, upd = e.doJSON(t, http.MethodPut, "/articles/"+vectorID, map[string]any{
		"title": "m-second-created", "body": "body two edited", "embedding": replacement,
	})
	tr.add("articles PUT set-embedding status=%d", status)
	_, got = e.doJSON(t, http.MethodGet, "/articles/"+vectorID, nil)
	tr.add("articles PUT set-embedding read-back %s", componentReport(replacement, embeddingOf(got)))

	status, upd = e.doJSON(t, http.MethodPut, "/articles/"+vectorID, map[string]any{
		"title": "m-second-created", "body": "body two cleared", "embedding": nil,
	})
	tr.add("articles PUT clear-embedding status=%d", status)
	_, got = e.doJSON(t, http.MethodGet, "/articles/"+vectorID, nil)
	tr.add("articles PUT clear-embedding read-back embedding-absent=%t body=%s",
		got["embedding"] == nil, str(got, "body"))

	// An update that OMITS embedding must leave it untouched.
	_, _ = e.doJSON(t, http.MethodPut, "/articles/"+thirdID, map[string]any{
		"title": "a-third-created", "body": "b3", "embedding": float32ExactEmbedding(4),
	})
	_, _ = e.doJSON(t, http.MethodPut, "/articles/"+thirdID, map[string]any{
		"title": "a-third-created", "body": "b3 again",
	})
	_, got = e.doJSON(t, http.MethodGet, "/articles/"+thirdID, nil)
	tr.add("articles PUT omitted-embedding untouched %s", componentReport(float32ExactEmbedding(4), embeddingOf(got)))

	// PUBLISH: idempotent, and the published_at must survive a second call.
	status, pub := e.doJSON(t, http.MethodPost, "/articles/"+plainID+"/publish", nil)
	firstPublish := str(pub, "published_at")
	tr.add("articles PUBLISH first status=%d published-present=%t", status, firstPublish != "<absent>")
	status, pub = e.doJSON(t, http.MethodPost, "/articles/"+plainID+"/publish", nil)
	tr.add("articles PUBLISH repeat status=%d unchanged=%t", status, str(pub, "published_at") == firstPublish)
	_, got = e.doJSON(t, http.MethodGet, "/articles/"+plainID, nil)
	tr.add("articles GET after-publish published-present=%t", got["published_at"] != nil)

	// THE 404s — a missing AND a malformed id, on every mutating route.
	status, nf := e.doJSON(t, http.MethodGet, "/articles/"+missingID, nil)
	tr.add("articles GET missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodGet, "/articles/"+malformedID, nil)
	tr.add("articles GET malformed status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodPut, "/articles/"+missingID, map[string]any{"title": "x", "body": "y"})
	tr.add("articles PUT missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodPut, "/articles/"+malformedID, map[string]any{"title": "x", "body": "y"})
	tr.add("articles PUT malformed status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodPost, "/articles/"+missingID+"/publish", nil)
	tr.add("articles PUBLISH missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodDelete, "/articles/"+missingID, nil)
	tr.add("articles DELETE missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodDelete, "/articles/"+malformedID, nil)
	tr.add("articles DELETE malformed status=%d error=%q", status, errorMessage(nf))

	status, _ = e.do(t, http.MethodDelete, "/articles/"+thirdID, nil)
	tr.add("articles DELETE existing status=%d", status)
	status, _ = e.do(t, http.MethodDelete, "/articles/"+thirdID, nil)
	tr.add("articles DELETE twice status=%d", status)
	_, list = e.doJSON(t, http.MethodGet, "/articles", nil)
	tr.add("articles LIST after-delete order=%v", titles(list, "articles"))

	// ======================================================================
	// 3. products — the decimal surface and the UNIQUE(sku) classification
	// ======================================================================
	status, p1 := e.doJSON(t, http.MethodPost, "/products", map[string]any{
		"title": "z-widget", "body": "first", "price": "19.99", "sku": "SKU-ONE",
	})
	tr.add("products POST status=%d title=%s price=%s sku=%s",
		status, str(p1, "title"), str(p1, "price"), str(p1, "sku"))
	productID := str(p1, "id")

	time.Sleep(5 * time.Millisecond)
	status, p2 := e.doJSON(t, http.MethodPost, "/products", map[string]any{
		"title": "a-gadget", "body": "second", "price": "1234567890.1234567890", "sku": "SKU-TWO",
	})
	tr.add("products POST high-precision-decimal status=%d price=%s", status, str(p2, "price"))
	secondProductID := str(p2, "id")

	// THE DUPLICATE SKU. This is the red-team item: it must be 400, not 500, and
	// the classification must come from the driver's structured code — the text
	// SQLite emits and the text PostgreSQL emits are different strings.
	status, dup := e.doJSON(t, http.MethodPost, "/products", map[string]any{
		"title": "clash", "body": "clash", "price": "1.00", "sku": "SKU-ONE",
	})
	tr.add("products POST duplicate-sku status=%d error=%q", status, errorMessage(dup))

	status, badPrice := e.doJSON(t, http.MethodPost, "/products", map[string]any{
		"title": "bad", "body": "bad", "price": "not-a-number", "sku": "SKU-BAD",
	})
	tr.add("products POST non-numeric-price status=%d error=%q", status, errorMessage(badPrice))

	status, list = e.doJSON(t, http.MethodGet, "/products", nil)
	tr.add("products LIST status=%d order=%v", status, titles(list, "products"))
	tr.add("products LIST prices=%v", field(list, "products", "price"))
	status, page = e.doJSON(t, http.MethodGet, "/products?limit=1&offset=1", nil)
	tr.add("products LIST page limit=1 offset=1 status=%d order=%v", status, titles(page, "products"))

	status, got = e.doJSON(t, http.MethodGet, "/products/"+productID, nil)
	tr.add("products GET status=%d title=%s price=%s sku=%s",
		status, str(got, "title"), str(got, "price"), str(got, "sku"))

	status, upd = e.doJSON(t, http.MethodPut, "/products/"+productID, map[string]any{
		"title": "z-widget", "body": "edited", "price": "29.95", "sku": "SKU-ONE",
	})
	tr.add("products PUT status=%d price=%s", status, str(upd, "price"))

	// A duplicate sku on UPDATE must also be 400.
	status, dup = e.doJSON(t, http.MethodPut, "/products/"+productID, map[string]any{
		"title": "z-widget", "body": "edited", "price": "29.95", "sku": "SKU-TWO",
	})
	tr.add("products PUT duplicate-sku status=%d error=%q", status, errorMessage(dup))
	_, got = e.doJSON(t, http.MethodGet, "/products/"+productID, nil)
	tr.add("products PUT duplicate-sku unchanged sku=%s", str(got, "sku"))

	status, nf = e.doJSON(t, http.MethodGet, "/products/"+missingID, nil)
	tr.add("products GET missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodPut, "/products/"+missingID, map[string]any{
		"title": "x", "body": "y", "price": "1", "sku": "SKU-NOPE",
	})
	tr.add("products PUT missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodDelete, "/products/"+malformedID, nil)
	tr.add("products DELETE malformed status=%d error=%q", status, errorMessage(nf))
	status, _ = e.do(t, http.MethodDelete, "/products/"+secondProductID, nil)
	tr.add("products DELETE existing status=%d", status)
	_, list = e.doJSON(t, http.MethodGet, "/products", nil)
	tr.add("products LIST after-delete order=%v", titles(list, "products"))

	// ======================================================================
	// 4. terms — the hierarchy and the views
	// ======================================================================
	status, t1 := e.doJSON(t, http.MethodPost, "/terms", map[string]any{
		"taxonomy": "category", "name": "Zoology", "slug": "zoology",
	})
	tr.add("terms POST parent status=%d taxonomy=%s name=%s slug=%s parent=%v",
		status, str(t1, "taxonomy"), str(t1, "name"), str(t1, "slug"), t1["parent_id"])
	parentTerm := str(t1, "id")

	status, t2 := e.doJSON(t, http.MethodPost, "/terms", map[string]any{
		"taxonomy": "category", "name": "Amphibians", "slug": "amphibians", "parent_id": parentTerm,
	})
	tr.add("terms POST child status=%d name=%s parent-set=%t", status, str(t2, "name"), t2["parent_id"] != nil)
	childTerm := str(t2, "id")

	status, t3 := e.doJSON(t, http.MethodPost, "/terms", map[string]any{
		"taxonomy": "tag", "name": "Amphibians", "slug": "amphibians",
	})
	tr.add("terms POST same-slug-other-taxonomy status=%d taxonomy=%s", status, str(t3, "taxonomy"))
	tagTerm := str(t3, "id")

	// THE DUPLICATE SLUG within a taxonomy — 400, classified by driver code.
	status, dup = e.doJSON(t, http.MethodPost, "/terms", map[string]any{
		"taxonomy": "category", "name": "Other", "slug": "amphibians",
	})
	tr.add("terms POST duplicate-slug status=%d error=%q", status, errorMessage(dup))

	status, bad = e.doJSON(t, http.MethodPost, "/terms", map[string]any{
		"taxonomy": "nonexistent", "name": "X", "slug": "x",
	})
	tr.add("terms POST unknown-taxonomy status=%d error=%q", status, errorMessage(bad))
	status, bad = e.doJSON(t, http.MethodPost, "/terms", map[string]any{
		"taxonomy": "category", "name": "X", "slug": "x2", "parent_id": missingID,
	})
	tr.add("terms POST unknown-parent status=%d error=%q", status, errorMessage(bad))
	status, bad = e.doJSON(t, http.MethodPut, "/terms/"+childTerm, map[string]any{
		"taxonomy": "category", "name": "Amphibians", "slug": "amphibians", "parent_id": childTerm,
	})
	tr.add("terms PUT self-parent status=%d error=%q", status, errorMessage(bad))

	// THE ORDER of the term listing, over the term_records view.
	status, list = e.doJSON(t, http.MethodGet, "/terms", nil)
	tr.add("terms LIST status=%d taxonomies=%v", status, field(list, "terms", "taxonomy"))
	tr.add("terms LIST names=%v", field(list, "terms", "name"))
	tr.add("terms LIST slugs=%v", field(list, "terms", "slug"))
	tr.add("terms LIST parents-null=%v", nullness(list, "terms", "parent_id"))

	status, got = e.doJSON(t, http.MethodGet, "/terms/"+childTerm, nil)
	tr.add("terms GET child status=%d taxonomy=%s name=%s parent-matches=%t",
		status, str(got, "taxonomy"), str(got, "name"), str(got, "parent_id") == parentTerm)
	status, nf = e.doJSON(t, http.MethodGet, "/terms/"+missingID, nil)
	tr.add("terms GET missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodGet, "/terms/"+malformedID, nil)
	tr.add("terms GET malformed status=%d error=%q", status, errorMessage(nf))

	status, upd = e.doJSON(t, http.MethodPut, "/terms/"+tagTerm, map[string]any{
		"taxonomy": "tag", "name": "Frogs", "slug": "frogs",
	})
	tr.add("terms PUT status=%d name=%s slug=%s", status, str(upd, "name"), str(upd, "slug"))
	status, nf = e.doJSON(t, http.MethodPut, "/terms/"+missingID, map[string]any{
		"taxonomy": "tag", "name": "X", "slug": "xx",
	})
	tr.add("terms PUT missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodDelete, "/terms/"+missingID, nil)
	tr.add("terms DELETE missing status=%d error=%q", status, errorMessage(nf))

	// ASSIGNMENT — the two junction views, with ORDER, and the duplicate-id case
	// that used to rely on ON CONFLICT DO NOTHING.
	status, assigned := e.doJSON(t, http.MethodPut, "/articles/"+plainID+"/terms", map[string]any{
		"term_ids": []string{tagTerm, childTerm, parentTerm},
	})
	tr.add("terms ASSIGN article status=%d order=%v", status, field(assigned, "terms", "name"))
	tr.add("terms ASSIGN article taxonomies=%v", field(assigned, "terms", "taxonomy"))

	status, assigned = e.doJSON(t, http.MethodPut, "/articles/"+plainID+"/terms", map[string]any{
		"term_ids": []string{childTerm, childTerm, childTerm},
	})
	tr.add("terms ASSIGN article duplicates status=%d order=%v len=%d",
		status, field(assigned, "terms", "name"), len(assigned["terms"].([]any)))

	status, assigned = e.doJSON(t, http.MethodPut, "/articles/"+plainID+"/terms", map[string]any{
		"term_ids": []string{},
	})
	tr.add("terms ASSIGN article empty status=%d len=%d", status, len(assigned["terms"].([]any)))

	status, bad = e.doJSON(t, http.MethodPut, "/articles/"+plainID+"/terms", map[string]any{
		"term_ids": []string{parentTerm, missingID},
	})
	tr.add("terms ASSIGN article unknown-term status=%d error=%q", status, errorMessage(bad))
	status, nf = e.doJSON(t, http.MethodPut, "/articles/"+missingID+"/terms", map[string]any{
		"term_ids": []string{parentTerm},
	})
	tr.add("terms ASSIGN article missing-content status=%d error=%q", status, errorMessage(nf))

	_, _ = e.doJSON(t, http.MethodPut, "/articles/"+plainID+"/terms", map[string]any{
		"term_ids": []string{parentTerm, childTerm},
	})
	_, got = e.doJSON(t, http.MethodGet, "/articles/"+plainID, nil)
	tr.add("terms embedded-in-article order=%v", nestedField(got, "terms", "name"))

	status, assigned = e.doJSON(t, http.MethodPut, "/products/"+productID+"/terms", map[string]any{
		"term_ids": []string{parentTerm, tagTerm},
	})
	tr.add("terms ASSIGN product status=%d order=%v", status, field(assigned, "terms", "name"))
	_, got = e.doJSON(t, http.MethodGet, "/products/"+productID, nil)
	tr.add("terms embedded-in-product order=%v", nestedField(got, "terms", "name"))

	// Deleting a PARENT term must orphan its child (ON DELETE SET NULL), not
	// delete it, and must cascade the junction rows away.
	status, _ = e.do(t, http.MethodDelete, "/terms/"+parentTerm, nil)
	tr.add("terms DELETE parent status=%d", status)
	_, got = e.doJSON(t, http.MethodGet, "/terms/"+childTerm, nil)
	tr.add("terms child after parent-delete parent-null=%t name=%s", got["parent_id"] == nil, str(got, "name"))
	_, got = e.doJSON(t, http.MethodGet, "/articles/"+plainID, nil)
	tr.add("terms article after parent-delete order=%v", nestedField(got, "terms", "name"))

	// ======================================================================
	// 5. content.go — the GENERIC CRUD over the DYNAMIC type created above
	// ======================================================================
	base := "/content/" + dynamicTypeName
	status, c1 := e.doJSON(t, http.MethodPost, base, map[string]any{
		"headline": "z-first", "score": 9, "price_paid": "12.50", "verified": true, "read_on": "2024-03-01",
	})
	tr.add("content POST status=%d headline=%v score=%v price=%v verified=%v read_on=%v",
		status, c1["headline"], c1["score"], c1["price_paid"], c1["verified"], c1["read_on"])
	tr.add("content POST types score=%T verified=%T price=%T", c1["score"], c1["verified"], c1["price_paid"])
	firstContent := str(c1, "id")

	time.Sleep(5 * time.Millisecond)
	status, c2 := e.doJSON(t, http.MethodPost, base, map[string]any{
		"headline": "a-second", "score": -3, "price_paid": 0.05, "verified": false, "read_on": "1999-12-31",
	})
	tr.add("content POST negatives status=%d score=%v price=%v verified=%v",
		status, c2["score"], c2["price_paid"], c2["verified"])
	secondContent := str(c2, "id")

	time.Sleep(5 * time.Millisecond)
	// Every field omitted → every field NULL (the documented CONTRACT-14 rule).
	status, c3 := e.doJSON(t, http.MethodPost, base, map[string]any{})
	tr.add("content POST all-omitted status=%d nulls=%v", status, allNull(c3, dynamicTypeDef))
	thirdContent := str(c3, "id")

	status, bad = e.doJSON(t, http.MethodPost, base, map[string]any{"headline": 42})
	tr.add("content POST wrong-type status=%d error=%q", status, errorMessage(bad))
	status, bad = e.doJSON(t, http.MethodPost, base, map[string]any{"score": 1.5})
	tr.add("content POST fractional-integer status=%d error=%q", status, errorMessage(bad))
	status, bad = e.doJSON(t, http.MethodPost, base, map[string]any{"read_on": "31/12/1999"})
	tr.add("content POST bad-date status=%d error=%q", status, errorMessage(bad))
	status, bad = e.doJSON(t, http.MethodPost, base, map[string]any{"nosuchfield": 1})
	tr.add("content POST unknown-field status=%d error=%q", status, errorMessage(bad))
	status, bad = e.doJSON(t, http.MethodPost, base, map[string]any{"id": "x"})
	tr.add("content POST reserved-column status=%d error=%q", status, errorMessage(bad))

	// A value that LOOKS like SQL must be stored and returned verbatim, as data.
	status, inj := e.doJSON(t, http.MethodPost, base, map[string]any{
		"headline": `";DROP TABLE users;--`, "score": 0,
	})
	tr.add("content POST sql-looking-value status=%d verbatim=%t", status, inj["headline"] == `";DROP TABLE users;--`)
	injectionContent := str(inj, "id")

	status, list = e.doJSON(t, http.MethodGet, base, nil)
	tr.add("content LIST status=%d type=%s headlines=%v", status, str(list, "type"), field(list, "items", "headline"))
	tr.add("content LIST scores=%v", field(list, "items", "score"))
	tr.add("content LIST verified=%v", field(list, "items", "verified"))
	tr.add("content LIST prices=%v", field(list, "items", "price_paid"))
	tr.add("content LIST dates=%v", field(list, "items", "read_on"))
	tr.add("content LIST metadata-null=%v", nullness(list, "items", "metadata"))

	status, page = e.doJSON(t, http.MethodGet, base+"?limit=2&offset=1", nil)
	tr.add("content LIST page limit=2 offset=1 status=%d headlines=%v", status, field(page, "items", "headline"))

	status, got = e.doJSON(t, http.MethodGet, base+"/"+firstContent, nil)
	tr.add("content GET status=%d headline=%v score=%v verified=%v price=%v date=%v",
		status, got["headline"], got["score"], got["verified"], got["price_paid"], got["read_on"])

	// PUT is a FULL replacement of the type's own fields: an omitted field is
	// reset to NULL. That rule must hold identically on both engines.
	status, upd = e.doJSON(t, http.MethodPut, base+"/"+firstContent, map[string]any{
		"headline": "z-first-edited", "score": 10,
	})
	tr.add("content PUT partial-body status=%d headline=%v score=%v price-null=%t verified-null=%t date-null=%t",
		status, upd["headline"], upd["score"], upd["price_paid"] == nil, upd["verified"] == nil, upd["read_on"] == nil)

	status, upd = e.doJSON(t, http.MethodPut, base+"/"+secondContent, map[string]any{
		"headline": "a-second", "score": 0, "price_paid": "0.000000000001", "verified": true, "read_on": "2030-01-01",
	})
	tr.add("content PUT full status=%d score=%v price=%v verified=%v date=%v",
		status, upd["score"], upd["price_paid"], upd["verified"], upd["read_on"])

	status, nf = e.doJSON(t, http.MethodGet, base+"/"+missingID, nil)
	tr.add("content GET missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodGet, base+"/"+malformedID, nil)
	tr.add("content GET malformed status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodPut, base+"/"+missingID, map[string]any{"headline": "x"})
	tr.add("content PUT missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodDelete, base+"/"+missingID, nil)
	tr.add("content DELETE missing status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodDelete, base+"/"+malformedID, nil)
	tr.add("content DELETE malformed status=%d error=%q", status, errorMessage(nf))
	status, nf = e.doJSON(t, http.MethodGet, "/content/nosuchtype", nil)
	tr.add("content GET unknown-type status=%d error=%q", status, errorMessage(nf))

	status, _ = e.do(t, http.MethodDelete, base+"/"+thirdContent, nil)
	tr.add("content DELETE existing status=%d", status)
	status, _ = e.do(t, http.MethodDelete, base+"/"+thirdContent, nil)
	tr.add("content DELETE twice status=%d", status)
	status, _ = e.do(t, http.MethodDelete, base+"/"+injectionContent, nil)
	tr.add("content DELETE injection-row status=%d", status)
	_, list = e.doJSON(t, http.MethodGet, base, nil)
	tr.add("content LIST after-delete headlines=%v", field(list, "items", "headline"))

	// The users table is intact — the SQL-looking value was data, not SQL.
	users, err := auth.ListUsers(ctx, st)
	tr.add("content injection users-table-intact=%t count=%d", err == nil, len(users))

	return tr.lines
}

// --- small helpers used by the scenario --------------------------------------

// roleIDOf resolves a role name to its id with a parameterized statement.
func roleIDOf(t *testing.T, st *compat.Store, name string) string {
	t.Helper()
	statement := `SELECT "id" FROM "roles" WHERE "name" = ` + compat.Placeholder(st.Target.Engine, 1)
	var id string
	if err := st.DB.QueryRowContext(context.Background(), statement, name).Scan(&id); err != nil {
		t.Fatalf("lookup role %q: %v", name, err)
	}
	return id
}

// presence reports, per element of a listing IN ORDER, whether a field is present.
func presence(body map[string]any, key, name string) []bool {
	items, _ := body[key].([]any)
	out := make([]bool, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		_, ok := row[name]
		out = append(out, ok)
	}
	return out
}

// nullness reports, per element of a listing IN ORDER, whether a field is null.
func nullness(body map[string]any, key, name string) []bool {
	items, _ := body[key].([]any)
	out := make([]bool, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		out = append(out, row[name] == nil)
	}
	return out
}

// nestedField extracts a field of every element of an EMBEDDED array (the terms
// nested inside a single article/product response), IN ORDER.
func nestedField(body map[string]any, key, name string) []string {
	items, _ := body[key].([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		out = append(out, fmt.Sprint(row[name]))
	}
	return out
}

// allNull reports which declared fields of a dynamic row are null, in a stable
// (sorted) order so the line is comparable.
func allNull(row map[string]any, def schema.ContentTypeDefinition) []string {
	out := make([]string, 0, len(def.Fields))
	for _, f := range def.Fields {
		if row[f.Name] == nil {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// --- the vector precision boundary, measured on purpose ----------------------

// TestDualEngineVectorPrecision measures the ONE difference in the vector
// surface that consumer code cannot remove, and pins it so a future change
// cannot make it worse silently.
//
// articles.embedding is declared vector(1536). compat maps that to a TEXT
// carrier on SQLite and to a NATIVE pgvector column on PostgreSQL, and pgvector
// stores float4 — SINGLE precision. So a component that needs more than 24 bits
// of mantissa round-trips EXACTLY on SQLite and is rounded on PostgreSQL. That
// is a property of the storage the canonical schema chose in CONTRACT-05, not of
// anything CONTRACT-20 does: binding, statement text, canonicalization and read
// path are identical on both engines, which is exactly what TestDualEngineServer
// proves with float32-exact components.
//
// This test asserts the boundary rather than hiding it: float32-exact components
// must round-trip identically on BOTH engines, and a high-precision component
// must survive on SQLite. What PostgreSQL returns for the high-precision case is
// LOGGED, not asserted, because it is pgvector's rounding and not a contract of
// this repository.
func TestDualEngineVectorPrecision(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set", pgDSNEnv)
	}
	sqliteStore, closeSQLite := openSQLiteEngine(t)
	defer closeSQLite()
	pgStore, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	// A component that is exactly representable in binary32, and one that is not.
	const exactComponent = 0.5
	const preciseComponent = 0.123456789012345

	probe := func(engineName string, st *compat.Store) (readExact, readPrecise float64) {
		e := newHarness(t, st)
		written := make([]float64, schema.EmbeddingDimension)
		written[0] = exactComponent
		written[1] = preciseComponent
		status, created := e.doJSON(t, http.MethodPost, "/articles", map[string]any{
			"title": "vector-precision", "body": "b", "embedding": written,
		})
		if status != http.StatusCreated {
			t.Fatalf("[%s] create: status %d", engineName, status)
		}
		_, got := e.doJSON(t, http.MethodGet, "/articles/"+str(created, "id"), nil)
		read := embeddingOf(got)
		if len(read) != schema.EmbeddingDimension {
			t.Fatalf("[%s] read back %d components, want %d", engineName, len(read), schema.EmbeddingDimension)
		}
		t.Logf("[%s] exact-representable  wrote=%v read=%v identical=%t",
			engineName, exactComponent, read[0], read[0] == exactComponent)
		t.Logf("[%s] high-precision       wrote=%v read=%v identical=%t",
			engineName, preciseComponent, read[1], read[1] == preciseComponent)
		return read[0], read[1]
	}

	sqliteExact, sqlitePrecise := probe("sqlite", sqliteStore)
	pgExact, pgPrecise := probe("postgres", pgStore)

	// The contract of this repository: a float32-exact component is byte-identical
	// on both engines, end to end through the HTTP surface.
	if sqliteExact != exactComponent || pgExact != exactComponent || sqliteExact != pgExact {
		t.Errorf("float32-exact component diverged: sqlite=%v postgres=%v want %v",
			sqliteExact, pgExact, exactComponent)
	}
	// SQLite's TEXT carrier keeps full float64 precision.
	if sqlitePrecise != preciseComponent {
		t.Errorf("sqlite lost precision on a high-precision component: got %v want %v",
			sqlitePrecise, preciseComponent)
	}
	// PostgreSQL's pgvector is float4. This is MEASURED and reported, not asserted
	// as an equality, and it is documented in the CONTRACT-20 report.
	if pgPrecise == preciseComponent {
		t.Logf("NOTE: PostgreSQL preserved the high-precision component exactly (%v). "+
			"pgvector's float4 storage is expected to round it; if this holds, the "+
			"documented precision boundary should be revisited.", pgPrecise)
	} else {
		t.Logf("MEASURED PRECISION BOUNDARY: pgvector stores float4, so %v is read back as %v "+
			"on PostgreSQL while SQLite's TEXT carrier returns %v. This is a property of the "+
			"vector(1536) column CONTRACT-05 declared, not of the CONTRACT-20 read/write path.",
			preciseComponent, pgPrecise, sqlitePrecise)
	}
}
