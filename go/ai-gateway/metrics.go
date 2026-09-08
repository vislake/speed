package aigateway

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// InstrumentationName identifies this package's own meter, mirroring
// go/jobs/standalone_queue.go's, go/notification/delivery.go's and
// go/authn/service.go's identical use of their own package path.
const InstrumentationName = "github.com/vislake/speed/go/ai-gateway"

// Metric instrument names registerAIGatewayMetrics wires under
// InstrumentationName -- the "AI gateway" row of
// docs/internal/09-observability.md's must-instrument table: per-provider
// call volume, latency and error rate, plus rate-limit hits. All
// low-cardinality labels, never tenant_id. The row is covered as
// follows:
//
//   - aigateway.provider.calls -- one count per genuine provider
//     invocation, labeled by the provider the route resolved to and the
//     kind of call (chat, chat_stream or image). Labeled by provider,
//     never by model: the row's purpose is isolating third-party
//     instability to the concrete provider.
//   - aigateway.provider.errors -- one count per failed provider
//     invocation (chat and image calls whose provider returned an
//     error; a chat stream whose terminal chunk carries Err), same
//     labels. A provider's error rate is errors over calls for that
//     provider.
//   - aigateway.provider.duration -- one provider invocation's
//     duration, same labels (for a stream, the establishment call's
//     duration; stream-tail latency is the relay path's own signal).
//   - aigateway.rate_limited -- one count per call the gateway's OWN
//     per-tenant rate limiter refused (ratelimit.go's checkRateLimit,
//     the single refusal site shared by every entry point). No
//     provider label: the check runs before provider resolution by
//     design. The row's rate-limit-hits half -- a hit means the gateway
//     protected its tenant dimension, never that a provider throttled
//     the gateway (that class surfaces as provider errors/duration).
//
// The image-generation half of the module invokes providers only from
// the async job path (image_gateway.go's imageGenerateHandler.callProvider,
// the single choke point every image operation's provider call passes
// through), so its calls/errors/duration are recorded there, under the
// job's own resolved provider, rather than at GenerateImage's enqueue
// site -- enqueuing is not a provider invocation.
const (
	providerCallsMetricName    = "aigateway.provider.calls"
	providerErrorsMetricName   = "aigateway.provider.errors"
	providerDurationMetricName = "aigateway.provider.duration"
	rateLimitedMetricName      = "aigateway.rate_limited"

	providerAttr = "provider"
	kindAttr     = "kind"

	providerCallChat       = "chat"
	providerCallChatStream = "chat_stream"
	providerCallImage      = "image"
)

// registerAIGatewayMetrics wires the four instruments this file's doc
// comment names, once per constructed Gateway (NewGateway), with the
// registration error ignored the same way go/observability/middleware.go
// ignores it -- the global otel API returns a working no-op instrument
// alongside any error.
func registerAIGatewayMetrics() (
	metric.Int64Counter, metric.Int64Counter, metric.Float64Histogram, metric.Int64Counter,
) {
	meter := otel.Meter(InstrumentationName)
	calls, _ := meter.Int64Counter(
		providerCallsMetricName,
		metric.WithDescription("Provider invocations, labeled by the resolved provider and the kind of call (chat, chat_stream or image)."),
		metric.WithUnit("{call}"),
	)
	errors, _ := meter.Int64Counter(
		providerErrorsMetricName,
		metric.WithDescription("Failed provider invocations (a chat or image call whose provider returned an error, a chat stream whose terminal chunk carries Err), labeled like the calls."),
		metric.WithUnit("{call}"),
	)
	duration, _ := meter.Float64Histogram(
		providerDurationMetricName,
		metric.WithDescription("Duration of one provider invocation, in seconds, labeled like the calls."),
		metric.WithUnit("s"),
	)
	rateLimited, _ := meter.Int64Counter(
		rateLimitedMetricName,
		metric.WithDescription("Calls the gateway's own per-tenant rate limiter refused (checkRateLimit's single refusal site); a hit means the gateway protected its tenant dimension."),
		metric.WithUnit("{call}"),
	)
	return calls, errors, duration, rateLimited
}

// recordProviderCall counts one provider invocation, records its
// duration and, on failure, the error -- called at the synchronous
// provider-call sites (Chat, ChatStream's establishment, the image job
// handler's callProvider). Instruments are nil-guarded for a Gateway
// built as a bare struct literal (no such construction exists in
// shipped code; tests build through NewGateway).
func (g *Gateway) recordProviderCall(
	ctx context.Context, provider, kind string, start time.Time, err error,
) {
	if g.providerCalls == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String(providerAttr, provider),
		attribute.String(kindAttr, kind),
	)
	g.providerCalls.Add(ctx, 1, attrs)
	if err != nil && g.providerErrors != nil {
		g.providerErrors.Add(ctx, 1, attrs)
	}
	if g.providerDuration != nil {
		g.providerDuration.Record(ctx, time.Since(start).Seconds(), attrs)
	}
}

// recordProviderStreamError counts a stream whose terminal chunk
// carried Err -- the stream error that arrives after establishment and
// therefore never passes through recordProviderCall's error leg.
func (g *Gateway) recordProviderStreamError(ctx context.Context, provider string) {
	if g.providerErrors == nil {
		return
	}
	g.providerErrors.Add(ctx, 1, metric.WithAttributes(
		attribute.String(providerAttr, provider),
		attribute.String(kindAttr, providerCallChatStream),
	))
}

// recordRateLimitHit counts one refusal by the gateway's own limiter.
func (g *Gateway) recordRateLimitHit(ctx context.Context) {
	if g.rateLimitedCounter == nil {
		return
	}
	g.rateLimitedCounter.Add(ctx, 1)
}
