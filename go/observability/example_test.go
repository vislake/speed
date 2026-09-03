package observability_test

// Runnable documentation for the observability public API, mirroring
// go/pkgcore/example_test.go's and go/dbkit/example_test.go's convention:
// every example here is compiled AND executed by `go test`, so an API
// change that invalidates the documented usage fails the build instead of
// only rotting in prose (root CLAUDE.md's Documentation section; this
// package's own doc.go and AGENTS.md).
//
// Deliberately self-contained: none of these examples reuse helpers from
// init_test.go or middleware_test.go, even though Go would allow it (all
// *_test.go files in this directory compile into the one
// observability_test package) -- an Example function cannot accept a
// *testing.T (the testing package's own Example signature rule), so
// nothing here could call their t.Helper()/t.Fatalf-based helpers anyway,
// and staying independent keeps this file's own compilation immune to
// unrelated churn in the sibling test files.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"

	"google.golang.org/grpc"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"

	observability "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// exampleMetricsCollector implements just enough of the OTLP collector's
// MetricsService to let an endpoint-configuring Init's shutdown function
// complete cleanly against it, for ExampleInit. It is a separate,
// minimal type rather than a reuse of init_test.go's own
// fakeMetricServer/startFakeCollector: those are built around a
// *testing.T (t.Helper(), t.Fatalf), which no Example function can supply.
type exampleMetricsCollector struct {
	colmetricpb.UnimplementedMetricsServiceServer
}

// Export implements colmetricpb.MetricsServiceServer by accepting
// everything, unconditionally -- this example only needs Init's shutdown
// to have somewhere real to flush to, not to inspect what was sent.
func (exampleMetricsCollector) Export(context.Context, *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

// ExampleInit shows Init's success path: supply an OTLP endpoint, wire
// providers, use MetricsHandler (as a host mounts it at /metrics), and
// shut down cleanly during graceful process shutdown. See
// examples/reference-app/cmd/server/main.go's run function for the same
// shape at a real call site -- obs.Init(ctx, obs.WithServiceName(...)),
// immediately followed by a deferred call to the returned shutdown
// function.
//
// Init's no-endpoint success path is deliberately not executed here: it
// writes real trace and metric data straight to os.Stdout by design
// (Init's own doc comment: stdout output is for "a developer tailing the
// process"), which would make this example's captured output
// non-deterministic. init_test.go's
// TestInit_NoEndpoint_WiresWorkingLocalExporters exercises that path
// directly instead, with a real recorded request and a real
// Prometheus-format scrape.
func ExampleInit() {
	// A minimal fake OTLP collector stands in for a real one (a real
	// LGTM stack's collector, wherever a host points its telemetry at
	// one) so the success path below stays deterministic and
	// dependency-free under `go test` -- see init_test.go's own
	// startFakeCollector for the fuller version (both signals, not just
	// metrics) this mirrors.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	grpcServer := grpc.NewServer()
	colmetricpb.RegisterMetricsServiceServer(grpcServer, exampleMetricsCollector{})
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	// The endpoint is the whole decision: no deployment mode is passed
	// (Init takes none), and supplying one alone wires the OTLP
	// exporters -- the same success path a single-process assembly
	// pointing at a real collector takes.
	shutdown, err := observability.Init(context.Background(),
		observability.WithServiceName("example-service"),
		observability.WithOTLPEndpoint(lis.Addr().String()),
		observability.WithOTLPInsecure(true),
	)
	fmt.Println("init:", err)

	// MetricsHandler continues to report 404 with the OTLP exporters
	// wired: there is no local Prometheus registry to scrape when
	// metrics are pushed via OTLP instead.
	rr := httptest.NewRecorder()
	observability.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	fmt.Println("metrics status with OTLP exporters:", rr.Code)

	// shutdown must run during graceful process shutdown so buffered
	// spans and metrics are flushed rather than dropped -- see Init's own
	// doc comment.
	fmt.Println("shutdown:", shutdown(context.Background()))

	// Output:
	// init: <nil>
	// metrics status with OTLP exporters: 404
	// shutdown: <nil>
}

// ExampleFromContext shows the one sanctioned way to obtain a logger
// inside request- or job-scoped code: FromContext automatically enriches
// whatever WithLogger attached with trace_id/span_id (when ctx carries an
// active OTel span) and tenant_id (when ctx carries one), so callers never
// build those key-value pairs by hand. See
// examples/reference-app/internal/notes/handler.go's NotesCreateNote
// method for a live call site (obs.FromContext(ctx).Info("note created",
// "note_id", note.ID)) that relies on exactly this enrichment.
//
// The exact trace_id/span_id values are random per run, so this example
// only checks that the fields are present, never their value -- the same
// technique logger_test.go's own tests use.
func ExampleFromContext() {
	// A real request's context already carries an active span (started by
	// Middleware), a logger (attached once, at process startup -- see
	// WithLogger's own doc comment), and a tenant (injected by
	// tenancy.Middleware from the access token claims). All three are
	// built explicitly here only for illustration.
	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ctx, span := tp.Tracer("example").Start(context.Background(), "op")
	defer span.End()

	var buf bytes.Buffer
	ctx = observability.WithLogger(ctx, slog.New(slog.NewTextHandler(&buf, nil)))
	ctx = pkgcore.WithTenant(ctx, pkgcore.TenantID("acme"))

	observability.FromContext(ctx).Info("note created", "note_id", "n-1")

	out := buf.String()
	fmt.Println(strings.Contains(out, `msg="note created"`))
	fmt.Println(strings.Contains(out, "note_id=n-1"))
	fmt.Println(strings.Contains(out, observability.TraceIDKey+"="))
	fmt.Println(strings.Contains(out, observability.SpanIDKey+"="))
	fmt.Println(strings.Contains(out, observability.TenantIDKey+"=acme"))

	// Output:
	// true
	// true
	// true
	// true
	// true
}

// ExampleRedactedValue shows the redaction layer in action on the log
// channel -- RedactedValue is exactly the marker a sink sees where a
// sensitive attribute used to be. Two distinct mechanisms are demonstrated,
// mirroring redact.go's doc comment: key-based redaction replaces the whole
// value of an attribute whose key is secret-shaped (access_token below),
// and value-shaped redaction masks a credential embedded inside an
// otherwise-benign attribute's text (the bearer token inside the error
// message below), while correlation fields and benign content pass through
// untouched. Redaction is on by default for every logger FromContext
// returns, with no per-call way to disable it.
func ExampleRedactedValue() {
	var buf bytes.Buffer
	ctx := observability.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&buf, nil)))

	// Key-based: the access_token attribute's value never reaches the sink.
	observability.FromContext(ctx).Info("outgoing request",
		"url", "https://api.example.com/v1/charge",
		"access_token", "sup3r-s3cr3t-v4lue-9876543210")

	// Value-shaped: a bearer credential embedded in an error message is
	// masked in place -- the error survives, its secret does not.
	observability.FromContext(ctx).Info("provider auth failed",
		"err", errors.New("provider auth failed: got 401 from https://idp.example.com with Bearer abcDEFgh1234567890XYZmnopQRSTuvWX"))

	// Benign content passes through untouched.
	observability.FromContext(ctx).Info("note created", "note_id", "n-1")

	out := buf.String()
	fmt.Println("key-redacted:", strings.Contains(out, "access_token="+observability.RedactedValue))
	fmt.Println("no plaintext token:", !strings.Contains(out, "sup3r-s3cr3t-v4lue-9876543210"))
	fmt.Println("masked in error text:", strings.Contains(out, "Bearer "+observability.RedactedValue))
	fmt.Println("benign attr survives:", strings.Contains(out, "note_id=n-1"))
	fmt.Println("msg survives:", strings.Contains(out, `msg="note created"`))

	// Output:
	// key-redacted: true
	// no plaintext token: true
	// masked in error text: true
	// benign attr survives: true
	// msg survives: true
}

