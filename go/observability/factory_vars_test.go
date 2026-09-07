package observability

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// TestFactoryVars_ConcurrentRegisterAndInit_NoDataRace exercises the two
// exporter-factory package variables (otlpFactory and
// metricsReaderFactory, init.go) under the exact concurrency a reviewer
// flagged: RegisterOTLPExporters / RegisterLocalMetricsReader write them
// while Init reads them, with no mutex between the two sides -- a state
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
// Fails under -race before the fix (verified: the race detector reports
// both factories' write-vs-read collisions the moment the loops overlap);
// passes after.
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

	// Let the three goroutines overlap long enough that, pre-fix, the race
	// detector cannot miss the collision; the loops are tight, so 100ms is
	// thousands of iterations each.
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
