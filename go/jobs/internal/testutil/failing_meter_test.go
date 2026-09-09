package testutil

import (
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// This file drives the failing-meter stub (failing_meter.go) -- the
// instrument-creation fault seam the queue implementations' fail-open
// metric-registration paths are tested through -- from its own package,
// the same same-package exercise metrics_test.go gives the assertion
// helpers.

// TestFailingMeter_FailsOnlyTheNamedInstruments pins the stub's contract:
// creation of a named instrument fails with the configured error, while
// every other instrument -- same type or not -- still creates through the
// noop fallback.
func TestFailingMeter_FailsOnlyTheNamedInstruments(t *testing.T) {
	boom := errors.New("boom")
	meter := NewFailingMeter(map[string]error{"jobs.queue.depth": boom})

	if _, err := meter.Int64ObservableGauge("jobs.queue.depth"); !errors.Is(err, boom) {
		t.Errorf("Int64ObservableGauge(jobs.queue.depth) error = %v, want %v", err, boom)
	}
	if _, err := meter.Int64Counter("jobs.queue.depth"); !errors.Is(err, boom) {
		t.Errorf("Int64Counter(jobs.queue.depth) error = %v, want %v (a name fails on every instrument type)", err, boom)
	}
	if _, err := meter.Float64Histogram("jobs.queue.depth"); !errors.Is(err, boom) {
		t.Errorf("Float64Histogram(jobs.queue.depth) error = %v, want %v", err, boom)
	}

	if _, err := meter.Int64Counter("jobs.job.attempts"); err != nil {
		t.Errorf("Int64Counter(unfailing name) error = %v, want nil (the noop fallback)", err)
	}
	if _, err := meter.Float64Histogram("jobs.job.duration"); err != nil {
		t.Errorf("Float64Histogram(unfailing name) error = %v, want nil", err)
	}
}

// TestFailingMeterProvider_MeterReturnsTheSharedFailingMeter pins the
// provider half: otel.SetMeterProvider accepts it, and otel.Meter hands
// out the failing meter.
func TestFailingMeterProvider_MeterReturnsTheSharedFailingMeter(t *testing.T) {
	boom := errors.New("boom")
	meter := NewFailingMeter(map[string]error{"jobs.job.duration": boom})
	otel.SetMeterProvider(NewFailingMeterProvider(meter))

	if _, err := otel.Meter("any").Float64Histogram("jobs.job.duration"); !errors.Is(err, boom) {
		t.Errorf("Float64Histogram via the global provider error = %v, want %v", err, boom)
	}
	var _ metric.MeterProvider = FailingMeterProvider{}
}
