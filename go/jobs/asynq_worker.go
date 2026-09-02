package jobs

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/hibiken/asynq"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// errAsynqTaskMissingTenant is processTask's defensive response to a task
// whose Headers carry no tenant_id at all. This should never happen for a
// task this package's own Enqueue created (buildTaskHeaders always sets it,
// and Task.validate rejects an empty TenantID before Enqueue ever reaches
// asynq's client) -- the only realistic cause is a non-jobs producer having
// written directly into the same Redis instance/queues. Treated like any
// other Handle failure (retried, then dead-lettered) rather than panicking
// the worker, exactly per this package's general "fail the attempt, not the
// process" discipline for malformed input (compare ErrHandlerNotRegistered).
// Deliberately its own sentinel rather than a reuse of
// ErrHandlerNotRegistered: the two causes are operationally distinct
// (register the missing Handler vs. find and fix whatever enqueued a
// tenant-less task), and collapsing them would send an operator chasing the
// wrong fix.
var errAsynqTaskMissingTenant = apperr.Internal("jobs.asynq_task_missing_tenant")

// This file is AsynqQueue's counterpart to worker.go: where worker.go's
// jobContext/execute close "the tenant context trap" and decide
// retry/backoff/dead-letter for DemoQueue, processTask (below) does the same
// job for the distributed deployment mode -- registered as the single asynq.
// HandlerFunc AsynqQueue.Start hands to asynq.Server.Start, deliberately
// NOT wrapped in an asynq.ServeMux (see its own doc comment for why).

// errTenantAtCapacity is returned by processTask, never by a jobs.Handler,
// when a Job's tenant is already at its concurrency limit. It is not a
// business failure: AsynqQueue's Config.IsFailure (asynq_queue.go) reports
// false for it, so asynq's own retry bookkeeping (internal/rdb's Retry Lua
// script: "if isFailure then Retried++") never increments Retried for a
// bounce caused by this error -- a Job can be bounced any number of times by
// its tenant being over capacity without ever burning into MaxRetries. See
// AGENTS.md's "Per-tenant concurrency limiting" section for the full
// reasoning, including the one narrow edge case this does not cover (a
// bounce landing exactly on what would otherwise be a Job's final allowed
// attempt).
var errTenantAtCapacity = errors.New("jobs: tenant is at its concurrency limit")

// tryReserveTenantSlot and releaseTenantSlot are AsynqQueue's own admission
// gate -- structurally the same map+mutex shape as DemoQueue's
// tryReserveTenantSlot/releaseTenantSlot (worker.go), but deliberately a
// separate, independent copy rather than a shared helper type: extracting
// one would mean touching DemoQueue's own fields/methods, and this task's
// own instructions require the demo implementation and its tests to come
// out completely unaffected. See AGENTS.md for why WHERE this gate applies
// differs from DemoQueue's (a fast bounce-and-redeliver inside the Handler
// call, not a pre-dequeue skip -- asynq gives us no way to peek at a task's
// tenant before dequeuing it off Redis).
func (q *AsynqQueue) tryReserveTenantSlot(tenant pkgcore.TenantID) bool {
	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	if q.runningPerTenant[tenant] >= q.tenantConcurrency {
		return false
	}
	q.runningPerTenant[tenant]++
	return true
}

func (q *AsynqQueue) releaseTenantSlot(tenant pkgcore.TenantID) {
	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	q.runningPerTenant[tenant]--
	if q.runningPerTenant[tenant] <= 0 {
		delete(q.runningPerTenant, tenant)
	}
}

