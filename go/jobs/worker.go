package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// ErrHandlerNotRegistered is the failure a worker records when it claims a
// Job whose Type has no registered Handler. It is treated exactly like any
// other Handle failure — retried with backoff, then dead-lettered once
// retries are exhausted — since the situation can resolve itself (the
// correct Handler registers before retries run out) or genuinely needs
// operator attention, in which case the Job dead-letters and shows up in
// StandaloneQueue.DeadLetterJobs.
var ErrHandlerNotRegistered = apperr.Internal("jobs.handler_not_registered")

// errStandaloneJobMissingTenant is execute's defensive response to a jobRecord
// whose TenantID column is empty. This should never happen for a Job this
// package's own Enqueue created — Task.validate rejects an empty TenantID
// before Enqueue ever inserts a row — the only realistic cause is a row
// that reached the jobs table by some path other than Enqueue: a migration
// bug, a manual SQL fixup, or a writer that bypasses this package's own
// API. Checked before calling Handle, exactly mirroring
// errTaskMissingTenant's identical defense for asynq.Queue
// (queue/asynq/worker.go's processTaskUncancelled) — without this guard, Handle
// would run on a context reporting no usable tenant at all
// (pkgcore.TenantFromContext returning ok=false), and only a Handler
// implementation that routes every tenant-sensitive operation through
// dbkit.Repository[T] would fail closed on its own; anything else (an
// outbound webhook call keyed by job.TenantID as a plain string, a billing
// charge, a notification dispatch, even just a log line) would silently
// run under a blank tenant identity instead of being refused. Deliberately
// its own sentinel rather than a reuse of ErrHandlerNotRegistered, for the
// same reason errTaskMissingTenant's own doc comment gives: the two
// causes are operationally distinct and would send an operator chasing the
// wrong fix.
var errStandaloneJobMissingTenant = apperr.Internal("jobs.standalone_job_missing_tenant")

// errStandaloneHandlerPanicked is invokeHandle's response when a Handler's
// Handle panics instead of returning normally, so the failure still flows
// through execute's ordinary retry/dead-letter accounting instead of an
// unrecovered panic reaching runWorker's goroutine. See invokeHandle's own
// doc comment for why recovering it matters.
var errStandaloneHandlerPanicked = apperr.Internal("jobs.standalone_handler_panicked")

// claimBatchSize bounds how many candidate rows one dispatch tick reads
// before applying per-tenant concurrency gating in Go. It is a package
// constant rather than configurable: unlike worker count or the per-tenant
// concurrency limit, which are genuine deployment-dependent tuning knobs,
// this is purely an internal batch size with one reasonable value.
const claimBatchSize = 100

// terminalPublishBatchSize bounds how many owed rows one publish pass
// reads and publishes before yielding to the next tick. Same reasoning as
// claimBatchSize: an internal batch size with one reasonable value, not a
// deployment knob. A backlog larger than this drains over successive
// passes; the ordering key (pendingTerminalRecords) keeps the sequence
// correct across passes.
const terminalPublishBatchSize = 100

// jobContext rebuilds the context a Handler (and FailureHook) call
// receives: pkgcore.WithTenant from the Job's own stored tenant, over a
// freshly detached context.Background(). This is deliberately NOT derived
// from whatever context the Queue.Enqueue call ran in — that
// context no longer exists by the time a worker claims the row back out of
// SQLite — and NOT derived from StandaloneQueue's own internal dispatcher/worker
// -loop lifecycle context either, so that closing the queue does not
// abruptly cancel a Handle call already in flight (see StandaloneQueue.Close's
// own doc comment).
//
// This is the one function responsible for closing the tenant-context
// trap: see worker_test.go's TestJobContext_ProducesTenantScopedContext
// (the positive case: a context built by this function carries exactly the
// tenant given) and
// TestJobContext_ContrastWithoutRebuild_FailsClosedWithErrNoTenant (the
// failure mode a worker reproduces if it ever calls Handle with any OTHER
// context instead — including, but not limited to, forgetting to call this
// function at all).
func jobContext(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tenant)
}

