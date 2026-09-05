// Package queuetest verifies that a jobs.Queue implementation upholds the
// contract Queue's own doc comment describes, independent of which
// deployment mode implements it. It plays the same role for Queue that
// go/pkgcore/kvstoretest.AssertConforms plays for KVStore,
// go/pkgcore/eventbustest.AssertConforms plays for EventBus, and
// go/pkgcore/objectstoretest/mailertest play for their own seams: one
// suite every implementation -- StandaloneQueue (this module's own
// standalone-mode implementation) and go/jobs/queue/asynq's distributed-mode
// Queue -- must pass, so drift between the two is caught here once instead
// of via two independently hand-maintained test files (docs/internal/
// 16-verification.md §2's own claim that Queue deserves the identical
// shared-conformance-suite treatment, which this package makes real).
//
// Unlike the four pkgcore seam packages above, AssertConforms needs more
// than the bare jobs.Queue interface to exercise retry and dead-letter
// behavior: it needs to register a Handler and actually run the queue's
// workers, which is deliberately NOT part of Queue itself (RegisterHandler/
// Start/Close differ enough between the standalone and distributed
// implementations' own setup shape that Queue's own doc comment names this
// exact reason for leaving them off the portable interface). See Runnable.
package queuetest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// Runnable is the subset of setup operations AssertConforms needs beyond
// the portable jobs.Queue interface: registering a Handler, starting the
// queue's workers, and (DeadLetterJobs) listing dead-lettered Jobs.
// StandaloneQueue and go/jobs/queue/asynq's Queue both satisfy this
// structurally already -- every one of these four methods has the exact
// same signature on both concrete types -- so a factory returning either
// one needs no adapter.
type Runnable interface {
	jobs.Queue

	// RegisterHandler adds h to the set this Runnable dispatches to.
	// AssertConforms always calls this before Start.
	RegisterHandler(h jobs.Handler) error

	// Start launches the Runnable's dispatcher/worker goroutines.
	Start(ctx context.Context) error

	// Close stops the Runnable, releasing whatever it holds.
	// AssertConforms always calls this via t.Cleanup.
	Close(ctx context.Context) error

	// DeadLetterJobs returns every Job currently StatusDeadLetter that ctx
	// may access. Not part of jobs.Queue itself (a convenience for
	// operating one implementation, per StandaloneQueue's own doc
	// comment), but present with an identical signature on both
	// implementations this package conforms.
	DeadLetterJobs(ctx context.Context) ([]*jobs.Job, error)
}

// conformWaitTimeout bounds how long AssertConforms waits for a Job to
// reach a terminal status. Generous relative to the sub-second backoffs a
// real Queue's own tests configure, since a factory's own construction
// (e.g. a fresh asynq.Client/Server pair over a real Redis) can add latency
// a purely in-process implementation would not.
const conformWaitTimeout = 15 * time.Second

