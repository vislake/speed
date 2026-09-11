package asynq

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"time"

	asynqlib "github.com/hibiken/asynq"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// errTaskMissingTenant is processTask's defensive response to a task whose
// Headers carry no tenant_id at all. This should never happen for a task
// this package's own Enqueue created (buildTaskHeaders always sets it, and
// jobs.Task.Validate rejects an empty TenantID before Enqueue ever reaches
// asynq's client) -- the only realistic cause is a non-jobs producer having
// written directly into the same Redis instance/queues. Treated like any
// other Handle failure (retried, then dead-lettered) rather than panicking
// the worker, exactly per this package's general "fail the attempt, not the
// process" discipline for malformed input (compare jobs.ErrHandlerNotRegistered).
// Deliberately its own sentinel rather than a reuse of
// jobs.ErrHandlerNotRegistered: the two causes are operationally distinct
// (register the missing Handler vs. find and fix whatever enqueued a
// tenant-less task), and collapsing them would send an operator chasing the
// wrong fix.
var errTaskMissingTenant = apperr.Internal("jobs.asynq_task_missing_tenant")

// This file is Queue's counterpart to jobs' own worker.go: where that file's
// jobContext/execute close "the tenant context trap" and decide
// retry/backoff/dead-letter for StandaloneQueue, processTask (below) does
// the same job for the distributed deployment mode -- registered as the
// single asynqlib.HandlerFunc Queue.Start hands to asynqlib.Server.Start,
// deliberately NOT wrapped in an asynqlib.ServeMux (see its own doc comment
// for why).

// errTenantAtCapacity is returned by processTask, never by a jobs.Handler,
// when a Job's tenant is already at its concurrency limit. It is not a
// business failure: Queue's Config.IsFailure (queue.go) reports false for
// it, so asynq's own retry bookkeeping (internal/rdb's Retry Lua script:
// "if isFailure then Retried++") never increments Retried for a bounce
// caused by this error -- a Job can be bounced any number of times by its
// tenant being over capacity without ever burning into MaxRetries, as long
// as retries remain. On a Job's FINAL allowed attempt a bounce can no
// longer be bounced again: asynq's own archive-vs-retry decision
// (processor.go's handleFailedMessage: archive whenever retried >= retry,
// isFailure not consulted) archives the task right after this package's
// ErrorHandler returns. handleErrorAttempt therefore fires the terminal
// attempt's FailureHook for such an archive-bound bounce too -- a
// dead-letter must never skip its compensation because its last attempt
// happened to bounce (see that function's doc comment).
// errCancelMarkerUnreadable
// (below) is the bounce-class sibling this same machinery treats
// identically.
var errTenantAtCapacity = errors.New("jobs: tenant is at its concurrency limit")

// errCancelMarkerUnreadable is returned by processTask when the
// cancellation-marker read (queue.go's readCancelMarker) that guards every
// attempt failed -- Redis answered an error, never "no marker". It is the
// fail-closed answer to an unverifiable cancellation state: a Job whose
// marker cannot be read might be cancelled, so it must not run; the
// attempt is refused instead. It is bounce-class, exactly like
// errTenantAtCapacity: isFailure reports false for it, so the refusal
// never burns a retry -- the task redelivers on the short throttle delay
// and, once the marker read works again, either finds the marker (skips,
// cancelled) or runs normally. See also handleErrorAttempt, which fires a
// terminal attempt's FailureHook for an archive-bound refusal of this
// class just as it does for errTenantAtCapacity.
var errCancelMarkerUnreadable = errors.New("jobs: cancellation marker unreadable")

// failedAttemptError is the error processTaskUncancelled returns when a
// genuine attempt fails (a Handle error, an unregistered handler type, a
// missing tenant header) -- a thin wrapper carrying the attempt's own
// measured duration alongside the failure's real cause. It exists purely
// to carry that duration from the attempt (where it is measured) to
// handleErrorAttempt (where asynq's dispatch loop decides the attempt's
// outcome and the jobs.job.* metrics are recorded, queue.go's
// recordJobMetrics) -- asynq's ErrorHandler hook receives only
// (ctx, task, err), with no timing metadata of its own, so the wrapper is
// the one channel that survives asynq's own bookkeeping between the two
// points. It is transparent to everything else: Error() delegates to the
// wrapped cause (so asynq's LastErr and this package's own log lines
// carry exactly the message they would have carried unwrapped), Unwrap()
// delegates for errors.Is/errors.As (so isFailure, retryDelay and
// apperr.As all keep recognizing the cause), and neither of the two
// bounce-class sentinels is ever wrapped -- see attemptDuration and
// recordFailedAttempt's own doc comments for why the wrapper's absence is
// itself the bounce marker.
type failedAttemptError struct {
	cause    error
	duration time.Duration
}

