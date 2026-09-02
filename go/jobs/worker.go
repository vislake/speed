package jobs

import (
	"context"
	"time"

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
// DemoQueue.DeadLetterJobs.
var ErrHandlerNotRegistered = apperr.Internal("jobs.handler_not_registered")

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
// SQLite — and NOT derived from DemoQueue's own internal dispatcher/worker
// -loop lifecycle context either, so that closing the queue does not
// abruptly cancel a Handle call already in flight (see DemoQueue.Close's
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
func (q *DemoQueue) runDispatcher(dispatch chan<- jobRecord) {
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
func (q *DemoQueue) dispatchOnce(dispatch chan<- jobRecord) {
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
func (q *DemoQueue) tryReserveTenantSlot(tenant pkgcore.TenantID) bool {
	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	if q.runningPerTenant[tenant] >= q.tenantConcurrency {
		return false
	}
	q.runningPerTenant[tenant]++
	return true
}

// releaseTenantSlot releases a slot reserved by tryReserveTenantSlot.
func (q *DemoQueue) releaseTenantSlot(tenant pkgcore.TenantID) {
	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	q.runningPerTenant[tenant]--
	if q.runningPerTenant[tenant] <= 0 {
		delete(q.runningPerTenant, tenant)
	}
}

// runWorker receives claimed Jobs from dispatch and executes them one at a
// time until dispatch is closed or q.stopCh fires.
func (q *DemoQueue) runWorker(dispatch <-chan jobRecord) {
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

// execute runs exactly one Handle attempt for rec (already StatusRunning
// in the database) and persists its outcome. Every persistence write in
// this method uses a context.Background()-rooted context, deliberately
// independent of handleCtx: a bookkeeping write (recording success,
// retry or dead-letter) must not itself fail merely because the Job's own
// per-attempt timeout happened to elapse at the exact moment Handle
// returned.
func (q *DemoQueue) execute(rec jobRecord) {
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
	if handler == nil {
		err = ErrHandlerNotRegistered.WithParam("type", rec.Type)
	} else {
		progress := func(pct int, msg string) {
			if perr := updateProgress(handleCtx, q.db, rec.ID, pct, msg); perr != nil {
				log.Warn("jobs: persisting progress failed", "job_id", rec.ID, "error", perr)
			}
		}
		result, err = handler.Handle(handleCtx, job, progress)
	}
	durationMS := time.Since(attemptStart).Milliseconds()

	now := time.Now()
	bg := context.Background()
	if err == nil {
		log.Info("job succeeded", "job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS)
		if werr := completeSucceeded(bg, q.db, rec.ID, result, now); werr != nil {
			log.Error("jobs: persisting success failed", "job_id", rec.ID, "error", werr)
		}
		return
	}

	if rec.Attempts > rec.MaxRetries {
		log.Error("job exhausted retries, moving to dead letter",
			"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS, "error", err)
		if werr := completeDeadLetter(bg, q.db, rec.ID, err.Error(), now); werr != nil {
			log.Error("jobs: persisting dead letter failed", "job_id", rec.ID, "error", werr)
			return
		}
		if hook, ok := handler.(FailureHook); ok {
			job.Status = StatusDeadLetter
			job.Error = err.Error()
			hookCtx, hookCancel := context.WithTimeout(jobContext(tenant), timeout)
			hook.OnFailure(hookCtx, job, err)
			hookCancel()
		}
		return
	}

	delay := q.backoffDelay(rec.Attempts)
	log.Warn("job attempt failed, scheduling retry",
		"job_id", rec.ID, "job_type", rec.Type, "attempts", rec.Attempts, "duration_ms", durationMS,
		"retry_in_ms", delay.Milliseconds(), "error", err)
	if werr := completeRetrying(bg, q.db, rec.ID, err.Error(), now.Add(delay), now); werr != nil {
		log.Error("jobs: persisting retry failed", "job_id", rec.ID, "error", werr)
	}
}

// backoffDelay computes the exponential backoff before the next attempt,
// given that the attempts-th one has just failed: q.backoffBase *
// 2^(attempts-1), capped at q.backoffMax. attempts below 1 is treated as 1.
func (q *DemoQueue) backoffDelay(attempts int) time.Duration {
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