// AssertConforms verifies that the jobs.Queue factory returns satisfies
// the contract documented on jobs.Queue, by registering handlers and
// running the queue for each subtest. Each subtest calls factory to get
// its own fresh, unstarted Runnable and registers its own Handler(s) under
// job types unique to that subtest, so subtests sharing infrastructure
// (asynq's own integration leg runs every subtest against the same backing
// Redis instance, one fresh Queue struct per subtest) never collide.
//
// What AssertConforms checks, in order: Enqueue followed by Get round-trips
// a Job through to StatusSucceeded with its Result, Attempts and
// CompletedAt all correctly populated; Get enforces the owning-tenant/
// system-context access rule (a different tenant, no tenant at all, and an
// unknown id all report ErrJobNotFound, a system context sees it); Cancel
// enforces the identical access rule, marks a pending Job StatusCancelled,
// and is idempotent on a second call; a Task.IdempotencyKey makes
// concurrent Enqueue calls for the same (TenantID, IdempotencyKey) pair
// converge on one Job that Handle runs exactly once for; a Handler that
// fails transiently before eventually succeeding is retried rather than
// immediately dead-lettered, with Attempts reflecting every attempt; and a
// Handler that always fails exhausts its retry budget into
// StatusDeadLetter, invokes FailureHook.OnFailure exactly once, and shows
// up in DeadLetterJobs.
func AssertConforms(t *testing.T, factory func() Runnable) {
	t.Helper()

	t.Run("enqueue_get_happy_path", func(t *testing.T) {
		t.Helper()
		q := startConformQueue(t, factory)
		if err := q.RegisterHandler(jobs.NewHandlerFunc("echo", func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
			return jobs.Result{Data: job.Payload}, nil
		})); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
		mustStart(t, q)

		id, err := q.Enqueue(context.Background(), jobs.Task{Type: "echo", TenantID: "tenant-a", Payload: []byte("hello")})
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}

		ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
		job := waitTerminal(t, q, ctx, id)
		if job.Status != jobs.StatusSucceeded {
			t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, jobs.StatusSucceeded, job)
		}
		if job.Result == nil || string(job.Result.Data) != "hello" {
			t.Errorf("Result = %+v, want Data = %q", job.Result, "hello")
		}
		if job.Attempts != 1 {
			t.Errorf("Attempts = %d, want 1", job.Attempts)
		}
		if job.CompletedAt == nil {
			t.Error("CompletedAt = nil, want set")
		}
	})

	t.Run("get_tenant_isolation", func(t *testing.T) {
		t.Helper()
		q := startConformQueue(t, factory)
		if err := q.RegisterHandler(jobs.NewHandlerFunc("noop-get", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
			return jobs.Result{}, nil
		})); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
		mustStart(t, q)

		id, err := q.Enqueue(context.Background(), jobs.Task{Type: "noop-get", TenantID: "tenant-a"}, jobs.WithDelay(time.Hour))
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}

		ownerCtx := pkgcore.WithTenant(context.Background(), "tenant-a")
		_, err = q.Get(ownerCtx, id)
		if err != nil {
			t.Errorf("Get() with the owning tenant error = %v, want nil", err)
		}

		otherCtx := pkgcore.WithTenant(context.Background(), "tenant-b")
		_, err = q.Get(otherCtx, id)
		if !errors.Is(err, jobs.ErrJobNotFound) {
			t.Errorf("Get() with a different tenant error = %v, want ErrJobNotFound", err)
		}

		_, err = q.Get(context.Background(), id)
		if !errors.Is(err, jobs.ErrJobNotFound) {
			t.Errorf("Get() with no tenant and no system context error = %v, want ErrJobNotFound", err)
		}

		pkgcore.RegisterSystemPurpose(conformSystemPurpose)
		sysCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{Actor: "queuetest", Purpose: conformSystemPurpose})
		if err != nil {
			t.Fatalf("WithSystemContext() error = %v", err)
		}
		_, err = q.Get(sysCtx, id)
		if err != nil {
			t.Errorf("Get() with a system context error = %v, want nil", err)
		}

		_, err = q.Get(ownerCtx, jobs.JobID("no-such-id"))
		if !errors.Is(err, jobs.ErrJobNotFound) {
			t.Errorf("Get() for a nonexistent id error = %v, want ErrJobNotFound", err)
		}
	})

	t.Run("cancel_tenant_isolation_and_idempotency", func(t *testing.T) {
		t.Helper()
		q := startConformQueue(t, factory)
		if err := q.RegisterHandler(jobs.NewHandlerFunc("noop-cancel", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
			return jobs.Result{}, nil
		})); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
		mustStart(t, q)

		id, err := q.Enqueue(context.Background(), jobs.Task{Type: "noop-cancel", TenantID: "tenant-a"}, jobs.WithDelay(time.Hour))
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}

		otherCtx := pkgcore.WithTenant(context.Background(), "tenant-b")
		err = q.Cancel(otherCtx, id)
		if !errors.Is(err, jobs.ErrJobNotFound) {
			t.Errorf("Cancel() from a different tenant error = %v, want ErrJobNotFound", err)
		}

		ownerCtx := pkgcore.WithTenant(context.Background(), "tenant-a")
		err = q.Cancel(ownerCtx, id)
		if err != nil {
			t.Fatalf("Cancel() error = %v", err)
		}
		job, err := q.Get(ownerCtx, id)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if job.Status != jobs.StatusCancelled {
			t.Errorf("Status = %v, want %v", job.Status, jobs.StatusCancelled)
		}

		if err := q.Cancel(ownerCtx, id); err != nil {
			t.Errorf("second Cancel() error = %v, want nil (idempotent)", err)
		}
	})

	t.Run("idempotency_concurrent_enqueue_dedupes_to_one_handle_call", func(t *testing.T) {
		t.Helper()
		q := startConformQueue(t, factory)
		var calls atomic.Int32
		if err := q.RegisterHandler(jobs.NewHandlerFunc("charge", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
			calls.Add(1)
			return jobs.Result{Data: []byte("charged")}, nil
		})); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
		mustStart(t, q)

		const tenant = pkgcore.TenantID("tenant-a")
		task := jobs.Task{Type: "charge", TenantID: tenant, IdempotencyKey: "invoice-42"}

		const concurrentCalls = 10
		ids := make(chan jobs.JobID, concurrentCalls)
		errs := make(chan error, concurrentCalls)
		for i := 0; i < concurrentCalls; i++ {
			go func() {
				id, err := q.Enqueue(context.Background(), task)
				ids <- id
				errs <- err
			}()
		}

		first := jobs.JobID("")
		for i := 0; i < concurrentCalls; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("Enqueue() call %d error = %v", i, err)
			}
			id := <-ids
			if first == "" {
				first = id
			} else if id != first {
				t.Errorf("Enqueue() call %d returned JobID %q, want the same id %q every time", i, id, first)
			}
		}

		job := waitTerminal(t, q, pkgcore.WithTenant(context.Background(), tenant), first)
		if job.Status != jobs.StatusSucceeded {
			t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, jobs.StatusSucceeded, job)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("Handle called %d times across %d concurrent Enqueue calls for the same idempotency key, want exactly 1", got, concurrentCalls)
		}
	})

	t.Run("retry_succeeds_after_transient_failures", func(t *testing.T) {
		t.Helper()
		q := startConformQueue(t, factory)
		var attempts atomic.Int32
		const failuresBefore = 2
		if err := q.RegisterHandler(jobs.NewHandlerFunc("flaky", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
			if attempts.Add(1) <= failuresBefore {
				return jobs.Result{}, errors.New("transient failure")
			}
			return jobs.Result{Data: []byte("recovered")}, nil
		})); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
		mustStart(t, q)

		id, err := q.Enqueue(context.Background(), jobs.Task{Type: "flaky", TenantID: "tenant-a"}, jobs.WithMaxRetries(5))
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}

		ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
		job := waitTerminal(t, q, ctx, id)
		if job.Status != jobs.StatusSucceeded {
			t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, jobs.StatusSucceeded, job)
		}
		if job.Attempts != failuresBefore+1 {
			t.Errorf("Attempts = %d, want %d (%d failures + 1 success)", job.Attempts, failuresBefore+1, failuresBefore)
		}
		if job.Error != "" {
			t.Errorf("Error = %q, want empty after eventual success", job.Error)
		}
	})

	t.Run("dead_letter_exhausts_retries_and_invokes_failure_hook", func(t *testing.T) {
		t.Helper()
		q := startConformQueue(t, factory)
		onFailureCh := make(chan *jobs.Job, 1)
		if err := q.RegisterHandler(&conformAlwaysFailsHandler{onFailureCh: onFailureCh}); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
		mustStart(t, q)

		id, err := q.Enqueue(context.Background(), jobs.Task{Type: "always-fails", TenantID: "tenant-a"}, jobs.WithMaxRetries(1))
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}

		ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
		job := waitTerminal(t, q, ctx, id)
		if job.Status != jobs.StatusDeadLetter {
			t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, jobs.StatusDeadLetter, job)
		}
		if job.Attempts != 2 {
			t.Errorf("Attempts = %d, want 2 (1 initial + 1 retry, MaxRetries=1)", job.Attempts)
		}
		if job.Error != "permanent failure" {
			t.Errorf("Error = %q, want %q", job.Error, "permanent failure")
		}

		select {
		case hookJob := <-onFailureCh:
			if hookJob.ID != id {
				t.Errorf("OnFailure job.ID = %q, want %q", hookJob.ID, id)
			}
			if hookJob.Status != jobs.StatusDeadLetter {
				t.Errorf("OnFailure job.Status = %v, want %v", hookJob.Status, jobs.StatusDeadLetter)
			}
		case <-time.After(conformWaitTimeout):
			t.Fatal("FailureHook.OnFailure was never called")
		}

		dead, err := q.DeadLetterJobs(ctx)
		if err != nil {
			t.Fatalf("DeadLetterJobs() error = %v", err)
		}
		found := false
		for _, j := range dead {
			if j.ID == id {
				found = true
			}
		}
		if !found {
			t.Errorf("DeadLetterJobs() = %v, want it to include %q", dead, id)
		}
	})
}

