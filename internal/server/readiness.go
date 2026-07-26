package server

// CONTRACT-24 — separating "alive" from "ready to serve", and refusing to
// disguise an infrastructure outage as a credential failure.
//
// The measured defect (docs/PENDIENTES.md, hueco 7): with the database
// container STOPPED, `/health` answered `{"status":"ok"}` and both the login and
// the read routes answered 401. A monitor saw a healthy instance that could not
// serve anything, and whoever diagnosed the incident chased a credentials
// problem while the cause was that the database was gone.
//
// This file holds the NEW surface (the availability probe) and the SHARED
// vocabulary for "this is infrastructure, not the caller's fault" that the
// login handlers and the identity resolver use.

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// readyPath is the availability probe. THE FORM, AND WHY.
//
// `/health` is NOT changed and NOT extended: there is external monitoring
// pointed at it and its contract is "the process is alive" — byte-exact
// `{"status":"ok"}`, no dependencies, no database. Redefining it would silently
// change what every existing monitor is asserting. So availability is a SECOND,
// SEPARATE route, which is also the only shape that lets an operator tell the
// two failures apart at a glance: `/health` 200 + `/ready` 503 means "the
// process is up, its database is not", which is exactly the diagnosis that was
// missing.
//
// The name is `/ready` and not `/readyz`: this project already spells the
// liveness route `/health`, not `/healthz`, and the pair has to read as a pair.
const readyPath = "GET /ready"

// readinessTimeout is the probe's OWN deadline, independent of any client or
// server timeout. It exists for the "slow but alive" database: a probe that
// simply waited for the driver would hang for as long as the database takes to
// answer, and a health endpoint that hangs is worse than one that lies —
// a balancer's own timeout would fire with no answer at all, and the goroutine
// would still be parked. Two seconds is far above a healthy round trip on a
// local or same-datacenter database and far below any sensible balancer probe
// timeout, so a database too slow to answer within it is, for serving purposes,
// not available.
const readinessTimeout = 2 * time.Second

// readinessTTL memoizes the probe's verdict for one second.
//
// It is here because the contract fixes a requirement that a bare ping does not
// meet: the check must not BECOME a load path against the database. `/ready` is
// unauthenticated by necessity (a balancer cannot hold credentials), so anyone
// can call it as fast as they like, and each call would otherwise take a
// connection out of the pool — under a flood, the probe itself would starve the
// real traffic it is supposed to report on. Memoizing bounds the database cost
// to at most one ping per second no matter the request rate.
//
// One second of staleness is irrelevant to the consumer: balancers and monitors
// poll on multi-second intervals, so the answer they get is never more stale
// than their own polling period already makes it.
const readinessTTL = time.Second

// infraUnavailableStatus is the status code for "the database is not
// available", both on the probe and on the request paths that used to collapse
// to 401.
//
// 503 and not 500: the case is "I cannot serve RIGHT NOW", not "I am broken" or
// "you got it wrong". That distinction is not cosmetic — 503 is the code
// balancers and proxies treat as "take this instance out of rotation and retry",
// it is the code retry policies and monitoring rules key off, and a database
// that is down is by nature transient. 500 would report a permanent defect in
// this process, which is precisely the wrong diagnosis to hand an operator, and
// it is the one already used for real internal errors (a failed token signing, a
// corrupted row) — keeping them apart is the point.
const infraUnavailableStatus = http.StatusServiceUnavailable

// infraUnavailableMessage is the ONLY thing an infrastructure failure says.
//
// It carries no detail BY CONSTRUCTION: not the driver error, not the DSN, not
// the host, not whether the database exists or merely refused the connection.
// The status code is the signal; the message is a fixed string. An unauthorized
// caller can already learn "the database is not reachable" from the status code
// (that is the endpoint's entire purpose), but nothing beyond it.
const infraUnavailableMessage = "service unavailable"

// writeInfraUnavailable emits the infrastructure-failure response on a JSON
// route: the standard {"error": …} envelope, the fixed message, 503.
func writeInfraUnavailable(w http.ResponseWriter) {
	writeError(w, infraUnavailableStatus, infraUnavailableMessage)
}

// readiness memoizes the last probe verdict. Its clock is time.Now and NOT
// handlers.now: handlers.now is the injectable BUSINESS clock (tests freeze it
// to get deterministic JWT timestamps), and a frozen clock here would freeze the
// cache forever, so a stopped database would keep reporting ready. Cache
// expiry is wall-clock infrastructure bookkeeping, not a business timestamp.
type readiness struct {
	mu        sync.Mutex
	checkedAt time.Time
	err       error
}

// check reports whether the database is reachable, reusing a verdict younger
// than readinessTTL. The mutex is held across the probe on purpose: concurrent
// callers that arrive during a probe wait for THAT probe instead of each
// starting one, which is the same single-flight property the TTL provides for
// sequential callers.
func (rd *readiness) check(ctx context.Context, ping func(context.Context) error) error {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if !rd.checkedAt.IsZero() && time.Since(rd.checkedAt) < readinessTTL {
		return rd.err
	}
	probeCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	rd.err = ping(probeCtx)
	rd.checkedAt = time.Now()
	return rd.err
}

// handleReady answers the availability probe: 200 {"status":"ready"} when a
// connection to the database can be obtained and answers within
// readinessTimeout, 503 with the fixed generic message otherwise.
//
// The check is a POOL PING (database/sql's PingContext), deliberately the
// cheapest reachability test available: it borrows a connection and asks the
// driver whether it is alive. It runs no query of ours, touches no table, and
// reads no row, so it cannot be turned into a data-extraction or a
// table-scanning load path.
//
// It answers with a STATUS CODE, not a 200 carrying a field that says something
// is wrong. A balancer reads the status line; a 200 with `{"ready":false}` keeps
// a dead instance in rotation, which is the failure this endpoint exists to
// prevent.
//
// The probe context descends from the request, so a client that disconnects
// cancels it rather than leaving the ping running.
func (h *handlers) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := h.ready.check(r.Context(), h.db.PingContext); err != nil {
		// The error is deliberately DROPPED here rather than rendered: see
		// infraUnavailableMessage. It is not logged from the handler either,
		// since an unauthenticated endpoint anyone can poll must not be a way to
		// fill the service's logs.
		writeInfraUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
