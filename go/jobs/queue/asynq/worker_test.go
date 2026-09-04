package asynq

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// newTestQueue returns a bare *Queue with only the fields the function
// under test reads populated -- no Redis, no asynqlib.Client/Server/
// Inspector construction. This is deliberately NOT NewQueue: every function
// tested from this file (tryReserveTenantSlot/releaseTenantSlot,
// retryDelay, isFailure, handleErrorAttempt, processTask's early-exit
// branches) is pure Go logic that never touches q.rdb/q.client/q.inspector/
// q.server, so constructing those (which requires a real or fake
// asynqlib.RedisConnOpt) would only add noise. Anything that DOES need
// them -- Enqueue/Get/Cancel, and processTask's own ResultWriter.Write
// calls past its early-exit branches -- is exercised in integration_test/
// against a real Redis instead, per this module's own testing convention.
func newTestQueue(t *testing.T) *Queue {
	t.Helper()
	return &Queue{
		tenantConcurrency:      DefaultTenantConcurrencyLimit,
		defaultTimeout:         jobs.DefaultTimeout,
		throttleRetryDelay:     DefaultThrottleRetryDelay,
		businessRetryDelayFunc: asynqlib.DefaultRetryDelayFunc,
		handlers:               make(map[string]jobs.Handler),
		runningPerTenant:       make(map[pkgcore.TenantID]int),
	}
}

func TestQueue_TryReserveTenantSlot_And_Release(t *testing.T) {
	q := newTestQueue(t)
	q.tenantConcurrency = 2
	const tenant = pkgcore.TenantID("tenant-a")

	if !q.tryReserveTenantSlot(tenant) {
		t.Fatal("first reservation should succeed")
	}
	if !q.tryReserveTenantSlot(tenant) {
		t.Fatal("second reservation (at the limit of 2) should succeed")
	}
	if q.tryReserveTenantSlot(tenant) {
		t.Fatal("third reservation should fail: tenant is already at its concurrency limit")
	}

	q.releaseTenantSlot(tenant)
	if !q.tryReserveTenantSlot(tenant) {
		t.Fatal("reservation after a release should succeed again")
	}

	q.releaseTenantSlot(tenant)
	q.releaseTenantSlot(tenant)
	if _, exists := q.runningPerTenant[tenant]; exists {
		t.Errorf("runningPerTenant still has an entry for %q after releasing every reservation, want it deleted", tenant)
	}
}

func TestQueue_TenantSlotReservation_ConcurrentAccessIsRaceFree(t *testing.T) {
	q := newTestQueue(t)
	q.tenantConcurrency = 3
	const tenant = pkgcore.TenantID("tenant-a")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if q.tryReserveTenantSlot(tenant) {
				q.releaseTenantSlot(tenant)
			}
		}()
	}
	wg.Wait()
}

func TestIsFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "generic error", err: errors.New("boom"), want: true},
		{name: "tenant at capacity", err: errTenantAtCapacity, want: false},
		{name: "wrapped tenant at capacity", err: wrapErr(errTenantAtCapacity), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFailure(tt.err); got != tt.want {
				t.Errorf("isFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func wrapErr(err error) error {
	return &wrappedErr{err}
}

type wrappedErr struct{ inner error }

func (w *wrappedErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }

func TestQueue_RetryDelay(t *testing.T) {
	q := newTestQueue(t)
	q.throttleRetryDelay = 100 * time.Millisecond
	businessCalled := false
	q.businessRetryDelayFunc = func(n int, e error, task *asynqlib.Task) time.Duration {
		businessCalled = true
		return 999 * time.Second
	}
	task := asynqlib.NewTask("t", nil)

	t.Run("throttle error gets a short jittered delay, never the business formula", func(t *testing.T) {
		businessCalled = false
		got := q.retryDelay(5, errTenantAtCapacity, task)
		if businessCalled {
			t.Error("businessRetryDelayFunc must not run for errTenantAtCapacity")
		}
		if got < q.throttleRetryDelay || got > 2*q.throttleRetryDelay {
			t.Errorf("retryDelay(errTenantAtCapacity) = %v, want in [%v, %v]", got, q.throttleRetryDelay, 2*q.throttleRetryDelay)
		}
	})

	t.Run("wrapped throttle error is still recognized", func(t *testing.T) {
		businessCalled = false
		got := q.retryDelay(0, wrapErr(errTenantAtCapacity), task)
		if businessCalled {
			t.Error("businessRetryDelayFunc must not run for a wrapped errTenantAtCapacity")
		}
		if got < q.throttleRetryDelay || got > 2*q.throttleRetryDelay {
			t.Errorf("retryDelay(wrapped errTenantAtCapacity) = %v, want in [%v, %v]", got, q.throttleRetryDelay, 2*q.throttleRetryDelay)
		}
	})

	t.Run("a genuine business failure defers to businessRetryDelayFunc", func(t *testing.T) {
		businessCalled = false
		got := q.retryDelay(2, errors.New("business failure"), task)
		if !businessCalled {
			t.Error("businessRetryDelayFunc should have run for a non-throttle error")
		}
		if got != 999*time.Second {
			t.Errorf("retryDelay(business failure) = %v, want the businessRetryDelayFunc's own return value", got)
		}
	})
}

// recordingFailureHook is a jobs.Handler+jobs.FailureHook test double
// recording every OnFailure call it receives.
type recordingFailureHook struct {
	jobType string
	calls   []*jobs.Job
}

func (h *recordingFailureHook) Type() string { return h.jobType }
func (h *recordingFailureHook) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	return jobs.Result{}, errors.New("not exercised by this test")
}

func (h *recordingFailureHook) OnFailure(_ context.Context, job *jobs.Job, _ error) {
	h.calls = append(h.calls, job)
}

func TestQueue_HandleErrorAttempt(t *testing.T) {
	t.Run("retries remain: FailureHook must not fire", func(t *testing.T) {
		q := newTestQueue(t)
		h := &recordingFailureHook{jobType: "always-fails"}
		if err := q.RegisterHandler(h); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
		task := asynqlib.NewTaskWithHeaders("always-fails", []byte("payload"), map[string]string{headerTenantID: "tenant-a"})

		q.handleErrorAttempt(task, errors.New("attempt failed"), 1 /* retried */, 3 /* maxRetry */, "job-1")

		if len(h.calls) != 0 {
			t.Errorf("OnFailure called %d times, want 0: retried(1) < maxRetry(3), asynq will retry rather than archive", len(h.calls))
		}
	})

	t.Run("retries exhausted: FailureHook fires exactly once with a DeadLetter Job", func(t *testing.T) {
		q := newTestQueue(t)
		h := &recordingFailureHook{jobType: "always-fails"}
		if err := q.RegisterHandler(h); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
		task := asynqlib.NewTaskWithHeaders("always-fails", []byte("payload"), map[string]string{
			headerTenantID:       "tenant-a",
			headerIdempotencyKey: "op-1",
		})

		q.handleErrorAttempt(task, errors.New("permanent failure"), 3 /* retried */, 3 /* maxRetry */, "job-1")

		if len(h.calls) != 1 {
			t.Fatalf("OnFailure called %d times, want exactly 1", len(h.calls))
		}
		got := h.calls[0]
		if got.ID != jobs.JobID("job-1") || got.TenantID != pkgcore.TenantID("tenant-a") || got.IdempotencyKey != "op-1" {
			t.Errorf("OnFailure job = %+v, want ID=job-1 TenantID=tenant-a IdempotencyKey=op-1", got)
		}
		if got.Status != jobs.StatusDeadLetter {
			t.Errorf("OnFailure job.Status = %v, want %v", got.Status, jobs.StatusDeadLetter)
		}
		if got.Attempts != 4 {
			t.Errorf("OnFailure job.Attempts = %d, want 4 (MaxRetries=3, total attempts = MaxRetries+1)", got.Attempts)
		}
		if got.Error != "permanent failure" {
			t.Errorf("OnFailure job.Error = %q, want %q", got.Error, "permanent failure")
		}
	})

	t.Run("no registered handler: no panic, no hook call", func(t *testing.T) {
		q := newTestQueue(t)
		task := asynqlib.NewTaskWithHeaders("unregistered-type", nil, map[string]string{headerTenantID: "tenant-a"})
		q.handleErrorAttempt(task, errors.New("boom"), 3, 3, "job-1") // must not panic.
	})

	t.Run("handler without FailureHook: no panic, no hook call", func(t *testing.T) {
		q := newTestQueue(t)
		plain := jobs.NewHandlerFunc("plain", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
			return jobs.Result{}, nil
		})
		if err := q.RegisterHandler(plain); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
		task := asynqlib.NewTaskWithHeaders("plain", nil, map[string]string{headerTenantID: "tenant-a"})
		q.handleErrorAttempt(task, errors.New("boom"), 3, 3, "job-1") // must not panic.
	})

	t.Run("tenant-at-capacity bounce is never treated as dead-letter-worthy", func(t *testing.T) {
		q := newTestQueue(t)
		h := &recordingFailureHook{jobType: "always-fails"}
		if err := q.RegisterHandler(h); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
		task := asynqlib.NewTaskWithHeaders("always-fails", nil, map[string]string{headerTenantID: "tenant-a"})

		// Even at retried==maxRetry, errTenantAtCapacity short-circuits
		// before the archive-boundary check.
		q.handleErrorAttempt(task, errTenantAtCapacity, 3, 3, "job-1")

		if len(h.calls) != 0 {
			t.Errorf("OnFailure called %d times, want 0 for a throttle bounce", len(h.calls))
		}
	})
}

