package server_test

// CONTRACT-25 — a database outage says the same thing on a data route as it
// does on the login and on /ready, and a GENUINE internal failure keeps saying
// 500.
//
// The gap this closes (docs/PENDIENTES.md, hueco 8): CONTRACT-24 moved the login
// and /ready to 503 when the database is unreachable, but every data route kept
// answering 500 for the same cause. Both are 5xx, so nothing looked broken — but
// 503 tells a balancer "take me out of rotation and retry" and 500 tells it
// "this instance is defective".
//
// The two halves are deliberately BOTH here, because the dangerous failure of
// this contract is not "the outage still says 500" — it is "everything now says
// 503", which would bury real bugs behind a reassuring "retry later". So the
// second test provokes an honest internal failure with the database VERIFIABLY
// UP (it asserts /ready is 200 in the same breath) and requires a 500.
//
// The outage is produced by CLOSING THE POOL — a real *sql.DB with no connection
// left to give, not a stub returning a canned error. The closing proof against a
// genuinely stopped PostgreSQL 17 container is in
// docs/reports/CONTRACT-25-REPORT.md; this file is the suite-resident guard.

import (
	"net/http"
	"testing"

	"github.com/MauricioPerera/librarian/internal/schema"
)

// TestDataRoutesReportAnOutageAsUnavailable is the contract's core assertion:
// with the database gone and a VALID JWT (so nothing here is an authentication
// problem), the data routes answer 503 with the same fixed body the login and
// /ready already answer with.
//
// The token is a JWT on purpose. A JWT is verified by signature alone and never
// touches the database, so the request reaches the HANDLER and fails on the data
// operation itself — which is exactly the path CONTRACT-24 did not cover and
// this one does. (A bearer token that is not a JWT would fail earlier, in the
// API-key lookup, and would only re-prove CONTRACT-24.)
func TestDataRoutesReportAnOutageAsUnavailable(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()

	grant(t, db, "administrator", "content.create", "content.update", "content.delete", "terms.manage")
	token := jwtFor(t, db, "c25@example.com", "pw", "administrator")

	// Take the database away for real.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// Both middleware shapes are covered: requireAuth (the read routes, where the
	// handler's own query is what fails) and requirePermission (where the
	// permission lookup fails before the handler is even reached).
	for _, route := range []struct {
		method, path, gate string
	}{
		{http.MethodGet, "/articles", "requireAuth"},
		{http.MethodGet, "/products", "requireAuth"},
		{http.MethodGet, "/terms", "requireAuth"},
		{http.MethodGet, "/content-types", "requireAuth"},
		{http.MethodPost, "/articles", "requirePermission"},
		{http.MethodPost, "/terms", "requirePermission"},
	} {
		var body map[string]any
		if route.method == http.MethodPost {
			body = map[string]any{"title": "x", "body": "y", "name": "x", "taxonomy": "category", "slug": "x"}
		}
		status, resp := doJSON(t, srv, route.method, route.path, body, authHeader(token))
		if status == http.StatusInternalServerError {
			t.Fatalf("%s %s (%s) with the database down answered 500 — the gap this contract closes",
				route.method, route.path, route.gate)
		}
		if status != http.StatusServiceUnavailable {
			t.Fatalf("%s %s (%s) with the database down: status = %d, want 503",
				route.method, route.path, route.gate, status)
		}
		// Same fixed, detail-free body as /ready and the login. An infrastructure
		// answer never names the operation that happened to be running.
		if resp["error"] != "service unavailable" || len(resp) != 1 {
			t.Fatalf("%s %s body = %v, want exactly {\"error\":\"service unavailable\"}",
				route.method, route.path, resp)
		}
	}

	// And the pair that makes the diagnosis readable at a glance is intact: the
	// process is alive, its database is not.
	if status, _ := doJSON(t, srv, http.MethodGet, "/health", nil, nil); status != http.StatusOK {
		t.Fatalf("GET /health with the database down: status = %d, want 200", status)
	}
	if status, _ := doJSON(t, srv, http.MethodGet, "/ready", nil, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("GET /ready with the database down: status = %d, want 503", status)
	}
}

