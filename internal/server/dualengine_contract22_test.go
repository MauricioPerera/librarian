//go:build dualengine

package server

// CONTRACT-22 T3 — THE PROOF THAT CLOSES THE HOLE, on BOTH engines.
//
// The contract is explicit that code quality is not what closes it: what closes
// it is a clean database that, after the bootstrap and ONLY after it, accepts a
// PERMISSION-GATED WRITE over real HTTP — the write that a clean install refuses
// today with 403, which is how the production incident was discovered
// (docs/PENDIENTES.md hueco 1: `POST /content-types` answered `403 forbidden`
// with `role_permissions` completely empty).
//
// So the scenario below is, per engine:
//
//	clean install → the gated write is IMPOSSIBLE (no identity at all)
//	              → bootstrap
//	              → log in over HTTP, and perform TWO gated writes that would
//	                have failed before: POST /articles (content.create) and
//	                POST /content-types (content_types.manage, the exact route
//	                that returned 403 in production)
//	              → a SECOND bootstrap is refused and changes nothing
//	              → a user with NO role still gets 403, so the 201s above are a
//	                real authorization decision and not an ungated route
//
// Both engines run the identical scenario and the two transcripts must match
// line for line. Run it with:
//
//	COMPAT_POSTGRES_DSN='postgres://user:***@host:port/db?sslmode=disable' \
//	  go test -tags dualengine -run TestDualEngineBootstrap -count=1 -v ./internal/server
//
// Without COMPAT_POSTGRES_DSN it SKIPS rather than passing vacuously. The engine
// fixtures (openSQLiteEngine / openPostgresEngine) and the HTTP harness are the
// CONTRACT-20 ones, reused unchanged: this battery drives the same real mux.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

func TestDualEngineBootstrap(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set — this battery is meaningless without a real PostgreSQL", pgDSNEnv)
	}

	sqliteStore, closeSQLite := openSQLiteEngine(t)
	defer closeSQLite()
	pgStore, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	sqliteTranscript := runBootstrapScenario(t, "sqlite", sqliteStore)
	pgTranscript := runBootstrapScenario(t, "postgres", pgStore)

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

// TestPostgresConcurrentBootstrap is the once-only guarantee where it is
// actually decided by the KEY and not by a file lock.
//
// On SQLite the concurrency test (internal/auth) is won by SQLITE_BUSY: the
// writer lock serializes the racers before the primary key is ever consulted,
// so it proves the OUTCOME but not the MECHANISM. PostgreSQL has no such lock —
// N connections of one pool genuinely execute the transaction at the same time —
// so here the marker's primary key is the only thing standing between the
// racers, which is precisely the property the contract demands be the database's.
func TestPostgresConcurrentBootstrap(t *testing.T) {
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set", pgDSNEnv)
	}
	st, closePG := openPostgresEngine(t, dsn)
	defer closePG()

	const racers = 6
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		losses  []error
		start   = make(chan struct{})
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			result, err := auth.Bootstrap(context.Background(), st, fmt.Sprintf("racer%d@example.com", i), "clave-de-carrera")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				losses = append(losses, err)
				return
			}
			winners = append(winners, result.Email)
		}(i)
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("SECURITY: %d of %d concurrent bootstraps succeeded (%v); exactly one must", len(winners), racers, winners)
	}
	ctx := context.Background()
	if got := countC22(t, st, "users"); got != 1 {
		t.Fatalf("users = %d after the race, want 1", got)
	}
	if got := countC22(t, st, schema.BootstrapTable); got != 1 {
		t.Fatalf("marker rows = %d after the race, want 1", got)
	}
	perms, _, err := auth.RolePermissions(ctx, st, auth.BootstrapRole)
	if err != nil {
		t.Fatalf("RolePermissions: %v", err)
	}
	if len(perms) != len(schema.Permissions) {
		t.Fatalf("the winner left %d grants, want %d — a racer's rollback took the winner's work with it", len(perms), len(schema.Permissions))
	}
	// The losers must not have left a half-created identity behind either.
	if got := countC22(t, st, "user_roles"); got != 1 {
		t.Fatalf("user_roles = %d, want 1", got)
	}
	for _, err := range losses {
		t.Logf("loser: %v", err)
	}
	t.Logf("EXACTLY ONE WON on PostgreSQL 17: %v; users=1 marker=1 grants=%d", winners, len(perms))
}

