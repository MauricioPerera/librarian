//go:build dualengine

package server

// CONTRACT-28 — THE RACE WINDOW, PROVOKED FOR REAL, ON BOTH ENGINES.
//
// CONTRACT-27 closed the ordinary case with two pre-checks and documented what
// it could not close: between the check and the statement it guards, the state
// can change, and the foreign key's honest rejection then arrived as a 500. This
// battery is the evidence that it no longer does.
//
// WHAT MAKES IT A REAL TEST AND NOT A SIMULATION. Nothing here fabricates an
// error. Every rejection asserted below is produced by the ENGINE — SQLite's
// SQLITE_CONSTRAINT_FOREIGNKEY, PostgreSQL's SQLSTATE 23503 — on a real table
// with a real foreign key, and is classified by compat v0.5.1's
// Store.IsForeignKeyViolation. There is no fake store, no injected error, no
// stubbed driver. The only thing the test controls is WHEN the interfering write
// happens: handlers.referenceRaceHook holds the window open for exactly one
// operation, so the interference lands after the pre-check has already passed
// and before the guarded statement runs. That hook is a synchronization point,
// not a double — it cannot change any outcome, it only decides the interleaving,
// and the interleaving is the entire subject of the contract. Without it the gap
// is a few microseconds wide and cannot be aimed at from outside the process.
//
// The three interleavings, in both directions:
//
//	CREATE — the referenced row is deleted after checkReferenceTargets passed.
//	UPDATE — the same, for a PUT that moves a reference onto that row.
//	DELETE — a referring row is created after checkNoIncomingReferences counted
//	         zero.
//
// Each must answer the status the pre-check would have answered (400), never a
// 500, and the database must be left exactly as the foreign key says it should
// be. The last section re-runs the ORDINARY cases with the window closed, to
// prove the good messages of the pre-checks are still the ones a normal client
// sees — the classifier is the net, not the replacement.
//
// ALL SIX CASES CLOSE, on both engines, and the transcript below is compared
// line by line with no exception carved out for any of them. The sixth — the
// DELETE race on SQLite — was open when this battery was written: CONTRACT-27
// declares every relation ON DELETE RESTRICT, so SQLite refuses the parent-side
// delete through its internal foreign-key trigger program and reports
// SQLITE_CONSTRAINT_TRIGGER (1811) rather than SQLITE_CONSTRAINT_FOREIGNKEY
// (787), which compat v0.5.0's IsForeignKeyViolation did not accept. compat
// v0.5.1 widened the predicate to cover 1811, the gap closed upstream, and the
// engine-specific assertion that pinned it here has been deleted. See the
// closing section of docs/reports/CONTRACT-28-REPORT.md.
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineForeignKeyRace -count=1 -v ./internal/server
//
// Without COMPAT_POSTGRES_DSN it SKIPS rather than passing vacuously. Driver
// error TEXT is never recorded: the transcript carries statuses, a classification
// of WHICH of this package's own messages was produced, and the state of the
// database afterwards.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

