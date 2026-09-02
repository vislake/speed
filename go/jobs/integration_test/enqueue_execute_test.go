//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// TestRedisQueue_EnqueueExecuteRoundTrip is this suite's core proof: a Task
// enqueued through AsynqQueue.Enqueue is genuinely persisted to Redis (not
// just handed to an in-process channel), dequeued and executed by a real
// asynq.Server/worker goroutine, and its outcome (status, attempts,
// progress, result) is observable through AsynqQueue.Get -- the same
// contract go/jobs's own example_test.go proves for StandaloneQueue, exercised
// here against a real backend per this task's own testing instructions.
func TestRedisQueue_EnqueueExecuteRoundTrip(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	var sawTenant pkgcore.TenantID
	h := jobs.NewHandlerFunc("greet", func(ctx context.Context, job *jobs.Job, progress jobs.ProgressFn) (jobs.Result, error) {
		sawTenant, _ = pkgcore.TenantFromContext(ctx)
		progress(50, "composing greeting")
		return jobs.Result{Data: []byte("Hello, " + string(job.Payload) + "!")}, nil
	})
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("acme")
	id, err := q.Enqueue(context.Background(), jobs.Task{
		Type:     "greet",
		TenantID: tenant,
		Payload:  []byte("speed"),
	})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if id == "" {
		t.Fatal("Enqueue() returned an empty JobID")
	}

	job := waitForTerminal(t, tenantCtx(tenant), q, id, 10*time.Second)

	if job.Status != jobs.StatusSucceeded {
		t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, jobs.StatusSucceeded, job)
	}
	if job.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", job.Attempts)
	}
	if job.Result == nil || string(job.Result.Data) != "Hello, speed!" {
		t.Errorf("Result = %+v, want Data = %q", job.Result, "Hello, speed!")
	}
	if job.TenantID != tenant {
		t.Errorf("Job.TenantID = %q, want %q", job.TenantID, tenant)
	}
	if sawTenant != tenant {
		t.Errorf("tenant observed inside Handle via pkgcore.TenantFromContext = %q, want %q", sawTenant, tenant)
	}
}

// TestRedisQueue_Idempotency proves Task.IdempotencyKey's contract --
// "a second Enqueue call for the same (TenantID, IdempotencyKey) pair
// returns the JobID of the Job already created for the first call" -- maps
// correctly onto asynq's own TaskID-uniqueness mechanism (asynq.TaskID +
// ErrTaskIDConflict; see AGENTS.md's idempotency section), including under
// genuinely concurrent Enqueue calls racing against Redis, not just
// sequential ones.
func TestRedisQueue_Idempotency(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	callCount := 0
	h := jobs.NewHandlerFunc("charge", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		callCount++
		return jobs.Result{Data: []byte("charged")}, nil
	})
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("acme")
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

	job := waitForTerminal(t, tenantCtx(tenant), q, first, 10*time.Second)
	if job.Status != jobs.StatusSucceeded {
		t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, jobs.StatusSucceeded, job)
	}
	if callCount != 1 {
		t.Errorf("Handle called %d times across %d concurrent Enqueue calls for the same idempotency key, want exactly 1", callCount, concurrentCalls)
	}
}