// bootstrapHarness builds the real mux over a store WITHOUT creating any
// identity — unlike CONTRACT-20's newHarness, which grants and creates one,
// because here the absence of both is the premise.
func bootstrapHarness(t *testing.T, st *compat.Store) *engineHarness {
	t.Helper()
	h := &handlers{db: st.DB, store: st, jwtSecret: dualJWTSecret, now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	h.registerRoutes(mux)
	return &engineHarness{mux: mux, store: st}
}

// loginC22 performs POST /auth/login over the real route and returns the status
// and the token (empty on failure).
func loginC22(t *testing.T, harness *engineHarness, email, password string) (int, string) {
	t.Helper()
	previous := harness.admin
	harness.admin = "" // the login route takes no bearer token
	status, body := harness.do(t, http.MethodPost, "/auth/login", map[string]any{
		"email": email, "password": password,
	})
	harness.admin = previous
	if status != http.StatusOK {
		return status, ""
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &login); err != nil {
		t.Fatalf("login body %q: %v", body, err)
	}
	if login.Token == "" {
		t.Fatalf("login returned 200 with no token: %s", body)
	}
	return status, login.Token
}

func countC22(t *testing.T, st *compat.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM "`+table+`"`).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func runBootstrapScenario(t *testing.T, engineLabel string, st *compat.Store) []string {
	t.Helper()
	ctx := context.Background()
	tr := &transcript{}
	errWord := func(err error) string {
		if err == nil {
			return "none"
		}
		return "yes"
	}

	harness := bootstrapHarness(t, st)

	// --- 1. THE CLEAN INSTALL, measured over HTTP ----------------------------
	//
	// The catalogs are seeded and role_permissions is empty: the exact state the
	// contract measured. There is no identity, so the gated write is not merely
	// forbidden, it is unreachable.
	tr.add("clean install users=%d roles=%d permissions=%d role_permissions=%d",
		countC22(t, st, "users"), countC22(t, st, "roles"),
		countC22(t, st, "permissions"), countC22(t, st, "role_permissions"))
	status, _ := loginC22(t, harness, "admin@example.com", "correct-horse")
	tr.add("before bootstrap: login status=%d", status)
	status, body := harness.doJSON(t, http.MethodPost, "/articles", map[string]any{"title": "T", "body": "B"})
	tr.add("before bootstrap: POST /articles status=%d error=%s", status, errorMessage(body))
	marker, found, err := auth.BootstrapStatus(ctx, st)
	tr.add("before bootstrap: marker found=%v err=%s", found, errWord(err))

	// --- 2. THE BOOTSTRAP ----------------------------------------------------
	result, err := auth.Bootstrap(ctx, st, "admin@example.com", "correct-horse")
	if err != nil {
		t.Fatalf("[%s] Bootstrap: %v", engineLabel, err)
	}
	tr.add("bootstrap err=%s role=%s permissions=%v", errWord(err), result.Role, result.Permissions)
	tr.add("after bootstrap users=%d role_permissions=%d marker=%d",
		countC22(t, st, "users"), countC22(t, st, "role_permissions"), countC22(t, st, schema.BootstrapTable))
	marker, found, err = auth.BootstrapStatus(ctx, st)
	tr.add("after bootstrap: marker found=%v email=%s owns-created-user=%v err=%s",
		found, marker.AdminEmail, marker.AdminUserID == result.UserID, errWord(err))
	// The other roles were NOT granted anything — the contract forbids it.
	for _, role := range []string{"editor", "author", "contributor"} {
		perms, roleFound, err := auth.RolePermissions(ctx, st, role)
		tr.add("after bootstrap: role %s found=%v permissions=%v err=%s", role, roleFound, perms, errWord(err))
	}

	// --- 3. ENTER BY HTTP AND WRITE — the proof ------------------------------
	status, token := loginC22(t, harness, "admin@example.com", "correct-horse")
	tr.add("after bootstrap: login status=%d token=%v", status, token != "")
	harness.admin = token

	status, body = harness.doJSON(t, http.MethodPost, "/articles", map[string]any{"title": "T", "body": "B"})
	tr.add("GATED WRITE POST /articles (content.create) status=%d has-id=%v error=%s",
		status, str(body, "id") != "", errorMessage(body))
	tr.add("articles rows=%d", countC22(t, st, "articles"))

	// The route that answered 403 in PRODUCTION, with role_permissions empty.
	status, body = harness.doJSON(t, http.MethodPost, "/content-types", map[string]any{
		"name": "resenas22",
		"fields": []map[string]any{
			{"name": "titular", "type": "text"},
			{"name": "puntaje", "type": "integer"},
		},
	})
	tr.add("GATED WRITE POST /content-types (content_types.manage) status=%d error=%s", status, errorMessage(body))
	tr.add("content_types rows=%d", countC22(t, st, schema.ContentTypesTable))

	// And the write over the type just defined, through the generic CRUD layer.
	status, body = harness.doJSON(t, http.MethodPost, "/content/resenas22", map[string]any{
		"titular": "Primera", "puntaje": 9,
	})
	tr.add("GATED WRITE POST /content/resenas22 (content.create) status=%d has-id=%v error=%s",
		status, str(body, "id") != "", errorMessage(body))

	// --- 4. A SECOND BOOTSTRAP IS REFUSED AND CHANGES NOTHING ----------------
	usersBefore := countC22(t, st, "users")
	grantsBefore := countC22(t, st, "role_permissions")
	_, secondErr := auth.Bootstrap(ctx, st, "otro@example.com", "otra-clave")
	tr.add("second bootstrap err=%s sentinel=%v", errWord(secondErr), errors.Is(secondErr, auth.ErrAlreadyBootstrapped))
	tr.add("second bootstrap changed-nothing users=%v grants=%v marker=%d",
		countC22(t, st, "users") == usersBefore,
		countC22(t, st, "role_permissions") == grantsBefore,
		countC22(t, st, schema.BootstrapTable))
	status, _ = loginC22(t, harness, "otro@example.com", "otra-clave")
	tr.add("second bootstrap: refused identity login status=%d", status)

	// --- 5. THE CONTROL ------------------------------------------------------
	//
	// Without it the 201s above prove nothing: they could come from a route that
	// is not gated at all. A user with NO role must still be refused.
	if _, err := auth.CreateUser(ctx, st, "sinrol@example.com", "sin-rol-clave", nil); err != nil {
		t.Fatalf("[%s] create roleless user: %v", engineLabel, err)
	}
	_, roleless := loginC22(t, harness, "sinrol@example.com", "sin-rol-clave")
	harness.admin = roleless
	status, body = harness.doJSON(t, http.MethodPost, "/articles", map[string]any{"title": "X", "body": "Y"})
	tr.add("CONTROL roleless POST /articles status=%d error=%s", status, errorMessage(body))
	harness.admin = token

	// And the gate is still open for the administrator afterwards.
	status, _ = harness.doJSON(t, http.MethodPost, "/articles", map[string]any{"title": "T2", "body": "B2"})
	tr.add("administrator still writes status=%d articles=%d", status, countC22(t, st, "articles"))

	return tr.lines
}