// runDispatcher polls q.db for eligible Jobs every q.pollInterval and sends
// each successfully claimed one to dispatch, until q.stopCh is closed. It
// is the one goroutine that writes runningPerTenant increments;
// runWorker's decrements run concurrently with it by construction, so both
// sides go through q.tenantMu. The dispatcher deliberately does NOT refresh
// the queue's writer registration: it cannot always reach its tick branch
// (it blocks on the worker handoff while every worker is busy, and it exits
// on stopCh while Close waits for workers to finish their current Handle),
// so the registration heartbeat lives on its own goroutine instead --
// runWriterHeartbeat -- which keeps a concurrent Start on the same database
// from mistaking this queue for a crashed one while any worker may still be
// executing a claimed row.
func (q *StandaloneQueue) runDispatcher(dispatch chan<- jobRecord) {
	defer q.wg.Done()
	defer close(dispatch)

	ticker := time.NewTicker(q.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-q.stopCh:
			return
		case <-ticker.C:
			q.dispatchOnce(dispatch)
		}
	}
}

// runWriterHeartbeat refreshes this queue's queue_writers registration row
// (store.go) on its own ticker, once per poll interval, until Close stops
// it. It exists because the registration's freshness is the fence that
// keeps a concurrent StandaloneQueue.Start on the same database from
// stealing this queue's rows while a worker still holds one -- see
// ErrQueueWriterActive and acquireWriterRegistration -- and the fence must
// hold in exactly the two periods the dispatcher cannot beat: while it is
// blocked handing a claimed Job to a worker that is busy (its tick branch
// unreachable), and after it exits on stopCh while Close waits for workers
// to finish their in-flight Handles (a graceful shutdown that outlives the
// stale window must not open the gate mid-drain). Close therefore stops
// this goroutine only after every worker has finished, immediately before
// the registration itself is released. A heartbeat that fails to reach its
// own row means the registration was stolen (its beats lapsed past the
// stale window and a sibling took over) or removed -- the queue keeps
// running rather than stopping mid-flight, which would strand every row it
// has claimed, and the error is logged every tick until the operator
// resolves the two-writer situation the steal implies.
func (q *StandaloneQueue) runWriterHeartbeat() {
	defer q.heartbeatWG.Done()
	ticker := time.NewTicker(q.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-q.heartbeatStopCh:
			return
		case <-ticker.C:
			q.heartbeat()
		}
	}
}

// heartbeat refreshes this queue's queue_writers registration row (store.go)
// so a concurrent StandaloneQueue.Start on the same database can tell a
// live writer from a crashed one -- see ErrQueueWriterActive and
// acquireWriterRegistration. It runs on the dedicated keeper goroutine
// runWriterHeartbeat (never on the dispatcher's own tick: the dispatcher
// cannot reach its tick while blocked on the worker handoff or after Close
// has shut it down, exactly the periods a held row must stay fenced), from
// Start until Close's drain of in-flight Handles has finished. A heartbeat
// that fails to reach its own row means the registration was stolen (its
// beats lapsed past the stale window and a sibling took over) or removed --
// the queue keeps running rather than stopping mid-flight, which would
// strand every row it has claimed, and the error is logged every tick until
// the operator resolves the two-writer situation the steal implies.
func (q *StandaloneQueue) heartbeat() {
	ctx := context.Background()
	ok, err := heartbeatWriterRegistration(ctx, q.db, q.owner, time.Now(), q.writerStaleAfter)
	if err != nil {
		obs.FromContext(ctx).Error("jobs: writer heartbeat failed", "error", err)
		return
	}
	if !ok {
		obs.FromContext(ctx).Error("jobs: writer registration lost; another StandaloneQueue may have started against this database", "owner", q.owner)
	}
}

