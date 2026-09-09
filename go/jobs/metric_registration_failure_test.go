package jobs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/vislake/speed/go/jobs/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// This file holds the fail-open metric-registration regressions: Start's
// documented contract that a metrics wiring failure warns and never
// prevents the queue itself from running, and registerJobMetrics'
// per-instrument error returns. The real SDK's Meter never fails an
// instrument creation, so every branch here is driven through
// testutil.NewFailingMeter, the instrument-creation fault seam (its own
// tests live in internal/testutil).
//
// Global provider note: these tests install a failing meter provider for
// their own duration, exactly like every other metric test here installs
// a real one via testutil.SetupTestMeterProvider -- the process-wide
// provider is each test's to set, and the queue-depth gauge registrations
// earlier tests left on their own (shut-down) providers are unaffected.

// TestRegisterJobMetrics_InstrumentCreationFailures_ReturnErrors covers
// registerJobMetrics' three error returns: the duration Histogram, the
// attempts Counter and the dead-letter Counter each failing to create
// must surface as that call's error, with the instruments registered
// before the failure left nil so the record-side nil guards (worker.go)
// keep the queue running.
func TestRegisterJobMetrics_InstrumentCreationFailures_ReturnErrors(t *testing.T) {
	cases := []struct {
		name string
		fail string // the instrument name whose creation fails
	}{
		{name: "duration histogram", fail: jobDurationMetricName},
		{name: "attempts counter", fail: jobAttemptsMetricName},
		{name: "dead-letter counter", fail: jobDeadLetterMetricName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meter := testutil.NewFailingMeter(map[string]error{tc.fail: testutil.ErrFailingMeterInstrument})
			otel.SetMeterProvider(testutil.NewFailingMeterProvider(meter))

			q := NewStandaloneQueue(newTestDB(t))
			if err := q.registerJobMetrics(); err == nil {
				t.Fatalf("registerJobMetrics() error = nil, want the %s creation failure to surface", tc.name)
			}
			if q.jobDuration != nil || q.jobAttempts != nil || q.jobDeadLetter != nil {
				t.Error("registerJobMetrics() left instruments set after failing, want them nil so the record-side nil guards hold")
			}
		})
	}
}

// TestStart_MetricRegistrationFails_WarnsAndQueueStillRuns covers Start's
// fail-open contract end to end: with both the queue-depth gauge and the
// job instruments refusing registration, Start logs the two warnings and
// still launches the queue -- a job enqueued and run to completion after
// the failed registration is the proof the queue genuinely runs.
func TestStart_MetricRegistrationFails_WarnsAndQueueStillRuns(t *testing.T) {
	meter := testutil.NewFailingMeter(map[string]error{
		"jobs.queue.depth":    testutil.ErrFailingMeterInstrument,
		jobDurationMetricName: testutil.ErrFailingMeterInstrument,
	})
	otel.SetMeterProvider(testutil.NewFailingMeterProvider(meter))

	q := NewStandaloneQueue(newTestDB(t),
		WithPollInterval(15*time.Millisecond),
		WithBackoff(20*time.Millisecond, 200*time.Millisecond),
	)
	handled := make(chan struct{}, 1)
	if err := q.RegisterHandler(NewHandlerFunc("metric-fail-open", func(context.Context, *Job, ProgressFn) (Result, error) {
		handled <- struct{}{}
		return Result{Data: []byte("done")}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer func() { slog.SetDefault(prevDefault) }()

	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil despite the metric registrations failing", err)
	}

	id, err := q.Enqueue(context.Background(), Task{Type: "metric-fail-open", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitSignal(t, handled, "Handle to run -- the queue must keep working after the failed metric registrations")
	// The handled signal fires inside Handle, before execute persists the
	// outcome -- poll until the row settles, then assert the outcome.
	job := pollJob(t, q, pkgcore.WithTenant(context.Background(), "tenant-a"), id, signalWaitTimeout, func(j *Job) bool {
		return j.Status.Terminal()
	})
	if job.Status != StatusSucceeded {
		t.Fatalf("Status = %v, want %v", job.Status, StatusSucceeded)
	}

	// The log assertions run only after Close has stopped every goroutine
	// that writes through the capture handler (the dispatcher, the workers
	// and the heartbeat keeper): reading the buffer while the live queue
	// still logs into it would race.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "registering queue depth gauge failed") {
		t.Errorf("Start() logged: %s, want the queue-depth-gauge warning", out)
	}
	if !strings.Contains(out, "registering job metrics failed") {
		t.Errorf("Start() logged: %s, want the job-metrics warning", out)
	}
}
