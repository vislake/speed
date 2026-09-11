package observability

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// TestFactoryVars_ConcurrentRegisterAndInit_NoDataRace exercises the two
// exporter-factory package variables (otlpFactory and
// metricsReaderFactory, init.go) under the exact concurrency this guards:
// RegisterOTLPExporters / RegisterLocalMetricsReader write them while Init
// reads them, with no mutex between the two sides -- a state
// inconsistent with currentMetricsHandler, which IS mutex-guarded for
// exactly the repeated-call reason the neighbouring doc comments name. In
// a real binary both registrars run once from exporter-subpackage init()
// functions before any Init, but they are exported and this package's own
// tests register stand-ins around repeated Init calls, so a host or test
// that registers at any other moment (or concurrently with an Init in
// flight) is a genuine -race report, not a theoretical one.
//
// This test lives in the internal observability package (not the external
// observability_test package every other test file uses) for one reason:
// it must RESTORE the two package variables when it finishes, and with no
// getter exported for them, only package-internal code can snapshot and
// restore -- leaving a probe factory registered would break the
// no-endpoint default tests that share this test binary
// (TestInit_NoEndpoint_MetricsHandlerIsNotConfiguredByDefault and
// TestInit_NoEndpoint_NoLocalMetricsReader_LogsStartupWarning both depend
// on the registrars staying in their blank-import state).
//
// The race detector is the assertion: a write-vs-read collision on either
// factory the moment the loops overlap fails this test under -race.
func TestFactoryVars_ConcurrentRegisterAndInit_NoDataRace(t *testing.T) {
	previousOTLP := otlpFactory
	previousReader := metricsReaderFactory
	defer func() {
		otlpFactory = previousOTLP
		metricsReaderFactory = previousReader
	}()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Two reader goroutines: one drives Init down the no-endpoint path
	// (which reads metricsReaderFactory in initLocalExporters), one down
	// the OTLP-endpoint path (which reads otlpFactory). Both factories are
	// probe stand-ins that fail fast, so every Init iteration returns
	// quickly and no real provider or exporter is ever assembled.
	for i := 0; i < 2; i++ {
		withEndpoint := i%2 == 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				opts := []Option{}
				if withEndpoint {
					opts = append(opts, WithOTLPEndpoint("127.0.0.1:1"))
				}
				shutdown, err := Init(context.Background(), opts...)
				if err == nil {
					_ = shutdown(context.Background())
				}
			}
		}()
	}

	// One writer goroutine flipping both registrars for the duration.
	wg.Add(1)
	go func() {
		defer wg.Done()
		readerProbe := func() (sdkmetric.Reader, http.Handler, error) {
			return nil, nil, errors.New("factory_vars_test: metrics reader probe")
		}
		otlpProbe := func(ctx context.Context, cfg Config, res *resource.Resource) (func(context.Context) error, error) {
			return nil, errors.New("factory_vars_test: OTLP exporter probe")
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			RegisterLocalMetricsReader(readerProbe)
			RegisterOTLPExporters(otlpProbe)
		}
	}()

	// Let the three goroutines overlap long enough that the race detector
	// cannot miss the collision; the loops are tight, so 100ms is thousands
	// of iterations each.
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestFactoryVars_OTLPFactoryError_InitPropagatesIt drives Init's
// OTLP-factory error branch directly: with a non-empty WithOTLPEndpoint
// and a registered otlpFactory that itself fails, Init must return that
// factory's own error, never ErrOTLPExporterNotRegistered -- the factory
// WAS registered, so the blank-import instruction that error carries
// would be a false diagnosis (the genuinely unregistered case is
// exporter/prometheus/otlp_not_registered_test.go's subject). The probe
// factory is registered and invoked on this test's own call path, so the
// branch runs deterministically on every execution.
func TestFactoryVars_OTLPFactoryError_InitPropagatesIt(t *testing.T) {
	previousOTLP := otlpFactory
	defer func() { otlpFactory = previousOTLP }()

	probeErr := errors.New("factory_vars_test: OTLP exporter probe failure")
	RegisterOTLPExporters(func(context.Context, Config, *resource.Resource) (func(context.Context) error, error) {
		return nil, probeErr
	})

	shutdown, err := Init(context.Background(), WithOTLPEndpoint("127.0.0.1:1"))
	if err == nil {
		_ = shutdown(context.Background())
		t.Fatal("Init with an OTLP endpoint and a failing registered factory succeeded, want the factory's error")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("Init error = %v, want the registered OTLP factory's own error", err)
	}
	if errors.Is(err, ErrOTLPExporterNotRegistered) {
		t.Errorf("Init error = %v, want the registered factory's error, not ErrOTLPExporterNotRegistered: a factory was registered, so that blank-import instruction would be a false diagnosis", err)
	}
}

