package observability_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"go.opentelemetry.io/otel"

	obs "github.com/vislake/speed/go/observability"
	// Blank-imported so this file's own OTLP-endpoint tests below (which
	// call obs.Init with WithOTLPEndpoint) have a registered exporter
	// factory to succeed against -- exactly what a real host does. This
	// import has no bearing on TestInit_NoEndpoint_* below, which stay on
	// Init's stdout-only default: registering the OTLP factory does not
	// register a local metrics reader.
	_ "github.com/vislake/speed/go/observability/exporter/otlp"
)

// TestInit_NoEndpoint_MetricsHandlerIsNotConfiguredByDefault is the
// acceptance bar for Init's true zero-third-party-dependency default,
// proportionate to go/observability/exporter/prometheus's own
// TestBuildReader_WiresWorkingLocalMetricsEndpoint, which proves the
// opposite (opted-in) half of the same behavior. This package's own tests
// never blank-import that subpackage, so nothing here ever registers a
// local metrics reader via obs.RegisterLocalMetricsReader -- if that ever
// stopped being true (a future test file in this package added such an
// import), this test would start failing, which is the point: it is
// meant to fail loudly the moment this package's own root import graph
// gains a dependency on the opt-in subpackage it exists to keep optional.
func TestInit_NoEndpoint_MetricsHandlerIsNotConfiguredByDefault(t *testing.T) {
	shutdown, err := obs.Init(context.Background())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := shutdown(context.Background()); shutdownErr != nil {
			t.Errorf("shutdown: %v", shutdownErr)
		}
	})

	rr := httptest.NewRecorder()
	obs.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("MetricsHandler() with no metrics reader registered: status = %d, want 404 (a local scrape endpoint is opt-in -- see go/observability/exporter/prometheus)", rr.Code)
	}
}

