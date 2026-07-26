package server

// CONTRACT-25 — one place decides whether a failed data-path operation is
// "the database is not available" (503) or "this process is broken" (500).
//
// The measured defect (docs/PENDIENTES.md, hueco 8): CONTRACT-24 made the login
// and /ready answer 503 when the database is down, but every data route kept
// answering 500 for the very same cause. Both are 5xx, so nothing was visibly
// broken — but they mean different things to whoever receives them. 503 tells a
// balancer "take me out of rotation and retry"; 500 tells it "this is broken".
// The same cause has to say the same thing everywhere.
//
// THE POLARITY, AND WHY IT IS THE OPPOSITE OF CONTRACT-24's.
//
// CONTRACT-24 classifies by a WHITELIST OF SENTINELS: in writeIdentityError, an
// error that is not errIdentityRejected is treated as infrastructure. That works
// there because the identity layer has one clear sentinel for "the caller got it
// wrong", and everything else genuinely is the lookup failing to run.
//
// On the data paths that polarity would be DANGEROUS, so it is deliberately NOT
// copied. Their errors include genuine internal failures — a row that cannot be
// scanned, a value that will not serialize, a persisted definition that stopped
// validating — which ARE an honest 500. Treating those as infrastructure would
// dress a real bug up as "retry later", which is the same sin this project just
// finished correcting, only pointing the other way.
//
// So here the recognition is EXPLICIT and narrow: a connection failure is
// recognized as such and answers 503; EVERYTHING ELSE stays 500, exactly as it
// was before this contract.

import (
	"context"
	"net/http"
)

// internalErrorMessage is the generic message the HTML admin routes have always
// rendered for an internal failure. It is a constant here only so the single
// classification point can emit the unchanged text.
const internalErrorMessage = "internal error"

// failureIsInfrastructure is THE ONE PLACE that decides. Every 500 on a data
// path goes through it (via writeOperationFailure / httpOperationFailure) and no
// site carries a decision of its own — a classification spread over a hundred
// call sites diverges; one does not.
//
// HOW IT DECIDES, AND WHY NOT BY DRIVER ERROR TYPE.
//
// It asks the CONTRACT-24 availability probe — the same probe /ready answers
// with, so the two can never disagree — and reads "the operation failed AND the
// database is not reachable" as infrastructure. The alternative was to recognize
// concrete driver errors (pq/pgconn connection errors, sqlite3 codes, wrapped
// net.OpError, driver.ErrBadConn…). That was rejected:
//
//   - It would tie `librarian` to the error taxonomy of a specific engine, which
//     is precisely what `sqlite-postgres-compat` exists to prevent. The set would
//     have to be maintained per engine and per driver version, and a driver that
//     renamed or wrapped an error would silently regress the 503 back to a 500
//     with nothing failing loudly.
//   - The probe is a POSITIVE, engine-agnostic observation of the actual fact we
//     want to report ("can this process reach its database right now?"), not an
//     inference from the shape of a string.
//   - It costs nothing in the happy path: this runs only after an operation has
//     ALREADY failed, and the probe memoizes for readinessTTL, so a full outage
//     costs at most one ping per second no matter the request rate.
//
// THE RACE, DOCUMENTED AND DELIBERATELY NOT ELIMINATED. The probe's verdict is
// memoized for one second, so it can be up to one second stale in both
// directions:
//
//   - Stale "reachable": the database died between the operation's failure and
//     this check, and the cached verdict still says it was up ⇒ 500 instead of
//     503. This is the CONSERVATIVE direction — the request is reported as a
//     defect of this process rather than as a transient outage — and it is
//     accepted, not worked around.
//   - Stale "unreachable": the database came back within the same second and the
//     operation failed for a genuine internal reason ⇒ 503 for what was really a
//     500. This window requires an outage that ended in the last second AND a
//     real bug firing inside it. Closing it would mean probing synchronously on
//     every failure, which reintroduces exactly the load path readinessTTL exists
//     to prevent.
//
// Making the window smaller is not free and not worth it: the consumer of these
// codes is a balancer or a retry policy operating on multi-second horizons.
func (h *handlers) failureIsInfrastructure(ctx context.Context, err error) bool {
	if err == nil {
		// A site that reports failure without an error (e.g. "written, but the
		// read-back found nothing") is not evidence about the database. 500.
		return false
	}
	if ctx.Err() != nil {
		// The REQUEST context is already done: the client hung up or its own
		// deadline fired, and the operation failed because WE cancelled it. That
		// is the caller going away, not the database going down — never 503 for
		// it, and do not spend a probe (nor a connection) on a response nobody is
		// going to read.
		return false
	}
	// context.WithoutCancel and not ctx itself: the probe's verdict is SHARED
	// (it is the same memo /ready reads). Handing it a context that a client can
	// cancel would let one disconnecting caller record a cancellation as the
	// database's verdict and poison /ready for everyone for a second. The probe
	// applies its own readinessTimeout deadline internally, so dropping the
	// caller's cancellation does not make it unbounded.
	return h.ready.check(context.WithoutCancel(ctx), h.db.PingContext) != nil
}

// writeOperationFailure is the JSON data routes' single exit for a failed
// operation: the unchanged {"error": message} envelope with 500, or the fixed
// infrastructure response with 503 when the database is not reachable.
//
// The per-site message ("could not list articles", …) is kept for the 500 case
// and DROPPED for the 503 case: an infrastructure answer carries no detail by
// construction (see infraUnavailableMessage), and which operation happened to be
// running when the database went away is not the caller's business.
func (h *handlers) writeOperationFailure(w http.ResponseWriter, r *http.Request, err error, message string) {
	if h.failureIsInfrastructure(r.Context(), err) {
		writeInfraUnavailable(w)
		return
	}
	writeError(w, http.StatusInternalServerError, message)
}

// httpOperationFailure is the same decision for the HTML admin routes, which
// answer plain text (http.Error) rather than the JSON envelope. Same single
// classifier, different renderer — the branch lives in failureIsInfrastructure,
// never here and never at a call site.
func (h *handlers) httpOperationFailure(w http.ResponseWriter, r *http.Request, err error) {
	if h.failureIsInfrastructure(r.Context(), err) {
		http.Error(w, infraUnavailableMessage, infraUnavailableStatus)
		return
	}
	http.Error(w, internalErrorMessage, http.StatusInternalServerError)
}