func (e *failedAttemptError) Error() string { return e.cause.Error() }
func (e *failedAttemptError) Unwrap() error { return e.cause }

// wrapFailedAttempt wraps cause with the attempt duration that measured
// it, returning a *failedAttemptError. Never used for the bounce-class
// sentinels (errTenantAtCapacity, errCancelMarkerUnreadable): a bounce is
// not a failed attempt in the jobs.job.* metrics' sense, and the
// wrapper's absence is what lets handleErrorAttempt tell the two apart
// without an extra error-class check of its own.
func wrapFailedAttempt(cause error, duration time.Duration) error {
	return &failedAttemptError{cause: cause, duration: duration}
}

// attemptDuration reports the measured duration of a failed attempt whose
// error was wrapped by wrapFailedAttempt, and whether the error carries
// one at all. An error without a wrapper reached handleErrorAttempt by a
// path that measured no attempt duration -- a bounce-class refusal, or a
// Handle panic that asynq's own processor recovered (worker.go's
// perform) -- and its outcome is recorded through
// recordJobMetricsAttemptOnly (queue.go) without a duration data point.
func attemptDuration(err error) (time.Duration, bool) {
	var fa *failedAttemptError
	if errors.As(err, &fa) {
		return fa.duration, true
	}
	return 0, false
}

// publishTerminal publishes one jobs.job.terminal event on the bus
// WithEventBus configured, at whichever of this Queue's three terminal
// points the caller sits: processTaskUncancelled's success site,
// handleErrorAttempt's archive-bound dead-letter site, and Cancel (queue.go)
// after the cancellation marker is durably written. Without a bus it is a
// no-op, so an unconfigured Queue pays nothing at any of the three.
//
// The publish runs on context.WithoutCancel(ctx): the terminal transition
// the event announces has already happened (the cancellation marker is
// durable; the attempt succeeded; asynq will archive this attempt the
// moment this hook returns), and neither the failed attempt's own
// cancellation -- live on the ctx handleError received -- nor a caller's
// deadline may be allowed to silently drop the notification before it
// reaches the bus. Trace values still ride through WithoutCancel, so the
// delivery keeps its correlation either way.
//
// A failed Publish is logged and dropped, never returned and never retried
// here: this leg has no outbox, and its at-least-once direction comes from
// asynq's own redelivery converting a crash into a repeated terminal
// arrival rather than a lost one (see WithEventBus's doc comment and
// jobs.EventJobTerminal's consumer contract). The payload's Event.TenantID
// is the task's own tenant header, the same field every tenant rebuild in
// this file reads -- including a platform-scoped task's sentinel value.
func (q *Queue) publishTerminal(ctx context.Context, tenantID pkgcore.TenantID, evt jobs.JobTerminalEvent) {
	if q.bus == nil {
		return
	}
	pubCtx := context.WithoutCancel(ctx)
	if err := q.bus.Publish(pubCtx, pkgcore.Event{
		Type:     jobs.EventJobTerminal,
		TenantID: tenantID,
		Payload:  evt,
	}); err != nil {
		obs.FromContext(pubCtx).Warn("jobs: publishing terminal event failed",
			"job_id", string(evt.JobID), "job_type", evt.JobType, "status", string(evt.Status), "error", err)
	}
}

// recordFailedAttempt records one failed attempt's outcome on the
// jobs.job.attempts Counter (and, when the attempt's duration was
// measured, the jobs.job.duration Histogram) at the point asynq's own
// dispatch loop has decided what the outcome is -- the mirror of
// StandaloneQueue's recordJobMetrics calls at the three outcome points of
// its own execute (jobs' worker.go), mapped onto handleErrorAttempt's
// replicated archive-vs-retry boundary. status is StatusRetrying when
// asynq will retry the attempt and StatusDeadLetter when its dispatch
// loop will archive it (see handleErrorAttempt's own doc comment for the
// boundary's exactness). A retryable tenant-concurrency bounce or
// cancellation-marker refusal records nothing: those attempts are never
// failures in the metrics' sense -- no Handle ran, no retry budget was
// consumed -- exactly as they are not failures anywhere else in this
// package (isFailure, the throttle delay, the missing logs). A cancelled
// attempt records nothing either -- handleErrorAttempt's cancellation
// check returns before this is ever called, so a cancelled Job shows up
// in none of the outcome counters, mirroring StandaloneQueue's own
// cancel-wins metric discipline. The record fires BEFORE asynq's own
// settlement write (there is no post-settlement hook); see recordJobMetrics'
// own doc comment for the ordering concession, which applies identically.
func (q *Queue) recordFailedAttempt(jobType string, status jobs.Status, err error) {
	if duration, ok := attemptDuration(err); ok {
		q.recordJobMetrics(jobType, status, duration)
	} else {
		// A terminal outcome whose attempt duration was never measured --
		// a Handle panic asynq's own processor recovered (its perform),
		// which reaches the ErrorHandler with no timing metadata, or a
		// terminal-attempt bounce. Count the outcome on the attempts
		// Counter without a duration data point: the Histogram measures
		// Handle-attempt durations, and none was measured (queue.go's
		// recordJobMetricsAttemptOnly doc comment has the full argument).
		q.recordJobMetricsAttemptOnly(jobType, status)
	}
	// The dead-letter Counter fires for a dead-letter outcome regardless
	// of whether the attempt carried a measured duration.
	if status == jobs.StatusDeadLetter {
		q.recordDeadLetter(jobType)
	}
}

