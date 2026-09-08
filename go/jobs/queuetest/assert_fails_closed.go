package queuetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is queuetest's injectable-fault tier: the counterpart, for the
// jobs.Queue seam, of the panic injections go/pkgcore/eventbustest's own
// suite performs. A contract suite that only ever drives healthy backends is
// structurally blind to how an implementation behaves when a read it makes
// fails — and failure-direction divergence between implementations of one
// seam is exactly the class such a suite cannot see, no matter how complete
// its healthy-path checks are. The jobs seam has a proven instance: commit
// c26b058b (fix(jobs): fail closed when Get's cancellation-marker read
// fails) corrected go/jobs/queue/asynq's Queue, whose Get and DeadLetterJobs
// swallowed a failure of the cancellation-marker read and reported the
// Job's natural asynq state — a cancelled Job whose skipped run asynq
// recorded as Completed read back as StatusSucceeded with an empty Result,
// the answer the documented ai-gateway poll pattern fails to unmarshal —
// while StandaloneQueue, whose every state read is the one row read with no
// second source to swallow through, failed closed all along. The
// healthy-path AssertConforms suite above ran fully green through that
// divergence: nothing in it can make an underlying state read fail, so the
// failure direction was unmeasured until a real-world report surfaced it.
//
// This tier closes that blind spot the way eventbustest's injections close
// the panic-isolation one: AssertFailsClosedOnUnreadableCancellationState
// runs the queue under a REAL injected failure of its own cancellation-state
// read — the read whose answer decides whether a Job reports as cancelled —
// and asserts the reporting surfaces fail closed, then answer correctly
// again once the read works. Each leg supplies the injection through its
// factory, sabotaging and repairing the real backend read its own
// implementation makes (go/jobs's standalone leg renames the jobs table out
// from under the queue; go/jobs/integration_test's asynq leg corrupts the
// Redis cancellation marker into a LIST, WRONGTYPE on every read), so what
// the tier measures is the implementations' genuine behaviour under a real
// read failure, never a simulation inside this package. StandaloneQueue's
// already-established fail-closed direction is the baseline both checks
// verify; asynq's post-c26b058b direction is what they measure.
//
// # The three-part shape, after eventbustest
//
// The tier follows eventbustest's own teeth discipline: the check functions
// below return errors instead of failing a test directly, so this package's
// own tests can drive them against a deliberately defective implementation
// — a queue that SWALLOWS the injected read failure and reports its jobs'
// natural state, the exact historical asynq shape — and require rejection.
// A rejection tier nobody has proven can fail is a constant-true harness
// (a passing test that cannot fail does not count), which is why
// assert_fails_closed_rejects_swallowing_queues_test.go exists; and
// assert_fails_closed_test.go runs the same checks against a fail-closed
// fake whose errors carry no identity or wrapping, pinning that the tier
// asserts only the direction, never an error shape no real implementation
// could share.
//
// # What the tier does NOT assert
//
// The checks cover the two REPORTING surfaces of the c26b058b class: Get on
// a possibly-cancelled Job, and a DeadLetterJobs listing. They deliberately
// do not assert about Cancel (not a reporting surface — asynq's marker read
// there is an idempotency short-circuit whose failure leaves the marker
// WRITE to answer), and they do not assert the dispatch-refusal direction,
// which is not a reporting surface either and is covered per
// implementation: asynq's dispatchAfterMarkerRead refusal against real
// Redis sabotage in go/jobs/integration_test/marker_read_fail_closed_test.go,
// StandaloneQueue's by construction (its claim is a single state read whose
// failure means no claim — a job whose cancellation state cannot be read
// never executes). They assert error PRESENCE only, never the error's
// identity or wrapping: each implementation returns its own plain wrapped
// read error, and a check that demanded one implementation's specific
// framing would reject the other.

// CancellationStateFault is the fault-injection handle the fault tier's
// factory must supply alongside queuetest.Runnable: Sabotage and Repair of
// the queue's own cancellation-state read, performed for real on the
// backend the queue reads through. What exactly "the cancellation-state
// read" is differs per implementation — asynq's Queue overlays
// StatusCancelled from a separate Redis marker whose read can fail while
// the rest of Redis keeps working; StandaloneQueue's every state read is
// the one jobs-table row read — so each leg implements this interface
// against its own backend, and the tier itself never touches one.
type CancellationStateFault interface {
	// Sabotage makes the Runnable's cancellation-state read for id fail, for
	// real, on the backend the Runnable reads through: the read that was
	// healthy a moment ago must answer an error from now until Repair. The
	// rest of the queue's machinery (dispatch, other jobs' reads) may
	// legitimately keep working — the fault class this tier measures is a
	// state read failing while the backend keeps running, never the whole
	// backend going away.
	Sabotage(ctx context.Context, id jobs.JobID) error

	// Repair undoes a prior Sabotage: the cancellation-state read for id
	// answers normally again, with the state it held before the sabotage —
	// a transient outage delays reports, it does not lose the cancellation.
	Repair(ctx context.Context, id jobs.JobID) error
}

