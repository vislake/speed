package queuetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// This file holds the fault tier's in-package acceptance side: the
// faultTierFake both mode variants of the tier's tests drive, and the tests
// proving the tier's checks ACCEPT a queue that fails closed — the mirror
// of eventbustest's accepts_buses_that_cannot_report_handler_errors_test.go,
// pinning that the tier asserts only the failure DIRECTION and never an
// error shape no real implementation could share (the fake's fail-closed
// errors are plain fmt.Errorf values carrying no identity, sentinel or
// wrapping; a check that demanded asynq's "jobs: read cancellation marker"
// framing or StandaloneQueue's gorm error would reject this conformant
// queue, exactly the over-assertion this file exists to catch).

// faultTierFake is an in-process jobs.Queue whose state model mirrors the
// shape in which the measured divergence actually happened: each Job has a
// main state record (the analogue of asynq's own TaskInfo, and of
// StandaloneQueue's single row) PLUS a separate cancellation marker (the
// analogue of asynq's Redis marker — for StandaloneQueue the marker and the
// record live in one row, but the tier's fake needs the two-source shape
// because it is the only shape in which a SWALLOWING implementation is
// expressible: a queue whose cancellation-state read fails while its main
// state read keeps working, and that answers from the main record anyway).
// Get and DeadLetterJobs overlay StatusCancelled from the marker exactly
// like asynq's reporting side does.
//
// The fake is deliberately broken in exactly one selectable dimension —
// what Get and DeadLetterJobs do when the cancellation-state read fails:
// in the default (fail-closed) mode they return the read error; in the
// swallow mode they log nothing and answer the Job's natural record state,
// the asynq behaviour this tier exists to reject. Everything else
// is a minimal faithful queue: Enqueue validates and resolves options
// through the real jobs helpers, Cancel writes only the marker (asynq's
// shape), a dispatcher goroutine runs eligible Jobs through their
// registered Handlers and settles a failed Job with no retries remaining
// into StatusDeadLetter, and the tenant access rule is jobs.CallerMayAccess
// itself. The fake exists for the tier's own tests only; the real
// implementations are measured by the legs' calls of
// AssertFailsClosedOnUnreadableCancellationState, never by this type.
type faultTierFake struct {
	mu sync.Mutex

	records   map[jobs.JobID]*faultFakeRecord
	cancelled map[jobs.JobID]time.Time
	handlers  map[string]jobs.Handler

	// swallow is the deliberate defect: when true, a failing
	// cancellation-state read is swallowed and the Job's natural record
	// state reported, asynq's pre-c26b058b shape.
	swallow bool

	// fault is the injector state: when true, every cancellation-state
	// read fails, exactly as if the backend read itself answered an error.
	fault bool

	started   bool
	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	seq       int
}

// faultFakeRecord is one Job's main state record: the fake's analogue of
// asynq's TaskInfo (and of StandaloneQueue's row minus the columns the fake
// never needs).
type faultFakeRecord struct {
	id          jobs.JobID
	jobType     string
	tenant      pkgcore.TenantID
	payload     []byte
	status      jobs.Status
	attempts    int
	maxRetries  int
	scheduledAt time.Time
	errMsg      string
}

// newFaultTierFake builds a fake with the given swallow setting.
func newFaultTierFake(swallow bool) *faultTierFake {
	return &faultTierFake{
		records:   make(map[jobs.JobID]*faultFakeRecord),
		cancelled: make(map[jobs.JobID]time.Time),
		handlers:  make(map[string]jobs.Handler),
		swallow:   swallow,
		stopCh:    make(chan struct{}),
	}
}

// errFaultTierSimulatedRead is what a sabotaged cancellation-state read
// answers on the fake. The swallow mode drops it without a trace; the
// fail-closed mode returns it (wrapped) — plain errors on purpose, so the
// acceptance tests below pin that the tier never asserts error identity.
var errFaultTierSimulatedRead = errors.New("fault-tier fake: simulated cancellation-state read failure (WRONGTYPE analogue)")

// RegisterHandler implements queuetest.Runnable.
func (q *faultTierFake) RegisterHandler(h jobs.Handler) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.handlers[h.Type()]; exists {
		return jobs.ErrDuplicateHandlerType
	}
	q.handlers[h.Type()] = h
	return nil
}

// Start implements queuetest.Runnable.
func (q *faultTierFake) Start(context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.started {
		return nil
	}
	q.started = true
	q.wg.Add(1)
	go q.run()
	return nil
}

// Close implements queuetest.Runnable. Idempotent via closeOnce: the
// tier's tests close a fake both through t.Cleanup and through a check that
// returned early, and a second close must be as harmless here as it is on
// the real implementations.
func (q *faultTierFake) Close(context.Context) error {
	q.closeOnce.Do(func() { close(q.stopCh) })
	q.wg.Wait()
	return nil
}