func TestQueue_ProcessTask_HandlerNotRegistered(t *testing.T) {
	q := newTestQueue(t)
	task := asynqlib.NewTaskWithHeaders("no-such-type", nil, map[string]string{headerTenantID: "tenant-a"})

	err := q.processTaskUncancelled(context.Background(), task, "job-1", obs.FromContext(context.Background()))

	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != jobs.ErrHandlerNotRegistered.Code {
		t.Errorf("processTask() error = %v, want ErrHandlerNotRegistered (code %q)", err, jobs.ErrHandlerNotRegistered.Code)
	}
}

func TestQueue_ProcessTask_MissingTenantHeader(t *testing.T) {
	q := newTestQueue(t)
	handleCalled := false
	h := jobs.NewHandlerFunc("t", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		handleCalled = true
		return jobs.Result{}, nil
	})
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("t", nil, map[string]string{}) // no tenant_id at all.

	err := q.processTaskUncancelled(context.Background(), task, "job-1", obs.FromContext(context.Background()))

	if err == nil {
		t.Fatal("processTask() error = nil, want a non-nil error for a task with no tenant_id header")
	}
	if handleCalled {
		t.Error("Handle must not run at all when the task carries no tenant_id header")
	}
}

func TestQueue_ProcessTask_TenantAtCapacity_BouncesWithoutCallingHandle(t *testing.T) {
	q := newTestQueue(t)
	q.tenantConcurrency = 1
	const tenant = pkgcore.TenantID("tenant-a")
	if !q.tryReserveTenantSlot(tenant) {
		t.Fatal("setup: reserving the only slot should succeed")
	}
	defer q.releaseTenantSlot(tenant)

	handleCalled := false
	h := jobs.NewHandlerFunc("t", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		handleCalled = true
		return jobs.Result{}, nil
	})
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("t", nil, map[string]string{headerTenantID: string(tenant)})

	err := q.processTaskUncancelled(context.Background(), task, "job-1", obs.FromContext(context.Background()))

	if !errors.Is(err, errTenantAtCapacity) {
		t.Errorf("processTask() error = %v, want errTenantAtCapacity", err)
	}
	if handleCalled {
		t.Error("Handle must not run at all when the tenant is already at its concurrency limit")
	}
}
