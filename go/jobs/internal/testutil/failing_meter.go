package testutil

import (
	"errors"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// FailingMeterProvider and FailingMeter let a test make instrument
// CREATION fail, per instrument name -- the one seam through which the
// queue implementations' fail-open metric-registration paths are
// reachable (Start's warn-and-continue on a gauge or counter that cannot
// be registered, and registerJobMetrics' per-instrument error returns):
// the real SDK's Meter never fails an instrument creation, so a real
// provider cannot drive those branches.
//
// Set a FailingMeterProvider as the process-wide OTel provider
// (otel.SetMeterProvider) exactly as SetupTestMeterProvider does, but
// with a meter that answers an error for every instrument whose name is
// in failNames and real (noop-backed) instruments for everything else.

// ErrFailingMeterInstrument is what FailingMeter's instrument creations
// fail with.
var ErrFailingMeterInstrument = errors.New("failing meter: instrument creation refused")

// FailingMeter is a metric.Meter whose instrument creation fails for the
// named instruments and delegates everything else to noop.Meter -- a real
// noop instrument is created for the non-failing names, so a sequence of
// registrations that only needs one of its instruments to fail keeps
// working for the rest.
type FailingMeter struct {
	noop.Meter
	failNames map[string]error
}

// NewFailingMeter returns a FailingMeter that fails creation of the
// instruments whose exact names appear in failNames.
func NewFailingMeter(failNames map[string]error) *FailingMeter {
	return &FailingMeter{failNames: failNames}
}

func (m *FailingMeter) fail(name string) error {
	if err, ok := m.failNames[name]; ok {
		return err
	}
	return nil
}

// Int64Counter implements metric.Meter.
func (m *FailingMeter) Int64Counter(name string, options ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	if err := m.fail(name); err != nil {
		return nil, err
	}
	return m.Meter.Int64Counter(name, options...)
}

// Float64Histogram implements metric.Meter.
func (m *FailingMeter) Float64Histogram(name string, options ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	if err := m.fail(name); err != nil {
		return nil, err
	}
	return m.Meter.Float64Histogram(name, options...)
}

// Int64ObservableGauge implements metric.Meter.
func (m *FailingMeter) Int64ObservableGauge(name string, options ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	if err := m.fail(name); err != nil {
		return nil, err
	}
	return m.Meter.Int64ObservableGauge(name, options...)
}

// FailingMeterProvider is a metric.MeterProvider that hands out one
// shared FailingMeter -- the process-wide-provider stand-in that fails
// instrument creation for the configured names. noop.MeterProvider's
// embedded interface method is what lets this type satisfy
// metric.MeterProvider at all (the OTel interfaces carry an unexported
// embedded method only noop and the SDK themselves provide).
type FailingMeterProvider struct {
	noop.MeterProvider
	meter metric.Meter
}

// NewFailingMeterProvider returns a provider that hands out meter to
// every otel.Meter call.
func NewFailingMeterProvider(meter metric.Meter) FailingMeterProvider {
	return FailingMeterProvider{meter: meter}
}

// Meter implements metric.MeterProvider.
func (p FailingMeterProvider) Meter(name string, options ...metric.MeterOption) metric.Meter {
	return p.meter
}