// Enqueue implements jobs.Queue.
func (q *faultTierFake) Enqueue(ctx context.Context, task jobs.Task, opts ...jobs.EnqueueOption) (jobs.JobID, error) {
	if err := task.Validate(); err != nil {
		return "", err
	}
	now := time.Now()
	resolved := jobs.ResolveEnqueueOptions(now, jobs.DefaultTimeout, opts)

	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	id := jobs.JobID(fmt.Sprintf("fault-tier-fake-%d", q.seq))
	q.records[id] = &faultFakeRecord{
		id:          id,
		jobType:     task.Type,
		tenant:      task.TenantID,
		payload:     task.Payload,
		status:      jobs.StatusPending,
		maxRetries:  resolved.MaxRetries,
		scheduledAt: resolved.ScheduledAt,
	}
	return id, nil
}

// Get implements jobs.Queue: the main record read first (which keeps
// working under the fault — the analogue of asynq's own TaskInfo read),
// then the cancellation-state read, whose failure the mode decides.
func (q *faultTierFake) Get(ctx context.Context, id jobs.JobID) (*jobs.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	rec, ok := q.records[id]
	if !ok {
		return nil, jobs.ErrJobNotFound
	}
	if !jobs.CallerMayAccess(ctx, rec.tenant) {
		return nil, jobs.ErrJobNotFound
	}
	cancelledAt, err := q.readCancellationLocked(id)
	if err != nil {
		if q.swallow {
			// The swallow-mode defect shape: the read failure is dropped and
			// the Job answered from its natural record state as if the read
			// had answered "no marker".
			return jobFromFaultRecord(rec, nil), nil
		}
		return nil, fmt.Errorf("fault-tier fake: Get: %w", err)
	}
	return jobFromFaultRecord(rec, cancelledAt), nil
}

// Cancel implements jobs.Queue, asynq's shape: the marker is the only
// authoritative effect; the main record is never touched.
func (q *faultTierFake) Cancel(ctx context.Context, id jobs.JobID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	rec, ok := q.records[id]
	if !ok {
		return jobs.ErrJobNotFound
	}
	if !jobs.CallerMayAccess(ctx, rec.tenant) {
		return jobs.ErrJobNotFound
	}
	if _, exists := q.cancelled[id]; exists {
		return nil // idempotent: already cancelled.
	}
	q.cancelled[id] = time.Now()
	return nil
}

// DeadLetterJobs implements queuetest.Runnable: every StatusDeadLetter
// record ctx may access, each one's cancellation-state read performed per
// listed Job exactly like asynq's listing does — so a failing read fails
// the whole listing in the fail-closed mode.
func (q *faultTierFake) DeadLetterJobs(ctx context.Context) ([]*jobs.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	result := make([]*jobs.Job, 0, len(q.records))
	for id, rec := range q.records {
		if rec.status != jobs.StatusDeadLetter {
			continue
		}
		if !jobs.CallerMayAccess(ctx, rec.tenant) {
			continue
		}
		cancelledAt, err := q.readCancellationLocked(id)
		if err != nil {
			if q.swallow {
				// Same defect as Get: the read failure is dropped and the
				// Job listed from its natural record state.
				result = append(result, jobFromFaultRecord(rec, nil))
				continue
			}
			return nil, fmt.Errorf("fault-tier fake: DeadLetterJobs: %w", err)
		}
		result = append(result, jobFromFaultRecord(rec, cancelledAt))
	}
	return result, nil
}

// readCancellationLocked performs the fake's cancellation-state read: the
// read every reporting surface answers from. Under the fault it fails;
// otherwise it reports the marker, if one exists. Callers hold q.mu.
func (q *faultTierFake) readCancellationLocked(id jobs.JobID) (*time.Time, error) {
	if q.fault {
		return nil, errFaultTierSimulatedRead
	}
	if t, ok := q.cancelled[id]; ok {
		return &t, nil
	}
	return nil, nil
}

// Sabotage implements queuetest.CancellationStateFault: from now until
// Repair, every cancellation-state read fails. The fake has no per-Job
// second source to break — the read itself is the injection point — so the
// fault is global and id is unused.
func (q *faultTierFake) Sabotage(context.Context, jobs.JobID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.fault = true
	return nil
}

// Repair implements queuetest.CancellationStateFault.
func (q *faultTierFake) Repair(context.Context, jobs.JobID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.fault = false
	return nil
}

