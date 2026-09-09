package testutil

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// This package's helpers assert on metrics recorded through a real
// SDK MeterProvider (SetupTestMeterProvider's ManualReader). This file
// gives those helpers their own same-package exercise -- the only way
// their statements are measured by the module's unit gate, since the
// queue implementations' metric tests run them from other packages'
// test binaries -- by recording one Counter and one Histogram through
// the same otel.Meter(jobs.InstrumentationName)-style global path the
// helpers' consumers use, then reading every accessor back against the
// collected ResourceMetrics.

// TestMetricsHelpers_RecordedAndAbsentDataPoints drives every accessor on
// a real collection: one Counter incremented under two label sets, one
// Histogram with two observations under one label set, and one label set
// deliberately never recorded, so both the found and the absent halves of
// CounterValueOrZero/HistogramCountOrZero execute.
func TestMetricsHelpers_RecordedAndAbsentDataPoints(t *testing.T) {
	reader := SetupTestMeterProvider(t)
	meter := otel.Meter("metrics-helpers-test")

	counter, err := meter.Int64Counter("helpers.counter")
	if err != nil {
		t.Fatalf("Int64Counter() error = %v", err)
	}
	hist, err := meter.Float64Histogram("helpers.histogram")
	if err != nil {
		t.Fatalf("Float64Histogram() error = %v", err)
	}

	ctx := context.Background()
	counter.Add(ctx, 3, metric.WithAttributes(attribute.String("job_type", "alpha"), attribute.String("status", "succeeded")))
	counter.Add(ctx, 5, metric.WithAttributes(attribute.String("job_type", "beta"), attribute.String("status", "dead_letter")))
	hist.Record(ctx, 2.5, metric.WithAttributes(attribute.String("job_type", "alpha"), attribute.String("status", "succeeded")))
	hist.Record(ctx, 1.5, metric.WithAttributes(attribute.String("job_type", "alpha"), attribute.String("status", "succeeded")))

	counterMetric := CollectMetric(t, reader, "helpers.counter")
	histMetric := CollectMetric(t, reader, "helpers.histogram")

	if got := CounterValue(t, counterMetric, "alpha", "succeeded"); got != 3 {
		t.Errorf("CounterValue(alpha/succeeded) = %d, want 3", got)
	}
	if got := CounterValue(t, counterMetric, "beta", "dead_letter"); got != 5 {
		t.Errorf("CounterValue(beta/dead_letter) = %d, want 5", got)
	}
	// A label combination never recorded emits no data point: the OrZero
	// accessor must answer (0, false), and the non-OrZero accessor must not
	// be asked about it.
	if got, ok := CounterValueOrZero(t, counterMetric, "alpha", "dead_letter"); ok || got != 0 {
		t.Errorf("CounterValueOrZero(alpha/dead_letter) = (%d, %v), want (0, false)", got, ok)
	}
	// The status-ignoring form (empty status) matches the single point with
	// the job_type label alone, whatever its status.
	if got, ok := CounterValueOrZero(t, counterMetric, "beta", ""); !ok || got != 5 {
		t.Errorf("CounterValueOrZero(beta, any status) = (%d, %v), want (5, true)", got, ok)
	}

	if got := HistogramCount(t, histMetric, "alpha", "succeeded"); got != 2 {
		t.Errorf("HistogramCount(alpha/succeeded) = %d, want 2 observations", got)
	}
	if got, ok := HistogramCountOrZero(t, histMetric, "beta", "succeeded"); ok || got != 0 {
		t.Errorf("HistogramCountOrZero(beta/succeeded) = (%d, %v), want (0, false)", got, ok)
	}
	if got, ok := HistogramCountOrZero(t, histMetric, "alpha", "succeeded"); !ok || got != 2 {
		t.Errorf("HistogramCountOrZero(alpha/succeeded) = (%d, %v), want (2, true)", got, ok)
	}

	// AttrString reads one label off a real data point: locate the alpha
	// point by its labels (data point order follows attribute ordering, not
	// recording order) and read its value through the same accessor the
	// package's consumers use.
	sum, _ := counterMetric.Data.(metricdata.Sum[int64])
	readAlpha := int64(-1)
	for _, dp := range sum.DataPoints {
		if AttrString(dp.Attributes, "job_type") == "alpha" {
			readAlpha = dp.Value
		}
	}
	if readAlpha != 3 {
		t.Errorf("value of the alpha data point read via AttrString = %d, want 3", readAlpha)
	}
}

// TestMetricsHelpers_CollectOptionalAbsent pins CollectOptional's
// expected-absent answer: a metric never recorded emits no metric at all
// on Collect, so the optional accessor reports (zero Metrics, false)
// rather than failing the test.
func TestMetricsHelpers_CollectOptionalAbsent(t *testing.T) {
	reader := SetupTestMeterProvider(t)

	// Nothing was recorded at all: every name is absent.
	if _, ok := CollectOptional(t, reader, "never.recorded"); ok {
		t.Error("CollectOptional(never.recorded) = (_, true), want (_, false)")
	}

	// Record one metric, then check an absent sibling alongside it.
	meter := otel.Meter("metrics-helpers-optional-test")
	counter, err := meter.Int64Counter("helpers.recorded")
	if err != nil {
		t.Fatalf("Int64Counter() error = %v", err)
	}
	counter.Add(context.Background(), 1)
	if m, ok := CollectOptional(t, reader, "helpers.recorded"); !ok {
		t.Error("CollectOptional(helpers.recorded) = (_, false), want (_, true)")
	} else if got := CounterValue(t, m, "", ""); got != 1 {
		t.Errorf("CounterValue(recorded, no labels) = %d, want 1", got)
	}
}

// TestMetricNames_ListsEveryRecordedName drives MetricNames -- the failure
// message's listing helper -- directly, so its scan of a real collection
// executes rather than only ever running inside a failing test's message.
func TestMetricNames_ListsEveryRecordedName(t *testing.T) {
	reader := SetupTestMeterProvider(t)
	meter := otel.Meter("metrics-helpers-names-test")

	first, err := meter.Int64Counter("helpers.names.one")
	if err != nil {
		t.Fatalf("Int64Counter() error = %v", err)
	}
	second, err := meter.Int64Counter("helpers.names.two")
	if err != nil {
		t.Fatalf("Int64Counter() error = %v", err)
	}
	first.Add(context.Background(), 1)
	second.Add(context.Background(), 1)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	names := MetricNames(rm)
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	for _, want := range []string{"helpers.names.one", "helpers.names.two"} {
		if !found[want] {
			t.Errorf("MetricNames() = %v, want it to include %q", names, want)
		}
	}
	if m := MetricByName(t, rm, "helpers.names.one"); m == nil {
		t.Error("MetricByName(helpers.names.one) = nil, want the recorded metric")
	}
	if m := MetricByName(t, rm, "helpers.names.never"); m != nil {
		t.Errorf("MetricByName(helpers.names.never) = %+v, want nil", m)
	}
}