// ExampleMiddleware shows the package's single most important behavioral
// guarantee, executed and checked, not just described in prose: two
// different tenants calling the exact same route must collapse into ONE
// metric series carrying no tenant_id label at all -- see the package doc
// comment's "tenant_id is not a metric label" section, and
// middleware_test.go's TestMiddleware_MetricsExcludeTenant_NoPerTenantSeries
// for the same negative control against the package's own tests.
func ExampleMiddleware() {
	// A private Prometheus registry, mirroring what a host's real
	// no-endpoint Init wires up internally (see init.go's
	// initLocalExporters), so this example can inspect exactly what
	// Middleware recorded without depending on Init or touching
	// os.Stdout.
	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		fmt.Println("build exporter:", err)
		return
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	otel.SetMeterProvider(mp)

	handler := observability.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, tenant := range []pkgcore.TenantID{"tenant-a", "tenant-b"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
		req = req.WithContext(pkgcore.WithTenant(req.Context(), tenant))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	families, err := reg.Gather()
	if err != nil {
		fmt.Println("gather:", err)
		return
	}

	var sawTenantLabel bool
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == observability.TenantIDKey {
					sawTenantLabel = true
				}
			}
		}
		if mf.GetName() != "http_server_request_count_total" {
			continue
		}
		fmt.Println("series:", len(mf.GetMetric()))
		fmt.Println("requests recorded:", mf.GetMetric()[0].GetCounter().GetValue())
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			// otel_scope_* are the exporter's own bookkeeping labels
			// (which meter produced this series), not a Middleware
			// request attribute -- irrelevant to what this example
			// demonstrates.
			if strings.HasPrefix(lp.GetName(), "otel_scope_") {
				continue
			}
			fmt.Printf("%s=%s\n", lp.GetName(), lp.GetValue())
		}
	}
	fmt.Println("tenant_id ever a label:", sawTenantLabel)

	// Output:
	// series: 1
	// requests recorded: 2
	// http_request_method=GET
	// http_response_status_code=200
	// http_route=/api/v1/notes
	// tenant_id ever a label: false
}

// ExampleAnnotateTenant shows why AnnotateTenant exists as its own
// function separate from Middleware: a trace Span is a shared mutable
// object that survives every context fork downstream of where it was
// created, so code that later resolves a tenant -- a business handler,
// per examples/reference-app/internal/notes/handler.go's NotesCreateNote
// method -- can still enrich the exact span Middleware started earlier
// in the chain, at a point where Middleware itself could not yet see
// one. It is a no-op with no tenant on ctx, which is Middleware's own
// expectation at its documented mounting point (see its own doc
// comment).
func ExampleAnnotateTenant() {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, span := tp.Tracer("example").Start(context.Background(), "op")
	observability.AnnotateTenant(pkgcore.WithTenant(ctx, pkgcore.TenantID("acme")))
	span.End()

	_, span2 := tp.Tracer("example").Start(context.Background(), "op")
	observability.AnnotateTenant(context.Background()) // no tenant on ctx: no-op
	span2.End()

	for i, s := range exp.GetSpans() {
		var tenant string
		for _, kv := range s.Attributes {
			if string(kv.Key) == observability.TenantIDKey {
				tenant = kv.Value.AsString()
			}
		}
		fmt.Printf("span %d tenant_id=%q\n", i, tenant)
	}

	// Output:
	// span 0 tenant_id="acme"
	// span 1 tenant_id=""
}
