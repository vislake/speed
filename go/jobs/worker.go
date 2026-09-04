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
// sides go through q.tenantMu.
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

// dispatchOnce runs one poll/claim/dispatch cycle: it reads a batch of
// eligible candidates in priority order and, for each, either claims it
// and hands it to a worker or skips it because its tenant is currently at
// q.tenantConcurrency — moving on to the next candidate (which may belong
// to a different tenant) rather than stalling on the first one, which is
// what makes one tenant's backlog unable to starve another's.
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

		claimed, err := claimOne(ctx, q.db, rec, time.Now())
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
		rec.Attempts++
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
			q.execute(rec)
			q.releaseTenantSlot(pkgcore.TenantID(rec.TenantID))
		}
	}
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
// StatusDeadLetter, the exact three outcomes execute can reach. Both
// instruments share one attribute set, computed once. A nil q.jobDuration
// (registerJobMetrics never ran, or failed) is the guard: registration
// always sets both fields together, so checking one stands for both --
// see the struct field's own doc comment for why this must never panic a
// job execution.
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
// jobType only. See recordJobMetrics's own doc comment for the nil-guard
// rationale, which applies identically here.
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
		log.Info("job succeeded", "job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS)
		q.recordJobMetrics(rec.Type, StatusSucceeded, duration)
		if werr := completeSucceeded(bg, q.db, rec.ID, result, now); werr != nil {
			log.Error("jobs: persisting success failed", "job_id", rec.ID, "error", werr)
		}
		return
	}

	if rec.Attempts > rec.MaxRetries {
		log.Error("job exhausted retries, moving to dead letter",
			"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS, "error", err)
		q.recordJobMetrics(rec.Type, StatusDeadLetter, duration)
		q.recordDeadLetter(rec.Type)
		if werr := completeDeadLetter(bg, q.db, rec.ID, err.Error(), now); werr != nil {
			log.Error("jobs: persisting dead letter failed", "job_id", rec.ID, "error", werr)
			return
		}
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
	log.Warn("job attempt failed, scheduling retry",
		"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS,
		"retry_in_ms", delay.Milliseconds(), "error", err)
	q.recordJobMetrics(rec.Type, StatusRetrying, duration)
	if werr := completeRetrying(bg, q.db, rec.ID, err.Error(), now.Add(delay), now); werr != nil {
		log.Error("jobs: persisting retry failed", "job_id", rec.ID, "error", werr)
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
