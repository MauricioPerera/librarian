package server

// CONTRACT-24 — internal tests for the readiness probe's two non-obvious
// properties, both of which are requirements the contract FIXES and neither of
// which is observable from the outside: that a database which is slow but alive
// cannot hang the check, and that the check cannot be turned into a load path
// against the database.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestReadinessDoesNotHangOnASlowDatabase is the red-team case: the database is
// ALIVE but not answering. A probe without its own deadline would block for as
// long as the driver does, so the endpoint that exists to report availability
// would itself become unavailable. The ping here never returns on its own — it
// returns only when the context it was handed is cancelled — so the test passes
// only if handleReady's own deadline is what ends it.
func TestReadinessDoesNotHangOnASlowDatabase(t *testing.T) {
	var rd readiness
	blockUntilCancelled := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	start := time.Now()
	err := rd.check(context.Background(), blockUntilCancelled)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a database that never answers must not be reported ready")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded (the probe's OWN deadline, not the caller's)", err)
	}
	// Generously bounded: the point is that it ends near readinessTimeout rather
	// than waiting for the database, not that it ends at an exact instant.
	if elapsed > readinessTimeout*3 {
		t.Fatalf("probe took %v, want it bounded near %v", elapsed, readinessTimeout)
	}
}

// TestReadinessProbeIsMemoizedSoItCannotBecomeALoadPath covers the contract's
// requirement that the check must not become a way to load the database. The
// endpoint is unauthenticated by necessity (a balancer holds no credentials), so
// the only thing standing between a flood of requests and a flood of pings is
// the TTL. A burst of calls must produce ONE ping.
func TestReadinessProbeIsMemoizedSoItCannotBecomeALoadPath(t *testing.T) {
	var rd readiness
	pings := 0
	countingPing := func(context.Context) error {
		pings++
		return nil
	}

	for i := 0; i < 500; i++ {
		if err := rd.check(context.Background(), countingPing); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	if pings != 1 {
		t.Fatalf("500 probes hit the database %d times, want 1 (the TTL is what bounds it)", pings)
	}
}

// TestReadinessVerdictExpires guards the other half of the memoization: a cached
// verdict that never expired would report a stopped database as ready forever,
// which is the very defect this contract closes. Expiry is driven by the WALL
// CLOCK on purpose (never handlers.now, which tests freeze), so the cache is
// aged here by rewinding the recorded instant rather than by sleeping.
func TestReadinessVerdictExpires(t *testing.T) {
	var rd readiness
	pings := 0
	countingPing := func(context.Context) error {
		pings++
		return nil
	}

	if err := rd.check(context.Background(), countingPing); err != nil {
		t.Fatalf("first check: %v", err)
	}
	rd.checkedAt = rd.checkedAt.Add(-2 * readinessTTL)
	if err := rd.check(context.Background(), countingPing); err != nil {
		t.Fatalf("second check: %v", err)
	}
	if pings != 2 {
		t.Fatalf("pings = %d, want 2 (an expired verdict must be re-probed)", pings)
	}
}
