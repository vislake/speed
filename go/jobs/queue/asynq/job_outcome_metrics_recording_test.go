package asynq

import (
	"context"
	"errors"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/internal/testutil"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file holds the regression proof that asynq.Queue emits the three
// jobs.job.* instruments beyond the "jobs.queue.depth" gauge -- the
// recorded fast-follow closing go/jobs/AGENTS.md's Known-limitations gap
// ("Only jobs.queue.depth is wired; jobs.job.duration / jobs.job.attempts
// / jobs.job.dead_letter ... are not"; docs/internal/17-risks.md's
// task-queue row). The recording sites are worker.go's
// processTaskUncancelled (success) and handleErrorAttempt (the replicated
// archive-vs-retry boundary that decides every failed attempt's outcome),
// so the failure-half of the proof is fully unit-testable against a bare
// *Queue with no Redis at all (newTestQueue's contract); the success-half
// site needs a real ResultWriter and is proven by the module's integration
// tier (go/jobs/integration_test/job_metrics_test.go) against a real
// asynq/Redis pipeline, exactly like the rest of processTask's success
// path. Named for the behaviour it verifies, per the backend coding
// standard's test-naming rule, since it exercises worker.go and queue.go
// together.

// TestQueue_JobMetrics_RegistrationWiresTheThreeJobInstruments is
// registerJobMetrics's smoke proof, mirroring StandaloneQueue's own
// job-metrics coverage (jobs' standalone_queue_test.go): after
// registration and one record on each instrument, a Collect answers all
// three by name -- the rows that simply did not exist on this Queue
// before the fix. Each instrument records one data point first because an
// OTel Counter/Histogram that was never recorded emits no metric at all
// on Collect (there is no proactive zero-valued row), so presence is only
// observable after a record; the recorders' own semantics are pinned by
// the behavioural tests below, which only need presence here.
func TestQueue_JobMetrics_RegistrationWiresTheThreeJobInstruments(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	q := newTestQueue(t)
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}

	q.recordJobMetrics("smoke", jobs.StatusSucceeded, 5*time.Millisecond)
	q.recordJobMetricsAttemptOnly("smoke", jobs.StatusRetrying)
	q.recordDeadLetter("smoke")

	for _, name := range []string{jobDurationMetricName, jobAttemptsMetricName, jobDeadLetterMetricName} {
		testutil.CollectMetric(t, reader, name) // fails the test when missing.
	}
}

// TestQueue_JobMetrics_RetryableFailure_RecordsRetryingOutcome pins the
// retrying half of the outcome accounting: a genuine failed attempt with
// retries remaining records the StatusRetrying row of the
// jobs.job.attempts Counter and one duration data point on the
// jobs.job.duration Histogram (the duration rides on the wrapped error
// processTaskUncancelled returns -- see failedAttemptError's own doc
// comment), fires no FailureHook, and records no dead letter. The attempt
// duration is fabricated in the fixture because handleErrorAttempt only
// receives the error; what is under test is that the record happens at the
// retry decision with the carried duration.
func TestQueue_JobMetrics_RetryableFailure_RecordsRetryingOutcome(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	q := newTestQueue(t)
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}
	h := &recordingFailureHook{jobType: "flaky"}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("flaky", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleErrorAttempt(context.Background(), task, wrapFailedAttempt(errors.New("attempt failed"), 250*time.Millisecond), 1 /* retried */, 3 /* maxRetry */, "job-1", nil, obs.FromContext(context.Background()))

	if len(h.calls) != 0 {
		t.Errorf("OnFailure called %d times, want 0 for a retryable failure", len(h.calls))
	}
	attempts := testutil.CollectMetric(t, reader, jobAttemptsMetricName)
	if got := testutil.CounterValue(t, attempts, "flaky", string(jobs.StatusRetrying)); got != 1 {
		t.Errorf("%s{job_type=flaky,status=retrying} = %d, want 1", jobAttemptsMetricName, got)
	}
	duration := testutil.CollectMetric(t, reader, jobDurationMetricName)
	if got := testutil.HistogramCount(t, duration, "flaky", string(jobs.StatusRetrying)); got != 1 {
		t.Errorf("%s{job_type=flaky,status=retrying} count = %d, want 1", jobDurationMetricName, got)
	}
	if got := counterValueOrAbsent(t, reader, jobDeadLetterMetricName, "flaky", ""); got != 0 {
		t.Errorf("%s{job_type=flaky} = %d, want no dead-letter record for a retryable failure", jobDeadLetterMetricName, got)
	}
}

