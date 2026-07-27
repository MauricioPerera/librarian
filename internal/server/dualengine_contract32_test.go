//go:build dualengine

package server

// CONTRACT-32 — THE RELATION SEARCH, on BOTH engines, and the divergence it had
// to neutralise to exist at all.
//
// WHY THIS BATTERY IS THE POINT OF THE CONTRACT. T2 left the technical decision
// open between pushing the filter into the database (which scales, but goes
// through compat's `like` → LIKE on SQLite, ILIKE on PostgreSQL) and matching in
// Go (identical by construction, but reading the whole table). The database road
// was taken, so the burden of proof is here: the two engines must return THE SAME
// THING, accents included.
//
// It runs in two halves:
//
//  1. THE RAW MEASUREMENT. The production search routine is called with a NAKED
//     pattern — the text as typed, wrapped in %…% — and the rows it returns are
//     logged per engine. This is the divergence the contract suspected, measured
//     rather than argued. It is LOGGED, not asserted: the day compat changes that
//     mapping this battery should report the new truth, not fail.
//  2. THE PANEL. The real /admin/content/{type}/reference fragment is rendered for
//     a list of queries designed to hit every trap (case, accents, ñ, the LIKE
//     metacharacters, a backslash, a quote) and the OFFERED LABELS are compared
//     character by character between SQLite and a real PostgreSQL 17.
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineReferenceSearch -count=1 -v ./internal/server
//
// Without COMPAT_POSTGRES_DSN it SKIPS rather than passing vacuously. The ids are
// random per engine, so every id — and the eight-character prefix the label rule
// embeds — is replaced by a stable token before the comparison.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

var c32OptionRe = regexp.MustCompile(`<option value="([^"]*)"([^>]*)>(.*?)</option>`)

// c32Rows are the target rows every trap is built from. They are created in this
// order, so "the newest N" is the same notion on both engines.
var c32Rows = []string{
	"ÑANDÚ", "ñandú menor", "Ábaco", "abaco sin tilde",
	"BORGES", "borges", "des%cuento", "des_cuento", "descuento",
	`barra\invertida`, "apo'strofe", "中文",
}