// FaultRunnable is what the fault tier's factory returns: a Runnable whose
// cancellation-state read the checks can break and fix through
// CancellationStateFault. Each leg's factory returns its own test-side
// adapter wrapping the concrete queue plus the handle that reaches its
// backend (see each leg's own test file) — queuetest itself never touches
// a backend, exactly like AssertConforms never does.
type FaultRunnable interface {
	Runnable
	CancellationStateFault
}

// AssertFailsClosedOnUnreadableCancellationState verifies that the
// FaultRunnable the factory returns fails closed on its reporting surfaces
// while its cancellation-state read is broken: a Job whose cancellation
// state cannot be read is a possibly-cancelled Job, and neither Get nor
// DeadLetterJobs may report it from its natural state — a possibly
// cancelled Job reported as succeeded (or retrying, or dead-lettered, or
// pending) is a caller acting on state that cannot be true. Each leg
// supplies the real injection itself (see CancellationStateFault and the
// file header); the checks' own doc comments describe the scenario each one
// drives.
//
// AssertFailsClosedOnUnreadableCancellationState calls factory once per
// checked property (t.Run subtest), exactly like AssertConforms, and never
// assumes state left by an earlier subtest is visible in the next: each
// subtest enqueues jobs under job types unique to that subtest, so subtests
// sharing one long-lived backend (asynq's own integration leg runs one
// Redis container per test file, not per case) never collide.
func AssertFailsClosedOnUnreadableCancellationState(t *testing.T, factory func() FaultRunnable) {
	t.Helper()

	t.Run("get_on_a_possibly_cancelled_job_errors_while_the_cancellation_state_cannot_be_read", func(t *testing.T) {
		t.Helper()
		fc := factory()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = fc.Close(ctx)
		})
		if err := checkGetFailsClosedOnUnreadableCancellationState(fc); err != nil {
			t.Errorf("%v", err)
		}
	})

	t.Run("dead_letter_listing_errors_while_a_listed_jobs_cancellation_state_cannot_be_read", func(t *testing.T) {
		t.Helper()
		fc := factory()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = fc.Close(ctx)
		})
		if err := checkDeadLetterListingFailsClosedOnUnreadableCancellationState(fc); err != nil {
			t.Errorf("%v", err)
		}
	})
}

// faultTierTenant is the tenant the fault tier's checks own their Jobs to.
const faultTierTenant = pkgcore.TenantID("fault-tier")

// faultTierGetType and faultTierDeadLetterType are the two job types the
// tier's checks enqueue, one per check.
const (
	faultTierGetType        = "fault-tier-cancel-guard"
	faultTierDeadLetterType = "fault-tier-dead-letter"
)

// checkGetFailsClosedOnUnreadableCancellationState verifies the Get
// direction of the fail-closed rule: a Job that was cancelled while pending
// (with a healthy read, Get answers StatusCancelled) must, once its
// cancellation-state read is sabotaged, be answered with an ERROR — never
// with its natural state reported as if the read had succeeded — and must
// answer StatusCancelled again once the sabotage is repaired. The natural
// state under the sabotage is the dangerous answer: asynq's Get, before
// c26b058b, reported a skipped-run Cancelled Job's underlying Completed
// record as StatusSucceeded with an empty Result when the marker read
// failed, and this check fails any implementation that does the same in
// whatever shape its own natural state takes.
func checkGetFailsClosedOnUnreadableCancellationState(fc FaultRunnable) error {
	ctx := context.Background()
	if err := fc.RegisterHandler(jobs.NewHandlerFunc(faultTierGetType, func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		return jobs.Result{}, nil
	})); err != nil {
		return fmt.Errorf("RegisterHandler() error = %w", err)
	}
	if err := fc.Start(ctx); err != nil {
		return fmt.Errorf("Start() error = %w", err)
	}

	// A one-hour delay keeps the Job pending for the whole check, so its
	// natural state stays unambiguous on every implementation (nothing ever
	// dispatches it): the only thing that can change how Get answers is the
	// cancellation state the check then breaks.
	id, err := fc.Enqueue(ctx, jobs.Task{Type: faultTierGetType, TenantID: faultTierTenant}, jobs.WithDelay(time.Hour))
	if err != nil {
		return fmt.Errorf("Enqueue() error = %w", err)
	}
	ownerCtx := pkgcore.WithTenant(ctx, faultTierTenant)
	if err = fc.Cancel(ownerCtx, id); err != nil {
		return fmt.Errorf("Cancel() error = %w", err)
	}
	job, err := fc.Get(ownerCtx, id)
	if err != nil {
		return fmt.Errorf("Get() on the healthy cancellation-state read error = %w, want nil", err)
	}
	if job.Status != jobs.StatusCancelled {
		return fmt.Errorf("Get() on the healthy cancellation-state read Status = %v, want %v (the baseline the sabotage must not corrupt)", job.Status, jobs.StatusCancelled)
	}

	if err = fc.Sabotage(ctx, id); err != nil {
		return fmt.Errorf("Sabotage() error = %w", err)
	}
	sabotaged, sErr := fc.Get(ownerCtx, id)
	if sErr == nil {
		return fmt.Errorf("Get() answered Status %v with nil error while the Job's cancellation-state read was failing: a possibly-cancelled Job was reported from its natural state instead of failing closed — the failure direction commit c26b058b corrected for asynq's Get, which answered a skipped-run Cancelled Job's underlying Completed record as StatusSucceeded with an empty Result", sabotaged.Status)
	}
	if err = fc.Repair(ctx, id); err != nil {
		return fmt.Errorf("Repair() error = %w", err)
	}
	job, err = fc.Get(ownerCtx, id)
	if err != nil {
		return fmt.Errorf("Get() error = %w after the repair, want nil (the outage must delay the report, not lose the cancellation)", err)
	}
	if job.Status != jobs.StatusCancelled {
		return fmt.Errorf("Get() Status = %v after the repair, want %v", job.Status, jobs.StatusCancelled)
	}
	return nil
}

