package prometheus_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

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

// TestBuildReader_InvalidUTF8Path_NeverVoidsTheScrape is the regression
// for a live, unauthenticated denial-of-service class in
// go/observability/Middleware's route label: net/http percent-decodes a
// request target before this package ever sees it, and percent-decoding is
// byte-oriented -- "%FF%FE" in a path yields two bytes that are not valid
// UTF-8, with no error anywhere in parsing. Recorded verbatim as the
// http.route metric label, such a
// value makes the Prometheus exporter's per-series validation fail on
// every Gather: the /metrics endpoint answers 500 with ZERO metrics -- not
// just the HTTP instruments, but every metric from every module, for the
// life of the process, since the offending cumulative data point and the
// limiter's cached "seen" key both persist. That is precisely the
// unauthenticated-DoS class obs.MaxRouteLabelValues (distinct-value count)
// and obs.MaxRouteLabelLength (per-value length) exist to close, through
// the third dimension neither bound checks: value VALIDITY.
//
// The fix sanitizes non-UTF-8 path bytes (to the Unicode replacement rune)
// before a label value is formed, so an invalid path is still counted --
// under a sanitized, exporter-safe label -- and never reaches the
// registry. The baseline shape: one normal request (200), then one GET
// with %FF in its path, then a scrape that must still answer 200 with the
// normal request's series present and no invalid byte anywhere in the
// payload. Without the sanitization the scrape answers 500 with an error
// naming the invalid label value and an empty metric body.
func TestBuildReader_InvalidUTF8Path_NeverVoidsTheScrape(t *testing.T) {
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

	// The normal request: the positive control whose series must survive.
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("normal request status = %d, want 200", rr.Code)
	}

	// The one bad request: a percent-encoded path whose decoded bytes are
	// not valid UTF-8. The test setup assertion pins that the request
	// really has the shape this regression is about.
	bad := httptest.NewRequest(http.MethodGet, "/api/%FFjunk", nil)
	if utf8.ValidString(bad.URL.Path) {
		t.Fatalf("test setup: URL.Path %q must not be valid UTF-8 for this regression to be exercised", bad.URL.Path)
	}
	handler.ServeHTTP(httptest.NewRecorder(), bad)

	// The scrape must keep answering 200 and must still serve the normal
	// request's series -- the whole point of the fix. (Fail-before: 500,
	// zero metrics.)
	metricsRR := httptest.NewRecorder()
	obs.MetricsHandler().ServeHTTP(metricsRR, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metricsRR.Code != http.StatusOK {
		t.Fatalf("GET /metrics after one request with an invalid-UTF-8 path: status = %d, want 200 -- an invalid route label must not void the scrape (the recorded data point is cumulative and permanent); body: %s",
			metricsRR.Code, metricsRR.Body.String())
	}
	body, err := io.ReadAll(metricsRR.Result().Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	if !strings.Contains(string(body), `http_route="/api/v1/notes"`) {
		t.Errorf("expected the normal request's series to survive the scrape; body:\n%s", body)
	}
	if !utf8.Valid(body) {
		t.Errorf("scrape payload is not valid UTF-8: an invalid path byte reached the exported label; body:\n%q", body)
	}
	// The bad request is still counted -- under a sanitized label where the
	// Unicode replacement rune stands in for each invalid byte -- rather
	// than silently dropped from the metrics.
	if !strings.Contains(string(body), `http_route="/api/`+"\ufffd"+`junk"`) {
		t.Errorf("expected the bad request to be counted under a sanitized route label (the invalid byte run replaced by U+FFFD); body:\n%s", body)
	}
}

// TestBuildReader_OneUnscrapableSeries_DoesNotVoidTheScrape pins the
// scrape handler's error-handling mode: promhttp's default (HTTPErrorOnError)
// turns ANY one gather error into a 500 with no metrics at all -- under it,
// one invalid-UTF-8 label value (the class the test above records) would
// take the whole /metrics endpoint down rather than just dropping the
// offending series. With ErrorHandling: promhttp.ContinueOnError, one
// unscrapable series costs only itself (and a logged error); every healthy
// series is still served.
//
// The unscrapable series is recorded through a raw instrument whose label
// value is invalid UTF-8 -- bypassing Middleware's own path sanitization,
// standing in for any series the exporter cannot translate (an
// invalid-UTF-8 label value is one instance of the class; see the test
// above for why the exporter rejects it). The scrape must answer 200 with
// the request counter family served alongside.
func TestBuildReader_OneUnscrapableSeries_DoesNotVoidTheScrape(t *testing.T) {
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

	// A healthy series first: Middleware's request counter.
	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil))

	// The deliberately unscrapable series: recorded straight into the
	// Init-installed MeterProvider, not through Middleware.
	counter, err := otel.Meter("prometheus_test").Int64Counter("prometheus_test_unscrapable_total")
	if err != nil {
		t.Fatalf("build unscrapable counter: %v", err)
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "/probe/\xff\xfe")))

	metricsRR := httptest.NewRecorder()
	obs.MetricsHandler().ServeHTTP(metricsRR, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metricsRR.Code != http.StatusOK {
		t.Fatalf("GET /metrics with one unscrapable series present: status = %d, want 200 -- the scrape handler must run with ContinueOnError so one bad series cannot void the whole scrape; body: %s",
			metricsRR.Code, metricsRR.Body.String())
	}
	body, err := io.ReadAll(metricsRR.Result().Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	if !strings.Contains(string(body), "http_server_request_count_total{") {
		t.Errorf("expected the healthy request-counter family to be served alongside the unscrapable series; body:\n%s", body)
	}
	if !utf8.Valid(body) {
		t.Errorf("scrape payload is not valid UTF-8; body:\n%q", body)
	}
}
