//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// flakyHandler fails its first failuresBefore attempts, then succeeds --
// the same shape as go/jobs's own demo_queue_test.go flakyHandler (parent
// package, unexported, redefined here since test doubles are never part of
// a package's importable surface).
type flakyHandler struct {
	jobType        string
	failuresBefore int32
	attempts       atomic.Int32
}

func (h *flakyHandler) Type() string { return h.jobType }

func (h *flakyHandler) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	n := h.attempts.Add(1)
	if n <= h.failuresBefore {
		return jobs.Result{}, errors.New("transient failure")
	}
	return jobs.Result{Data: []byte("recovered")}, nil
}

// TestRedisQueue_RetryOnFailure proves a genuine Handler failure is
// retried by asynq's own machinery (asynq.MaxRetry + the configured
// RetryDelayFunc) rather than dead-lettering immediately, and that Attempts
// / Error are reported correctly across the retry -- the production-profile
// counterpart of demo_queue_test.go's TestRetry_SucceedsAfterTransientFailures,
// run here against a real asynq.Server actually re-delivering the task
// through real Redis.
func TestRedisQueue_RetryOnFailure(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	h := &flakyHandler{jobType: "flaky", failuresBefore: 2}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(context.Background(), jobs.Task{Type: "flaky", TenantID: tenant}, jobs.WithMaxRetries(5))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	job := waitForTerminal(t, tenantCtx(tenant), q, id, 15*time.Second)

	if job.Status != jobs.StatusSucceeded {
		t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, jobs.StatusSucceeded, job)
	}
	if job.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3 (2 failures + 1 success)", job.Attempts)
	}
	if job.Error != "" {
		t.Errorf("Error = %q, want empty after eventual success", job.Error)
	}
}

// countingFailureHandler always fails, and records every OnFailure call it
// receives on onFailureCh -- mirrors demo_queue_test.go's identically-named
// type in the parent package.
type countingFailureHandler struct {
	jobType     string
	onFailureCh chan *jobs.Job
}

func (h *countingFailureHandler) Type() string { return h.jobType }

func (h *countingFailureHandler) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	return jobs.Result{}, errors.New("permanent failure")
}

func (h *countingFailureHandler) OnFailure(_ context.Context, job *jobs.Job, _ error) {
	h.onFailureCh <- job
}

// TestRedisQueue_DeadLetterAndFailureHook proves a Job that exhausts its
// retries is archived by asynq (Inspector.ListArchivedTasks / DeadLetterJobs
// -- this package's mapping of DemoQueue.DeadLetterJobs onto asynq's own
// archived-task mechanism, AGENTS.md's dead-letter mapping section) and
// that FailureHook.OnFailure fires exactly once, built from AsynqQueue's own
// Config.ErrorHandler replicating asynq's archive-boundary decision -- the
// production-profile counterpart of demo_queue_test.go's
// TestDeadLetter_ExhaustsRetries_And_InvokesFailureHook.
func TestRedisQueue_DeadLetterAndFailureHook(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	h := &countingFailureHandler{jobType: "always-fails", onFailureCh: make(chan *jobs.Job, 1)}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(context.Background(), jobs.Task{Type: "always-fails", TenantID: tenant}, jobs.WithMaxRetries(1))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	job := waitForTerminal(t, tenantCtx(tenant), q, id, 15*time.Second)

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
	case hookJob := <-h.onFailureCh:
		if hookJob.ID != id {
			t.Errorf("OnFailure job.ID = %q, want %q", hookJob.ID, id)
		}
		if hookJob.Status != jobs.StatusDeadLetter {
			t.Errorf("OnFailure job.Status = %v, want %v", hookJob.Status, jobs.StatusDeadLetter)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FailureHook.OnFailure was never called")
	}

	dead, err := q.DeadLetterJobs(tenantCtx(tenant))
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
}
