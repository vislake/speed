package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// This file holds the regression tests for execute's record-ordering
// discipline -- every outcome log line and metric record must fire strictly
// AFTER the conditional write that persisted the outcome reported the
// transition (worker.go's execute), never before. These two pin the
// success and retry halves; the dead-letter half lives with the other
// execute-level tests in worker_test.go's
// TestExecute_FinalFailureAfterCancel_RecordsNoDeadLetterLogOrMetric.
// Named for the behaviour they verify, since they exercise execute across
// worker.go and store.go.

// succeedingCancelRaceHandler always succeeds from Handle -- the
// success-path counterpart of worker_test.go's
// cancelledBeforeDeadLetterHandler -- and records nothing.
type succeedingCancelRaceHandler struct{}

func (*succeedingCancelRaceHandler) Type() string { return "cancel-race.succeed" }

func (*succeedingCancelRaceHandler) Handle(context.Context, *Job, ProgressFn) (Result, error) {
	return Result{Data: []byte("done")}, nil
}

var _ Handler = (*succeedingCancelRaceHandler)(nil)

// TestExecute_SuccessAfterCancel_RecordsNoSuccessMetricOrLog is the success
// half of the metric-ordering defect the dead-letter branch already fixed:
// execute recorded the "job succeeded" log line and the StatusSucceeded
// rows of the attempts/duration instruments BEFORE calling
// completeSucceeded, so a concurrent Cancel that no-op'd the write (the row
// was already StatusCancelled) still produced a fake success log and fake
// success metrics for a Job that never succeeded. The records must now fire
// strictly after completeSucceeded's transition report and only for a
// genuine running -> succeeded move, leaving the cancelled Job exactly one
// truthful record: the "job cancelled before its outcome could be recorded,
// outcome discarded" Info line carrying the discarded_outcome=succeeded
// attribute. Deterministic by construction, exactly like the dead-letter
// half: markCancelled lands before execute's success path runs, so a
// no-op'd write that still produced a success log or success metrics fails
// this test.
func TestExecute_SuccessAfterCancel_RecordsNoSuccessMetricOrLog(t *testing.T) {
	reader := setupTestMeterProvider(t)
	q := NewStandaloneQueue(newTestDB(t))
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}

	// Capture execute's log lines: execute derives its logger from
	// obs.FromContext over a freshly built context carrying no attached
	// logger, which falls back to slog.Default() read fresh per call -- so a
	// temporary slog.SetDefault is the module's established capture seam
	// (worker_test.go). No test in this package runs in parallel, so the
	// process-wide swap cannot leak into a concurrent test.
	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	h := &succeedingCancelRaceHandler{}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const jobType = "cancel-race.succeed"

	// Control first: a genuine success must still be logged and counted
	// exactly once -- and guarantees the data points exist to read back (a
	// Counter never Add()-ed emits no data point at all, which would make
	// the cancelled Job's expected absence indistinguishable from a
	// never-instrumented run).
	control := fixtureRunningRecord("tenant-a", jobType)
	// Both legs are seeded under this queue's own claim, like every other
	// direct-execute seed in the package: the outcome write that settles an
	// attempt carries claimed_by = owner.
	control.ClaimedBy = q.owner
	if err := q.db.Create(control).Error; err != nil {
		t.Fatalf("seed control running record: %v", err)
	}
	q.execute(*control)

	// The cancelled Job: Cancel (markCancelled) settles the row before the
	// attempt's success path runs -- the same deterministic race the
	// dead-letter tests construct.
	rec := fixtureRunningRecord("tenant-a", jobType)
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	if err := markCancelled(context.Background(), q.db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled() error = %v", err)
	}
	q.execute(*rec)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	got, err := q.Get(ctx, JobID(rec.ID))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusCancelled {
		t.Errorf("Status = %v, want %v (the success write must not overwrite the cancellation)", got.Status, StatusCancelled)
	}

	attempts := collectMetric(t, reader, jobAttemptsMetricName)
	if got := counterValue(t, attempts, jobType, string(StatusSucceeded)); got != 1 {
		t.Errorf("%s{job_type=%s,status=succeeded} = %d, want 1 (the control's genuine success only; the cancelled Job's discarded outcome must not be counted)", jobAttemptsMetricName, jobType, got)
	}
	duration := collectMetric(t, reader, jobDurationMetricName)
	if got := histogramCount(t, duration, jobType, string(StatusSucceeded)); got != 1 {
		t.Errorf("%s{job_type=%s,status=succeeded} count = %d, want 1 (the control's genuine success only)", jobDurationMetricName, jobType, got)
	}

	out := buf.String()
	if got := strings.Count(out, "job succeeded"); got != 1 {
		t.Errorf(`"job succeeded" log line appears %d times, want exactly 1 (the control's genuine success only)`, got)
	}
	if !strings.Contains(out, "job cancelled before its outcome could be recorded, outcome discarded") {
		t.Error("missing the cancelled-outcome Info line -- the one truthful record a cancelled Job's discarded success is allowed to emit")
	}
	if !strings.Contains(out, "discarded_outcome=succeeded") {
		t.Error("cancelled-outcome Info line lacks the discarded_outcome=succeeded attribute naming the success record the cancellation discarded")
	}
}