// runTerminalPublisher is the terminal-signal publish pass's loop: it
// publishes owed jobs.job.terminal events (a terminal row with
// terminal_published_at still NULL, the row-level outbox definition in
// store.go) every poll interval until Close, starting with an immediate
// first pass. Start launches it only when WithEventBus configured a bus and
// strictly after the writer registration was acquired, so the pass runs
// exclusively under the same single-writer gate the dispatcher claims rows
// under and a multi-replica race on the outbox is closed by construction.
//
// The immediate first pass is the crash catch-up: rows an earlier process
// left owed -- it died between a terminal write and the stamp, or ran
// without a bus -- are republished here, the same way
// resetInterruptedRecords recovers interrupted rows for execution. There is
// deliberately no flush at Close: a pass in flight when stopCh closes
// finishes its current iteration (an unbounded bus call cannot be
// interrupted -- the same residual Close documents for an in-flight Handle)
// and the loop exits between rows; anything still owed is republished by
// the next Start's first pass, not by a second shutdown path.
//
// A publish failure never touches the worker's terminal path -- the pass
// shares nothing with execute/settleFailedAttempt but the database, which
// it only reads plus the stamp write -- it is logged and the row stays
// owed for the next pass. A stamp failure after a successful publish leaves
// the row owed too, so the next pass republishes: duplicate delivery is the
// only safe direction for a lost stamp, and the consumer contract's
// idempotency clause (EventJobTerminal) is written for it.
func (q *StandaloneQueue) runTerminalPublisher() {
	defer q.wg.Done()
	ticker := time.NewTicker(q.pollInterval)
	defer ticker.Stop()
	for {
		q.terminalPublishOnce()
		select {
		case <-q.stopCh:
			return
		case <-ticker.C:
		}
	}
}

// terminalPublishOnce is one publish pass: read up to
// terminalPublishBatchSize owed rows, oldest terminal transition first, and
// publish each — stamping the row only after Publish returned nil
// (publish-then-stamp). The pass runs on a bare context.Background(): it
// belongs to the queue's own lifecycle, has no ambient tenant to inherit or
// per-call caller to attribute, and deliberately attaches no tenant either
// — the Job's tenant travels in the event's Event.TenantID field, and a
// subscriber must rebuild its own context from it (the tenant trap's
// publishing-side half, spelled out in EventJobTerminal's doc comment)
// rather than rely on a context an in-process bus would happen to deliver
// and a broker-backed bus would not.
func (q *StandaloneQueue) terminalPublishOnce() {
	ctx := context.Background()
	recs, err := pendingTerminalRecords(ctx, q.db, terminalPublishBatchSize)
	if err != nil {
		obs.FromContext(ctx).Error("jobs: querying pending terminal events failed", "error", err)
		return
	}
	log := obs.FromContext(ctx)
	for _, rec := range recs {
		select {
		case <-q.stopCh:
			// Shutting down between rows: the remaining owed rows are
			// republished by the next Start's first pass; no flush here.
			return
		default:
		}
		if perr := q.bus.Publish(ctx, terminalEventFor(rec)); perr != nil {
			log.Warn("jobs: publishing terminal event failed",
				"job_id", rec.ID, "job_type", rec.Type, "status", rec.Status, "error", perr)
			continue
		}
		if serr := stampTerminalPublished(ctx, q.db, rec.ID, time.Now()); serr != nil {
			// The event went out; the mark did not land. The row stays
			// owed and the next pass republishes it -- a duplicate, which
			// the consumer contract covers -- never a silent loss.
			log.Warn("jobs: stamping published terminal event failed; the event will be republished",
				"job_id", rec.ID, "job_type", rec.Type, "error", serr)
		}
	}
}

// dispatchOnce runs one poll/claim/dispatch cycle: it reads a batch of
// eligible candidates — interleaved round-robin across distinct tenants,
// with Priority ordering the rows within any one tenant's own share (see
// claimCandidates' own doc comment) — and, for each, either claims it and
// hands it to a worker or skips it because its tenant is currently at
// q.tenantConcurrency — moving on to the next candidate (which may belong
// to a different tenant) rather than stalling on the first one. Together,
// these two mechanisms are what make one tenant's backlog unable to starve
// another's: claimCandidates' own interleaving keeps a different tenant's
// eligible row from being excluded from the batch in the first place no
// matter how deep the flooding tenant's own backlog runs, and this
// skip-and-continue loop keeps a tenant already at its concurrency limit
// from blocking a batch-mate that belongs to someone else. Neither one
// alone is sufficient — see candidate_window_fairness_test.go's
// TestDispatchOnce_CandidateWindowDoesNotStarveOtherTenants, which pins
// exactly the starvation gap claimCandidates' own interleaving closes, and
// TestPerTenantConcurrencyLimiting, which pins the complementary
// concurrency-admission property this file's skip-and-continue loop
// provides.
func (q *StandaloneQueue) dispatchOnce(dispatch chan<- jobRecord) {
	ctx := context.Background()
	candidates, err := claimCandidates(ctx, q.db, time.Now(), claimBatchSize)
	if err != nil {
		obs.FromContext(ctx).Error("jobs dispatch: query candidates failed", "error", err)
		return
	}

	for _, rec := range candidates {
		tenant := pkgcore.TenantID(rec.TenantID)
		if !q.tryReserveTenantSlot(tenant) {
			continue
		}

		claimed, err := claimOne(ctx, q.db, rec, time.Now(), q.owner)
		if err != nil {
			q.releaseTenantSlot(tenant)
			obs.FromContext(ctx).Error("jobs dispatch: claim failed", "job_id", rec.ID, "error", err)
			continue
		}
		if !claimed {
			// Lost a race (see claimOne's doc comment) -- release the slot
			// this candidate never actually used.
			q.releaseTenantSlot(tenant)
			continue
		}

		rec.Status = string(StatusRunning)
		// The attempt itself is counted only at the worker handoff
		// (runWorker's runAttempt / markAttemptStarted), not here: a Job
		// sitting claimed-but-not-started must not report an Attempts
		// figure that pre-counts a Handle call nothing has made yet.
		select {
		case dispatch <- rec:
		case <-q.stopCh:
			// Shutting down: the claim already landed in the database as
			// StatusRunning, so resetInterruptedRecords on the next Start
			// recovers it -- nothing further to do here but stop cleanly.
			q.releaseTenantSlot(tenant)
			return
		}
	}
}

