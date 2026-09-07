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
// bug, a manual SQL fixup, or a future writer that bypasses this package's
// own API. Checked before calling Handle, exactly mirroring
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
// constant (backend coding standard §10) rather than configurable: unlike
// worker count or the per-tenant concurrency limit, which are genuine
// deployment-dependent tuning knobs, this is purely an internal batch size
// with one reasonable value.
const claimBatchSize = 100

// jobContext rebuilds the context a Handler (and FailureHook) call
// receives: pkgcore.WithTenant from the Job's own stored tenant, over a
// freshly detached context.Background(). This is deliberately NOT derived
// from whatever context the original Queue.Enqueue call ran in — that
// context no longer exists by the time a worker claims the row back out of
// SQLite — and NOT derived from StandaloneQueue's own internal dispatcher/worker
// -loop lifecycle context either, so that closing the queue does not
// abruptly cancel a Handle call already in flight (see StandaloneQueue.Close's
// own doc comment).
//
// This is the one function responsible for closing "the tenant context
// trap" described in AGENTS.md: see worker_test.go's
// TestJobContext_ProducesTenantScopedContext (the positive case: a context
// built by this function carries exactly the tenant given) and
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
// sides go through q.tenantMu. Every tick also refreshes this queue's
// writer registration (heartbeat), so a live dispatcher is what keeps a
// concurrent Start on the same database from mistaking this queue for a
// crashed one.
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
			q.heartbeat()
			q.dispatchOnce(dispatch)
		}
	}
}

// heartbeat refreshes this queue's queue_writers registration row (store.go)
// so a concurrent StandaloneQueue.Start on the same database can tell a
// live writer from a crashed one — see ErrQueueWriterActive and
// acquireWriterRegistration. It rides the dispatcher's own tick, so no
// extra goroutine or lifecycle is needed: while the dispatcher beats, this
// queue is alive by definition. A heartbeat that fails to reach its own row
// means the registration was stolen (its beats lapsed past the stale window
// and a sibling took over) or removed — the queue keeps running rather than
// stopping mid-flight, which would strand every row it has claimed, and the
// error is logged every tick until the operator resolves the two-writer
// situation the steal implies.
func (q *StandaloneQueue) heartbeat() {
	ctx := context.Background()
	ok, err := heartbeatWriterRegistration(ctx, q.db, q.owner, time.Now())
	if err != nil {
		obs.FromContext(ctx).Error("jobs: writer heartbeat failed", "error", err)
		return
	}
	if !ok {
		obs.FromContext(ctx).Error("jobs: writer registration lost; another StandaloneQueue may have started against this database", "owner", q.owner)
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
// exactly the gap that existed before claimCandidates' own interleaving
// was added, and TestPerTenantConcurrencyLimiting, which pins the
// complementary concurrency-admission property this file's skip-and-
// continue loop provides.
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
// and every OTHER module's in-flight and queued Jobs down with it (root
// CLAUDE.md: speed's modules "compile into one binary"). The resulting
// error is handled identically to any other Handle failure by execute:
// retried while attempts remain, then dead-lettered.
//
// The panic value becomes errStandaloneHandlerPanicked's cause, so job.Error
// stays a short, operator-readable line exactly like every other failure
// this package records; the full stack trace is only logged, never
// persisted — backend coding standard §6.2 forbids letting a stack trace
// reach a response body, and Job.Error is operator-facing text a caller
// can read back through Get/DeadLetterJobs.
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
// registerJobMetrics wires (standalone_queue.go), labeled by jobType and status
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
			if perr := updateProgress(handleCtx, q.db, rec.ID, pct, msg); perr != nil {
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
		// The success log line and the success metrics fire strictly AFTER
		// completeSucceeded' transition report, mirroring the dead-letter
		// branch below -- see the moved == true comment there for the full
		// rationale, which applies identically: a concurrent Cancel can
		// no-op the write, and a record emitted ahead of the write would
		// survive the no-op and show a cancelled Job as succeeded.
		moved, werr := completeSucceeded(bg, q.db, rec.ID, result, now)
		switch {
		case werr != nil:
			log.Error("jobs: persisting success failed", "job_id", rec.ID, "error", werr)
		case moved:
			log.Info("job succeeded", "job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS)
			q.recordJobMetrics(rec.Type, StatusSucceeded, duration)
		default:
			log.Info("job cancelled before its success could be recorded, outcome discarded",
				"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS)
			job.Status = StatusCancelled
		}
		return
	}

	if rec.Attempts > rec.MaxRetries {
		moved, werr := completeDeadLetter(bg, q.db, rec.ID, err.Error(), now)
		if werr != nil {
			log.Error("jobs: persisting dead letter failed", "job_id", rec.ID, "error", werr)
			return
		}
		if !moved {
			// A concurrent Cancel (markCancelled) already moved the row out
			// of StatusRunning while this final attempt was executing, so
			// the dead-letter write was a no-op: the persisted terminal
			// state is StatusCancelled, never StatusDeadLetter. Cancel wins
			// over a concurrent final failure by design (Queue.Cancel's own
			// doc comment), so this attempt's failure outcome -- and any
			// compensation it would have triggered -- is discarded: OnFailure
			// must NOT run, since its contract (handler.go) requires the
			// dead-letter to actually have been persisted first, and a
			// cancelled Job has no dead-letter. Nothing is recorded for it
			// beyond this one truthful Info line: no dead-letter log, no
			// dead-letter metric, no attempt-outcome metric -- the outcome
			// was discarded, not dead-lettered, and ops dashboards must not
			// show a cancelled Job as dead-lettered. The in-memory job
			// mirrors the persisted terminal state instead.
			log.Info("job cancelled before its final failure could dead-letter, outcome discarded",
				"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS)
			job.Status = StatusCancelled
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
			"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS, "error", err)
		q.recordJobMetrics(rec.Type, StatusDeadLetter, duration)
		q.recordDeadLetter(rec.Type)
		if hook, ok := handler.(FailureHook); ok {
			job.Status = StatusDeadLetter
			job.Error = err.Error()
			hookCtx, hookCancel := context.WithTimeout(jobContext(tenant), timeout)
			invokeOnFailure(hookCtx, hook, job, err, log)
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
	moved, werr := completeRetrying(bg, q.db, rec.ID, err.Error(), now.Add(delay), now)
	switch {
	case werr != nil:
		log.Error("jobs: persisting retry failed", "job_id", rec.ID, "error", werr)
	case moved:
		log.Warn("job attempt failed, scheduling retry",
			"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS,
			"retry_in_ms", delay.Milliseconds(), "error", err)
		q.recordJobMetrics(rec.Type, StatusRetrying, duration)
	default:
		log.Info("job cancelled before its failure could schedule a retry, outcome discarded",
			"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS)
		job.Status = StatusCancelled
	}
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
