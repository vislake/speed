package otlp_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	obs "github.com/vislake/speed/go/observability"
	_ "github.com/vislake/speed/go/observability/exporter/otlp"
)

// TestMiddleware_InvalidUTF8Request_ExportBatchStillArrives is the
// export-side half of the invalid-UTF-8 span regression whose
// label-formation half lives in the root package's middleware_test.go
// (TestMiddleware_InvalidUTF8Request_SpanNameAndAttributesCarryNoRawByte).
// That test proves, at span-formation time, that the exported span's name
// and attributes never carry the raw byte a request's %FF path (or
// invalid-byte header) delivers; this test proves the consequence that made
// the class a P1 rather than a cosmetic defect: the batch really encodes
// and really arrives at a collector.
//
// The mechanism this guards: net/http percent-decodes a request target
// byte-wise, so a %FF in the path reaches the middleware as a raw invalid
// byte with no parse error anywhere (header values pass raw bytes through
// the same net/http reader untouched). proto3 string fields must be valid
// UTF-8, the Go protobuf encoder refuses an entire ExportTraceServiceRequest
// containing one invalid string ("string field contains invalid UTF-8"),
// and otlptracegrpc drops the failed batch (codes.Internal sits outside its
// retry whitelist) -- so one request carrying one invalid byte would
// silently kill the export of every span in its batch, continuously, for
// the life of the process: sustained 100% trace loss, and traces are
// exactly what an operator reaches for to investigate the request that did
// it. The exporter has no way to save such a batch; the guard lives at
// span formation, and this test drives the whole composed path a real host
// wires --
// obs.Init with WithOTLPEndpoint (blank-importing this package, exactly as
// a host does), then obs.Middleware -- against an in-process OTLP/gRPC
// collector, and asserts on what the collector actually received.
//
// The batcher's shutdown flush must encode and deliver the one-span batch:
// the collector receives it, and the received span's name, url.path,
// user_agent.original and client.address carry the Unicode replacement rune
// in the invalid byte's place, never the byte itself.
func TestMiddleware_InvalidUTF8Request_ExportBatchStillArrives(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	collector := &invalidUTF8TraceCollector{got: make(chan *coltracepb.ExportTraceServiceRequest, 8)}
	grpcServer := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(grpcServer, collector)
	colmetricpb.RegisterMetricsServiceServer(grpcServer, invalidUTF8MetricsCollector{})
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	shutdown, err := obs.Init(context.Background(),
		obs.WithOTLPEndpoint(lis.Addr().String()),
		obs.WithOTLPInsecure(true),
	)
	if err != nil {
		t.Fatalf("Init with the in-process collector: %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := shutdown(context.Background()); shutdownErr != nil {
			t.Errorf("shutdown: %v", shutdownErr)
		}
	})

	handler := obs.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/%FFjunk", nil)
	if utf8.ValidString(req.URL.Path) {
		t.Fatalf("test setup: URL.Path %q must not be valid UTF-8 for this regression to be exercised", req.URL.Path)
	}
	req.Header.Set("User-Agent", "probe\xffagent")
	req.Header.Set("X-Forwarded-For", "1.2.3.4\xff")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Init's shutdown flushes the trace batcher synchronously: after it
	// returns, the one-span batch has either reached the collector or has
	// been dropped by the exporter. (The cleanup's later invocation of the
	// same shutdown func is a once-guarded no-op.)
	if shutdownErr := shutdown(context.Background()); shutdownErr != nil {
		t.Fatalf("shutdown flush after one invalid-UTF-8 request: %v -- the trace batch must encode; before the fix the protobuf marshal fails on the raw byte and the batch is dropped", shutdownErr)
	}

	var export *coltracepb.ExportTraceServiceRequest
	select {
	case export = <-collector.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-process collector received no export within 5s: an invalid-UTF-8 request string must not stop the trace batch from encoding and exporting")
	}

	spans := collectSpans(export)
	if len(spans) != 1 {
		t.Fatalf("expected the received batch to carry exactly 1 span, got %d", len(spans))
	}
	span := spans[0]

	wantPath := "/api/" + string(utf8.RuneError) + "junk"
	if want := "GET " + wantPath; span.Name != want {
		t.Errorf("exported span name = %q, want %q: the raw invalid path byte must not reach the span name", span.Name, want)
	}
	if got, ok := spanStringAttr(span, "url.path"); !ok || got != wantPath {
		t.Errorf("exported url.path = %q (present: %v), want %q", got, ok, wantPath)
	}
	wantUA := "probe" + string(utf8.RuneError) + "agent"
	if got, ok := spanStringAttr(span, "user_agent.original"); !ok || got != wantUA {
		t.Errorf("exported user_agent.original = %q (present: %v), want %q", got, ok, wantUA)
	}
	wantAddr := "1.2.3.4" + string(utf8.RuneError)
	if got, ok := spanStringAttr(span, "client.address"); !ok || got != wantAddr {
		t.Errorf("exported client.address = %q (present: %v), want %q", got, ok, wantAddr)
	}
	// Blanket scan over the decoded batch: no string field of the received
	// span may hold an invalid-UTF-8 value -- the condition that would make
	// a batch unencodable.
	for _, kv := range span.Attributes {
		if _, isString := kv.Value.Value.(*commonv1.AnyValue_StringValue); isString && !utf8.ValidString(kv.Value.GetStringValue()) {
			t.Errorf("exported span attribute %q carries a value that is not valid UTF-8 (%q)", kv.Key, kv.Value.GetStringValue())
		}
	}
}

// invalidUTF8TraceCollector is an in-process OTLP/gRPC trace collector that
// records every ExportTraceServiceRequest it receives on a channel for the
// test to assert against. The channel is buffered beyond the number of
// batches this test can produce, so a receive is never dropped.
type invalidUTF8TraceCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	got chan *coltracepb.ExportTraceServiceRequest
}

func (c *invalidUTF8TraceCollector) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	c.got <- req
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// invalidUTF8MetricsCollector accepts every OTLP metrics export
// unconditionally, so Init's metric exporter (whose periodic reader flushes
// on the same shutdown) has a registered service to talk to -- mirroring
// example_test.go's exampleMetricsCollector.
type invalidUTF8MetricsCollector struct {
	colmetricpb.UnimplementedMetricsServiceServer
}

func (invalidUTF8MetricsCollector) Export(_ context.Context, _ *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

// collectSpans flattens every span in req into one slice.
func collectSpans(req *coltracepb.ExportTraceServiceRequest) []*tracepb.Span {
	var out []*tracepb.Span
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			out = append(out, ss.Spans...)
		}
	}
	return out
}

// spanStringAttr returns the string value of the attribute with key key,
// reporting presence. A non-string value under the same key reports absent.
func spanStringAttr(span *tracepb.Span, key string) (string, bool) {
	for _, kv := range span.Attributes {
		if kv.Key != key {
			continue
		}
		sv, ok := kv.Value.Value.(*commonv1.AnyValue_StringValue)
		if !ok {
			return "", false
		}
		return sv.StringValue, true
	}
	return "", false
}
