// Package testutil holds test helpers shared across this module's own test
// files, per the backend coding standard's "put shared test helpers in a
// dedicated internal/testutil package, never duplicated" rule. It sits at
// go/jobs/internal/testutil so both the root jobs package's own tests and
// the queue/asynq subpackage's tests may import it (Go's internal-package
// visibility rule allows any package rooted under go/jobs to import
// anything under go/jobs/internal), without exporting it to consumers
// outside this module.
package testutil

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// MetricByName returns the metric named name within rm, or nil when rm
// carries no such metric. Shared by StandaloneQueue's and the queue/asynq
// subpackage's Queue's identically-shaped "jobs.queue.depth" gauge
// lifecycle tests.
func MetricByName(t *testing.T, rm metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for i := range sm.Metrics {
			if sm.Metrics[i].Name == name {
				return &sm.Metrics[i]
			}
		}
	}
	return nil
}

// MetricNames lists every metric name present in rm, for failure messages.
func MetricNames(rm metricdata.ResourceMetrics) []string {
	var names []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
		}
	}
	return names
}

// SetupTestMeterProvider installs a real SDK MeterProvider backed by a
// ManualReader as the process-wide OTel provider for the duration of the
// test (never a mock, and never a Prometheus/OTLP exporter -- the tests
// that use this helper only need to read back exactly what was recorded),
// returning the reader to Collect from. Both Queue implementations'
// registerJobMetrics/registerQueueDepthGauge wire their instruments
// through otel.Meter(jobs.InstrumentationName), i.e. the global provider,
// so this is the one setup shape a metric assertion test needs; the
// reader is shut down via t.Cleanup.
func SetupTestMeterProvider(t *testing.T) *metric.ManualReader {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otel.SetMeterProvider(mp)
	return reader
}

// CollectMetric runs a fresh Collect and returns the single metric named
// name, failing the test if it is missing.
func CollectMetric(t *testing.T, reader *metric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	if m := MetricByName(t, rm, name); m != nil {
		return *m
	}
	t.Fatalf("metric %q not found; metrics present: %v", name, MetricNames(rm))
	return metricdata.Metrics{}
}

// CollectOptional is CollectMetric's non-fatal counterpart: it reports
// (zero Metrics, false) instead of failing the test when no metric named
// name was recorded. Use it for expected-absent assertions: an OTel
// Counter/Histogram that was never recorded emits no metric at all on
// Collect, so "this outcome must not be counted" checks have nothing to
// find when every record of that instrument was legitimately skipped.
func CollectOptional(t *testing.T, reader *metric.ManualReader, name string) (metricdata.Metrics, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	if m := MetricByName(t, rm, name); m != nil {
		return *m, true
	}
	return metricdata.Metrics{}, false
}

// AttrString reads key out of attrs as a plain string, for comparing
// against a metric data point's own Attributes.
func AttrString(attrs attribute.Set, key string) string {
	v, _ := attrs.Value(attribute.Key(key))
	return v.AsString()
}

// CounterValue returns the int64 Sum value of m's data point labeled
// exactly by jobType and status (status ignored when empty), failing the
// test if m is not a Sum[int64] or no matching data point exists. Use this
// when the data point is expected to exist; a Counter never incremented
// for a given label combination emits no data point at all (there is no
// proactive zero-valued row), so an EXPECTED-ABSENT check must use
// CounterValueOrZero instead, not this function.
func CounterValue(t *testing.T, m metricdata.Metrics, jobType, status string) int64 {
	t.Helper()
	v, ok := CounterValueOrZero(t, m, jobType, status)
	if !ok {
		t.Fatalf("metric %q has no data point for job_type=%q status=%q", m.Name, jobType, status)
	}
	return v
}

// CounterValueOrZero is CounterValue's non-fatal counterpart: it reports
// (0, false) instead of failing the test when no data point matches, for
// asserting a label combination was deliberately never recorded.
func CounterValueOrZero(t *testing.T, m metricdata.Metrics, jobType, status string) (int64, bool) {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	for _, dp := range sum.DataPoints {
		if AttrString(dp.Attributes, "job_type") != jobType {
			continue
		}
		if status != "" && AttrString(dp.Attributes, "status") != status {
			continue
		}
		return dp.Value, true
	}
	return 0, false
}

// HistogramCount returns the observation Count of m's data point labeled
// exactly by jobType/status, failing the test if m is not a
// Histogram[float64] or no matching data point exists.
func HistogramCount(t *testing.T, m metricdata.Metrics, jobType, status string) uint64 {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Histogram[float64]", m.Name, m.Data)
	}
	for _, dp := range hist.DataPoints {
		if AttrString(dp.Attributes, "job_type") == jobType && AttrString(dp.Attributes, "status") == status {
			return dp.Count
		}
	}
	t.Fatalf("metric %q has no data point for job_type=%q status=%q; data points: %+v", m.Name, jobType, status, hist.DataPoints)
	return 0
}

// HistogramCountOrZero is HistogramCount's non-fatal counterpart: it
// reports (0, false) when no data point matches, for asserting a label
// combination was deliberately never recorded on the Histogram (the
// attempt-only records of the queue/asynq subpackage's job metrics, whose
// attempts Counter row exists without a duration data point).
func HistogramCountOrZero(t *testing.T, m metricdata.Metrics, jobType, status string) (uint64, bool) {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Histogram[float64]", m.Name, m.Data)
	}
	for _, dp := range hist.DataPoints {
		if AttrString(dp.Attributes, "job_type") == jobType && AttrString(dp.Attributes, "status") == status {
			return dp.Count, true
		}
	}
	return 0, false
}
