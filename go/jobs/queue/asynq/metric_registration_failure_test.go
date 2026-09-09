package asynq

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/internal/testutil"
)

// This file holds the distributed queue's fail-open metric-registration
// regressions -- the mirror of the root package's own
// metric_registration_failure_test.go: registerJobMetrics and
// registerQueueDepthGauge per-instrument error returns, and Start's
// documented warn-and-continue when those registrations fail. The real
// SDK's Meter never fails an instrument creation, so every branch is
// driven through testutil.NewFailingMeter, the instrument-creation fault
// seam (its own tests live in internal/testutil).

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

			q := newTestQueue(t)
			if err := q.registerJobMetrics(); err == nil {
				t.Fatalf("registerJobMetrics() error = nil, want the %s creation failure to surface", tc.name)
			}
			if q.jobDuration != nil || q.jobAttempts != nil || q.jobDeadLetter != nil {
				t.Error("registerJobMetrics() left instruments set after failing, want them nil so the record-side nil guards hold")
			}
		})
	}
}

// TestRegisterQueueDepthGauge_InstrumentCreationFails_ReturnsError covers
// the depth gauge's registration failure surfacing from the call -- the
// answer Start's warn-and-continue branch is built on.
func TestRegisterQueueDepthGauge_InstrumentCreationFails_ReturnsError(t *testing.T) {
	meter := testutil.NewFailingMeter(map[string]error{"jobs.queue.depth": testutil.ErrFailingMeterInstrument})
	otel.SetMeterProvider(testutil.NewFailingMeterProvider(meter))

	q := newTestQueue(t)
	if err := q.registerQueueDepthGauge(otel.Meter(jobs.InstrumentationName)); err == nil {
		t.Error("registerQueueDepthGauge() error = nil, want the instrument-creation failure to surface")
	}
}

// TestStart_MetricRegistrationsFail_WarnsAndServerStillLaunches covers
// Start's fail-open contract for the distributed queue: with both the
// queue-depth gauge and the job instruments refusing registration, Start
// logs the two warnings and still launches asynq's server (against the
// unreachable backend of the same shape unreachable_redis_test.go uses,
// since this test only asserts the launch answer, never a processed
// task). A metrics wiring failure must not prevent the queue from
// running.
func TestStart_MetricRegistrationsFail_WarnsAndServerStillLaunches(t *testing.T) {
	meter := testutil.NewFailingMeter(map[string]error{
		"jobs.queue.depth":    testutil.ErrFailingMeterInstrument,
		jobDurationMetricName: testutil.ErrFailingMeterInstrument,
	})
	otel.SetMeterProvider(testutil.NewFailingMeterProvider(meter))

	q := NewQueue(deadRedisOpt)
	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer func() { slog.SetDefault(prevDefault) }()

	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil despite the metric registrations failing", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	out := buf.String()
	if !strings.Contains(out, "registering queue depth gauge failed") {
		t.Errorf("Start() logged: %s, want the queue-depth-gauge warning", out)
	}
	if !strings.Contains(out, "registering job metrics failed") {
		t.Errorf("Start() logged: %s, want the job-metrics warning", out)
	}
}