// TestQueue_JobMetrics_TerminalFailure_RecordsDeadLetterOutcome pins the
// dead-letter half: a genuine failed attempt whose retries are exhausted
// (retried == maxRetry, the exact archive-boundary handleErrorAttempt
// replicates) records the jobs.job.dead_letter Counter, the
// StatusDeadLetter row of the attempts Counter and one duration data
// point, and fires the FailureHook exactly as before. Fails on the
// pre-fix code, where no instrument exists to record onto.
func TestQueue_JobMetrics_TerminalFailure_RecordsDeadLetterOutcome(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	q := newTestQueue(t)
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}
	h := &recordingFailureHook{jobType: "always-fails"}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("always-fails", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleErrorAttempt(context.Background(), task, wrapFailedAttempt(errors.New("permanent failure"), 900*time.Millisecond), 3 /* retried */, 3 /* maxRetry */, "job-1", nil, obs.FromContext(context.Background()))

	if len(h.calls) != 1 {
		t.Errorf("OnFailure called %d times, want exactly 1", len(h.calls))
	}
	deadLetter := testutil.CollectMetric(t, reader, jobDeadLetterMetricName)
	if got := testutil.CounterValue(t, deadLetter, "always-fails", ""); got != 1 {
		t.Errorf("%s{job_type=always-fails} = %d, want 1", jobDeadLetterMetricName, got)
	}
	attempts := testutil.CollectMetric(t, reader, jobAttemptsMetricName)
	if got := testutil.CounterValue(t, attempts, "always-fails", string(jobs.StatusDeadLetter)); got != 1 {
		t.Errorf("%s{job_type=always-fails,status=dead_letter} = %d, want 1", jobAttemptsMetricName, got)
	}
	duration := testutil.CollectMetric(t, reader, jobDurationMetricName)
	if got := testutil.HistogramCount(t, duration, "always-fails", string(jobs.StatusDeadLetter)); got != 1 {
		t.Errorf("%s{job_type=always-fails,status=dead_letter} count = %d, want 1", jobDurationMetricName, got)
	}
}

// TestQueue_JobMetrics_TerminalAttemptBounce_RecordsDeadLetterWithoutDuration
// pins the archive-bound terminal bounce: a tenant-concurrency bounce (or
// cancellation-marker refusal) landing on the job's final allowed attempt
// is archived by asynq's own dispatch loop right after this hook returns
// -- a dead letter DeadLetterJobs will list -- so the dead-letter Counter
// and the StatusDeadLetter attempts row are recorded for it exactly like
// any terminal failure's, with one deliberate difference: no duration data
// point, since no Handle ran for the bounced attempt (recordJobMetrics'
// attempt-only branch; queue.go's recordJobMetricsAttemptOnly doc comment
// has the full argument). The FailureHook fires as before -- this is the
// existing terminal-bounce behaviour, with the metric record added
// alongside it.
func TestQueue_JobMetrics_TerminalAttemptBounce_RecordsDeadLetterWithoutDuration(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	q := newTestQueue(t)
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}
	h := &recordingFailureHook{jobType: "contended"}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("contended", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleErrorAttempt(context.Background(), task, errTenantAtCapacity, 3 /* retried */, 3 /* maxRetry */, "job-1", nil, obs.FromContext(context.Background()))

	if len(h.calls) != 1 {
		t.Errorf("OnFailure called %d times, want exactly 1 for an archive-bound terminal bounce", len(h.calls))
	}
	deadLetter := testutil.CollectMetric(t, reader, jobDeadLetterMetricName)
	if got := testutil.CounterValue(t, deadLetter, "contended", ""); got != 1 {
		t.Errorf("%s{job_type=contended} = %d, want 1 (the archive is a dead letter DeadLetterJobs will list)", jobDeadLetterMetricName, got)
	}
	attempts := testutil.CollectMetric(t, reader, jobAttemptsMetricName)
	if got := testutil.CounterValue(t, attempts, "contended", string(jobs.StatusDeadLetter)); got != 1 {
		t.Errorf("%s{job_type=contended,status=dead_letter} = %d, want 1", jobAttemptsMetricName, got)
	}
	if got := histogramCountOrAbsent(t, reader, jobDurationMetricName, "contended", string(jobs.StatusDeadLetter)); got != 0 {
		t.Errorf("%s{job_type=contended,status=dead_letter} count = %d, want no duration point (no Handle ran for the bounced attempt)", jobDurationMetricName, got)
	}
}