// conformSystemPurpose is the pkgcore.SystemPurpose the get_tenant_isolation
// subtest declares for exercising the pkgcore.WithSystemContext branch of a
// Queue's own access rule. Registration is idempotent
// (pkgcore.RegisterSystemPurpose's own doc comment), so this constant is
// safe to declare from both StandaloneQueue's and asynq's own conformance
// call sites in the same test binary.
const conformSystemPurpose = pkgcore.SystemPurpose("jobs.queuetest_system_access")

// conformAlwaysFailsHandler always fails with a fixed error and records
// every OnFailure call it receives on onFailureCh -- the dead-letter
// subtest's Handler, mirroring both go/jobs's own standalone_queue_test.go
// countingFailureHandler and go/jobs/integration_test's identically-shaped
// type (now redundant, converted to call this package instead).
type conformAlwaysFailsHandler struct {
	onFailureCh chan *jobs.Job
}

func (*conformAlwaysFailsHandler) Type() string { return "always-fails" }

func (*conformAlwaysFailsHandler) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	return jobs.Result{}, errors.New("permanent failure")
}

func (h *conformAlwaysFailsHandler) OnFailure(_ context.Context, job *jobs.Job, _ error) {
	h.onFailureCh <- job
}

// startConformQueue returns a fresh Runnable from factory, registering its
// Close via t.Cleanup so a subtest never needs to remember to release it
// itself.
func startConformQueue(t *testing.T, factory func() Runnable) Runnable {
	t.Helper()
	q := factory()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = q.Close(ctx)
	})
	return q
}

// mustStart calls q.Start, failing the test on error.
func mustStart(t *testing.T, q Runnable) {
	t.Helper()
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
}

// waitTerminal polls Get until id's Job reaches a terminal Status or
// conformWaitTimeout passes, failing the test on timeout.
func waitTerminal(t *testing.T, q jobs.Queue, ctx context.Context, id jobs.JobID) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(conformWaitTimeout)
	var last *jobs.Job
	for {
		job, err := q.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%q) error = %v", id, err)
		}
		last = job
		if job.Status.Terminal() {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for job %q to reach a terminal status; last observed state = %+v", conformWaitTimeout, id, last)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
