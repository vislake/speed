//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// This file proves, against a real asynq.Server dequeuing from a real
// Redis, that the asynq.Queue emits the jobs.job.duration /
// jobs.job.attempts / jobs.job.dead_letter outcome metrics on a real
// run -- the integration half of go/jobs/queue/asynq's job_outcome_metrics_recording_test.go
// unit suite. The unit suite covers the failure-side recording site
// (handleErrorAttempt) and the registration, which need no Redis; the
// success-side site (processTaskUncancelled) needs a real ResultWriter and
// is therefore exercised here, exactly like the rest of processTask's
// success path lives in this package's real-backend tests.

// okHandler succeeds from Handle -- the Job whose genuine success must
// record the StatusSucceeded attempt row.
type metricOkHandler struct{}

func (*metricOkHandler) Type() string { return "metric-ok" }

func (*metricOkHandler) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	return jobs.Result{Data: []byte("done")}, nil
}

// metricFailHandler always fails Handle -- the Job whose retries are
// exhausted must record the StatusRetrying attempt, then the dead-letter
// outcome.
type metricFailHandler struct{}

func (*metricFailHandler) Type() string { return "metric-fail" }

func (*metricFailHandler) Handle(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
	return jobs.Result{}, errors.New("genuine business failure")
}

// TestRedisQueue_JobMetrics_SucceededAndDeadLetterAttemptsRecorded is the
// success-side and end-to-end half of the metric-triple proof: over one
// real boot, a succeeding Job and a retries-exhausted failing Job must
// leave the three instruments carrying the exact rows the unit suite pins
// for the failure side alone -- attempts{succeeded}, duration{succeeded},
// attempts{retrying} (the first failed attempt), attempts{dead_letter},
// duration{dead_letter} and the dead_letter counter, all labeled by the
// jobs' own types. All three instruments must carry their rows after one
// real boot (the CollectMetric calls below fail if a name is missing).
func TestRedisQueue_JobMetrics_SucceededAndDeadLetterAttemptsRecorded(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	ctx := context.Background()

	q := startTestAsynqQueue(t, ctx)
	if err := q.RegisterHandler(&metricOkHandler{}); err != nil {
		t.Fatalf("RegisterHandler(metric-ok) error = %v", err)
	}
	if err := q.RegisterHandler(&metricFailHandler{}); err != nil {
		t.Fatalf("RegisterHandler(metric-fail) error = %v", err)
	}

	okID, err := q.Enqueue(ctx, jobs.Task{Type: "metric-ok", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue(metric-ok) error = %v", err)
	}
	failID, err := q.Enqueue(ctx, jobs.Task{Type: "metric-fail", TenantID: "tenant-a"}, jobs.WithMaxRetries(1))
	if err != nil {
		t.Fatalf("Enqueue(metric-fail) error = %v", err)
	}

	ctxT := tenantCtx(pkgcore.TenantID("tenant-a"))
	if job := waitForTerminal(t, ctxT, q, okID, 10*time.Second); job.Status != jobs.StatusSucceeded {
		t.Fatalf("metric-ok Job status = %v, want %v", job.Status, jobs.StatusSucceeded)
	}
	if job := waitForTerminal(t, ctxT, q, failID, 10*time.Second); job.Status != jobs.StatusDeadLetter {
		t.Fatalf("metric-fail Job status = %v, want %v", job.Status, jobs.StatusDeadLetter)
	}

	attempts := testutil.CollectMetric(t, reader, "jobs.job.attempts")
	if got := testutil.CounterValue(t, attempts, "metric-ok", string(jobs.StatusSucceeded)); got != 1 {
		t.Errorf("jobs.job.attempts{job_type=metric-ok,status=succeeded} = %d, want 1", got)
	}
	if got := testutil.CounterValue(t, attempts, "metric-fail", string(jobs.StatusRetrying)); got != 1 {
		t.Errorf("jobs.job.attempts{job_type=metric-fail,status=retrying} = %d, want 1 (the first failed attempt schedules the retry)", got)
	}
	if got := testutil.CounterValue(t, attempts, "metric-fail", string(jobs.StatusDeadLetter)); got != 1 {
		t.Errorf("jobs.job.attempts{job_type=metric-fail,status=dead_letter} = %d, want 1", got)
	}
	duration := testutil.CollectMetric(t, reader, "jobs.job.duration")
	if got := testutil.HistogramCount(t, duration, "metric-ok", string(jobs.StatusSucceeded)); got != 1 {
		t.Errorf("jobs.job.duration{job_type=metric-ok,status=succeeded} count = %d, want 1", got)
	}
	if got := testutil.HistogramCount(t, duration, "metric-fail", string(jobs.StatusDeadLetter)); got != 1 {
		t.Errorf("jobs.job.duration{job_type=metric-fail,status=dead_letter} count = %d, want 1", got)
	}
	deadLetter := testutil.CollectMetric(t, reader, "jobs.job.dead_letter")
	if got := testutil.CounterValue(t, deadLetter, "metric-fail", ""); got != 1 {
		t.Errorf("jobs.job.dead_letter{job_type=metric-fail} = %d, want 1", got)
	}
}
