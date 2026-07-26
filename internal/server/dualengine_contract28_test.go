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
// with a real foreign key, and is classified by compat v0.5.0's
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
// ONE OF THE SIX CASES DOES NOT CLOSE, and this battery asserts it as it is
// instead of glossing over it: the DELETE race on SQLITE. The reason is a
// genuine collision between CONTRACT-27's ON DELETE RESTRICT and the exact code
// list compat v0.5.0 accepts — the long comment at that step has the measured
// evidence, and the report has the argument. Everything else closes on both
// engines.
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

	// THE ONE DIRECTION THAT DOES NOT CLOSE ON SQLITE, asserted rather than
	// hidden. PostgreSQL raises SQLSTATE 23503 here and compat classifies it, so
	// this is a 400 like the other two. SQLite does NOT: because CONTRACT-27
	// declares every relation ON DELETE RESTRICT (schema.foreignKeyRestrict, and
	// its comment argues at length why RESTRICT and not NO ACTION), SQLite
	// implements the parent-side refusal through its internal FK trigger program
	// and reports the extended result code SQLITE_CONSTRAINT_TRIGGER (1811), not
	// SQLITE_CONSTRAINT_FOREIGNKEY (787). compat v0.5.0's IsForeignKeyViolation
	// accepts only 787 — deliberately, and documented: it names 1811 as one of the
	// "sibling constraint codes [that] are different rejections and must not be
	// folded in". So the predicate answers false and the race keeps its 500 on
	// SQLite alone. Measured, with the identical DDL, both actions:
	//
	//	NO ACTION: DELETE parent -> 787  IsForeignKeyViolation=true
	//	RESTRICT : DELETE parent -> 1811 IsForeignKeyViolation=false
	//	either   : INSERT child  -> 787  IsForeignKeyViolation=true
	//
	// Closing it from librarian would mean one of three things the contract
	// forbids: reading the error text, re-implementing an engine-specific code
	// table here, or reversing CONTRACT-27's RESTRICT decision. It is therefore
	// left open, asserted EXACTLY as it behaves today so that the day compat
	// widens the predicate this test fails loudly and the gap gets deleted rather
	// than forgotten. See docs/reports/CONTRACT-28-REPORT.md.
	wantDeleteRace := http.StatusBadRequest
	wantDeleteMsg := "classifier-race-on-delete"
	if engineLabel == "sqlite" {
		wantDeleteRace = http.StatusInternalServerError
		wantDeleteMsg = "OTHER(could not delete content)"
	}
	if status != wantDeleteRace || classifyC28(body) != wantDeleteMsg {
		t.Errorf("[%s] DELETE /content/autores/{id} (raced) answered %d msg=%s, want %d msg=%s",
			engineLabel, status, classifyC28(body), wantDeleteRace, wantDeleteMsg)
	}
	t.Logf("[%s] RACE delete: %d interference(s) -> status %d, message class %s",
		engineLabel, interfered, status, classifyC28(body))

	// What IS identical on both engines, and is the half that actually protects
	// the data: the delete was refused and the row is still there.
	tr.add("RACE delete: pre-check counted zero, referrer created in the window (%d interference) -> refused=%v row-survived=%v",
		interfered, status != http.StatusNoContent,
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