// TestGenuineInternalFailureIsStillFiveHundred is the guard on the property the
// contract says must NOT be traded away. A row that cannot be interpreted is an
// honest 500, and turning it into a 503 would tell an operator "retry later"
// about a defect that no amount of retrying fixes.
//
// The failure provoked here is real: a content type whose DEFINITION is still
// registered while the table backing it is gone. Dropping cpt_reviews leaves the
// registry describing a table that does not exist, so every read of that type
// composes a perfectly valid statement over a missing relation and fails. That
// is the state a half-applied migration or a hand-run DROP leaves behind, it is
// engine-agnostic (the same edit produces the same failure on SQLite and on
// PostgreSQL), and it happens with the database fully up and serving — which the
// /ready assertion in the same test pins.
//
// Two cheaper corruptions were tried first and are recorded here because their
// failure is informative: (a) writing a bogus field_type is IMPOSSIBLE — the
// schema defends itself with a CHECK constraint on content_type_fields; (b)
// renaming a declared field does NOT fail, the read simply surfaces the unknown
// column as null. Neither is a usable provocation.
func TestGenuineInternalFailureIsStillFiveHundred(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()

	admin := contentAdmin(t, db)
	createDynamicType(t, srv, admin, reviewsBody())

	// Sanity: the route works before the corruption, so the 500 below is the
	// corruption's doing and not a broken fixture.
	if status, _ := doJSON(t, srv, http.MethodGet, "/content/reviews", nil, authHeader(admin)); status != http.StatusOK {
		t.Fatalf("GET /content/reviews before the corruption: status = %d, want 200", status)
	}

	// Break the store while leaving the CONNECTION untouched: the pool is
	// intact and the engine keeps answering, only this schema is now incoherent.
	if _, err := db.Exec(`DROP TABLE ` + schema.DynamicTableName("reviews")); err != nil {
		t.Fatalf("drop the backing table: %v", err)
	}

	// THE DATABASE IS UP. This is what makes the assertion below meaningful:
	// the failure that follows cannot be blamed on infrastructure.
	if status, body := doJSON(t, srv, http.MethodGet, "/ready", nil, nil); status != http.StatusOK {
		t.Fatalf("GET /ready with a healthy database: status = %d body = %v, want 200", status, body)
	}

	status, resp := doJSON(t, srv, http.MethodGet, "/content/reviews", nil, authHeader(admin))
	if status == http.StatusServiceUnavailable {
		t.Fatal("a corrupted persisted definition answered 503 — a real bug disguised as \"retry later\", which is what this contract forbids")
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("GET /content/reviews over a corrupted definition: status = %d, want 500", status)
	}
	// The message is the one that shipped before this contract, unchanged.
	if resp["error"] != "could not list content" {
		t.Fatalf("body = %v, want {\"error\":\"could not list content\"}", resp)
	}
}

// TestHappyPathIsUnchangedByTheClassifier pins the contract's "no behaviour
// change on the happy path" clause at the one place the classifier could have
// leaked into it: a route whose operation SUCCEEDS must never consult the probe
// and must return exactly what it always returned.
func TestHappyPathIsUnchangedByTheClassifier(t *testing.T) {
	db, srv, cleanup := openAuthMux(t)
	defer cleanup()

	token := jwtFor(t, db, "happy25@example.com", "pw", "editor")

	status, body := doJSON(t, srv, http.MethodGet, "/articles", nil, authHeader(token))
	if status != http.StatusOK {
		t.Fatalf("GET /articles: status = %d, want 200", status)
	}
	if _, ok := body["articles"]; !ok {
		t.Fatalf("GET /articles body = %v, want an articles array", body)
	}

	// A 404 stays a 404: the classifier only ever sees a failed operation, and a
	// missing row is not one.
	if status, _ := doJSON(t, srv, http.MethodGet, "/articles/00000000-0000-0000-0000-000000000000", nil, authHeader(token)); status != http.StatusNotFound {
		t.Fatalf("GET /articles/{missing}: status = %d, want 404", status)
	}
}