// checkDeadLetterListingFailsClosedOnUnreadableCancellationState verifies
// the DeadLetterJobs direction of the same rule: a listing whose per-job
// cancellation-state read fails must ERROR rather than report the listed
// Job's natural state — an archived Job a concurrent Cancel may have settled
// as StatusCancelled must never be reported as StatusDeadLetter while its
// cancellation state cannot be read. The scenario first dead-letters a Job
// for real (a handler that always fails, no retries), confirms the listing
// shows it, then sabotages the read, demands the whole listing fail, and
// confirms the listing shows the Job again once the read is repaired.
func checkDeadLetterListingFailsClosedOnUnreadableCancellationState(fc FaultRunnable) error {
	ctx := context.Background()
	if err := fc.RegisterHandler(jobs.NewHandlerFunc(faultTierDeadLetterType, func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		return jobs.Result{}, errors.New("fault-tier: permanent failure")
	})); err != nil {
		return fmt.Errorf("RegisterHandler() error = %w", err)
	}
	if err := fc.Start(ctx); err != nil {
		return fmt.Errorf("Start() error = %w", err)
	}

	id, err := fc.Enqueue(ctx, jobs.Task{Type: faultTierDeadLetterType, TenantID: faultTierTenant}, jobs.WithMaxRetries(0))
	if err != nil {
		return fmt.Errorf("Enqueue() error = %w", err)
	}
	ownerCtx := pkgcore.WithTenant(ctx, faultTierTenant)
	job, err := waitTerminalErr(fc, ownerCtx, id, conformWaitTimeout)
	if err != nil {
		return fmt.Errorf("waiting for the Job to dead-letter: %w", err)
	}
	if job.Status != jobs.StatusDeadLetter {
		return fmt.Errorf("job Status = %v, want %v (the dead-letter the sabotage must not corrupt)", job.Status, jobs.StatusDeadLetter)
	}

	if err = fc.Sabotage(ctx, id); err != nil {
		return fmt.Errorf("Sabotage() error = %w", err)
	}
	if listed, lErr := fc.DeadLetterJobs(ownerCtx); lErr == nil {
		return fmt.Errorf("DeadLetterJobs() returned %d job(s) with nil error while a listed Job's cancellation-state read was failing: a possibly-cancelled Job was reported as its natural StatusDeadLetter instead of failing closed — the failure direction commit c26b058b corrected for asynq's DeadLetterJobs, which reported an archived Job's natural state when the marker read failed", len(listed))
	}
	if err = fc.Repair(ctx, id); err != nil {
		return fmt.Errorf("Repair() error = %w", err)
	}
	got, err := fc.DeadLetterJobs(ownerCtx)
	if err != nil {
		return fmt.Errorf("DeadLetterJobs() error = %w after the repair, want nil (the outage must delay the listing, not lose the Job)", err)
	}
	found := false
	for _, listed := range got {
		if listed.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("DeadLetterJobs() after the repair does not list job %q (listed %d jobs)", id, len(got))
	}
	return nil
}

// waitTerminalErr polls Get until id's Job reaches a terminal Status or
// timeout elapses, returning an error describing what instead of failing a
// test directly — the error-returning sibling of waitTerminal in
// assert_conforms.go, so the fault-tier checks can run against deliberately
// defective fakes in this package's own teeth tests as well as against the
// real implementations.
func waitTerminalErr(q jobs.Queue, ctx context.Context, id jobs.JobID, timeout time.Duration) (*jobs.Job, error) {
	deadline := time.Now().Add(timeout)
	var last *jobs.Job
	for {
		job, err := q.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("Get(%q) error = %w", id, err)
		}
		last = job
		if job.Status.Terminal() {
			return job, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %v waiting for job %q to reach a terminal status; last observed state = %+v", timeout, id, last)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