// retryDelay is AsynqQueue's Config.RetryDelayFunc. It special-cases
// errTenantAtCapacity with a short, jittered, NON-exponential delay
// (independent of n, which counts genuine business failures only -- see
// errTenantAtCapacity's own doc comment for why a throttle bounce never
// advances it) and defers to q.businessRetryDelayFunc (asynq.
// DefaultRetryDelayFunc unless overridden by WithAsynqRetryDelayFunc) for
// every real Handler failure, exactly matching DemoQueue's own backoffDelay
// role but implemented on top of asynq's own extension point instead of a
// hand-rolled formula.
func (q *AsynqQueue) retryDelay(n int, err error, t *asynq.Task) time.Duration {
	if errors.Is(err, errTenantAtCapacity) {
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

// isFailure is AsynqQueue's Config.IsFailure. See errTenantAtCapacity's own
// doc comment: reporting false here is what keeps a throttle bounce from
// consuming retry budget.
func isFailure(err error) bool {
	return err != nil && !errors.Is(err, errTenantAtCapacity)
}

// handleError is AsynqQueue's Config.ErrorHandler. asynq invokes it exactly
// once per failed attempt (processor.go's handleFailedMessage), BEFORE
// deciding whether that attempt retries or archives -- which is the only
// hook point asynq offers for "this Job is about to dead-letter" (there is
// no separate archived/on-archive callback). It replicates asynq's own
// archive-vs-retry boundary (msg.Retried >= msg.Retry, processor.go) using
// the same values a Handler itself could read via asynq.GetRetryCount/
// GetMaxRetry, purely to decide whether THIS is the terminal attempt; it
// schedules or persists nothing itself, so this is reading asynq's
// documented MaxRetry contract, not reimplementing its retry machinery. See
// AGENTS.md's dead-letter mapping section.
func (q *AsynqQueue) handleError(ctx context.Context, t *asynq.Task, err error) {
	retried, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)
	taskID, _ := asynq.GetTaskID(ctx)
	q.handleErrorAttempt(t, err, retried, maxRetry, taskID)
}

// handleErrorAttempt is handleError's ctx-free core: everything handleError
// needs from ctx (retried, maxRetry, taskID -- asynq.GetRetryCount/
// GetMaxRetry/GetTaskID, all backed by an internal, package-private context
// key this package cannot fabricate on its own) is passed in explicitly
// instead, so this logic -- the archive-boundary replication and the
// FailureHook invocation itself -- is unit-testable without a real asynq
// server ever having dequeued anything; see asynq_worker_test.go. Only
// handleError's own three GetXxx(ctx) calls are, correctly, untested at the
// unit level: they are asynq's own accessors, not this package's logic.
func (q *AsynqQueue) handleErrorAttempt(t *asynq.Task, err error, retried, maxRetry int, taskID string) {
	if errors.Is(err, errTenantAtCapacity) {
		return // a bounce is never itself a dead-letter-worthy event.
	}
	if retried < maxRetry {
		return // more retries remain; asynq will retry, not archive.
	}

	h := q.handler(t.Type())
	hook, ok := h.(FailureHook)
	if !ok {
		return
	}

	tenantID := pkgcore.TenantID(t.Headers()[headerTenantID])
	job := &Job{
		ID:             JobID(taskID),
		Type:           t.Type(),
		TenantID:       tenantID,
		Payload:        t.Payload(),
		IdempotencyKey: t.Headers()[headerIdempotencyKey],
		Status:         StatusDeadLetter,
		Attempts:       retried + 1,
		MaxRetries:     maxRetry,
		Error:          err.Error(),
		CreatedAt:      headerCreatedAtTime(t.Headers()),
	}

	// A fresh context, not ctx (which belongs to the attempt that just
	// failed and may already be at or past its own deadline) -- exactly
	// DemoQueue.execute's own choice for the same call, and for the same
	// reason: OnFailure runs real business compensation (refunding
	// credits, e.g.) that must not inherit a context already on its way
	// out. q.defaultTimeout approximates the per-job timeout DemoQueue
	// uses here exactly; asynq's context accessors expose TaskID/
	// RetryCount/MaxRetry/QueueName but not a task's own Timeout/Deadline
	// (context.go), so recovering the exact value would cost an extra
	// Inspector round trip purely for this bound -- not worth it for a
	// hook whose own timeout is already a documented approximation.
	hookCtx, cancel := context.WithTimeout(pkgcore.WithTenant(context.Background(), tenantID), q.defaultTimeout)
	defer cancel()
	hook.OnFailure(hookCtx, job, err)
}

// processTask is the single asynq.HandlerFunc AsynqQueue.Start registers.
// It closes the tenant-context trap exactly like worker.go's execute does
// for DemoQueue (pkgcore.WithTenant from the Job's own stored tenant --
// here read back from Task.Headers, never from any ambient context), and
// implements per-tenant concurrency gating, progress reporting, and the
// StartedAt bookkeeping AGENTS.md documents. See asynq_queue.go's Start for
// how this gets wired in place of an asynq.ServeMux.
func (q *AsynqQueue) processTask(ctx context.Context, t *asynq.Task) error {
	taskID, _ := asynq.GetTaskID(ctx)
	log := obs.FromContext(ctx)

	// Checked before anything else, including the handler lookup: a Job
	// cancelled while StatusPending/Scheduled/Retrying must never reach
	// Handle at all (Queue.Cancel's own doc comment). AsynqQueue.Cancel
	// deliberately leaves the task's own asynq record alone rather than
	// deleting it (see Cancel's doc comment in asynq_queue.go for why),
	// so this is the one place that actually enforces "never dispatched"
	// -- returning nil here (never calling Handle) lets asynq record this
	// attempt as an ordinary successful completion; Get() still reports
	// StatusCancelled for it unconditionally, from the very same marker
	// this checks, regardless of what asynq's own state naturally becomes.
	// The extra Redis round trip this adds to every dispatched task is the
	// accepted cost of that correctness guarantee.
	//
	// This is the ONLY part of processTask that touches q.rdb -- split out
	// so processTaskUncancelled (everything else: handler lookup, tenant
	// header check, admission gate, progress/ResultWriter plumbing, the
	// Handle call itself) stays unit-testable against a bare *AsynqQueue
	// with no Redis at all; see asynq_worker_test.go. The same split
	// handleError/handleErrorAttempt already uses for the same reason.
	if cancelledAt, cerr := q.readCancelMarker(ctx, taskID); cerr != nil {
		log.Warn("jobs: reading cancellation marker failed", "job_id", taskID, "error", cerr)
	} else if cancelledAt != nil {
		log.Info("job was cancelled before this attempt started; skipping Handle", "job_id", taskID, "job_type", t.Type())
		return nil
	}

	return q.processTaskUncancelled(ctx, t, taskID, log)
}

// processTaskUncancelled is processTask's core, everything after the
// cancellation-marker check above.
func (q *AsynqQueue) processTaskUncancelled(ctx context.Context, t *asynq.Task, taskID string, log *slog.Logger) error {
	h := q.handler(t.Type())
	if h == nil {
		// Treated exactly like any other Handle failure -- retried, then
		// dead-lettered -- mirroring worker.go's identical handling of
		// ErrHandlerNotRegistered for DemoQueue. Reusing the exact same
		// sentinel (not a new asynq-specific one) keeps this one error
		// identical across both deployment modes.
		return ErrHandlerNotRegistered.WithParam("type", t.Type())
	}

	tenantID := pkgcore.TenantID(t.Headers()[headerTenantID])
	if tenantID == "" {
		return errAsynqTaskMissingTenant.WithParam("type", t.Type()).WithParam("job_id", taskID)
	}

	if !q.tryReserveTenantSlot(tenantID) {
		log.Info("jobs dispatch: tenant at concurrency limit, bouncing for redelivery",
			"job_id", taskID, "job_type", t.Type(), "tenant_id", string(tenantID))
		return errTenantAtCapacity
	}
	defer q.releaseTenantSlot(tenantID)

	startedAt := time.Now()
	writeEnvelope := func(env asynqResultEnvelope) {
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
	// back by Get() as StartedAt, exactly mirroring DemoQueue's claimOne
	// setting started_at at claim time. Progress starts at zero for this
	// attempt rather than carrying forward a previous attempt's stale
	// value; see AGENTS.md for why this is a deliberate, documented,
	// low-stakes difference from DemoQueue (which does carry it forward,
	// simply because nothing ever resets it).
	writeEnvelope(asynqResultEnvelope{})

	retried, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)
	job := &Job{
		ID:             JobID(taskID),
		Type:           t.Type(),
		TenantID:       tenantID,
		Payload:        t.Payload(),
		IdempotencyKey: t.Headers()[headerIdempotencyKey],
		Status:         StatusRunning,
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
		writeEnvelope(asynqResultEnvelope{ProgressPct: pct, ProgressMsg: msg})
	}

	// handleCtx carries job.TenantID via pkgcore.WithTenant, rebuilt from
	// the Task's own stored header -- never inherited from whatever context
	// the original Enqueue call happened to run in (see queue.go's Enqueue
	// doc comment: that context is long gone by the time a worker picks
	// this up). It is built ON TOP of ctx (asynq's own per-task context),
	// not a fresh context.Background() the way DemoQueue's jobContext is --
	// see AGENTS.md's "The tenant context trap, asynq edition" for why
	// adopting asynq's own ctx here (rather than discarding it) is the
	// correct choice, not a shortcut: it is what makes asynq's own Timeout/
	// Deadline task options actually bound this call, and what makes
	// Inspector.CancelProcessing (asynq_queue.go's Cancel) able to
	// interrupt an in-flight attempt at all.
	handleCtx := pkgcore.WithTenant(ctx, tenantID)

	attemptStart := time.Now()
	result, err := h.Handle(handleCtx, job, progress)
	durationMS := time.Since(attemptStart).Milliseconds()
	if err != nil {
		log.Warn("job attempt failed", "job_id", taskID, "job_type", t.Type(),
			"attempts", job.Attempts, "duration_ms", durationMS, "error", err)
		return err // asynq's own retry/archive machinery decides what happens next.
	}

	writeEnvelope(asynqResultEnvelope{ProgressPct: lastPct, ProgressMsg: lastMsg, Data: result.Data})
	log.Info("job succeeded", "job_id", taskID, "job_type", t.Type(),
		"attempts", job.Attempts, "duration_ms", durationMS)
	return nil
}

// mustQueueName reads the queue name asynq.GetQueueName stashed on ctx,
// falling back to asynqQueueDefault in the (never expected in practice)
// case it is absent -- processTask is only ever invoked by asynq's own
// processor, which always sets it (processor.go's asynqcontext.New).
func mustQueueName(ctx context.Context) string {
	if name, ok := asynq.GetQueueName(ctx); ok {
		return name
	}
	return asynqQueueDefault
}
