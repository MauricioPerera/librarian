//go:build dualengine

package server

// CONTRACT-31 — THE RELATION COLUMNS OF THE PANEL LISTING, on BOTH engines.
//
// WHY THIS BATTERY EXISTS AT ALL, given that this contract emits no SQL of its
// own. That is precisely what it has to prove. The obvious way to translate the
// ids of a listing is a raw `... WHERE id IN (…)` over the target table, and the
// contract explicitly blesses that road. It was NOT taken, and the reason is an
// engine divergence: a raw SELECT hands back DRIVER values, and the driver's
// answer for the same declared column genuinely differs between the two engines
// (a boolean is INTEGER 1 on SQLite and BOOLEAN true on PostgreSQL, a decimal is
// TEXT on one and NUMERIC on the other). A label built from those values would
// read differently on each engine — the exact class of divergence this project
// exists to remove. Reading the target page through the SAME routine every other
// read uses makes compat canonicalize the values first, so there is one
// representation to label.
//
// This battery is the evidence for that claim: it renders the panel listing of a
// type with one relation to a target of EVERY declared field family, and
// compares the rendered cells, character by character, between SQLite and a real
// PostgreSQL 17.
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineRelationColumns -count=1 -v ./internal/server
//
// Without COMPAT_POSTGRES_DSN it SKIPS rather than passing vacuously. The ids
// are random per engine, so every id is replaced by a stable token before the
// comparison: what is compared is the RENDERING RULE, never the uuid it embeds.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

var (
	c31RowRe  = regexp.MustCompile(`(?s)<tr id="content-([0-9a-f-]+)">(.*?)</tr>`)
	c31CellRe = regexp.MustCompile(`<td class="cell-value">(.*?)</td>`)
)

func TestDualEngineRelationColumns(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteStore, closeSQLite := openSQLiteEngine(t)
	defer closeSQLite()
	pgStore, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	sqliteTranscript := runRelationColumnsScenario(t, "sqlite", sqliteStore)
	pgTranscript := runRelationColumnsScenario(t, "postgres", pgStore)

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

func runRelationColumnsScenario(t *testing.T, engineLabel string, st *compat.Store) []string {
	t.Helper()
	e := newHarness(t, st)
	tr := &transcript{}

	// One target type per declared field family, because the label is built from
	// the target's FIRST field and that is where a driver's typing would show.
	targets := []struct {
		name  string
		field string
		kind  string
		value any
	}{
		{"t_texto", "nombre", "text", "Borges"},
		{"t_entero", "numero", "integer", 42},
		{"t_decimal", "precio", "decimal", "9.90"},
		{"t_bool", "activo", "boolean", true},
		{"t_fecha", "fecha", "date", "2026-07-26"},
		{"t_nulo", "nombre", "text", nil},
	}
	references := []map[string]string{}
	rowIDs := map[string]string{}
	for _, target := range targets {
		status, _ := e.doJSON(t, http.MethodPost, "/content-types", map[string]any{
			"name":   target.name,
			"fields": []map[string]string{{"name": target.field, "type": target.kind}},
		})
		tr.add("POST /content-types %-10s -> %d", target.name, status)
		body := map[string]any{}
		if target.value != nil {
			body[target.field] = target.value
		}
		status, row := e.doJSON(t, http.MethodPost, "/content/"+target.name, body)
		id, _ := row["id"].(string)
		tr.add("POST /content/%-10s      -> %d id-present=%v", target.name, status, id != "")
		rowIDs[target.name] = id
		references = append(references, map[string]string{"name": "r_" + target.name, "target": target.name})
	}
	// And one target with NO declared fields: the label rule's poorest input.
	status, _ := e.doJSON(t, http.MethodPost, "/content-types", map[string]any{"name": "t_sin_campos"})
	tr.add("POST /content-types t_sin_campos -> %d", status)
	status, row := e.doJSON(t, http.MethodPost, "/content/t_sin_campos", map[string]any{})
	sinCamposID, _ := row["id"].(string)
	tr.add("POST /content/t_sin_campos      -> %d", status)
	rowIDs["t_sin_campos"] = sinCamposID
	references = append(references, map[string]string{"name": "r_t_sin_campos", "target": "t_sin_campos"})

	status, _ = e.doJSON(t, http.MethodPost, "/content-types", map[string]any{
		"name":       "items",
		"fields":     []map[string]string{{"name": "titulo", "type": "text"}},
		"references": references,
	})
	tr.add("POST /content-types items (%d relaciones) -> %d", len(references), status)

	// One row with EVERY relation set, and one with every relation NULL.
	conRelaciones := map[string]any{"titulo": "con"}
	for name, id := range rowIDs {
		conRelaciones["r_"+name] = id
	}
	status, _ = e.doJSON(t, http.MethodPost, "/content/items", conRelaciones)
	tr.add("POST /content/items (con relaciones) -> %d", status)
	status, _ = e.doJSON(t, http.MethodPost, "/content/items", map[string]any{"titulo": "sin"})
	tr.add("POST /content/items (sin relaciones) -> %d", status)

	// THE PANEL LISTING. The handler is called directly, with the {type} segment
	// set exactly as the mux would set it: what is under test is the rendering,
	// not the session middleware that guards it (covered by the SQLite battery).
	h := &handlers{db: st.DB, store: st, jwtSecret: dualJWTSecret, now: time.Now}
	req := httptest.NewRequest(http.MethodGet, "/admin/content/items", nil)
	req.SetPathValue("type", "items")
	rec := httptest.NewRecorder()
	h.handleAdminContentList(rec, req)
	page := rec.Body.String()
	tr.add("GET /admin/content/items -> %d", rec.Code)

	for _, matched := range c31RowRe.FindAllStringSubmatch(page, -1) {
		cells := []string{}
		for _, cell := range c31CellRe.FindAllStringSubmatch(matched[2], -1) {
			cells = append(cells, maskIDsC31(cell[1], rowIDs))
		}
		tr.add("row: %s", strings.Join(cells, " | "))
	}
	return tr.lines
}

// maskIDsC31 replaces every target id — and the eight-character prefix the label
// rule embeds — with the NAME of the type it belongs to, so two engines whose
// uuids necessarily differ can still be compared character by character.
func maskIDsC31(cell string, rowIDs map[string]string) string {
	for name, id := range rowIDs {
		if id == "" {
			continue
		}
		cell = strings.ReplaceAll(cell, id, "<id:"+name+">")
		cell = strings.ReplaceAll(cell, id[:8], "<id8:"+name+">")
	}
	return cell
}