// TestQueue_JobMetrics_RetryableBounce_RecordsNothing pins the bounce
// exclusion on the retryable side: a tenant-concurrency bounce with
// retries remaining is not a failure in the metrics' sense -- no Handle
// ran, no retry budget was consumed, the task silently redelivers on the
// throttle delay -- so it records no attempt outcome at all, exactly as it
// logs nothing today. The expected-absent assertions use the OrZero
// helpers because a Counter never incremented for a label combination
// emits no data point.
func TestQueue_JobMetrics_RetryableBounce_RecordsNothing(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	q := newTestQueue(t)
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("contended", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleErrorAttempt(context.Background(), task, errTenantAtCapacity, 1 /* retried */, 3 /* maxRetry */, "job-1", nil, obs.FromContext(context.Background()))

	if got := counterValueOrAbsent(t, reader, jobAttemptsMetricName, "contended", string(jobs.StatusRetrying)); got != 0 {
		t.Errorf("%s{job_type=contended,status=retrying} = %d, want no record for a retryable bounce", jobAttemptsMetricName, got)
	}
	if got := counterValueOrAbsent(t, reader, jobAttemptsMetricName, "contended", string(jobs.StatusDeadLetter)); got != 0 {
		t.Errorf("%s{job_type=contended,status=dead_letter} = %d, want no record for a retryable bounce", jobAttemptsMetricName, got)
	}
	if got := counterValueOrAbsent(t, reader, jobDeadLetterMetricName, "contended", ""); got != 0 {
		t.Errorf("%s{job_type=contended} = %d, want no dead-letter record for a retryable bounce", jobDeadLetterMetricName, got)
	}
}

// TestQueue_JobMetrics_CancelWinsOverTerminalFailure_RecordsNothing pins
// the cancel-wins metric discipline: a Cancel that landed during the
// terminal attempt settles the Job as StatusCancelled (the marker
// handleError read back), so the failure's dead-letter outcome is
// discarded and recorded under none of the outcome counters -- the mirror
// of StandaloneQueue's own "a cancelled Job shows up in none of the
// outcome logs or jobs.job.attempts/jobs.job.dead_letter counters"
// discipline (jobs' worker.go).
func TestQueue_JobMetrics_CancelWinsOverTerminalFailure_RecordsNothing(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	q := newTestQueue(t)
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}
	h := &recordingFailureHook{jobType: "always-fails"}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("always-fails", nil, map[string]string{headerTenantID: "tenant-a"})
	cancelledAt := time.Now()

	q.handleErrorAttempt(context.Background(), task, wrapFailedAttempt(errors.New("permanent failure"), time.Second), 3 /* retried */, 3 /* maxRetry */, "job-1", &cancelledAt, obs.FromContext(context.Background()))

	if len(h.calls) != 0 {
		t.Errorf("OnFailure called %d times, want 0: a concurrent Cancel already settled the Job as StatusCancelled", len(h.calls))
	}
	if got := counterValueOrAbsent(t, reader, jobAttemptsMetricName, "always-fails", string(jobs.StatusDeadLetter)); got != 0 {
		t.Errorf("%s{job_type=always-fails,status=dead_letter} = %d, want no record for the cancelled Job's discarded failure", jobAttemptsMetricName, got)
	}
	if got := counterValueOrAbsent(t, reader, jobDeadLetterMetricName, "always-fails", ""); got != 0 {
		t.Errorf("%s{job_type=always-fails} = %d, want no dead-letter record for the cancelled Job", jobDeadLetterMetricName, got)
	}
	if got := histogramCountOrAbsent(t, reader, jobDurationMetricName, "always-fails", string(jobs.StatusDeadLetter)); got != 0 {
		t.Errorf("%s{job_type=always-fails,status=dead_letter} count = %d, want no duration point for the cancelled Job", jobDurationMetricName, got)
	}
}

// TestQueue_JobMetrics_UnwrappedFailure_RecordsAttemptWithoutDuration pins
// the attempt-only branch for an error that carries no measured duration:
// an error reaching handleErrorAttempt without failedAttemptError's
// wrapper is one asynq's own processor recovered (a panicked Handle) or a
// pre-Handle refusal that was never wrapped -- the outcome is still an
// attempt and still records its StatusRetrying row, only without a
// duration data point (no attempt duration was measured for it).
func TestQueue_JobMetrics_UnwrappedFailure_RecordsAttemptWithoutDuration(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	q := newTestQueue(t)
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("panicky", nil, map[string]string{headerTenantID: "tenant-a"})

	q.handleErrorAttempt(context.Background(), task, errors.New("panic: boom"), 1 /* retried */, 3 /* maxRetry */, "job-1", nil, obs.FromContext(context.Background()))

	attempts := testutil.CollectMetric(t, reader, jobAttemptsMetricName)
	if got := testutil.CounterValue(t, attempts, "panicky", string(jobs.StatusRetrying)); got != 1 {
		t.Errorf("%s{job_type=panicky,status=retrying} = %d, want 1", jobAttemptsMetricName, got)
	}
	if got := histogramCountOrAbsent(t, reader, jobDurationMetricName, "panicky", string(jobs.StatusRetrying)); got != 0 {
		t.Errorf("%s{job_type=panicky,status=retrying} count = %d, want no duration point (no attempt duration was measured)", jobDurationMetricName, got)
	}
}