// tryReserveTenantSlot reports whether tenant is under q.tenantConcurrency,
// reserving a slot (incrementing its running count) if so.
func (q *StandaloneQueue) tryReserveTenantSlot(tenant pkgcore.TenantID) bool {
	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	if q.runningPerTenant[tenant] >= q.tenantConcurrency {
		return false
	}
	q.runningPerTenant[tenant]++
	return true
}

// releaseTenantSlot releases a slot reserved by tryReserveTenantSlot.
func (q *StandaloneQueue) releaseTenantSlot(tenant pkgcore.TenantID) {
	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	q.runningPerTenant[tenant]--
	if q.runningPerTenant[tenant] <= 0 {
		delete(q.runningPerTenant, tenant)
	}
}

// runWorker receives claimed Jobs from dispatch and executes them one at a
// time until dispatch is closed or q.stopCh fires.
func (q *StandaloneQueue) runWorker(dispatch <-chan jobRecord) {
	defer q.wg.Done()
	for {
		select {
		case <-q.stopCh:
			return
		case rec, ok := <-dispatch:
			if !ok {
				return
			}
			q.runAttempt(rec)
			q.releaseTenantSlot(pkgcore.TenantID(rec.TenantID))
		}
	}
}

// runAttempt is the worker-handoff half of a claim — the moment a worker
// actually takes possession of a Job the dispatcher claimed (claimOne). It
// counts the attempt: rec.Attempts is incremented in memory (execute's
// retry/dead-letter arithmetic reads it), and the increment plus the
// attempt's StartedAt are persisted by markAttemptStarted before Handle
// runs. Doing this at the handoff rather than at the claim is what keeps
// Get() honest for a Job that sits claimed-but-not-started (see claimOne's
// and markAttemptStarted's own doc comments), and what keeps an unclean
// exit in the claim-to-handoff window from consuming an attempt that never
// ran.
func (q *StandaloneQueue) runAttempt(rec jobRecord) {
	rec.Attempts++
	startedAt := time.Now()
	rec.StartedAt = &startedAt
	ctx := jobContext(pkgcore.TenantID(rec.TenantID))
	if werr := markAttemptStarted(context.Background(), q.db, rec.ID, startedAt); werr != nil {
		obs.FromContext(ctx).Warn("jobs: persisting attempt start failed", "job_id", rec.ID, "error", werr)
	}
	q.execute(rec)
}

