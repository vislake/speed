package log

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// TestFromContextWithoutLoggerReturnsDefault pins the fallback: the process
// default logger itself, not a panic, not a logger that drops records, and not
// a freshly built one.
func TestFromContextWithoutLoggerReturnsDefault(t *testing.T) {
	if got := FromContext(context.Background()); got != Default() {
		t.Errorf("FromContext on a bare context returned %p, want the default logger %p", got, Default())
	}
}

// TestFallbackDoesNotReachTheConfiguredDestination pins where the fallback
// writes, which is the half that is easy to get wrong.
//
// A fallback onto the assembled chain reads like the helpful thing to do, and
// it is exactly what must not happen: a path that was never injected would
// then be indistinguishable from one that was, and the missing injection would
// never be noticed. The record has to stay off the configured file.
//
// The chain here is assembled by this module's own New, over a configuration
// with no output to standard output at all, because that is the shape the
// mistake takes: a product exists, and the fallback is tempted to use it.
func TestFallbackDoesNotReachTheConfiguredDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configured.log")
	product := newTestLogger(t, fileConfig(path))

	captureBootstrapStdout(t, func() {
		FromContext(context.Background()).Info("orphan record")
	})

	if out := read(t, path); strings.Contains(out, "orphan record") {
		t.Errorf("the fallback wrote into the configured destination: %q", out)
	}
	// The chain is not idle by construction: a logger taken from the product
	// does reach that file, so the assertion above is about where the
	// fallback went, not about a chain that was never connected.
	product.Named("http").Info("injected record")
	if out := read(t, path); !strings.Contains(out, "injected record") {
		t.Fatalf("the configured destination took nothing at all: %q", out)
	}
}

// TestFallbackAppearsOnStandardOutput is the other half: the record is not
// lost, it is on standard output. Together the two say where a missing
// injection shows up, which is what makes it noticeable without a failure
// signal.
func TestFallbackAppearsOnStandardOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configured.log")
	newTestLogger(t, fileConfig(path))

	out := captureBootstrapStdout(t, func() {
		FromContext(context.Background()).Info("orphan record")
	})

	if !strings.Contains(out, "orphan record") {
		t.Errorf("the fallback record did not reach standard output: %q", out)
	}
}

// TestInjectedLoggerReachesTheConfiguredDestination is the positive control
// for the pair above: once the context carries the logger the product handed
// out, the records of that path are in the configured file.
func TestInjectedLoggerReachesTheConfiguredDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configured.log")
	product := newTestLogger(t, fileConfig(path))

	ctx := WithLogger(context.Background(), product.Named("http"))
	ctx = WithAttrs(ctx, "trace_id", "t-3")
	FromContext(ctx).Info("served")

	out := read(t, path)
	for _, want := range []string{`"msg":"served"`, `"trace_id":"t-3"`, `"` + moduleAttrKey + `":"http"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the configured destination lacks %s: %q", want, out)
		}
	}
}

// TestFromContextFallbackStillPassesThroughRedaction pins that the fallback is
// not a way around the redaction layer: the bootstrap chain carries one, so a
// record written from a path with no logger is judged by the same rules.
func TestFromContextFallbackStillPassesThroughRedaction(t *testing.T) {
	isolateRules(t)
	const secret = "fallback-plaintext-credential"
	processRedaction.AddKeys("fallback-context-key")

	out := captureBootstrapStdout(t, func() {
		FromContext(context.Background()).Info("no logger here", "fallback-context-key", secret)
	})

	if strings.Contains(out, secret) {
		t.Errorf("the fallback path wrote the credential in the clear: %q", out)
	}
	if !strings.Contains(out, maskText) {
		t.Errorf("nothing was masked on the fallback path: %q", out)
	}
}

// TestWithLoggerRoundTrips pins that what was injected is what comes back, and
// that the context it was derived from is untouched.
func TestWithLoggerRoundTrips(t *testing.T) {
	injected := slog.New(newCapture())
	bare := context.Background()
	ctx := WithLogger(bare, injected)

	if got := FromContext(ctx); got != injected {
		t.Errorf("FromContext returned %p, want the injected logger %p", got, injected)
	}
	if got := FromContext(bare); got != Default() {
		t.Errorf("the context the injection derived from changed: %p", got)
	}
}

// TestWithAttrsAccumulatesAlongTheContext pins the take, bind and put back:
// each injection point adds its own fields and the ones bound upstream survive.
//
// The assertion is on the rendered record, because binding is what the
// downstream handler preprocesses: an attribute that never reaches the encoder
// is an attribute the destination does not carry.
func TestWithAttrsAccumulatesAlongTheContext(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithLogger(context.Background(),
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: allLevels})))
	ctx = WithAttrs(ctx, "trace_id", "t-1")
	ctx = WithAttrs(ctx, "tenant_id", "n-2")

	FromContext(ctx).Info("served")

	out := buf.String()
	for _, want := range []string{"trace_id=t-1", "tenant_id=n-2", "msg=served"} {
		if !strings.Contains(out, want) {
			t.Errorf("the record lacks %q: %q", want, out)
		}
	}
}

// TestWithAttrsOnBareContextAccumulatesOnDefaultChain pins that the fallback
// composes: attributes bound onto a context that never had a logger are kept,
// the record is complete, and it is still on the bootstrap chain.
func TestWithAttrsOnBareContextAccumulatesOnDefaultChain(t *testing.T) {
	ctx := WithAttrs(context.Background(), "trace_id", "t-9")
	ctx = WithAttrs(ctx, "tenant_id", "n-9")

	out := captureBootstrapStdout(t, func() { FromContext(ctx).Info("served") })

	for _, want := range []string{"trace_id=t-9", "tenant_id=n-9", "msg=served"} {
		if !strings.Contains(out, want) {
			t.Errorf("the bootstrap chain's record lacks %q: %q", want, out)
		}
	}
}

// TestFromContextIgnoresAForeignValueUnderItsKey pins that the fallback holds
// when a context carries something that is not a logger, rather than panicking
// on the type assertion in the middle of a log call.
func TestFromContextIgnoresAForeignValueUnderItsKey(t *testing.T) {
	//nolint:staticcheck // the point is a value stored under a key this
	// package does not control, which is what a raw context.WithValue does.
	ctx := context.WithValue(context.Background(), contextKey{}, "not a logger")
	if got := FromContext(ctx); got != Default() {
		t.Errorf("FromContext returned %p for a foreign value, want the default logger", got)
	}

	var missing *slog.Logger
	if got := FromContext(WithLogger(context.Background(), missing)); got != Default() {
		t.Errorf("FromContext returned %p for an injected nil logger, want the default logger", got)
	}
}
