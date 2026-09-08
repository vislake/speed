package asynq

import (
	"context"
	"errors"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file holds the cancellation-marker fail-closed regressions: an
// attempt whose marker read FAILED must be refused (a Job whose
// cancellation state cannot be verified might be cancelled and must not
// execute), and a refusal that lands on a terminal attempt archives -- so
// its FailureHook fires, exactly like a terminal tenant bounce (see
// handleErrorAttempt's doc comment). Named for the behaviour it verifies,
//  since it spans
// worker.go's dispatchAfterMarkerRead/handleErrorAttempt and queue.go's
// readCancelMarker. Everything here is pure Go logic against a bare
// *Queue: the only Redis touch (the read itself) is passed in as its
// result, the same ctx-free split processTask/handleError already use.

// TestQueue_DispatchAfterMarkerRead_FailsClosedOnUnreadableMarker pins the
// decision processTask makes from the marker read's outcome: an
// UNREADABLE marker must refuse the run with errCancelMarkerUnreadable
// (fail closed -- the Job might be cancelled, so it must not execute);
// a readable marker that says "cancelled" must skip Handle without an
// error (asynq records the skip as a clean completion; Get overlays the
// cancelled status); and only a readable "not cancelled" answer may
// proceed into the attempt. The run-through branch is proven by reaching
// processTaskUncancelled's own first gate (ErrHandlerNotRegistered on a
// bare queue with no handler -- the branches before it never write
// ResultWriter envelopes, which would panic on a synthetic task).
func TestQueue_DispatchAfterMarkerRead_FailsClosedOnUnreadableMarker(t *testing.T) {
	q := newTestQueue(t) // no handler registered: the run-through branch stops at ErrHandlerNotRegistered.
	task := asynqlib.NewTaskWithHeaders("marker-guarded", []byte("payload"), map[string]string{headerTenantID: "tenant-a"})
	log := obs.FromContext(context.Background())

	// An unreadable marker must refuse the run: logging the read failure
	// while letting the attempt proceed would let a cancelled Job run
	// anyway, then report StatusCancelled later.
	err := q.dispatchAfterMarkerRead(context.Background(), task, "job-1", log, nil /* cancelledAt */, errors.New("WRONGTYPE simulated marker read failure"))
	if !errors.Is(err, errCancelMarkerUnreadable) {
		t.Fatalf("dispatchAfterMarkerRead(unreadable marker) error = %v, want errCancelMarkerUnreadable", err)
	}

	// A readable marker that says "cancelled" skips Handle without an
	// error -- the skip path keeps working alongside the refusal.
	cancelledAt := time.Now()
	if skipErr := q.dispatchAfterMarkerRead(context.Background(), task, "job-1", log, &cancelledAt, nil); skipErr != nil {
		t.Fatalf("dispatchAfterMarkerRead(cancelled marker) error = %v, want nil (skip)", skipErr)
	}

	// A readable "not cancelled" answer proceeds into the attempt path.
	err = q.dispatchAfterMarkerRead(context.Background(), task, "job-1", log, nil, nil)
	if appErr, ok := apperr.As(err); !ok || appErr.Code != jobs.ErrHandlerNotRegistered.Code {
		t.Fatalf("dispatchAfterMarkerRead(readable, not cancelled) error = %v, want it to proceed into the attempt (ErrHandlerNotRegistered from the bare queue)", err)
	}
}

// TestQueue_HandleErrorAttempt_TerminalMarkerUnreadableRefusal_FiresFailureHook
// pins the archive-bound half of the fail-closed refusal: on a Job whose
// retries are exhausted (retried == maxRetry) a marker-unreadable refusal
// still archives -- asynq's dispatch loop archives every terminal-attempt
// error right after this hook returns -- so the dead-letter's OnFailure
// must fire exactly like a genuine terminal failure's, never be skipped
// silently (the money-path rule handleErrorAttempt's doc comment states).
func TestQueue_HandleErrorAttempt_TerminalMarkerUnreadableRefusal_FiresFailureHook(t *testing.T) {
	q := newTestQueue(t)
	h := &recordingFailureHook{jobType: "always-fails"}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("always-fails", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleErrorAttempt(task, errCancelMarkerUnreadable, 3 /* retried */, 3 /* maxRetry */, "job-1", nil /* not cancelled */, obs.FromContext(context.Background()))

	if len(h.calls) != 1 {
		t.Errorf("OnFailure called %d times, want exactly 1 for an archive-bound terminal marker-unreadable refusal", len(h.calls))
	}
}

// TestQueue_HandleErrorAttempt_MarkerUnreadableRefusalWithRetriesRemaining_FiresNothing
// pins the retryable half: with retries remaining the refusal bounces
// (isFailure reports false, no budget consumed) instead of archiving, so
// nothing fires.
func TestQueue_HandleErrorAttempt_MarkerUnreadableRefusalWithRetriesRemaining_FiresNothing(t *testing.T) {
	q := newTestQueue(t)
	h := &recordingFailureHook{jobType: "always-fails"}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("always-fails", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleErrorAttempt(task, errCancelMarkerUnreadable, 1 /* retried */, 3 /* maxRetry */, "job-1", nil, obs.FromContext(context.Background()))

	if len(h.calls) != 0 {
		t.Errorf("OnFailure called %d times, want 0 for a marker-unreadable refusal with retries remaining", len(h.calls))
	}
}

// TestIsFailure_MarkerUnreadableIsBounceClass pins the other half of the
// fail-closed sentinel's contract: like errTenantAtCapacity, an
// unreadable-marker refusal must never count as a business failure, so
// asynq's retry bookkeeping never burns a retry on it.
func TestIsFailure_MarkerUnreadableIsBounceClass(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "marker unreadable", err: errCancelMarkerUnreadable, want: false},
		{name: "wrapped marker unreadable", err: wrapErr(errCancelMarkerUnreadable), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFailure(tt.err); got != tt.want {
				t.Errorf("isFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestQueue_RetryDelay_MarkerUnreadableGetsThrottleDelay pins that the
// refusal redelivers on the short throttle delay, never the business
// backoff -- a transient marker outage must delay the attempt, not subject
// it to a genuine failure's exponential wait.
func TestQueue_RetryDelay_MarkerUnreadableGetsThrottleDelay(t *testing.T) {
	q := newTestQueue(t)
	q.throttleRetryDelay = 100 * time.Millisecond
	businessCalled := false
	q.businessRetryDelayFunc = func(n int, e error, task *asynqlib.Task) time.Duration {
		businessCalled = true
		return 999 * time.Second
	}
	task := asynqlib.NewTask("t", nil)

	got := q.retryDelay(0, errCancelMarkerUnreadable, task)
	if businessCalled {
		t.Error("businessRetryDelayFunc must not run for errCancelMarkerUnreadable")
	}
	if got < q.throttleRetryDelay || got > 2*q.throttleRetryDelay {
		t.Errorf("retryDelay(errCancelMarkerUnreadable) = %v, want in [%v, %v]", got, q.throttleRetryDelay, 2*q.throttleRetryDelay)
	}
}