// c32Queries are what a person types. Each one exists to catch a specific way the
// two engines could disagree.
var c32Queries = []string{
	"",               // the unfiltered page: today's behaviour
	"ñandú",          // lowercase accented + ñ vs an UPPERCASE row  ← the suspicion
	"ÑANDÚ",          // the same, the other way round
	"Ñandú",          // mixed case
	"ábaco",          // accented, one row with the accent and one without
	"abaco",          // unaccented must NOT find the accented one
	"borges",         // pure ASCII case folding, where the engines already agree
	"BORGES",         //
	"%",              // a LIKE metacharacter typed as text
	"_",              // the other one
	"des%cuento",     // the metacharacter inside a real word
	"des_cuento",     //
	"descuento",      //
	`barra\inv`,      // the backslash: PostgreSQL's default LIKE escape, SQLite has none
	`\`,              // the backslash alone
	"apo'st",         // a quote, which must never leave the bound parameter
	"中文",             // non-ASCII with no case at all
	"no-existe-nada", // no matches: the control must still be usable
}

func TestDualEngineReferenceSearch(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteStore, closeSQLite := openSQLiteEngine(t)
	defer closeSQLite()
	pgStore, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	sqliteRaw, sqliteTranscript := runReferenceSearchScenario(t, "sqlite", sqliteStore)
	pgRaw, pgTranscript := runReferenceSearchScenario(t, "postgres", pgStore)

	// --- half 1: the measurement, logged ------------------------------------
	t.Log("RAW `like` THROUGH THE PRODUCTION ROUTINE (pattern = %<texto>%, no repair):")
	t.Logf("%-14s | %-6s | %-34s | %s", "texto", "same?", "sqlite LIKE", "postgres ILIKE")
	divergences := 0
	for _, q := range c32Queries {
		mark := "SAME"
		if sqliteRaw[q] != pgRaw[q] {
			mark = "DIFF"
			divergences++
		}
		t.Logf("%-14q | %-6s | %-34s | %s", q, mark, sqliteRaw[q], pgRaw[q])
	}
	t.Logf("raw divergences: %d of %d queries — this is WHY referenceSearchPattern exists",
		divergences, len(c32Queries))

	// --- half 2: the panel, asserted ----------------------------------------
	t.Logf("panel transcript (%d lines):\n%s", len(sqliteTranscript), strings.Join(sqliteTranscript, "\n"))
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
		t.Logf("OK: %d search observations identical on SQLite and PostgreSQL 17", len(sqliteTranscript))
	}
}

// runReferenceSearchScenario builds the fixture on one engine and returns (a) the
// RAW routine answers per query and (b) the transcript of what the panel offers.
func runReferenceSearchScenario(t *testing.T, engineLabel string, st *compat.Store) (map[string]string, []string) {
	t.Helper()
	e := newHarness(t, st)
	tr := &transcript{}
	ctx := context.Background()

	status, _ := e.doJSON(t, http.MethodPost, "/content-types", map[string]any{
		"name":   "autores",
		"fields": []map[string]string{{"name": "nombre", "type": "text"}},
	})
	tr.add("POST /content-types autores -> %d", status)
	names := map[string]string{} // id → nombre, for masking
	for _, name := range c32Rows {
		status, row := e.doJSON(t, http.MethodPost, "/content/autores", map[string]any{"nombre": name})
		id, _ := row["id"].(string)
		if status != http.StatusCreated || id == "" {
			t.Fatalf("%s: create autor %q: status %d", engineLabel, name, status)
		}
		names[id] = name
	}
	tr.add("POST /content/autores x%d -> 201", len(c32Rows))

	status, _ = e.doJSON(t, http.MethodPost, "/content-types", map[string]any{
		"name":       "libros",
		"fields":     []map[string]string{{"name": "titulo", "type": "text"}},
		"references": []map[string]string{{"name": "autor", "target": "autores"}},
	})
	tr.add("POST /content-types libros -> %d", status)

	// --- the RAW measurement, through the production routine -----------------
	def, err := store.FetchContentType(ctx, st, "autores")
	if err != nil {
		t.Fatalf("%s: fetch autores: %v", engineLabel, err)
	}
	h := &handlers{db: st.DB, store: st, jwtSecret: dualJWTSecret, now: time.Now}
	raw := map[string]string{}
	for _, q := range c32Queries {
		rows, err := h.searchContentRows(ctx, def, "%"+q+"%", referenceSearchScanLimit, 0)
		if err != nil {
			raw[q] = "ERROR: " + err.Error()
			continue
		}
		found := []string{}
		for _, row := range rows {
			found = append(found, displayValue(row["nombre"]))
		}
		sort.Strings(found) // the ORDER of a raw probe is not what is being measured
		raw[q] = "[" + strings.Join(found, " ") + "]"
	}

	// --- the PANEL, through the real fragment handler -------------------------
	for _, q := range c32Queries {
		params := url.Values{"name": {"autor"}, referenceSearchParam("autor"): {q}}
		req := httptest.NewRequest(http.MethodGet, "/admin/content/libros/reference?"+params.Encode(), nil)
		req.SetPathValue("type", "libros")
		rec := httptest.NewRecorder()
		h.handleAdminContentReferenceOptions(rec, req)
		body := rec.Body.String()
		labels := []string{}
		for _, opt := range c32OptionRe.FindAllStringSubmatch(body, -1) {
			labels = append(labels, maskIDsC32(opt[3], names))
		}
		tr.add("q=%-14q -> %d  no-matches=%v truncated=%v  %s",
			q, rec.Code,
			strings.Contains(body, `id="reference-no-matches-autor"`),
			strings.Contains(body, `id="reference-truncated-autor"`),
			strings.Join(labels, " | "))
	}
	return raw, tr.lines
}

// maskIDsC32 replaces the eight-character id prefix referenceOptionLabel embeds
// with the row's own name, so two engines whose uuids necessarily differ can be
// compared character by character.
func maskIDsC32(label string, names map[string]string) string {
	for id, name := range names {
		label = strings.ReplaceAll(label, id, "<id:"+name+">")
		label = strings.ReplaceAll(label, id[:8], "<id8:"+name+">")
	}
	return label
}