// invokeHandle calls handler.Handle for job, recovering a panic raised
// inside it into an ordinary error instead of letting it propagate out of
// the worker goroutine that called execute. asynq's own processor.perform
// (github.com/hibiken/asynq) already wraps the equivalent call in its own
// defer/recover, which is what protects asynq.Queue for free; StandaloneQueue
// hand-rolls its own worker loop (runWorker, this file), so it must do the
// same here. Without this, a bug in any ONE tenant's Handler implementation
// — a nil dereference, an out-of-range slice index against a malformed
// job.Payload, a failed type assertion, a panicking third-party dependency
// — crashes the entire worker-pool process, taking every OTHER tenant's
// and every OTHER module's in-flight and queued Jobs down with it: the
// modules compile into one binary, so no process boundary contains the
// crash. The resulting error is handled identically to any other Handle
// failure by execute: retried while attempts remain, then dead-lettered.
//
// The panic value becomes errStandaloneHandlerPanicked's cause, so job.Error
// stays a short, operator-readable line exactly like every other failure
// this package records; the full stack trace is only logged, never
// persisted in job.Error, which is operator-facing text a caller reads
// back through Get/DeadLetterJobs.
func invokeHandle(ctx context.Context, handler Handler, job *Job, progress ProgressFn, log *slog.Logger) (result Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("jobs: handler panicked",
				"job_id", string(job.ID), "job_type", job.Type,
				"panic", r, "stack", string(debug.Stack()))
			err = errStandaloneHandlerPanicked.WithParam("type", job.Type).WithParam("job_id", string(job.ID)).
				WithCause(fmt.Errorf("%v", r))
		}
	}()
	return handler.Handle(ctx, job, progress)
}

// invokeOnFailure calls hook.OnFailure for job, recovering a panic the same
// way invokeHandle does for Handle — see its doc comment for why this
// matters: OnFailure is exactly as much a business-module-authored callback
// as Handle is, and just as capable of panicking (a refund call against a
// malformed job.Payload, for example). OnFailure returns nothing, so
// recovering here only prevents a process crash; there is no result to
// hand back, matching OnFailure's own "not retried or otherwise observed
// by the queue" contract (handler.go).
func invokeOnFailure(ctx context.Context, hook FailureHook, job *Job, cause error, log *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("jobs: failure hook panicked",
				"job_id", string(job.ID), "job_type", job.Type,
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	hook.OnFailure(ctx, job, cause)
}

// recordJobMetrics records one completed Handle attempt on the
// "jobs.job.duration" Histogram and "jobs.job.attempts" Counter
// registerJobMetrics wires (queue_standalone.go), labeled by jobType and status
// -- status is always one of StatusSucceeded/StatusRetrying/
// StatusDeadLetter, the exact three outcomes whose attempt records exist:
// an attempt whose outcome a concurrent Cancel already discarded (any of
// execute's three !moved branches) is deliberately recorded under none of
// them, so a cancelled Job never shows up as succeeded, retrying or
// dead-lettered on either instrument. Every call site sits strictly after
// the conditional write that persisted the outcome reported the transition
// (execute, worker.go). Both instruments share one attribute set, computed
// once. A nil q.jobDuration (registerJobMetrics never ran, or failed) is
// the guard: registration always sets both fields together, so checking
// one stands for both -- see the struct field's own doc comment for why
// this must never panic a job execution.
func (q *StandaloneQueue) recordJobMetrics(jobType string, status Status, duration time.Duration) {
	if q.jobDuration == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("job_type", jobType),
		attribute.String("status", string(status)),
	)
	q.jobDuration.Record(context.Background(), duration.Seconds(), attrs)
	q.jobAttempts.Add(context.Background(), 1, attrs)
}

// recordDeadLetter records one job moving to StatusDeadLetter on the
// "jobs.job.dead_letter" Counter registerJobMetrics wires, labeled by
// jobType only. Called only from execute's dead-letter branch, strictly
// after completeDeadLetter reported a genuine running -> dead-letter
// transition -- never for a cancelled job whose dead-letter write no-op'd
// -- so the counter counts dead-letters actually persisted. See
// recordJobMetrics's own doc comment for the nil-guard rationale, which
// applies identically here.
func (q *StandaloneQueue) recordDeadLetter(jobType string) {
	if q.jobDeadLetter == nil {
		return
	}
	q.jobDeadLetter.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("job_type", jobType),
	))
}

