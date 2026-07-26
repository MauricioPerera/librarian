//go:build dualengine

package server

// CONTRACT-27 T5 — THE FULL HTTP CYCLE OF A RELATION, on BOTH engines.
//
// The contract asks for exactly this: over real HTTP, create the target type,
// create the one that references it, load rows WITH and WITHOUT the reference,
// confirm AGAINST THE ENGINE'S OWN CATALOG that the foreign key is really there
// (not a loose column), prove that the constraint is ENFORCED with a 400 and not
// a 500, that deleting a referenced ROW is refused legibly, that the T3 guard
// refuses editing and deleting the referenced TYPE naming the referrer, and that
// deleting the referrer frees the target again.
//
// It reuses the CONTRACT-20 fixtures and HTTP harness unchanged, and is excluded
// from the default suite by the same build tag:
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineRelationsHTTP -count=1 -v ./internal/server
//
// Without COMPAT_POSTGRES_DSN it SKIPS rather than passing vacuously. Driver
// error TEXT is never recorded — only statuses, catalog facts and the state of
// the database afterwards.

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

func TestDualEngineRelationsHTTP(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteStore, closeSQLite := openSQLiteEngine(t)
	defer closeSQLite()
	pgStore, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	sqliteTranscript := runRelationHTTPScenario(t, "sqlite", sqliteStore)
	pgTranscript := runRelationHTTPScenario(t, "postgres", pgStore)

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

func runRelationHTTPScenario(t *testing.T, engineLabel string, st *compat.Store) []string {
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
	// catalogFK reads the foreign key of a column from the ENGINE'S catalog
	// through compat's inspection — the evidence that the relation is a real FK
	// and not merely a uuid column that happens to hold an id.
	catalogFK := func(table, column string) string {
		inspection, err := st.InspectSchema(context.Background())
		if err != nil {
			t.Fatalf("[%s] inspect: %v", engineLabel, err)
		}
		for _, tbl := range inspection.Schema.Tables {
			if tbl.Name != table {
				continue
			}
			for _, c := range tbl.Constraints {
				if c.Kind != compat.ForeignKey || c.References == nil || len(c.Columns) != 1 || c.Columns[0] != column {
					continue
				}
				action := string(c.References.OnDelete)
				if action == "" {
					action = "(none)"
				}
				return c.References.Table + "(" + strings.Join(c.References.Columns, ",") + ") on_delete=" + action
			}
			return "NO FOREIGN KEY"
		}
		return "NO SUCH TABLE"
	}

	autores := map[string]any{
		"name":   "autores",
		"fields": []map[string]string{{"name": "nombre", "type": "text"}},
	}
	libros := map[string]any{
		"name":       "libros",
		"fields":     []map[string]string{{"name": "titulo", "type": "text"}},
		"references": []map[string]string{{"name": "autor", "target": "autores"}},
	}

	// --- 1. THE ORDER THE CONTRACT IMPOSES -----------------------------------
	status, body := e.doJSON(t, http.MethodPost, "/content-types", libros)
	tr.add("POST /content-types libros (target missing) -> %d catalog-has-table=%v", status, catalogHas("cpt_libros"))
	status, _ = e.doJSON(t, http.MethodPost, "/content-types", autores)
	tr.add("POST /content-types autores                 -> %d catalog-has-table=%v", status, catalogHas("cpt_autores"))
	status, body = e.doJSON(t, http.MethodPost, "/content-types", libros)
	refs, _ := body["references"].([]any)
	tr.add("POST /content-types libros                  -> %d references=%d catalog-has-table=%v", status, len(refs), catalogHas("cpt_libros"))

	// --- 2. THE FOREIGN KEY IS REAL ------------------------------------------
	tr.add("catalog: cpt_libros.autor -> %s", catalogFK("cpt_libros", "autor"))

	// --- 3. ROWS WITH AND WITHOUT THE RELATION -------------------------------
	status, autor := e.doJSON(t, http.MethodPost, "/content/autores", map[string]any{"nombre": "Borges"})
	autorID, _ := autor["id"].(string)
	tr.add("POST /content/autores                       -> %d id-present=%v", status, autorID != "")
	status, con := e.doJSON(t, http.MethodPost, "/content/libros", map[string]any{"titulo": "Ficciones", "autor": autorID})
	tr.add("POST /content/libros WITH the relation      -> %d round-tripped=%v", status, con["autor"] == autorID)
	status, sin := e.doJSON(t, http.MethodPost, "/content/libros", map[string]any{"titulo": "Anonimo"})
	tr.add("POST /content/libros WITHOUT the relation   -> %d value-is-null=%v", status, sin["autor"] == nil)

	// --- 4. THE CONSTRAINT IS ENFORCED, AS A 400 NOT A 500 -------------------
	status, _ = e.doJSON(t, http.MethodPost, "/content/libros",
		map[string]any{"titulo": "Fantasma", "autor": "99999999-9999-4999-8999-999999999999"})
	tr.add("POST /content/libros -> id that does not exist -> %d", status)
	status, _ = e.doJSON(t, http.MethodPost, "/content/libros",
		map[string]any{"titulo": "Malformado", "autor": "not-a-uuid"})
	tr.add("POST /content/libros -> malformed id           -> %d", status)
	status, listing := e.doJSON(t, http.MethodGet, "/content/libros", nil)
	items, _ := listing["items"].([]any)
	tr.add("GET  /content/libros                        -> %d rows=%d", status, len(items))

	// --- 5. DELETING A REFERENCED ROW IS REFUSED -----------------------------
	status, _ = e.doJSON(t, http.MethodDelete, "/content/autores/"+autorID, nil)
	tr.add("DELETE /content/autores/{referenced}        -> %d", status)
	status, _ = e.doJSON(t, http.MethodGet, "/content/autores/"+autorID, nil)
	tr.add("GET  /content/autores/{referenced}          -> %d (it survived)", status)
	status, otro := e.doJSON(t, http.MethodPost, "/content/autores", map[string]any{"nombre": "Cortazar"})
	otroID, _ := otro["id"].(string)
	tr.add("POST /content/autores (second)              -> %d", status)
	status, _ = e.doJSON(t, http.MethodDelete, "/content/autores/"+otroID, nil)
	tr.add("DELETE /content/autores/{unreferenced}      -> %d", status)

	// --- 6. THE T3 GUARD ON BOTH OPERATIONS ----------------------------------
	_, def := e.doJSON(t, http.MethodGet, "/content-types/autores", nil)
	fields, _ := def["fields"].([]any)
	fieldID, _ := fields[0].(map[string]any)["id"].(string)
	edit := map[string]any{"fields": []map[string]any{{"id": fieldID, "name": "nombre_completo", "type": "text"}}}

	status, body = e.doJSON(t, http.MethodPut, "/content-types/autores", edit)
	tr.add("PUT    /content-types/autores (referenced)  -> %d referenced_by=%s nothing_was_done=%v",
		status, describeReferencedByC27(body), body["nothing_was_done"])
	status, body = e.doJSON(t, http.MethodDelete, "/content-types/autores",
		map[string]any{"confirm_name": "autores", "confirm_rows": 1})
	tr.add("DELETE /content-types/autores (referenced)  -> %d referenced_by=%s nothing_was_done=%v",
		status, describeReferencedByC27(body), body["nothing_was_done"])
	_, def = e.doJSON(t, http.MethodGet, "/content-types/autores", nil)
	fields, _ = def["fields"].([]any)
	tr.add("after both refusals: catalog-has-table=%v field-name=%v",
		catalogHas("cpt_autores"), fields[0].(map[string]any)["name"])

	// --- 7. FREE IT, AND BOTH OPERATIONS WORK AGAIN --------------------------
	status, _ = e.doJSON(t, http.MethodDelete, "/content-types/libros",
		map[string]any{"confirm_name": "libros", "confirm_rows": 2})
	tr.add("DELETE /content-types/libros (the referrer) -> %d catalog-has-table=%v", status, catalogHas("cpt_libros"))
	status, _ = e.doJSON(t, http.MethodPut, "/content-types/autores", edit)
	tr.add("PUT    /content-types/autores (freed)       -> %d", status)
	status, _ = e.doJSON(t, http.MethodDelete, "/content/autores/"+autorID, nil)
	tr.add("DELETE /content/autores/{was referenced}    -> %d", status)
	status, _ = e.doJSON(t, http.MethodDelete, "/content-types/autores",
		map[string]any{"confirm_name": "autores", "confirm_rows": 0})
	tr.add("DELETE /content-types/autores (freed)       -> %d catalog-has-table=%v", status, catalogHas("cpt_autores"))

	return tr.lines
}

// describeReferencedByC27 renders the actionable half of the T3 refusal as an
// engine-neutral string, so the transcript compares the FACT and never the
// sentence.
func describeReferencedByC27(body map[string]any) string {
	raw, ok := body["referenced_by"].([]any)
	if !ok {
		return "-"
	}
	parts := make([]string, 0, len(raw))
	for _, entry := range raw {
		m, _ := entry.(map[string]any)
		parts = append(parts, strings.TrimSpace(toStringC27(m["content_type"])+"."+toStringC27(m["reference"])))
	}
	return strings.Join(parts, "+")
}

func toStringC27(v any) string {
	s, _ := v.(string)
	return s
}