// TestFactoryVars_MetricsReaderFactoryError_InitPropagatesIt drives the
// no-endpoint half of the same seam directly: with no WithOTLPEndpoint
// and a registered metricsReaderFactory that fails, Init must fail with
// an error wrapping that factory's own error (initLocalExporters names
// the reader in the wrap) rather than leave a half-built local exporter
// set behind -- nothing past the reader factory is installed on this
// path, and the partially built TracerProvider is shut down before the
// error returns.
func TestFactoryVars_MetricsReaderFactoryError_InitPropagatesIt(t *testing.T) {
	previousReader := metricsReaderFactory
	defer func() { metricsReaderFactory = previousReader }()

	probeErr := errors.New("factory_vars_test: metrics reader probe failure")
	RegisterLocalMetricsReader(func() (sdkmetric.Reader, http.Handler, error) {
		return nil, nil, probeErr
	})

	shutdown, err := Init(context.Background())
	if err == nil {
		_ = shutdown(context.Background())
		t.Fatal("Init with a failing registered metrics reader factory succeeded, want the factory's error")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("Init error = %v, want it to wrap the registered metrics reader factory's own error", err)
	}
}

// TestFactoryVars_PreviousShutdownFailure_SurfacedViaOtelHandle drives the
// repeated-Init swap's failure branch directly: the first Init installs a
// shutdown func that fails, the second Init succeeds and invokes that
// superseded shutdown while tearing the previous pair down, and the
// failure is reported through otel.Handle -- see Init's own doc comment
// on the swap. The second Init itself still succeeds: the replacement
// pair is fully installed by the time the superseded func runs, and a
// teardown failure of providers already being replaced must not fail the
// caller's fresh Init.
func TestFactoryVars_PreviousShutdownFailure_SurfacedViaOtelHandle(t *testing.T) {
	previousOTLP := otlpFactory
	defer func() { otlpFactory = previousOTLP }()

	probeErr := errors.New("factory_vars_test: previous providers shutdown probe failure")
	RegisterOTLPExporters(func(context.Context, Config, *resource.Resource) (func(context.Context) error, error) {
		return func(context.Context) error { return probeErr }, nil
	})

	firstShutdown, err := Init(context.Background(), WithOTLPEndpoint("127.0.0.1:1"))
	if err != nil {
		t.Fatalf("Init installing a failing shutdown: %v", err)
	}
	// The second Init consumes this shutdown func below; this defer covers
	// the path where the test exits before that happens, so its once-guard
	// fires inside this test either way. A later invocation is a no-op
	// returning the first run's result (see onceShutdown).
	defer func() { _ = firstShutdown(context.Background()) }()

	previousHandler := otel.GetErrorHandler()
	var mu sync.Mutex
	var handled []error
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		mu.Lock()
		defer mu.Unlock()
		handled = append(handled, err)
	}))
	t.Cleanup(func() { otel.SetErrorHandler(previousHandler) })

	RegisterOTLPExporters(func(context.Context, Config, *resource.Resource) (func(context.Context) error, error) {
		return func(context.Context) error { return nil }, nil
	})
	secondShutdown, err := Init(context.Background(), WithOTLPEndpoint("127.0.0.1:1"))
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	defer func() { _ = secondShutdown(context.Background()) }()

	mu.Lock()
	defer mu.Unlock()
	var reported error
	for _, err := range handled {
		if errors.Is(err, probeErr) {
			reported = err
			break
		}
	}
	if reported == nil {
		t.Fatalf("the superseded shutdown's failure was not reported through otel.Handle; captured errors: %v", handled)
	}
	if msg := reported.Error(); !strings.Contains(msg, "previous Init") {
		t.Errorf("reported error = %q, want it to name the superseded providers' teardown", msg)
	}
}