func TestDualEngineForeignKeyRaceHTTP(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteStore, closeSQLite := openSQLiteEngine(t)
	defer closeSQLite()
	pgStore, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	sqliteTranscript := runForeignKeyRaceScenario(t, "sqlite", sqliteStore)
	pgTranscript := runForeignKeyRaceScenario(t, "postgres", pgStore)

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

func runForeignKeyRaceScenario(t *testing.T, engineLabel string, st *compat.Store) []string {
	t.Helper()
	e, h := newRaceHarnessC28(t, st)
	tr := &transcript{}

	// armC28 opens the window for exactly ONE occurrence of one operation. The
	// op filter matters: the interference itself is a real HTTP request through
	// the same mux, so without it the hook would re-enter on the nested write.
	armC28 := func(op string, action func()) {
		fired := false
		h.referenceRaceHook = func(_ context.Context, gotOp string) {
			if fired || gotOp != op {
				return
			}
			fired = true
			action()
		}
	}
	disarmC28 := func() { h.referenceRaceHook = nil }

	countLibros := func() int {
		status, listing := e.doJSON(t, http.MethodGet, "/content/libros", nil)
		if status != http.StatusOK {
			t.Fatalf("[%s] list libros: status %d", engineLabel, status)
		}
		items, _ := listing["items"].([]any)
		return len(items)
	}
	// mustNot500 is the single assertion the whole contract reduces to.
	mustNot500 := func(label string, status int) {
		if status >= 500 {
			t.Errorf("[%s] %s answered %d — the race is still degrading to a server error", engineLabel, label, status)
		}
		if status != http.StatusBadRequest {
			t.Errorf("[%s] %s answered %d, want 400 (the status the pre-check would have produced)", engineLabel, label, status)
		}
	}

	// --- 0. THE TYPES, THE SAME PAIR CONTRACT-27 USES ------------------------
	autores := map[string]any{
		"name":   "autores",
		"fields": []map[string]string{{"name": "nombre", "type": "text"}},
	}
	libros := map[string]any{
		"name":       "libros",
		"fields":     []map[string]string{{"name": "titulo", "type": "text"}},
		"references": []map[string]string{{"name": "autor", "target": "autores"}},
	}
	status, _ := e.doJSON(t, http.MethodPost, "/content-types", autores)
	tr.add("POST /content-types autores -> %d", status)
	status, _ = e.doJSON(t, http.MethodPost, "/content-types", libros)
	tr.add("POST /content-types libros  -> %d", status)

	newAutor := func(name string) string {
		st, body := e.doJSON(t, http.MethodPost, "/content/autores", map[string]any{"nombre": name})
		if st != http.StatusCreated {
			t.Fatalf("[%s] create autor %q: status %d", engineLabel, name, st)
		}
		id, _ := body["id"].(string)
		return id
	}

	// --- 1. THE CREATE RACE ---------------------------------------------------
	// checkReferenceTargets sees the author. The window opens. The author is
	// deleted through the real DELETE route. Then the INSERT runs and the REAL
	// foreign key rejects it.
	vanishing := newAutor("Se-Borra-En-El-Medio")
	interfered := 0
	armC28("create", func() {
		st, _ := e.doJSON(t, http.MethodDelete, "/content/autores/"+vanishing, nil)
		if st != http.StatusNoContent {
			t.Fatalf("[%s] interfering delete: status %d, want 204", engineLabel, st)
		}
		interfered++
	})
	before := countLibros()
	status, body := e.doJSON(t, http.MethodPost, "/content/libros",
		map[string]any{"titulo": "Raced", "autor": vanishing})
	disarmC28()
	mustNot500("POST /content/libros (raced)", status)
	tr.add("RACE create: pre-check passed, target deleted in the window (%d interference) -> %d msg=%s",
		interfered, status, classifyC28(body))
	tr.add("RACE create: nothing was stored=%v, target really gone=%v",
		countLibros() == before, statusOfC28(t, e, http.MethodGet, "/content/autores/"+vanishing) == http.StatusNotFound)

	// --- 2. THE UPDATE RACE ---------------------------------------------------
	// Same window, the other write statement: a PUT that moves a reference onto a
	// row deleted after the check.
	vanishing2 := newAutor("Se-Borra-En-El-Update")
	status, movable := e.doJSON(t, http.MethodPost, "/content/libros", map[string]any{"titulo": "Movible"})
	if status != http.StatusCreated {
		t.Fatalf("[%s] create movable libro: status %d", engineLabel, status)
	}
	movableID, _ := movable["id"].(string)
	interfered = 0
	armC28("update", func() {
		st, _ := e.doJSON(t, http.MethodDelete, "/content/autores/"+vanishing2, nil)
		if st != http.StatusNoContent {
			t.Fatalf("[%s] interfering delete (update race): status %d, want 204", engineLabel, st)
		}
		interfered++
	})
	status, body = e.doJSON(t, http.MethodPut, "/content/libros/"+movableID,
		map[string]any{"titulo": "Movible", "autor": vanishing2})
	disarmC28()
	mustNot500("PUT /content/libros/{id} (raced)", status)
	tr.add("RACE update: pre-check passed, target deleted in the window (%d interference) -> %d msg=%s",
		interfered, status, classifyC28(body))
	_, movedRow := e.doJSON(t, http.MethodGet, "/content/libros/"+movableID, nil)
	tr.add("RACE update: reference stayed null=%v (the UPDATE did not land)", movedRow["autor"] == nil)

	// --- 3. THE DELETE RACE ---------------------------------------------------
	// The mirror direction: checkNoIncomingReferences counts zero referrers, the
	// window opens, a referring row is created, and the DELETE hits the real
	// foreign key.
	popular := newAutor("Se-Referencia-En-El-Medio")
	interfered = 0
	armC28("delete", func() {
		st, _ := e.doJSON(t, http.MethodPost, "/content/libros",
			map[string]any{"titulo": "Referencia-Repentina", "autor": popular})
		if st != http.StatusCreated {
			t.Fatalf("[%s] interfering create: status %d, want 201", engineLabel, st)
		}
		interfered++
	})
	status, body = e.doJSON(t, http.MethodDelete, "/content/autores/"+popular, nil)
	disarmC28()

	// The parent-side direction, now on the same footing as the other two.
	// PostgreSQL raises SQLSTATE 23503 and SQLite — refusing CONTRACT-27's ON
	// DELETE RESTRICT through its internal foreign-key trigger program — raises
	// SQLITE_CONSTRAINT_TRIGGER (1811); compat v0.5.1 classifies both, so the
	// answer is the pre-check's 400 on either engine and the transcript line is
	// compared rather than being asserted per engine.
	mustNot500("DELETE /content/autores/{id} (raced)", status)
	tr.add("RACE delete: pre-check counted zero, referrer created in the window (%d interference) -> %d msg=%s",
		interfered, status, classifyC28(body))

	// And the half that actually protects the data: the delete was refused and
	// the row is still there.
	tr.add("RACE delete: the delete was refused=%v, row survived=%v",
		status != http.StatusNoContent,
		statusOfC28(t, e, http.MethodGet, "/content/autores/"+popular) == http.StatusOK)

	// --- 4. THE NORMAL PATH IS UNCHANGED --------------------------------------
	// With the window closed, the SAME two refusals must come from the PRE-CHECKS,
	// with their specific, actionable messages — not from the classifier's
	// generic one.
	status, body = e.doJSON(t, http.MethodPost, "/content/libros",
		map[string]any{"titulo": "Fantasma", "autor": "99999999-9999-4999-8999-999999999999"})
	tr.add("NORMAL create with a missing reference -> %d msg=%s names-the-id=%v",
		status, classifyC28(body), strings.Contains(errorMessage(body), "99999999-9999-4999-8999-999999999999"))
	status, body = e.doJSON(t, http.MethodDelete, "/content/autores/"+popular, nil)
	tr.add("NORMAL delete of a referenced row    -> %d msg=%s names-the-referrer=%v",
		status, classifyC28(body), strings.Contains(errorMessage(body), `"libros"`))

	// --- 5. AND THE ORDINARY SUCCESSES STILL SUCCEED --------------------------
	free := newAutor("Sin-Referencias")
	status, _ = e.doJSON(t, http.MethodDelete, "/content/autores/"+free, nil)
	tr.add("NORMAL delete of an unreferenced row -> %d", status)
	status, ok := e.doJSON(t, http.MethodPost, "/content/libros",
		map[string]any{"titulo": "Normal", "autor": popular})
	tr.add("NORMAL create with a live reference  -> %d round-tripped=%v", status, ok["autor"] == popular)

	return tr.lines
}

// classifyC28 names WHICH of this package's own messages was produced, so the
// transcript compares the decision and never a driver sentence. The four
// outcomes are deliberately distinguishable: the contract's claim is precisely
// that the pre-check messages survive and the classifier only speaks in the
// race.
func classifyC28(body map[string]any) string {
	msg := errorMessage(body)
	switch {
	case strings.Contains(msg, "and no ") && strings.Contains(msg, " row has the id "):
		return "precheck-missing-target"
	case strings.Contains(msg, "row(s) reference it through"):
		return "precheck-incoming-references"
	case strings.Contains(msg, "deleted after the reference was checked"):
		return "classifier-race-on-write"
	case strings.Contains(msg, "started referencing it after the check"):
		return "classifier-race-on-delete"
	case msg == "<none>":
		return "no-error"
	default:
		return "OTHER(" + msg + ")"
	}
}

func statusOfC28(t *testing.T, e *engineHarness, method, path string) int {
	t.Helper()
	status, _ := e.do(t, method, path, nil)
	return status
}

// newRaceHarnessC28 is newHarness plus the one thing this battery needs and the
// existing one does not expose: the *handlers value, so the synchronization
// point can be installed on it. It is a separate constructor rather than a
// change to newHarness because CONTRACT-27's and CONTRACT-20's batteries must
// keep passing untouched.
func newRaceHarnessC28(t *testing.T, st *compat.Store) (*engineHarness, *handlers) {
	t.Helper()
	ctx := context.Background()

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
	return harness, h
}
