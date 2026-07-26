package server

// CONTRACT-25 — internal tests for the two guards inside the single
// classification point that are NOT observable from the outside: that a failure
// with no error and a failure on an already-dead request context never reach the
// probe, and that a live cached verdict keeps a failed operation at 500.
//
// They matter because both guards protect something shared. The probe's verdict
// is the same memo /ready reads, so anything that lets a data-path failure spend
// or poison it is a defect with blast radius beyond the request that caused it.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// primedHandlers returns a handlers whose readiness memo already holds verdict
// and is fresh, so failureIsInfrastructure answers from the cache. db is nil on
// purpose: any code path that actually pings would panic, which is exactly the
// signal these tests want.
func primedHandlers(verdict error) *handlers {
	h := &handlers{}
	h.ready.err = verdict
	h.ready.checkedAt = time.Now()
	return h
}

// TestNoErrorIsNeverInfrastructure covers the sites that report failure without
// an error to show for it — "the write succeeded but the read-back found
// nothing", for instance. That is not evidence about the database, so it must
// stay a 500 and must not consume a probe.
func TestNoErrorIsNeverInfrastructure(t *testing.T) {
	// The memo says the database is DOWN. Even so, a nil error is not an outage.
	h := primedHandlers(errors.New("database is unreachable"))
	if h.failureIsInfrastructure(context.Background(), nil) {
		t.Fatal("a nil error was classified as infrastructure; it carries no such evidence")
	}
}

// TestACancelledRequestIsNotAnOutage is the red-team case the contract asks
// about: the client hung up (or its own deadline fired), so OUR context was
// cancelled and the operation failed because of that. The caller going away is
// not the database going down.
//
// The stronger half of the assertion is that no probe runs: h.db is nil here, so
// a version that probed anyway would panic rather than quietly answer 503. That
// is the property worth pinning — a disconnecting client must not be able to
// record its own cancellation as the shared verdict and poison /ready for
// everyone else for a second.
func TestACancelledRequestIsNotAnOutage(t *testing.T) {
	h := primedHandlers(nil) // memo says the database is fine…
	h.ready.checkedAt = time.Time{}
	h.ready.err = nil // …and is stale, so a probe WOULD run if the guard were missing.

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if h.failureIsInfrastructure(ctx, context.Canceled) {
		t.Fatal("a cancelled request was classified as a database outage")
	}
}

// TestAReachableDatabaseKeepsAFailureAtFiveHundred is the contract's central
// polarity, at the level of the decision itself: the operation failed, the
// database answers, therefore this is OUR bug and stays a 500. A whitelist
// polarity — "anything not recognized as the caller's fault is infrastructure",
// which is what CONTRACT-24 does in writeIdentityError — would answer the
// opposite here, and that is precisely why it is not copied.
func TestAReachableDatabaseKeepsAFailureAtFiveHundred(t *testing.T) {
	h := primedHandlers(nil) // fresh verdict: the database is reachable
	if h.failureIsInfrastructure(context.Background(), errors.New("scan permission: unexpected type")) {
		t.Fatal("a genuine internal failure was dressed up as an outage")
	}
}

// TestAnUnreachableDatabaseMakesAFailureInfrastructure is the other half, so a
// classifier that simply always answered false could not pass this file.
func TestAnUnreachableDatabaseMakesAFailureInfrastructure(t *testing.T) {
	h := primedHandlers(errors.New("dial tcp: connect: connection refused"))
	if !h.failureIsInfrastructure(context.Background(), errors.New("query articles: driver: bad connection")) {
		t.Fatal("a failure while the database is unreachable was not classified as infrastructure")
	}
}