// TestQueue_JobMetrics_UnregisteredHandler_ErrorSurvivesWrapAndRecords pins
// the real failure path end to end at the unit level: processTaskUncancelled
// refuses an unregistered handler type with the wrapped
// jobs.ErrHandlerNotRegistered (the wrapper must be transparent to
// apperr.As -- the pre-existing test TestQueue_ProcessTask_HandlerNotRegistered
// pins the same property through the same call), and handing that same
// error to handleErrorAttempt records the retrying outcome exactly like
// any other genuine failure -- mirroring StandaloneQueue's own counting of
// ErrHandlerNotRegistered attempts (jobs' worker.go's execute treats them
// as ordinary Handle failures).
func TestQueue_JobMetrics_UnregisteredHandler_ErrorSurvivesWrapAndRecords(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	q := newTestQueue(t)
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("no-such-type", nil, map[string]string{headerTenantID: "tenant-a"})

	err := q.processTaskUncancelled(context.Background(), task, "job-1", obs.FromContext(context.Background()))
	if !apperr.HasCode(err, jobs.ErrHandlerNotRegistered.Code) {
		t.Fatalf("processTaskUncancelled() error = %v, want ErrHandlerNotRegistered (code %q) through the duration wrapper", err, jobs.ErrHandlerNotRegistered.Code)
	}

	q.handleErrorAttempt(context.Background(), task, err, 1 /* retried */, 3 /* maxRetry */, "job-1", nil, obs.FromContext(context.Background()))

	attempts := testutil.CollectMetric(t, reader, jobAttemptsMetricName)
	if got := testutil.CounterValue(t, attempts, "no-such-type", string(jobs.StatusRetrying)); got != 1 {
		t.Errorf("%s{job_type=no-such-type,status=retrying} = %d, want 1 (an unregistered-handler attempt is an attempt)", jobAttemptsMetricName, got)
	}
}

// TestQueue_JobMetrics_CancelledBeforeDispatch_RecordsNoSuccess pins the
// never-dispatched cancellation exclusion on the success side: a Job whose
// cancellation marker exists is skipped before Handle (dispatchAfterMarkerRead
// returns nil) and must record nothing -- it is not a successful attempt,
// and StandaloneQueue's equivalent never-dispatched cancellation records
// nothing either. A success that genuinely ran would record through
// processTaskUncancelled; the assert-no-Succeeded-row check here uses the
// OrZero helpers for the same never-incremented reason the other
// expected-absent assertions do.
func TestQueue_JobMetrics_CancelledBeforeDispatch_RecordsNoSuccess(t *testing.T) {
	reader := testutil.SetupTestMeterProvider(t)
	q := newTestQueue(t)
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}
	task := asynqlib.NewTaskWithHeaders("skipped", nil, map[string]string{headerTenantID: "tenant-a"})
	cancelledAt := time.Now()

	if err := q.dispatchAfterMarkerRead(context.Background(), task, "job-1", obs.FromContext(context.Background()), &cancelledAt, nil); err != nil {
		t.Fatalf("dispatchAfterMarkerRead() error = %v, want nil (the marker skip is a clean, unrecorded no-op)", err)
	}

	if got := counterValueOrAbsent(t, reader, jobAttemptsMetricName, "skipped", string(jobs.StatusSucceeded)); got != 0 {
		t.Errorf("%s{job_type=skipped,status=succeeded} = %d, want no success record for a Job that never reached Handle", jobAttemptsMetricName, got)
	}
}

// counterValueOrAbsent collects name optionally (an instrument never
// recorded emits no metric at all on Collect -- see testutil.CollectOptional's
// doc comment) and returns the Counter value for (jobType, status),
// treating a wholly absent instrument or label combination as zero. The
// expected-absent assertions throughout this file use it because an
// outcome that must not be counted often leaves the instrument with no
// data point at all.
func counterValueOrAbsent(t *testing.T, reader *sdkmetric.ManualReader, name, jobType, status string) int64 {
	t.Helper()
	m, ok := testutil.CollectOptional(t, reader, name)
	if !ok {
		return 0
	}
	v, _ := testutil.CounterValueOrZero(t, m, jobType, status)
	return v
}

// histogramCountOrAbsent is counterValueOrAbsent's Histogram twin.
func histogramCountOrAbsent(t *testing.T, reader *sdkmetric.ManualReader, name, jobType, status string) uint64 {
	t.Helper()
	m, ok := testutil.CollectOptional(t, reader, name)
	if !ok {
		return 0
	}
	v, _ := testutil.HistogramCountOrZero(t, m, jobType, status)
	return v
}
