package observability_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// newRecordingSpan starts and ends a real span from a real TracerProvider
// wired to the OTel SDK's own in-memory exporter (tracetest), returning
// the live SpanContext it was assigned. This is deliberately not a mock:
// FromContext's job is to read whatever a genuine OTel SDK span produces,
// so the test has to hand it one.
func newRecordingSpan(t *testing.T) (context.Context, trace.SpanContext) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("observability_test").Start(context.Background(), "op")
	sc := span.SpanContext()
	span.End()

	if !sc.IsValid() {
		t.Fatal("expected the SDK to assign a valid SpanContext")
	}
	return ctx, sc
}

// textLoggerCtx returns a context carrying (via obs.WithLogger) a
// *slog.Logger that writes key=value text to buf, so a test can assert on
// exactly which fields FromContext attached.
func textLoggerCtx(ctx context.Context, buf *bytes.Buffer) context.Context {
	return obs.WithLogger(ctx, slog.New(slog.NewTextHandler(buf, nil)))
}

func TestFromContext_AttachesTraceAndSpanID(t *testing.T) {
	ctx, sc := newRecordingSpan(t)
	var buf bytes.Buffer
	ctx = textLoggerCtx(ctx, &buf)

	obs.FromContext(ctx).Info("test event")

	out := buf.String()
	if want := obs.TraceIDKey + "=" + sc.TraceID().String(); !strings.Contains(out, want) {
		t.Errorf("log line missing %q; got: %s", want, out)
	}
	if want := obs.SpanIDKey + "=" + sc.SpanID().String(); !strings.Contains(out, want) {
		t.Errorf("log line missing %q; got: %s", want, out)
	}
}

func TestFromContext_AttachesTenantID(t *testing.T) {
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("acme"))
	var buf bytes.Buffer
	ctx = textLoggerCtx(ctx, &buf)

	obs.FromContext(ctx).Info("test event")

	if want := obs.TenantIDKey + "=acme"; !strings.Contains(buf.String(), want) {
		t.Errorf("log line missing %q; got: %s", want, buf.String())
	}
}

func TestFromContext_AttachesBothWhenBothPresent(t *testing.T) {
	ctx, sc := newRecordingSpan(t)
	ctx = pkgcore.WithTenant(ctx, pkgcore.TenantID("acme"))
	var buf bytes.Buffer
	ctx = textLoggerCtx(ctx, &buf)

	obs.FromContext(ctx).Info("test event")

	out := buf.String()
	for _, want := range []string{
		obs.TraceIDKey + "=" + sc.TraceID().String(),
		obs.SpanIDKey + "=" + sc.SpanID().String(),
		obs.TenantIDKey + "=acme",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log line missing %q; got: %s", want, out)
		}
	}
}

// TestFromContext_NoSpanNoTenant_OmitsFields is the negative control for
// the two tests above: with neither a span nor a tenant on the context,
// none of the three fields should appear -- FromContext must not invent a
// placeholder value (an empty string, a zero ID) for a signal that simply
// is not there. This matters just as much as the positive-attachment
// tests: a background job with no HTTP request in flight, or a request
// upstream of tenancy.Middleware, must still get a working logger.
func TestFromContext_NoSpanNoTenant_OmitsFields(t *testing.T) {
	var buf bytes.Buffer
	ctx := textLoggerCtx(context.Background(), &buf)

	obs.FromContext(ctx).Info("test event")

	out := buf.String()
	for _, key := range []string{obs.TraceIDKey, obs.SpanIDKey, obs.TenantIDKey} {
		if strings.Contains(out, key+"=") {
			t.Errorf("expected no %s field with neither a span nor a tenant in context; got: %s", key, out)
		}
	}
}

func TestFromContext_FallsBackToSlogDefault(t *testing.T) {
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

	// No WithLogger call anywhere on this context: FromContext must fall
	// back to whatever slog.Default() currently is, read fresh rather
	// than cached, per its own doc comment.
	obs.FromContext(context.Background()).Info("via default")

	if !strings.Contains(buf.String(), "via default") {
		t.Errorf("expected FromContext to fall back to slog.Default(); got: %s", buf.String())
	}
}

// TestFromContext_NoEnrichment_WritesThroughAttachedSink pins down what
// FromContext returns when ctx carries nothing to enrich: no longer the
// exact *slog.Logger WithLogger attached -- the redaction layer sits
// between the logger and the sink now -- but a logger that writes through
// the very same sink handler, so the attached sink's output (formatting,
// level filter, destination) is preserved bit for bit.
func TestFromContext_NoEnrichment_WritesThroughAttachedSink(t *testing.T) {
	var buf bytes.Buffer
	custom := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := obs.WithLogger(context.Background(), custom)

	logger := obs.FromContext(ctx)
	if logger == custom {
		t.Error("expected FromContext to return a wrapped logger, not the exact attached one: redaction must sit between the logger and the sink even when there is nothing to enrich")
	}

	// The sink's own filtering still applies to the wrapped logger...
	logger.Info("dropped")
	logger.Warn("kept")
	// ...and the redaction layer is active even with zero enrichment attrs.
	logger.Warn("sink shared", "token", "plaintext-token-value")

	out := buf.String()
	if strings.Contains(out, "dropped") {
		t.Errorf("expected the attached sink's level filter to still apply to the wrapped logger; got: %s", out)
	}
	if !strings.Contains(out, "kept") {
		t.Errorf("expected the wrapped logger to write through the attached sink; got: %s", out)
	}
	if strings.Contains(out, "plaintext-token-value") {
		t.Errorf("redaction must apply even when FromContext has nothing to enrich: token value reached the sink; got: %s", out)
	}
	if !strings.Contains(out, obs.RedactedValue) {
		t.Errorf("expected the token value to be replaced by %q; got: %s", obs.RedactedValue, out)
	}
}

func TestWithLogger_ChildContextInheritsLogger(t *testing.T) {
	var buf bytes.Buffer
	ctx := textLoggerCtx(context.Background(), &buf)

	// A context derived from ctx (as every downstream call in a request or
	// job does) must still find the attached logger -- WithLogger's whole
	// point is surviving exactly this kind of derivation.
	child := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme"))

	obs.FromContext(child).Info("test event")

	if want := obs.TenantIDKey + "=acme"; !strings.Contains(buf.String(), want) {
		t.Errorf("expected the logger attached to the parent context to still be used; got: %s", buf.String())
	}
}