// failingCancelRaceHandler always fails from Handle but with retries
// remaining, so execute's retry path (not its dead-letter path) settles the
// attempt -- the retry-path counterpart of the two handlers above.
type failingCancelRaceHandler struct{}

func (*failingCancelRaceHandler) Type() string { return "cancel-race.retry" }

func (*failingCancelRaceHandler) Handle(context.Context, *Job, ProgressFn) (Result, error) {
	return Result{}, errors.New("transient failure")
}

var _ Handler = (*failingCancelRaceHandler)(nil)

// TestExecute_RetryAfterCancel_RecordsNoRetryMetricOrLog is the retry half
// of the same metric-ordering defect: execute recorded the "job attempt
// failed, scheduling retry" log line and the StatusRetrying rows of the
// attempts/duration instruments BEFORE calling completeRetrying, so a
// concurrent Cancel that no-op'd the write still produced a fake retry log
// and fake retry metrics for a Job that was cancelled, never retrying. The
// records must now fire strictly after completeRetrying's transition report
// and only for a genuine running -> retrying move, leaving the cancelled
// Job exactly one truthful record: the "job cancelled before its outcome
// could be recorded, outcome discarded" Info line carrying the
// discarded_outcome=retrying attribute. Deterministic by construction,
// exactly like the success and dead-letter halves.
func TestExecute_RetryAfterCancel_RecordsNoRetryMetricOrLog(t *testing.T) {
	reader := setupTestMeterProvider(t)
	q := NewStandaloneQueue(newTestDB(t))
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}

	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	h := &failingCancelRaceHandler{}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const jobType = "cancel-race.retry"

	// Control first: a genuine retry must still be logged and counted
	// exactly once (and guarantees the data points exist to read back).
	control := fixtureRunningRecord("tenant-a", jobType)
	control.MaxRetries = 5
	control.Attempts = 1 // matches the post-handoff state: runAttempt counted this first attempt
	control.ClaimedBy = q.owner
	if err := q.db.Create(control).Error; err != nil {
		t.Fatalf("seed control running record: %v", err)
	}
	q.execute(*control)

	// The cancelled Job: the identical deterministic race.
	rec := fixtureRunningRecord("tenant-a", jobType)
	rec.MaxRetries = 5
	rec.Attempts = 1
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	if err := markCancelled(context.Background(), q.db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled() error = %v", err)
	}
	q.execute(*rec)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	got, err := q.Get(ctx, JobID(rec.ID))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusCancelled {
		t.Errorf("Status = %v, want %v (the retry write must not overwrite the cancellation)", got.Status, StatusCancelled)
	}

	attempts := collectMetric(t, reader, jobAttemptsMetricName)
	if got := counterValue(t, attempts, jobType, string(StatusRetrying)); got != 1 {
		t.Errorf("%s{job_type=%s,status=retrying} = %d, want 1 (the control's genuine retry only; the cancelled Job's discarded outcome must not be counted)", jobAttemptsMetricName, jobType, got)
	}
	duration := collectMetric(t, reader, jobDurationMetricName)
	if got := histogramCount(t, duration, jobType, string(StatusRetrying)); got != 1 {
		t.Errorf("%s{job_type=%s,status=retrying} count = %d, want 1 (the control's genuine retry only)", jobDurationMetricName, jobType, got)
	}

	out := buf.String()
	if got := strings.Count(out, "scheduling retry"); got != 1 {
		t.Errorf(`"scheduling retry" log line appears %d times, want exactly 1 (the control's genuine retry only)`, got)
	}
	if !strings.Contains(out, "job cancelled before its outcome could be recorded, outcome discarded") {
		t.Error("missing the cancelled-outcome Info line -- the one truthful record a cancelled Job's discarded failure is allowed to emit")
	}
	if !strings.Contains(out, "discarded_outcome=retrying") {
		t.Error("cancelled-outcome Info line lacks the discarded_outcome=retrying attribute naming the retry record the cancellation discarded")
	}
}
