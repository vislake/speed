//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// progressHandler reports 30% then blocks until the test lets it continue,
// then reports 90% and succeeds -- mirrors demo_queue_test.go's
// identically-named type and scenario shape in the parent package.
type progressHandler struct {
	afterFirstReport chan struct{}
	resume           chan struct{}
}

func (*progressHandler) Type() string { return "progress" }

func (h *progressHandler) Handle(_ context.Context, _ *jobs.Job, progress jobs.ProgressFn) (jobs.Result, error) {
	progress(30, "step one")
	close(h.afterFirstReport)
	<-h.resume
	progress(90, "step two")
	return jobs.Result{Data: []byte("done")}, nil
}

// TestRedisQueue_ProgressReporting proves ProgressFn calls made from inside
// a Handler running under a real asynq.Server reach a caller polling Get()
// against real Redis -- this package's mapping of progress reporting onto
// asynq.Task.ResultWriter (AGENTS.md's "Progress reporting" section), the
// distributed deployment mode's counterpart of demo_queue_test.go's
// TestProgressReporting. Unlike that unit-tier-adjacent test (DemoQueue's
// dispatcher is in-process), this proves the write actually round-trips
// through Redis: ResultWriter.Write on one goroutine (asynq's worker) and
// Inspector.GetTaskInfo on another (this test's own polling), the real
// shape a production caller (an HTTP handler backing useJob(jobId), per
// docs/internal/07-platform-services.md) would see.
func TestRedisQueue_ProgressReporting(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	h := &progressHandler{afterFirstReport: make(chan struct{}), resume: make(chan struct{})}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-a")
	id, err := q.Enqueue(context.Background(), jobs.Task{Type: "progress", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	select {
	case <-h.afterFirstReport:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never reported its first progress update")
	}

	tctx := tenantCtx(tenant)
	mid := pollUntil(t, tctx, q, id, 5*time.Second, func(j *jobs.Job) bool { return j.ProgressPct == 30 })
	if mid.ProgressMsg != "step one" {
		t.Errorf("mid-flight ProgressMsg = %q, want %q", mid.ProgressMsg, "step one")
	}
	if mid.Status != jobs.StatusRunning {
		t.Errorf("Status while progress is mid-flight = %v, want %v", mid.Status, jobs.StatusRunning)
	}
	if mid.StartedAt == nil {
		t.Error("StartedAt = nil while the Job is Running, want it set")
	}

	close(h.resume)
	final := waitForTerminal(t, tctx, q, id, 10*time.Second)
	if final.ProgressPct != 90 || final.ProgressMsg != "step two" {
		t.Errorf("final progress = (%d, %q), want (90, %q)", final.ProgressPct, final.ProgressMsg, "step two")
	}
	if final.Result == nil || string(final.Result.Data) != "done" {
		t.Errorf("final Result = %+v, want Data = %q", final.Result, "done")
	}
}
