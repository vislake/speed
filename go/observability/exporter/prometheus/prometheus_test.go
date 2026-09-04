package prometheus_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	obs "github.com/vislake/speed/go/observability"
	// The subpackage under test. Imported here the same way a real host
	// does -- purely for its init() side effect -- even though this
	// directory's own package is always compiled into this test binary
	// regardless: the explicit import documents the actual usage pattern
	// (see this package's own doc comment).
	_ "github.com/vislake/speed/go/observability/exporter/prometheus"
)

// TestBuildReader_WiresWorkingLocalMetricsEndpoint is this subpackage's
// acceptance bar for the opt-in path: with it blank-imported, obs.Init's
// no-endpoint (local exporters) call must serve real Prometheus-format
// output from obs.MetricsHandler after a recorded request, using nothing
// but obs.Init and obs.Middleware exactly as a real host would wire them.
// go/observability's own TestInit_NoEndpoint_MetricsHandlerIsNotConfiguredByDefault
// proves the complementary, opted-out default in the root package's own
// test suite, which never imports this subpackage.
func TestBuildReader_WiresWorkingLocalMetricsEndpoint(t *testing.T) {
	ctx := context.Background()
	shutdown, err := obs.Init(ctx)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := shutdown(context.Background()); shutdownErr != nil {
			t.Errorf("shutdown: %v", shutdownErr)
		}
	})

	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRR := httptest.NewRecorder()
	obs.MetricsHandler().ServeHTTP(metricsRR, metricsReq)

	if metricsRR.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", metricsRR.Code)
	}
	body, err := io.ReadAll(metricsRR.Result().Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}

	// A real Prometheus exposition-format line for the request this test
	// just issued: the counter's family name, in the "_total" form the
	// exporter appends, carrying a value of (at least) 1.
	if !strings.Contains(string(body), "http_server_request_count_total{") {
		t.Errorf("expected /metrics body to contain the request counter family after a recorded request; body:\n%s", body)
	}
	if !strings.Contains(string(body), `http_route="/api/v1/notes"`) {
		t.Errorf("expected /metrics body to contain the recorded route label; body:\n%s", body)
	}
}

// TestBuildReader_MetricsHandlerIsIsolatedAcrossCalls proves the
// fresh-registry-per-call design in buildReader actually holds: a second
// obs.Init call in the same process (exactly what happens across this
// file's own tests) must not panic with Prometheus's "duplicate metrics
// collector registration" and must not carry over the previous call's
// recorded data into the new registry.
func TestBuildReader_MetricsHandlerIsIsolatedAcrossCalls(t *testing.T) {
	ctx := context.Background()

	shutdown1, err := obs.Init(ctx)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/first", nil))
	if shutdownErr := shutdown1(ctx); shutdownErr != nil {
		t.Fatalf("first shutdown: %v", shutdownErr)
	}

	// A second Init call is exactly what this test is for: it must not
	// panic on re-registering the same Prometheus collector names.
	shutdown2, err := obs.Init(ctx)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown2(context.Background()) })

	rr := httptest.NewRecorder()
	obs.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rr.Result().Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	if strings.Contains(string(body), `http_route="/first"`) {
		t.Errorf("expected the second Init's registry to start empty, but it still carries the first call's data:\n%s", body)
	}
}
