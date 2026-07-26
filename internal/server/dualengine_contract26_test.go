//go:build dualengine

package server

// CONTRACT-26 T4 — THE FULL HTTP CYCLE OF A DELETION, on BOTH engines.
//
// The contract asks for exactly this and nothing less: over real HTTP, create a
// type, load rows, try to delete WITHOUT confirming (refused, with NO effects),
// confirm WRONG (refused), confirm RIGHT (deleted), and then verify AGAINST THE
// ENGINE'S OWN CATALOG that the table is gone and that the definition and its
// fields are gone with it — plus the reuse of the name.
//
// It reuses the CONTRACT-20 fixtures and HTTP harness unchanged (they drive the
// same real mux), and is excluded from the default suite by the same build tag:
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineDeleteContentType -count=1 -v ./internal/server
//
// Without COMPAT_POSTGRES_DSN it SKIPS rather than passing vacuously. Both
// engines run the identical scenario and the transcripts must match line for
// line; driver error TEXT is never recorded, only statuses and the state of the
// database afterwards.

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

func TestDualEngineDeleteContentType(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteStore, closeSQLite := openSQLiteEngine(t)
	defer closeSQLite()
	pgStore, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	sqliteTranscript := runDeleteHTTPScenario(t, "sqlite", sqliteStore)
	pgTranscript := runDeleteHTTPScenario(t, "postgres", pgStore)

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

func runDeleteHTTPScenario(t *testing.T, engineLabel string, st *compat.Store) []string {
	t.Helper()
	e := newHarness(t, st)
	tr := &transcript{}

	catalogHas := func(name string) bool {
		present, err := st.TableExists(context.Background(), name)
		if err != nil {
			t.Fatalf("[%s] TableExists(%q): %v", engineLabel, name, err)
		}
		return present
	}
	listedRows := func() int {
		status, body := e.doJSON(t, http.MethodGet, "/content/eventos", nil)
		if status != http.StatusOK {
			return -1
		}
		items, _ := body["items"].([]any)
		return len(items)
	}

	// --- 1. Create the type and load real content ----------------------------
	status, _ := e.doJSON(t, http.MethodPost, "/content-types", map[string]any{
		"name": "eventos",
		"fields": []map[string]string{
			{"name": "titulo", "type": "text"},
			{"name": "lugar", "type": "text"},
			{"name": "asistentes", "type": "integer"},
		},
	})
	tr.add("POST /content-types                       -> %d catalog-has-table=%v", status, catalogHas("cpt_eventos"))
	for i, titulo := range []string{"Feria", "Charla Go", "Taller"} {
		status, _ := e.doJSON(t, http.MethodPost, "/content/eventos", map[string]any{
			"titulo": titulo, "lugar": "Montevideo", "asistentes": 10 + i,
		})
		tr.add("POST /content/eventos #%d                  -> %d", i+1, status)
	}
	tr.add("GET  /content/eventos                     -> %d rows", listedRows())

	// --- 2. THE GUARD: no confirmation, then wrong ones ----------------------
	status, body := e.doJSON(t, http.MethodDelete, "/content-types/eventos", nil)
	tr.add("DELETE unconfirmed                        -> %d rows=%v confirm_name=%v confirm_rows=%v nothing_was_done=%v",
		status, body["rows"], body["confirm_name"], body["confirm_rows"], body["nothing_was_done"])
	for _, wrong := range []struct {
		label   string
		payload map[string]any
	}{
		{"a boolean confirm:true", map[string]any{"confirm": true}},
		{"the name only", map[string]any{"confirm_name": "eventos"}},
		{"the count only", map[string]any{"confirm_rows": 3}},
		{"another type's name", map[string]any{"confirm_name": "articles", "confirm_rows": 3}},
		{"a wrong count", map[string]any{"confirm_name": "eventos", "confirm_rows": 2}},
	} {
		status, body := e.doJSON(t, http.MethodDelete, "/content-types/eventos", wrong.payload)
		tr.add("DELETE %-34s -> %d rows=%v nothing_was_done=%v", wrong.label, status, body["rows"], body["nothing_was_done"])
	}
	tr.add("after 6 refusals: catalog-has-table=%v rows=%d", catalogHas("cpt_eventos"), listedRows())

	// --- 3. The exact confirmation -------------------------------------------
	status, body = e.doJSON(t, http.MethodDelete, "/content-types/eventos",
		map[string]any{"confirm_name": "eventos", "confirm_rows": 3})
	tr.add("DELETE confirmed                          -> %d deleted=%v rows_deleted=%v table=%v table_was_missing=%v",
		status, body["deleted"], body["rows_deleted"], body["table"], body["table_was_missing"])

	// --- 4. THE CATALOG, THE REGISTRY AND THE ROUTES -------------------------
	tr.add("catalog: cpt_eventos=%v articles=%v users=%v",
		catalogHas("cpt_eventos"), catalogHas("articles"), catalogHas("users"))
	tr.add("registry: types=%d fields=%d", countRowsC26(t, st, "content_types"), countRowsC26(t, st, "content_type_fields"))
	status, _ = e.doJSON(t, http.MethodGet, "/content-types/eventos", nil)
	tr.add("GET  /content-types/eventos               -> %d", status)
	status, _ = e.doJSON(t, http.MethodGet, "/content/eventos", nil)
	tr.add("GET  /content/eventos                     -> %d", status)
	status, listing := e.doJSON(t, http.MethodGet, "/content-types", nil)
	types, _ := listing["content_types"].([]any)
	tr.add("GET  /content-types                       -> %d %d definitions", status, len(types))
	status, _ = e.doJSON(t, http.MethodDelete, "/content-types/eventos",
		map[string]any{"confirm_name": "eventos", "confirm_rows": 0})
	tr.add("DELETE again                              -> %d", status)

	// --- 5. THE NAME IS REUSABLE, and the new table is EMPTY -----------------
	status, _ = e.doJSON(t, http.MethodPost, "/content-types", map[string]any{
		"name": "eventos",
		"fields": []map[string]string{
			{"name": "encabezado", "type": "text"},
			{"name": "cupos", "type": "integer"},
		},
	})
	tr.add("POST /content-types (same name again)     -> %d catalog-has-table=%v", status, catalogHas("cpt_eventos"))
	tr.add("GET  /content/eventos (re-created)        -> %d rows", listedRows())
	status, _ = e.doJSON(t, http.MethodPost, "/content/eventos", map[string]any{"encabezado": "Nuevo", "cupos": 5})
	tr.add("POST /content/eventos (new shape)         -> %d rows=%d", status, listedRows())

	return tr.lines
}

// countRowsC26 counts a table through the store's own pool, on either engine.
func countRowsC26(t *testing.T, st *compat.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM "`+table+`"`).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