// tryReserveTenantSlot and releaseTenantSlot are Queue's own admission gate
// -- structurally the same map+mutex shape as StandaloneQueue's
// tryReserveTenantSlot/releaseTenantSlot (jobs' own worker.go), but
// deliberately a separate, independent copy rather than a shared helper
// type: extracting one would mean touching StandaloneQueue's own
// fields/methods, and the standalone implementation stays untouched by
// design. WHERE this gate applies differs from StandaloneQueue's: here it
// is a fast bounce-and-redeliver inside the Handler call, not a
// pre-dequeue skip, because asynq gives us no way to peek at a task's
// tenant before dequeuing it off Redis.
func (q *Queue) tryReserveTenantSlot(tenant pkgcore.TenantID) bool {
	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	if q.runningPerTenant[tenant] >= q.tenantConcurrency {
		return false
	}
	q.runningPerTenant[tenant]++
	return true
}

func (q *Queue) releaseTenantSlot(tenant pkgcore.TenantID) {
	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	q.runningPerTenant[tenant]--
	if q.runningPerTenant[tenant] <= 0 {
		delete(q.runningPerTenant, tenant)
	}
}

// retryDelay is Queue's Config.RetryDelayFunc. It special-cases the two
// bounce-class errors -- errTenantAtCapacity and errCancelMarkerUnreadable
// -- with a short, jittered, NON-exponential delay (independent of n,
// which counts genuine business failures only -- see errTenantAtCapacity's
// own doc comment for why a throttle bounce never advances it) and defers
// to q.businessRetryDelayFunc (asynqlib.DefaultRetryDelayFunc unless
// overridden by WithRetryDelayFunc) for every real Handler failure, exactly
// matching StandaloneQueue's own backoffDelay role but implemented on top
// of asynq's own extension point instead of a hand-rolled formula.
func (q *Queue) retryDelay(n int, err error, t *asynqlib.Task) time.Duration {
	if errors.Is(err, errTenantAtCapacity) || errors.Is(err, errCancelMarkerUnreadable) {
		base := q.throttleRetryDelay
		// #nosec G404 -- jitter to avoid a redelivery thundering herd, not
		// a security-sensitive value (no token/credential/crypto material
		// derives from it). math/rand/v2 is the same, non-cryptographic
		// generator asynq's own DefaultRetryDelayFunc (server.go) uses for
		// its identical jittering purpose.
		return base + time.Duration(rand.Int64N(int64(base)+1))
	}
	return q.businessRetryDelayFunc(n, err, t)
}

// isFailure is Queue's Config.IsFailure. See errTenantAtCapacity's and
// errCancelMarkerUnreadable's own doc comments: reporting false for both
// bounce-class errors is what keeps a throttle bounce -- and a
// cancellation-state check that could not be answered -- from consuming
// retry budget.
func isFailure(err error) bool {
	return err != nil && !errors.Is(err, errTenantAtCapacity) && !errors.Is(err, errCancelMarkerUnreadable)
}

