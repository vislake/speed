package prometheus_test

import (
	"context"
	"errors"
	"testing"

	obs "github.com/vislake/speed/go/observability"
)

// TestInit_WithOTLPEndpoint_WithoutRegisteredExporter_FailsActionably
// proves observability.Init's negative OTLP path: WithOTLPEndpoint
// supplied but go/observability/exporter/otlp never blank-imported, so
// obs.RegisterOTLPExporters was never called and Init must fail with the
// actionable obs.ErrOTLPExporterNotRegistered rather than silently falling
// back to the local exporters or panicking on a nil factory.
//
// This test deliberately lives in this package's test binary rather than
// go/observability's own (init_test.go, example_test.go) or
// exporter/otlp's own: all *_test.go files in one directory share a
// single compiled test binary, and both of those directories'
// test binaries blank-import exporter/otlp so their own *success*-path
// WithOTLPEndpoint tests have a registered factory to succeed against --
// which, as a side effect, leaves otlpFactory permanently non-nil for
// every test in either binary, including ones that never mean to exercise
// the OTLP path at all. This package's test binary imports
// exporter/prometheus (for the local-metrics-reader tests above), never
// exporter/otlp, so it is the one binary in this module where obs.Init
// here genuinely calls into a nil otlpFactory -- exactly the state a host
// that forgot the blank import would be in.
func TestInit_WithOTLPEndpoint_WithoutRegisteredExporter_FailsActionably(t *testing.T) {
	shutdown, err := obs.Init(context.Background(), obs.WithOTLPEndpoint("127.0.0.1:4317"))
	if err == nil {
		_ = shutdown(context.Background())
		t.Fatal("Init with WithOTLPEndpoint but no registered OTLP exporter: got nil error, want obs.ErrOTLPExporterNotRegistered")
	}
	if !errors.Is(err, obs.ErrOTLPExporterNotRegistered) {
		t.Errorf("Init error = %v, want errors.Is(err, obs.ErrOTLPExporterNotRegistered)", err)
	}
	if shutdown != nil {
		t.Errorf("Init returned a non-nil shutdown func alongside an error")
	}
}