// execute runs exactly one Handle attempt for rec (already StatusRunning
// in the database) and persists its outcome. Handle is never called when
// handler is nil (ErrHandlerNotRegistered) or when rec.TenantID is empty
// (errStandaloneJobMissingTenant — defense in depth against a row that reached
// this table by some path other than Enqueue); both are treated as an
// ordinary Handle failure instead. A panic raised inside Handle, or inside
// a FailureHook.OnFailure this method calls after a dead-letter, is
// recovered by invokeHandle/invokeOnFailure rather than left to crash the
// worker-pool process — see their own doc comments. Every persistence
// write in this method uses a context.Background()-rooted context,
// deliberately independent of handleCtx: a bookkeeping write (recording
// success, retry or dead-letter) must not itself fail merely because the
// Job's own per-attempt timeout happened to elapse at the exact moment
// Handle returned.
func (q *StandaloneQueue) execute(rec jobRecord) {
	tenant := pkgcore.TenantID(rec.TenantID)
	baseCtx := jobContext(tenant)

	timeout := time.Duration(rec.TimeoutNanos)
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	handleCtx, cancel := context.WithTimeout(baseCtx, timeout)
	defer cancel()

	log := obs.FromContext(handleCtx)
	job := toJob(&rec)
	handler := q.handler(rec.Type)

	var (
		result Result
		err    error
	)
	attemptStart := time.Now()
	switch {
	case handler == nil:
		err = ErrHandlerNotRegistered.WithParam("type", rec.Type)
	case rec.TenantID == "":
		// Defense in depth: Task.validate blocks an empty TenantID from
		// ever reaching Enqueue, so this is not reachable through the
		// public API today — but a row that reached this table by some
		// other path must still never run Handle with no usable tenant.
		// See errStandaloneJobMissingTenant's own doc comment.
		err = errStandaloneJobMissingTenant.WithParam("type", rec.Type).WithParam("job_id", rec.ID)
	default:
		progress := func(pct int, msg string) {
			if perr := updateProgress(handleCtx, q.db, q.owner, rec.ID, pct, msg); perr != nil {
				log.Warn("jobs: persisting progress failed", "job_id", rec.ID, "error", perr)
			}
		}
		result, err = invokeHandle(handleCtx, handler, job, progress, log)
	}
	duration := time.Since(attemptStart)
	durationMS := duration.Milliseconds()

	now := time.Now()
	bg := context.Background()
	if err == nil {
		moved, werr := completeSucceeded(bg, q.db, q.owner, rec.ID, result, now)
		switch {
		case werr != nil:
			// Persisting the success result itself failed. The row is still
			// StatusRunning and the handler's outcome was never persisted --
			// the identical database state a handler failure leaves -- so the
			// attempt converges through the same terminal machinery
			// settleFailedAttempt applies to a handler failure, with an
			// internal cause naming the persistence failure. See that
			// function's doc comment for the chosen semantics and why the
			// at-least-once re-attempt is this module's standing answer to an
			// unpersisted outcome.
			log.Error("jobs: persisting success failed", "job_id", rec.ID, "error", werr)
			q.settleFailedAttempt(log, rec, fmt.Errorf("jobs: persisting success result failed: %w", werr), duration)
		case moved:
			// The success log line and the success metrics fire strictly AFTER
			// completeSucceeded' transition report, mirroring the dead-letter
			// branch below -- see the moved == true comment there for the full
			// rationale, which applies identically: a concurrent Cancel can
			// no-op the write, and a record emitted ahead of the write would
			// survive the no-op and show a cancelled Job as succeeded.
			log.Info("job succeeded", "job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS)
			q.recordJobMetrics(rec.Type, StatusSucceeded, duration)
		default:
			// moved == false: the success write no-op'd -- the row was no
			// longer this writer's to settle when the write landed (its
			// WHERE names both the running status and this writer's own
			// claim). That is NOT proof of a concurrent Cancel: a row
			// another writer reset and re-claimed after a writer-gate lapse
			// no-ops the identical write. logDiscardedOutcome probes the
			// row and reports what it actually says.
			if cancelled := q.logDiscardedOutcome(log, rec, durationMS, StatusSucceeded); cancelled {
				job.Status = StatusCancelled
			}
		}
		return
	}
	q.settleFailedAttempt(log, rec, err, duration)
}

// settleFailedAttempt converges one failed attempt of rec -- whose row is
// StatusRunning -- onto the attempt's terminal machinery: a dead-letter
// (completeDeadLetter, with the standard FailureHook run for a genuine
// running -> dead-letter transition) once rec.Attempts exceeds
// rec.MaxRetries, a scheduled retry (completeRetrying under the exponential
// backoff) otherwise. cause is what the attempt failed with: the handler's
// own error on the ordinary failure path, or -- for an attempt whose
// HANDLER succeeded but whose outcome write failed (execute's success
// branch) -- an internal error naming the persistence failure.
//
// execute funnels both shapes through this one function because they leave
// the database in the identical state: a row still StatusRunning whose
// outcome was never persisted. The module's recovery for that state is
// at-least-once re-attempt -- resetInterruptedRecords re-runs every
// running row at the next Start after a crash between Handle returning and
// its outcome write landing -- so a success whose result could not be
// persisted converges exactly like a handler failure: scheduled to re-run
// later, or, once the retry budget is exhausted, an operator-visible
// dead-letter whose recorded cause names the persistence failure, with
// OnFailure run under the same genuine-transition rule every dead-letter
// follows (handler.go). Re-executing the handler after an unconfirmable
// success is the same at-least-once trade the crash path already accepts;
// the cause text is the hook author's and operator's signal that this
// dead-letter's attempt actually reported success.
//
// A persistence failure whose own convergence write errors in turn (the
// completeRetrying/completeDeadLetter transition failing too) is logged
// and left: two consecutive write failures mean the database is refusing
// writes outright, nothing further can be persisted in-process, and the
// row is recovered by the next Start's resetInterruptedRecords -- the
// standing crash recovery -- exactly as every other failed write behaves.
func (q *StandaloneQueue) settleFailedAttempt(log *slog.Logger, rec jobRecord, cause error, duration time.Duration) {
	bg := context.Background()
	now := time.Now()
	durationMS := duration.Milliseconds()
	timeout := time.Duration(rec.TimeoutNanos)
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	if rec.Attempts > rec.MaxRetries {
		moved, werr := completeDeadLetter(bg, q.db, q.owner, rec.ID, cause.Error(), now)
		if werr != nil {
			log.Error("jobs: persisting dead letter failed", "job_id", rec.ID, "error", werr)
			return
		}
		if !moved {
			// The dead-letter write no-op'd: the row was no longer this
			// writer's to settle when the write landed (its WHERE names
			// both the running status and this writer's own claim). Two
			// causes, with opposite
			// meanings -- a concurrent Cancel (markCancelled) already moved
			// the row out of StatusRunning, in which case the persisted
			// terminal state is StatusCancelled and Cancel wins over the
			// concurrent final failure by design (Queue.Cancel's own doc
			// comment); or another writer's resetInterruptedRecords/claimOne
			// stole the row after a writer-gate lapse, in which case this
			// very no-op is the first evidence of a double execution and
			// must be logged as such, never as a cancellation. Either way
			// this attempt's failure outcome -- and any compensation it
			// would have triggered -- is discarded: OnFailure must NOT run,
			// since its contract (handler.go) requires the dead-letter to
			// actually have been persisted first, and neither a cancelled
			// nor a stolen Job has a dead-letter of this attempt's. Nothing
			// further is recorded: no dead-letter log, no dead-letter
			// metric, no attempt-outcome metric -- the outcome was
			// discarded, not dead-lettered, and ops dashboards must not
			// show it as dead-lettered. logDiscardedOutcome probes the row
			// and picks the truthful record.
			q.logDiscardedOutcome(log, rec, durationMS, StatusDeadLetter)
			return
		}
		// The transition report above (moved == true) is the one and only
		// ground truth that a dead-letter really was persisted, so the
		// dead-letter log line and metrics record -- recordJobMetrics'
		// StatusDeadLetter attempt-outcome row and recordDeadLetter's
		// jobs.job.dead_letter counter -- fire strictly AFTER it, never
		// before: a record emitted ahead of the write would survive a
		// no-op write (a concurrent Cancel) or a failed write and show a
		// cancelled or still-running Job as dead-lettered.
		log.Error("job exhausted retries, moved to dead letter",
			"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS, "error", cause)
		q.recordJobMetrics(rec.Type, StatusDeadLetter, duration)
		q.recordDeadLetter(rec.Type)
		if hook, ok := q.handler(rec.Type).(FailureHook); ok {
			job := toJob(&rec)
			job.Status = StatusDeadLetter
			job.Error = cause.Error()
			hookCtx, hookCancel := context.WithTimeout(jobContext(pkgcore.TenantID(rec.TenantID)), timeout)
			invokeOnFailure(hookCtx, hook, job, cause, log)
			hookCancel()
		}
		return
	}

	delay := q.backoffDelay(rec.Attempts)
	// The retry log line and the retry metrics fire strictly AFTER
	// completeRetrying's transition report, for the identical reason the
	// success branch above and the dead-letter branch both give: a
	// concurrent Cancel can no-op the write, and a record emitted ahead of
	// the write would show a cancelled Job as retrying.
	moved, werr := completeRetrying(bg, q.db, q.owner, rec.ID, cause.Error(), now.Add(delay), now)
	switch {
	case werr != nil:
		log.Error("jobs: persisting retry failed", "job_id", rec.ID, "error", werr)
	case moved:
		log.Warn("job attempt failed, scheduling retry",
			"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS,
			"retry_in_ms", delay.Milliseconds(), "error", cause)
		q.recordJobMetrics(rec.Type, StatusRetrying, duration)
	default:
		// moved == false: the retry write no-op'd -- the same two-cause
		// no-op as the success and dead-letter branches (see
		// logDiscardedOutcome), never cancellation by assumption.
		q.logDiscardedOutcome(log, rec, durationMS, StatusRetrying)
	}
}

// logDiscardedOutcome is execute's response to an outcome write that
// no-op'd -- completeSucceeded/completeRetrying/completeDeadLetter reported
// moved == false, meaning the row was no longer this writer's to settle
// when the conditional write landed (the write's WHERE names both the
// running status AND the writer's own claim, store.go). It probes the row
// to classify that no-op honestly, because a plain no-op has causes with
// opposite meanings: a concurrent Cancel (Queue.Cancel's markCancelled won
// the race and the row is StatusCancelled -- discardedStatus, the terminal
// status the no-op'd write was trying to persist, is then the truthful
// record, logged as the discarded_outcome attribute), or another writer's
// resetInterruptedRecords/claimOne stealing the row after a writer-gate
// lapse -- the row back in Pending, running under the other writer's claim,
// or already settled by the other execution: the first symptom of a double
// execution, never to be explained away as a cancellation. The strongest
// of those shapes, a row
// STILL RUNNING under another writer's claim (the other execution is in
// flight right now), is logged at Error with its own message; the other
// stolen shapes fall through to the Warn below. Returns whether the row was
// genuinely StatusCancelled, so the caller mirrors that state onto its
// in-memory Job only then. A probe that cannot read the row logs the
// failure and answers false: the discard is logged, its cause is not
// guessed.
func (q *StandaloneQueue) logDiscardedOutcome(log *slog.Logger, rec jobRecord, durationMS int64, discardedStatus Status) bool {
	found, err := findByID(context.Background(), q.db, JobID(rec.ID))
	if err != nil {
		log.Warn("jobs: outcome not persisted; row state unreadable, outcome discarded",
			"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS, "error", err)
		return false
	}
	if found.Status == string(StatusCancelled) {
		log.Info("job cancelled before its outcome could be recorded, outcome discarded",
			"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS,
			"discarded_outcome", string(discardedStatus))
		return true
	}
	if found.Status == string(StatusRunning) {
		// A no-op'd write can only leave the row running when it runs under
		// a claim this writer does not own -- the no-op itself proves the
		// row did not match this writer's own claim an instant ago, and a
		// row a single writer owns cannot change owner within one attempt.
		// Another writer is executing this job RIGHT NOW while this attempt
		// finishes: the double execution the writer gate exists to prevent,
		// logged as such -- never as a cancellation.
		log.Error("jobs: outcome not persisted: the row is running under another writer, outcome discarded",
			"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS,
			"discarded_outcome", string(discardedStatus), "row_status", found.Status, "claimed_by", found.ClaimedBy)
		return false
	}
	log.Warn("jobs: outcome not persisted: row not running when the write landed, outcome discarded",
		"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS,
		"row_status", found.Status, "claimed_by", found.ClaimedBy)
	return false
}

// backoffDelay computes the exponential backoff before the next attempt,
// given that the attempts-th one has just failed: q.backoffBase *
// 2^(attempts-1), capped at q.backoffMax. attempts below 1 is treated as 1.
func (q *StandaloneQueue) backoffDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := q.backoffBase
	for i := 1; i < attempts; i++ {
		if delay >= q.backoffMax {
			return q.backoffMax
		}
		delay *= 2
	}
	if delay > q.backoffMax {
		return q.backoffMax
	}
	return delay
}