// handleError is Queue's Config.ErrorHandler. asynq invokes it exactly once
// per failed attempt (processor.go's handleFailedMessage), BEFORE deciding
// whether that attempt retries or archives -- which is the only hook point
// asynq offers for "this Job is about to dead-letter" (there is no separate
// archived/on-archive callback). It replicates asynq's own archive-vs-retry
// boundary (msg.Retried >= msg.Retry, processor.go) using the same values a
// Handler itself could read via asynqlib.GetRetryCount/GetMaxRetry, purely
// to decide whether THIS is the terminal attempt; it schedules or persists
// nothing itself, so this is reading asynq's documented MaxRetry contract,
// not reimplementing its retry machinery.
//
// Because handleFailedMessage calls this BEFORE its own switch statement's
// archive branch (p.archive, which is what makes a Job's Get() answer
// StatusDeadLetter), this is genuinely unreorderable within asynq's own
// dispatch loop: the library exposes no hook that runs after
// broker.Archive persists, so handleErrorAttempt's own FailureHook.OnFailure
// call below unavoidably runs before that write. This is the reason
// go/jobs's handler.go documents a weaker FailureHook ordering guarantee for
// this Queue than for StandaloneQueue -- see that type's own doc comment --
// and TestAsynqQueue_OnFailure_ObservesTaskNotYetArchived
// (integration_test/failure_hook_ordering_test.go) pins it against a real
// asynq/Redis backend.
//
// One more check lives here, at the same hook point: whether a concurrent
// Cancel (writeCancelMarker, queue.go) already settled this Job while the
// failed attempt was executing. OnFailure must not run for a cancelled Job
// (handler.go's FailureHook contract), and because a terminal attempt's
// failure reaches this hook in every interleaving asynq offers -- the
// handler's own error, and equally a ctx cancellation that the attempt's
// own interruption (CancelProcessing's signal) triggered, which asynq's
// dispatch loop turns into a failure of the very same attempt -- the
// marker is what tells the two apart. Cancel writes it BEFORE sending that
// signal (queue.go's Cancel), so whenever the signal's own effects reach
// this hook the cancellation is already durably observable here, never
// racing it.
func (q *Queue) handleError(ctx context.Context, t *asynqlib.Task, err error) {
	retried, _ := asynqlib.GetRetryCount(ctx)
	maxRetry, _ := asynqlib.GetMaxRetry(ctx)
	taskID, _ := asynqlib.GetTaskID(ctx)
	log := obs.FromContext(ctx)

	// Only a terminal attempt can ever reach OnFailure (handleErrorAttempt's
	// retried < maxRetry early return below), so the marker -- one extra
	// Redis GET -- is read only for those, never for a retryable failure or
	// a tenant-concurrency bounce that still has retries left.
	//
	// The read deliberately runs on a context stripped of the failed
	// attempt's own cancellation, not on ctx itself: the interleaving where
	// CancelProcessing's signal (queue.go's Cancel) already cancelled this
	// attempt's ctx is exactly one of the two ways a terminal attempt's
	// failure reaches this hook -- handleFailedMessage fails the attempt on
	// the queue's own behalf once ctx is done -- and on that path ctx is
	// already cancelled here, so a Redis GET under it would abort with
	// "context canceled" before ever asking Redis, silently defeating the
	// very check this exists to make. WithoutCancel keeps the read live
	// while changing nothing else; the marker write Cancel made is ordered
	// before that signal, so the read still finds it.
	var cancelledAt *time.Time
	if retried >= maxRetry {
		readCtx := context.WithoutCancel(ctx)
		if c, cerr := q.readCancelMarker(readCtx, taskID); cerr != nil {
			log.Warn("jobs: reading cancellation marker failed", "job_id", taskID, "error", cerr)
		} else {
			cancelledAt = c
		}
	}
	q.handleErrorAttempt(ctx, t, err, retried, maxRetry, taskID, cancelledAt, log)
}