// run is the dispatcher goroutine: every 2ms it claims the oldest eligible
// Pending Job with a registered Handler and runs one attempt. The tier's
// checks never make a Job eligible while a fault is on (the Get check's Job
// is delayed an hour; the DeadLetter check's Job has already settled), so
// the dispatcher needs no fault awareness of its own.
func (q *faultTierFake) run() {
	defer q.wg.Done()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-q.stopCh:
			return
		case now := <-ticker.C:
			q.dispatchEligible(now)
		}
	}
}

// dispatchEligible runs one eligible Job, if any: claim (under the lock),
// Handle outside the lock on a tenant-rebuilt context, then settle the
// outcome — StatusSucceeded on success, StatusDeadLetter once attempts
// exceed maxRetries, otherwise StatusRetrying parked a safe hour out.
func (q *faultTierFake) dispatchEligible(now time.Time) {
	q.mu.Lock()
	var (
		id  jobs.JobID
		rec *faultFakeRecord
		h   jobs.Handler
	)
	for rid, r := range q.records {
		if r.status == jobs.StatusPending && !r.scheduledAt.After(now) {
			if handler, ok := q.handlers[r.jobType]; ok {
				id, rec, h = rid, r, handler
				break
			}
		}
	}
	if id == "" {
		q.mu.Unlock()
		return
	}
	rec.attempts++
	rec.status = jobs.StatusRunning
	q.mu.Unlock()

	job := jobFromFaultRecord(rec, nil)
	result, err := h.Handle(pkgcore.WithTenant(context.Background(), rec.tenant), job, func(int, string) {})

	q.mu.Lock()
	defer q.mu.Unlock()
	if err != nil {
		if rec.attempts > rec.maxRetries {
			rec.status = jobs.StatusDeadLetter
		} else {
			rec.status = jobs.StatusRetrying
			// No tier scenario retries; parking the retry a safe hour out
			// keeps the dispatcher from ever spinning on it.
			rec.scheduledAt = time.Now().Add(time.Hour)
		}
		rec.errMsg = err.Error()
		return
	}
	rec.status = jobs.StatusSucceeded
	rec.errMsg = ""
	_ = result
}

// jobFromFaultRecord overlays the cancellation marker onto a record the way
// queue/asynq's jobFromTaskInfo overlays it onto a TaskInfo: a present
// marker answers StatusCancelled unconditionally, its absence leaves the
// record's natural status.
func jobFromFaultRecord(rec *faultFakeRecord, cancelledAt *time.Time) *jobs.Job {
	job := &jobs.Job{
		ID:          rec.id,
		Type:        rec.jobType,
		TenantID:    rec.tenant,
		Payload:     rec.payload,
		Status:      rec.status,
		Attempts:    rec.attempts,
		MaxRetries:  rec.maxRetries,
		ScheduledAt: rec.scheduledAt,
		Error:       rec.errMsg,
	}
	if cancelledAt != nil {
		job.Status = jobs.StatusCancelled
	}
	return job
}

// TestAssertFailsClosedOnUnreadableCancellationState_PassesAgainstAFailClosedFake
// proves the tier passes end to end, inside this package's own unit run,
// against the fail-closed fake — the same role eventbustest's
// TestAssertConforms_MemoryEventBus plays for its suite. The fake's
// fail-closed errors are plain wrapped values with no identity: this run is
// also the pin that the tier never over-asserts an error shape — a future
// tightening that demanded asynq's specific framing or StandaloneQueue's
// gorm error would go red right here.
func TestAssertFailsClosedOnUnreadableCancellationState_PassesAgainstAFailClosedFake(t *testing.T) {
	AssertFailsClosedOnUnreadableCancellationState(t, func() FaultRunnable {
		return newFaultTierFake(false)
	})
}

// TestGetCheck_AcceptsAFailClosedQueue pins the acceptance side of the Get
// check directly: a queue that returns its (plain) read error and answers
// StatusCancelled again after repair passes.
func TestGetCheck_AcceptsAFailClosedQueue(t *testing.T) {
	if err := checkGetFailsClosedOnUnreadableCancellationState(newFaultTierFake(false)); err != nil {
		t.Errorf("the Get check rejected a queue that fails closed: %v", err)
	}
}

// TestDeadLetterCheck_AcceptsAFailClosedQueue pins the acceptance side of
// the DeadLetterJobs check directly.
func TestDeadLetterCheck_AcceptsAFailClosedQueue(t *testing.T) {
	if err := checkDeadLetterListingFailsClosedOnUnreadableCancellationState(newFaultTierFake(false)); err != nil {
		t.Errorf("the DeadLetterJobs check rejected a queue that fails closed: %v", err)
	}
}

// compile-time checks: the fake satisfies everything the tier drives it as.
var (
	_ FaultRunnable = (*faultTierFake)(nil)
	_ jobs.Queue    = (*faultTierFake)(nil)
)