func TestMetricsHandler_WithOTLPEndpoint_ReturnsNotFound(t *testing.T) {
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

	shutdown, err := obs.Init(context.Background(),
		obs.WithOTLPEndpoint(lis.Addr().String()),
		obs.WithOTLPInsecure(true),
	)
	if err != nil {
		t.Fatalf("Init with an OTLP endpoint: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	rr := httptest.NewRecorder()
	obs.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("MetricsHandler() with the OTLP exporters wired: status = %d, want 404 (metrics are pushed via OTLP, not pulled locally)", rr.Code)
	}
}

// TestInit_WithOTLPEndpoint_ExportsRealSpansAndMetricsOverOTLP is this
// package's substitute for a testcontainers-based OTel Collector
// integration test. No dedicated testcontainers-go module exists for the
// OTel Collector, and hand-rolling one (a generic container plus a
// mounted YAML pipeline config) would need Docker in this package's test
// environment for the first time in this repository's Go modules to
// prove what is, underneath, a wire-protocol concern -- so instead this
// test stands up a real gRPC server implementing the actual generated
// OTLP collector service interfaces (go.opentelemetry.io/proto/otlp; not
// a mock of this package's own code) and proves Init genuinely
// serializes and transmits a real span and a real metric to it,
// deterministically and in-process.
// Because it needs no external process or Docker, it stays in the
// regular unit-test set rather than integration_test/ -- see
// backend-coding-standards.md §13's own reasoning for why
// integration_test/ is reserved specifically for tests that need a real
// external dependency such as testcontainers' PostgreSQL or Redis.
//
// The Init call below passes no deployment mode: Init no longer takes
// one, and the endpoint alone decides that the OTLP exporters get wired.
// That is docs/internal/03-deployment-modes.md's composition axis applied
// to observability -- exporter choice is an implementation-composition
// decision, never a mode decision -- so this success path is exactly the
// one a single-process assembly pointing at a real collector configures,
// no less than a multi-replica deployment does.
func TestInit_WithOTLPEndpoint_ExportsRealSpansAndMetricsOverOTLP(t *testing.T) {
	lis, srv := startFakeCollector(t)
	traces, metrics := srv.traces, srv.metrics
	defer stopFakeCollector(t, lis, srv)

	ctx := context.Background()
	shutdown, err := obs.Init(ctx,
		obs.WithOTLPEndpoint(lis.Addr().String()),
		obs.WithOTLPInsecure(true),
		obs.WithServiceName("observability-init-test"),
	)
	if err != nil {
		t.Fatalf("Init with an OTLP endpoint: %v", err)
	}
	// Plain defer, not t.Cleanup, registered here -- immediately after
	// the error check, and (crucially) *after* stopFakeCollector's own
	// defer above so it runs first, LIFO -- for exactly the reason
	// TestMetricsHandler_WithOTLPEndpoint_ReturnsNotFound's comment
	// spells out: t.Cleanup callbacks all run strictly after this
	// function (and its own defers) return, so a t.Cleanup registered
	// here would run shutdown AFTER stopFakeCollector has already torn
	// down the fake server. Without this defer, any assertion firing
	// between here and the manual shutdown call below (e.g. the
	// counter-construction check just after) would exit via
	// runtime.Goexit() without ever calling shutdown, leaking the
	// TracerProvider's batch-span-processor goroutine and the
	// MeterProvider's periodic-reader goroutine for the rest of the test
	// binary's process life -- see
	// TestInit_WithOTLPEndpoint_EarlyExitLeaksGoroutinesWithoutAnImmediateDefer
	// below for a reproduction. The manual call below still runs first
	// on the happy path, since it must: the trace/metric-count
	// assertions need the data flushed before they run. This defer then
	// runs a second time while the fake collector is still up (its own
	// defer has not run yet), which is safe because OTel SDK Shutdown is
	// idempotent.
	defer func() { _ = shutdown(context.Background()) }()

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

// TestInit_WithOTLPEndpoint_EarlyExitLeaksGoroutinesWithoutAnImmediateDefer
// is a regression test for a goroutine-leak bug found in the test this one
// grew out of: the endpoint-configuring OTLP export test used to call
// shutdown(ctx) only once, near the end of its happy path, with no
// defer or t.Cleanup registered right after Init succeeded. Any assertion
// firing between Init succeeding and that manual call would exit the test
// via t.Fatal's runtime.Goexit() without ever invoking shutdown, leaking
// the OTLP-exporter TracerProvider's batch-span-processor goroutine and
// the MeterProvider's periodic-reader goroutine for the rest of the test
// binary's process life. The fix is a defer registered immediately
// after Init's error check, matching the pattern
// TestMetricsHandler_WithOTLPEndpoint_ReturnsNotFound already
// established (see that test's own comment for why it must be a plain
// defer, not t.Cleanup).
//
// This proves both directions of that fix with a real negative control,
// using t.Fatal's own exit mechanism -- runtime.Goexit -- inside a
// throwaway goroutine so it can reproduce an early test failure without
// actually failing this test:
//   - without a defer registered before the early exit (the pre-fix
//     shape), Init's background goroutines measurably leak. If this
//     stopped being true (e.g. a future OTel SDK version started
//     lazily starting these goroutines, or stopped needing Shutdown to
//     stop them), this test would no longer be proving anything, so
//     the leak is asserted explicitly rather than assumed.
//   - with a defer registered right after Init's error check -- the
//     fixed shape, and what every endpoint-configuring Init call in
//     this file now uses -- they do not.
func TestInit_WithOTLPEndpoint_EarlyExitLeaksGoroutinesWithoutAnImmediateDefer(t *testing.T) {
	lis, srv := startFakeCollector(t)
	defer stopFakeCollector(t, lis, srv)

	// runInitThenExitEarly reproduces "Init succeeds, then something
	// fails before any cleanup runs" inside its own goroutine, exiting
	// it via runtime.Goexit -- exactly the mechanism t.Fatal uses -- so
	// that, for the deferShutdown=false case, omitting a defer before
	// the exit reproduces the original leak without failing this test
	// itself. t.Fatal/t.Errorf are deliberately never called from inside
	// the goroutine (only the main test goroutine may call them safely);
	// it reports failure to the caller by returning a nil shutdown. It
	// always returns whatever shutdown func Init produced so the caller
	// can reclaim it afterward, whether or not the goroutine itself
	// deferred it.
	runInitThenExitEarly := func(t *testing.T, deferShutdown bool) func(context.Context) error {
		t.Helper()
		var shutdown func(context.Context) error
		done := make(chan struct{})
		go func() {
			defer close(done)
			var err error
			shutdown, err = obs.Init(context.Background(),
				obs.WithOTLPEndpoint(lis.Addr().String()),
				obs.WithOTLPInsecure(true),
			)
			if err != nil {
				shutdown = nil
				return
			}
			if deferShutdown {
				defer func() { _ = shutdown(context.Background()) }()
			}
			// Stand in for any assertion firing between Init succeeding
			// and a later manual shutdown call -- exactly the window
			// the original bug lived in.
			runtime.Goexit()
		}()
		<-done
		if shutdown == nil {
			t.Fatal("Init did not succeed inside the simulated test goroutine")
		}
		return shutdown
	}

	baseline := settledGoroutineCount(t)

	// Negative control: the pre-fix shape (no defer before the early
	// exit) must actually leak, or the rest of this test proves nothing.
	leakedShutdown := runInitThenExitEarly(t, false)
	if got := settledGoroutineCount(t); got <= baseline {
		t.Fatalf("negative control did not reproduce the leak: goroutine count = %d after an undeferred early exit, want > baseline %d -- has OTel SDK's shutdown-less behavior changed?", got, baseline)
	}
	// Reclaim what was deliberately leaked, both so it doesn't outlive
	// this test retrying exports against the fake collector this
	// function's own defer is about to tear down, and to confirm the
	// leaked goroutines really were Init's own (they go away once its
	// shutdown runs) rather than unrelated noise.
	if err := leakedShutdown(context.Background()); err != nil {
		t.Errorf("reclaiming the deliberately leaked shutdown: %v", err)
	}
	if got := settledGoroutineCount(t); got > baseline {
		t.Fatalf("goroutine count = %d after reclaiming the negative control's leak, want <= baseline %d", got, baseline)
	}

	// Fixed shape: a defer registered right after Init's error check
	// must not leak, even on the same early-exit path.
	_ = runInitThenExitEarly(t, true)
	if got := settledGoroutineCount(t); got > baseline {
		t.Errorf("goroutine count = %d after an early exit with shutdown deferred immediately after Init, want <= baseline %d -- the fix should have let both background goroutines exit", got, baseline)
	}
}

// settledGoroutineCount returns runtime.NumGoroutine() after giving
// recently-exited goroutines a chance to actually unwind: a goroutine's
// exit is not instantaneous relative to the statement that lets it exit
// (closing a gRPC connection, or an OTel SDK Shutdown call returning), so
// a bare single read taken immediately afterward is inherently racy. It
// polls until the count stops falling for a short stability window or a
// generous deadline elapses, and is meant only to be compared against
// another call captured the same way -- never as an exact expected value.
func settledGoroutineCount(t *testing.T) int {
	t.Helper()
	runtime.GC()
	lowest := runtime.NumGoroutine()
	deadline := time.Now().Add(3 * time.Second)
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if n := runtime.NumGoroutine(); n < lowest {
			lowest = n
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= 200*time.Millisecond {
			break
		}
	}
	return lowest
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
// services. See TestInit_WithOTLPEndpoint_ExportsRealSpansAndMetricsOverOTLP's
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