// handleErrorAttempt is handleError's asynq-accessor-free core: everything
// handleError needs from asynq's own ctx (retried, maxRetry, taskID --
// asynqlib.GetRetryCount/GetMaxRetry/GetTaskID, all backed by an internal,
// package-private context key this package cannot fabricate on its own) is
// passed in explicitly instead, plus cancelledAt (the cancellation marker
// handleError read back for a terminal attempt, nil when no Cancel has
// landed) and the ctx-derived logger, so this logic -- the archive-boundary
// replication, the Cancel-wins-over-the-terminal-failure check, the
// terminal event's publish and the FailureHook invocation itself -- is
// unit-testable without a real asynq server ever having dequeued anything;
// see worker_test.go. Only handleError's own three GetXxx(ctx) calls and
// the marker read are, correctly, untested at the unit level: they are
// asynq's own accessors plus a Redis read, not this package's logic.
//
// ctx is the failed attempt's own context, taken unchanged from handleError
// and used for exactly one thing: the terminal event's publish
// (publishTerminal, which strips its cancellation and deadline). It is not
// an asynq accessor channel -- every value this function needs from ctx is
// still a parameter.
func (q *Queue) handleErrorAttempt(ctx context.Context, t *asynqlib.Task, err error, retried, maxRetry int, taskID string, cancelledAt *time.Time, log *slog.Logger) {
	if retried < maxRetry {
		// More retries remain; asynq will retry, not archive. The one thing
		// recorded here is the retrying outcome of a genuine failed
		// attempt, on the jobs.job.attempts Counter (and, when the
		// attempt's duration was measured, the jobs.job.duration
		// Histogram) -- the mirror of StandaloneQueue's own
		// "job attempt failed, scheduling retry" record point. A
		// bounce-class error (errTenantAtCapacity, errCancelMarkerUnreadable)
		// records nothing: it retries on the short throttle delay without
		// consuming budget, and is never a failure in the metrics' sense
		// (see recordFailedAttempt's own doc comment). A retryable panic
		// asynq's own processor recovered records the retrying outcome
		// without a duration point (recordFailedAttempt's attempt-only
		// branch), since no attempt duration was measured for it.
		if !errors.Is(err, errTenantAtCapacity) && !errors.Is(err, errCancelMarkerUnreadable) {
			q.recordFailedAttempt(t.Type(), jobs.StatusRetrying, err)
		}
		return
	}
	if cancelledAt != nil {
		// A concurrent Cancel (writeCancelMarker, queue.go) already settled
		// this Job as StatusCancelled while this final attempt was
		// executing, so the cancellation wins over the attempt's failure
		// outcome, exactly mirroring StandaloneQueue's completeDeadLetter
		// no-transition guard (jobs' own worker.go): the
		// failure's compensation -- FailureHook.OnFailure, whose contract
		// (jobs' handler.go) requires the Job to actually dead-letter first
		// -- must NOT run for a cancelled Job. Nothing here flips or
		// records any state of its own: the cancellation marker Cancel
		// wrote is the cancelled state, it stays in place, and every
		// read-back through Get()/DeadLetterJobs keeps reporting
		// StatusCancelled from it regardless of what asynq's own archive
		// write (which the dispatch loop still performs after this hook
		// returns) does to the underlying task record.
		log.Info("job cancelled before its final failure was processed; failure hook skipped",
			"job_id", taskID, "job_type", t.Type(), "attempts", retried+1)
		return
	}
	// From here on this is the terminal attempt and no cancellation is
	// durably recorded, which means asynq's own dispatch loop WILL archive
	// the task (dead-letter it) immediately after this hook returns:
	// processor.go's handleFailedMessage archives whenever retried >=
	// maxRetry, and isFailure is not consulted for that decision --
	// confirmed against the pinned v0.26.0 source. That archive is the
	// money event: FailureHook.OnFailure is the dead-letter's compensation,
	// and an archive that skips it silently strands whatever the Job was
	// paying for. Bounce-class terminal attempts (errTenantAtCapacity,
	// errCancelMarkerUnreadable) are not exempt from the hook: a bounce on
	// the final attempt still archives microseconds after this hook
	// returns, so its OnFailure fires too, with the bounce itself as the
	// recorded cause (the task's LastErr will carry the same bounce
	// message -- the approximation this mapping accepts). Only the marker
	// check above can silence the hook, exactly as for a genuine terminal
	// failure.
	if errors.Is(err, errTenantAtCapacity) || errors.Is(err, errCancelMarkerUnreadable) {
		log.Error("job's final attempt was bounced and will be archived; firing failure hook",
			"job_id", taskID, "job_type", t.Type(), "attempts", retried+1, "error", err)
	}

	// This is the archive decision: asynq's own dispatch loop will
	// dead-letter the task (archive it) right after this hook returns
	// (handleFailedMessage's retried >= maxRetry branch), so the
	// dead-letter outcome is recorded here -- the jobs.job.dead_letter
	// Counter plus the StatusDeadLetter row of the jobs.job.attempts
	// Counter, with the jobs.job.duration point whenever the attempt's
	// duration was measured (recordFailedAttempt picks the branch). A
	// terminal-attempt bounce records too -- its archive is a dead letter
	// in exactly the sense the module's own "dead-letter must never skip
	// its compensation because its last attempt happened to bounce"
	// language names it, and DeadLetterJobs lists it -- but without a
	// duration point, since no Handle ran. The record fires before the
	// archive write itself, exactly like the FailureHook call below; see
	// recordJobMetrics' own doc comment for the ordering concession.
	q.recordFailedAttempt(t.Type(), jobs.StatusDeadLetter, err)

	// The terminal signal's dead-letter point: this archive-bound branch is
	// the one place that knows asynq will dead-letter the task the moment
	// this hook returns, so the event is published here -- for a genuine
	// terminal failure and for an archive-bound bounce alike, exactly like
	// the dead-letter record above (a bounce that archives IS a dead
	// letter). The ordering concession is the same one OnFailure and the
	// metrics already document: this runs BEFORE asynq's own archive write,
	// and CompletedAt carries this moment, the closest honest value the
	// before-archive position allows. The cancellation check above already
	// returned, so a Cancel that raced this attempt publishes
	// nothing from here (its cancellation event went out at Cancel); the
	// same row-is-truth clause covers the residual where a cancellation
	// landed but its marker could not be read.
	q.publishTerminal(ctx, pkgcore.TenantID(t.Headers()[headerTenantID]), jobs.JobTerminalEvent{
		JobID:       jobs.JobID(taskID),
		JobType:     t.Type(),
		Status:      jobs.StatusDeadLetter,
		Error:       err.Error(),
		Attempts:    retried + 1,
		CompletedAt: time.Now(),
	})

	h := q.handler(t.Type())
	hook, ok := h.(jobs.FailureHook)
	if !ok {
		return
	}

	tenantID := pkgcore.TenantID(t.Headers()[headerTenantID])
	job := &jobs.Job{
		ID:             jobs.JobID(taskID),
		Type:           t.Type(),
		TenantID:       tenantID,
		Payload:        t.Payload(),
		IdempotencyKey: t.Headers()[headerIdempotencyKey],
		Status:         jobs.StatusDeadLetter,
		Attempts:       retried + 1,
		MaxRetries:     maxRetry,
		Error:          err.Error(),
		CreatedAt:      headerCreatedAtTime(t.Headers()),
	}

	// A fresh context, not ctx (which belongs to the attempt that just
	// failed and may already be at or past its own deadline) -- exactly
	// StandaloneQueue.execute's own choice for the same call, and for the
	// same reason: OnFailure runs real business compensation (refunding
	// credits, e.g.) that must not inherit a context already on its way
	// out. q.defaultTimeout approximates the per-job timeout StandaloneQueue
	// uses here exactly; asynq's context accessors expose TaskID/
	// RetryCount/MaxRetry/QueueName but not a task's own Timeout/Deadline
	// (context.go), so recovering the exact value would cost an extra
	// Inspector round trip purely for this bound -- not worth it for a
	// hook whose own timeout is already a documented approximation.
	hookCtx, cancel := context.WithTimeout(pkgcore.WithTenant(context.Background(), tenantID), q.defaultTimeout)
	defer cancel()
	invokeOnFailure(hookCtx, hook, job, err, log)
}

