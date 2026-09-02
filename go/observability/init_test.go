package observability_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"go.opentelemetry.io/otel"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

func TestInit_InvalidProfile_ReturnsError(t *testing.T) {
	_, err := obs.Init(context.Background(), pkgcore.Profile("staging"))
	if !errors.Is(err, pkgcore.ErrInvalidProfile) {
		t.Errorf("Init with an unrecognized profile: got %v, want an error wrapping pkgcore.ErrInvalidProfile", err)
	}
}

func TestInit_Production_RequiresOTLPEndpoint(t *testing.T) {
	_, err := obs.Init(context.Background(), pkgcore.ProfileProduction)
	if !errors.Is(err, obs.ErrMissingOTLPEndpoint) {
		t.Errorf("Init(ProfileProduction) with no endpoint: got %v, want an error wrapping ErrMissingOTLPEndpoint", err)
	}
}

// TestInit_Demo_ProducesWorkingExporters is the task's own acceptance
// bar for the demo profile: hit MetricsHandler and confirm real
// Prometheus-format output appears after a recorded request, using
// nothing but Init and Middleware exactly as a real host would wire them.
func TestInit_Demo_ProducesWorkingExporters(t *testing.T) {
	ctx := context.Background()
	shutdown, err := obs.Init(ctx, pkgcore.ProfileDemo)
	if err != nil {
		t.Fatalf("Init(ProfileDemo): %v", err)
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

// TestInit_Demo_MetricsHandlerIsIsolatedAcrossCalls proves the fresh-
// registry-per-Init design actually holds: a second Init(ProfileDemo)
// call in the same process (exactly what happens across this file's own
// tests) must not panic with Prometheus's "duplicate metrics collector
// registration" and must not carry over the previous call's recorded
// data into the new registry.
func TestInit_Demo_MetricsHandlerIsIsolatedAcrossCalls(t *testing.T) {
	ctx := context.Background()

	shutdown1, err := obs.Init(ctx, pkgcore.ProfileDemo)
	if err != nil {
		t.Fatalf("first Init(ProfileDemo): %v", err)
	}
	obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/first", nil))
	if shutdownErr := shutdown1(ctx); shutdownErr != nil {
		t.Fatalf("first shutdown: %v", shutdownErr)
	}

	// A second Init call is exactly what this test is for: it must not
	// panic on re-registering the same Prometheus collector names.
	shutdown2, err := obs.Init(ctx, pkgcore.ProfileDemo)
	if err != nil {
		t.Fatalf("second Init(ProfileDemo): %v", err)
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

func TestMetricsHandler_ProductionProfile_ReturnsNotFound(t *testing.T) {
	lis, srv := startFakeCollector(t)
	// Deliberately plain defer statements, not t.Cleanup, and in this
	// exact order: t.Cleanup callbacks all run strictly after the test
	// function (and every one of its own defers) has already returned,
	// so a shutdown registered via t.Cleanup here would run AFTER
	// stopFakeCollector already tore down the fake server -- exactly the
	// ordering bug this comment exists to prevent reintroducing. Plain
	// defers run LIFO within this function, so registering
	// stopFakeCollector first and shutdown second makes shutdown (which
	// needs the fake server still listening to flush against) run first.
	defer stopFakeCollector(t, lis, srv)

	shutdown, err := obs.Init(context.Background(), pkgcore.ProfileProduction,
		obs.WithOTLPEndpoint(lis.Addr().String()),
		obs.WithOTLPInsecure(true),
	)
	if err != nil {
		t.Fatalf("Init(ProfileProduction): %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	rr := httptest.NewRecorder()
	obs.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("MetricsHandler() under ProfileProduction: status = %d, want 404 (metrics are pushed via OTLP, not pulled locally)", rr.Code)
	}
}

// TestInit_Production_ExportsRealSpansAndMetricsOverOTLP is this
// package's substitute for a testcontainers-based OTel Collector
// integration test. No dedicated testcontainers-go module exists for the
// OTel Collector, and hand-rolling one (a generic container plus a
// mounted YAML pipeline config) would need Docker in this package's test
// environment for the first time in this repository's Go modules to
// prove what is, underneath, a wire-protocol concern -- so instead this
// test stands up a real gRPC server implementing the actual generated
// OTLP collector service interfaces (go.opentelemetry.io/proto/otlp; not
// a mock of this package's own code) and proves Init(...,
// ProfileProduction, ...) genuinely serializes and transmits a real span
// and a real metric to it, deterministically and in-process. Because it
// needs no external process or Docker, it stays in the regular unit-test
// set rather than integration_test/ -- see
// backend-coding-standards.md §13's own reasoning for why
// integration_test/ is reserved specifically for tests that need a real
// external dependency such as testcontainers' PostgreSQL or Redis.
func TestInit_Production_ExportsRealSpansAndMetricsOverOTLP(t *testing.T) {
	lis, srv := startFakeCollector(t)
	traces, metrics := srv.traces, srv.metrics
	defer stopFakeCollector(t, lis, srv)

	ctx := context.Background()
	shutdown, err := obs.Init(ctx, pkgcore.ProfileProduction,
		obs.WithOTLPEndpoint(lis.Addr().String()),
		obs.WithOTLPInsecure(true),
		obs.WithServiceName("observability-init-test"),
	)
	if err != nil {
		t.Fatalf("Init(ProfileProduction): %v", err)
	}

	_, span := otel.Tracer("observability_test").Start(ctx, "op")
	span.End()

	counter, err := otel.Meter("observability_test").Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("build counter: %v", err)
	}
	counter.Add(ctx, 1)

	// Shutdown flushes the batch span processor and force-collects the
	// periodic metric reader before closing both providers -- this is
	// what actually delivers the buffered span and metric to the fake
	// collector; see Init's own doc comment on why a caller must invoke
	// the returned function during graceful shutdown rather than just
	// exiting.
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if got := traces.count(); got != 1 {
		t.Errorf("fake collector received %d trace export requests, want 1", got)
	}
	if got := metrics.count(); got == 0 {
		t.Errorf("fake collector received %d metric export requests, want at least 1", got)
	}
}

// fakeTraceServer and fakeMetricServer implement the real, generated OTLP
// collector gRPC service interfaces (go.opentelemetry.io/proto/otlp),
// recording every request they receive. They must be two separate types
// rather than one: TraceServiceServer and MetricsServiceServer both
// declare a method named Export with different request/response types,
// which Go cannot overload on a single receiver.
type fakeTraceServer struct {
	coltracepb.UnimplementedTraceServiceServer
	mu       sync.Mutex
	requests []*coltracepb.ExportTraceServiceRequest
}

func (f *fakeTraceServer) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (f *fakeTraceServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type fakeMetricServer struct {
	colmetricpb.UnimplementedMetricsServiceServer
	mu       sync.Mutex
	requests []*colmetricpb.ExportMetricsServiceRequest
}

func (f *fakeMetricServer) Export(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

func (f *fakeMetricServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// fakeCollectorServer bundles both fake services behind the one
// grpc.Server they are registered on.
type fakeCollectorServer struct {
	grpcServer *grpc.Server
	traces     *fakeTraceServer
	metrics    *fakeMetricServer
	serveErr   chan error
}

// startFakeCollector starts a real gRPC server, on an OS-assigned
// loopback port, implementing the OTLP collector's trace and metrics
// services. See TestInit_Production_ExportsRealSpansAndMetricsOverOTLP's
// doc comment for why this substitutes for a testcontainers-based
// integration test against a real otel/opentelemetry-collector image.
func startFakeCollector(t *testing.T) (net.Listener, *fakeCollectorServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &fakeCollectorServer{
		grpcServer: grpc.NewServer(),
		traces:     &fakeTraceServer{},
		metrics:    &fakeMetricServer{},
		serveErr:   make(chan error, 1),
	}
	coltracepb.RegisterTraceServiceServer(srv.grpcServer, srv.traces)
	colmetricpb.RegisterMetricsServiceServer(srv.grpcServer, srv.metrics)

	go func() { srv.serveErr <- srv.grpcServer.Serve(lis) }()
	return lis, srv
}

// stopFakeCollector stops the server started by startFakeCollector and
// waits for its Serve goroutine to actually return, so a test does not
// exit (and its t.Cleanup-registered OTel provider shutdowns run) while
// the fake collector's goroutine is still alive.
func stopFakeCollector(t *testing.T, _ net.Listener, srv *fakeCollectorServer) {
	t.Helper()
	srv.grpcServer.Stop()
	if err := <-srv.serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		t.Errorf("fake collector Serve: %v", err)
	}
}