// invokeOnFailure calls hook.OnFailure for job, recovering a panic the
// same way jobs' own worker.go's invokeOnFailure does for StandaloneQueue
// -- see that function's doc comment for why this matters. OnFailure is
// exactly as much a business-module-authored callback as Handle is, and
// just as capable of panicking (a refund call against a malformed
// job.Payload, for example). The containment is not free on this side of
// the module: jobs' own invokeHandle doc comment notes that asynq's
// processor.perform wraps its own handler call in a defer/recover, but
// that recover covers ONLY ProcessTask -- the ErrorHandler (this package's
// handleError) runs strictly outside it, in handleFailedMessage on asynq's
// worker goroutine (processor.go, confirmed against the pinned v0.26.0
// source), so a panic here would otherwise propagate out of the worker
// goroutine and crash the whole process -- taking every replica's in-flight
// work down with it. The FailureHook contract (jobs' handler.go) therefore
// promises hook authors their panics are contained on BOTH queue
// implementations; a hook written to that promise must not be able to kill
// the worker either way. OnFailure returns nothing, so recovering here only
// prevents a process crash; there is no result to hand back, matching
// OnFailure's own "not retried or otherwise observed by the queue"
// contract (handler.go). The panic value and stack are recorded as an
// Error through the same structured logging convention jobs' own
// invokeOnFailure uses, so the hook bug is operator-visible.
func invokeOnFailure(ctx context.Context, hook jobs.FailureHook, job *jobs.Job, cause error, log *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("jobs: failure hook panicked",
				"job_id", string(job.ID), "job_type", job.Type,
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	hook.OnFailure(ctx, job, cause)
}

// processTask is the single asynqlib.HandlerFunc Queue.Start registers. It
// closes the tenant-context trap exactly like jobs' own worker.go's execute
// does for StandaloneQueue (pkgcore.WithTenant from the Job's own stored
// tenant -- here read back from Task.Headers, never from any ambient
// context), and implements per-tenant concurrency gating, progress
// reporting, and the StartedAt bookkeeping this package documents. See
// queue.go's Start for how this gets wired in place of an asynqlib.ServeMux.
func (q *Queue) processTask(ctx context.Context, t *asynqlib.Task) error {
	taskID, _ := asynqlib.GetTaskID(ctx)
	log := obs.FromContext(ctx)

	// Checked before anything else, including the handler lookup: a Job
	// cancelled while StatusPending/Scheduled/Retrying must never reach
	// Handle at all (jobs.Queue.Cancel's own doc comment). Queue.Cancel
	// deliberately leaves the task's own asynq record alone rather than
	// deleting it (see Cancel's doc comment in queue.go for why), so this
	// is the one place that actually enforces "never dispatched" --
	// returning nil here (never calling Handle) lets asynq record this
	// attempt as an ordinary successful completion; Get() still reports
	// StatusCancelled for it unconditionally, from the very same marker
	// this checks, regardless of what asynq's own state naturally becomes.
	// The extra Redis round trip this adds to every dispatched task is the
	// accepted cost of that correctness guarantee.
	//
	// This is the ONLY part of processTask that touches q.rdb -- kept in
	// its own function so the marker decision (dispatchAfterMarkerRead)
	// stays unit-testable against a bare *Queue with no Redis at all, the
	// same split handleError/handleErrorAttempt already uses for the same
	// reason, and processTaskUncancelled (everything else: handler lookup,
	// tenant header check, admission gate, progress/ResultWriter plumbing,
	// the Handle call itself) stays testable the way it already is; see
	// worker_test.go.
	cancelledAt, cerr := q.readCancelMarker(ctx, taskID)
	return q.dispatchAfterMarkerRead(ctx, t, taskID, log, cancelledAt, cerr)
}

// dispatchAfterMarkerRead is processTask's marker decision, factored out of
// the Redis read so every branch is unit-testable: an UNREADABLE marker
// refuses the run (fail closed -- the Job might be cancelled, so it must
// not execute; see errCancelMarkerUnreadable), a readable marker that says
// "cancelled" skips Handle, and only a readable "not cancelled" answer lets
// the attempt proceed to processTaskUncancelled.
func (q *Queue) dispatchAfterMarkerRead(ctx context.Context, t *asynqlib.Task, taskID string, log *slog.Logger, cancelledAt *time.Time, readErr error) error {
	if readErr != nil {
		// Fail closed: an unreadable cancellation state must never let a
		// possibly-cancelled Job execute. The refusal is bounce-class --
		// no retry budget consumed, short throttle delay -- so a transient
		// marker outage delays the attempt instead of losing it, and a
		// permanent one keeps the Job safely retryable until an operator
		// looks at Redis.
		log.Warn("jobs: cancellation marker unreadable; refusing to run", "job_id", taskID, "error", readErr)
		return errCancelMarkerUnreadable
	}
	if cancelledAt != nil {
		log.Info("job was cancelled before this attempt started; skipping Handle", "job_id", taskID, "job_type", t.Type())
		return nil
	}
	return q.processTaskUncancelled(ctx, t, taskID, log)
}

// processTaskUncancelled is processTask's core, everything after the
// cancellation-marker check above.
func (q *Queue) processTaskUncancelled(ctx context.Context, t *asynqlib.Task, taskID string, log *slog.Logger) error {
	// attemptStart measures the whole attempt from dispatch, exactly like
	// StandaloneQueue.execute's own attemptStart (jobs' worker.go), which
	// is taken before its handler lookup -- so the duration recorded with
	// a succeeded or failed attempt covers the same span on both
	// deployment modes: the unregistered-handler and missing-tenant
	// refusals below are attempts in the metrics' sense, exactly as they
	// are in StandaloneQueue's execute, and their durations are measured
	// from here. The success record (recordJobMetrics below) and every
	// wrapped failure (wrapFailedAttempt) share this one measurement.
	attemptStart := time.Now()
	h := q.handler(t.Type())
	if h == nil {
		// Treated exactly like any other Handle failure -- retried, then
		// dead-lettered -- mirroring jobs' own worker.go's identical
		// handling of ErrHandlerNotRegistered for StandaloneQueue. Reusing
		// the exact same sentinel (not a new asynq-specific one) keeps this
		// one error identical across both deployment modes. Wrapped with
		// this attempt's own measured duration so the outcome's metric
		// record (handleErrorAttempt's recordFailedAttempt) can carry it.
		return wrapFailedAttempt(jobs.ErrHandlerNotRegistered.WithParam("type", t.Type()), time.Since(attemptStart))
	}

	tenantID := pkgcore.TenantID(t.Headers()[headerTenantID])
	if tenantID == "" {
		return wrapFailedAttempt(
			errTaskMissingTenant.WithParam("type", t.Type()).WithParam("job_id", taskID),
			time.Since(attemptStart),
		)
	}

	if !q.tryReserveTenantSlot(tenantID) {
		log.Info("jobs dispatch: tenant at concurrency limit, bouncing for redelivery",
			"job_id", taskID, "job_type", t.Type(), "tenant_id", string(tenantID))
		return errTenantAtCapacity
	}
	defer q.releaseTenantSlot(tenantID)

	startedAt := time.Now()
	writeEnvelope := func(env resultEnvelope) {
		env.StartedAt = startedAt.UnixNano()
		data, err := encodeResultEnvelope(env)
		if err != nil {
			log.Warn("jobs: encoding progress envelope failed", "job_id", taskID, "error", err)
			return
		}
		if _, err := t.ResultWriter().Write(data); err != nil {
			log.Warn("jobs: persisting progress failed", "job_id", taskID, "error", err)
		}
	}
	// Marks this attempt's start immediately, before Handle runs -- read
	// back by Get() as StartedAt, exactly mirroring StandaloneQueue's
	// claimOne setting started_at at claim time. Progress starts at zero
	// for this attempt rather than carrying forward a previous attempt's
	// stale value -- a deliberate, low-stakes difference from
	// StandaloneQueue (which does carry it forward, simply because nothing
	// ever resets it).
	writeEnvelope(resultEnvelope{})

	retried, _ := asynqlib.GetRetryCount(ctx)
	maxRetry, _ := asynqlib.GetMaxRetry(ctx)
	job := &jobs.Job{
		ID:             jobs.JobID(taskID),
		Type:           t.Type(),
		TenantID:       tenantID,
		Payload:        t.Payload(),
		IdempotencyKey: t.Headers()[headerIdempotencyKey],
		Status:         jobs.StatusRunning,
		Priority:       priorityForQueue(mustQueueName(ctx)),
		Attempts:       retried + 1,
		MaxRetries:     maxRetry,
		ScheduledAt:    startedAt,
		CreatedAt:      headerCreatedAtTime(t.Headers()),
		UpdatedAt:      startedAt,
		StartedAt:      &startedAt,
	}

	var lastPct int
	var lastMsg string
	progress := func(pct int, msg string) {
		lastPct, lastMsg = pct, msg
		writeEnvelope(resultEnvelope{ProgressPct: pct, ProgressMsg: msg})
	}

	// handleCtx carries job.TenantID via pkgcore.WithTenant, rebuilt from
	// the Task's own stored header -- never inherited from whatever context
	// the Enqueue call happened to run in (see jobs.Queue's
	// Enqueue doc comment: that context is long gone by the time a worker
	// picks this up). It is built ON TOP of ctx (asynq's own per-task
	// context), not a fresh context.Background() the way StandaloneQueue's
	// jobContext is: adopting asynq's own ctx here rather than discarding
	// it is the correct choice, not a shortcut -- it is what
	// makes asynq's own Timeout/Deadline task options actually bound this
	// call, and what makes Inspector.CancelProcessing (queue.go's Cancel)
	// able to interrupt an in-flight attempt at all.
	handleCtx := pkgcore.WithTenant(ctx, tenantID)

	result, err := h.Handle(handleCtx, job, progress)
	duration := time.Since(attemptStart)
	durationMS := duration.Milliseconds()
	if err != nil {
		log.Warn("job attempt failed", "job_id", taskID, "job_type", t.Type(),
			"attempts", job.Attempts, "duration_ms", durationMS, "error", err)
		// asynq's own retry/archive machinery decides what happens next;
		// the error is wrapped with this attempt's measured duration so
		// handleErrorAttempt's outcome record (recordFailedAttempt) can
		// carry it onto the jobs.job.duration Histogram -- the same
		// duration_ms this log line already reports.
		return wrapFailedAttempt(err, duration)
	}

	writeEnvelope(resultEnvelope{ProgressPct: lastPct, ProgressMsg: lastMsg, Data: result.Data})
	log.Info("job succeeded", "job_id", taskID, "job_type", t.Type(),
		"attempts", job.Attempts, "duration_ms", durationMS)
	// The attempt concluded successfully here -- the mirror of
	// StandaloneQueue's own success record point (jobs' worker.go's
	// execute, "job succeeded" + recordJobMetrics), recorded with the
	// attempt's measured duration on the jobs.job.duration Histogram and
	// the StatusSucceeded row of the jobs.job.attempts Counter.
	q.recordJobMetrics(t.Type(), jobs.StatusSucceeded, duration)
	// The terminal signal's success point. One residual this position
	// carries, recorded here rather than silently: a Cancel that landed
	// while this attempt ran and whose interruption the handler ignored
	// (Handle returned nil anyway) publishes StatusSucceeded here -- before
	// asynq's own completion write -- while Get() reports StatusCancelled
	// from the marker, the same bounded cancel-racing-an-outcome residual
	// the success metric record above already carries, and the row-is-truth
	// clause of the consumer contract (jobs.EventJobTerminal) is what
	// covers it. No marker re-read is added for the event: it would cost a
	// Redis round trip on every success, and the cancellation remains
	// authoritative on every read-back either way.
	q.publishTerminal(ctx, tenantID, jobs.JobTerminalEvent{
		JobID:       jobs.JobID(taskID),
		JobType:     t.Type(),
		Status:      jobs.StatusSucceeded,
		Attempts:    job.Attempts,
		CompletedAt: time.Now(),
	})
	return nil
}

// mustQueueName reads the queue name asynqlib.GetQueueName stashed on ctx,
// falling back to queueDefault in the (never expected in practice) case it
// is absent -- processTask is only ever invoked by asynq's own processor,
// which always sets it (processor.go's asynqcontext.New).
func mustQueueName(ctx context.Context) string {
	if name, ok := asynqlib.GetQueueName(ctx); ok {
		return name
	}
	return queueDefault
}
