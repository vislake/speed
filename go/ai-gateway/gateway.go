package aigateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/ratelimit"
	"github.com/vislake/speed/go/storage"
)

// usageFeatureChatTokens is the Feature dimension Gateway reports for every
// successful chat call -- the token-count dimension the design doc names
// as the default AI metering case.
const usageFeatureChatTokens = "ai.chat_tokens"

// Gateway is the facade business code calls: gateway.Chat and
// gateway.ChatStream are the ONLY entry points a business module uses --
// never a ChatProvider directly. See this package's own doc comment for
// the full six-step pipeline every call runs.
//
// Round 2 adds Gateway.GenerateImage, the async-only counterpart for image
// generation -- see image_gateway.go for its own pipeline and the
// ImageProvider/storage boundary it documents.
//
// The zero value is not ready to use; construct one with NewGateway.
type Gateway struct {
	credentials *CredentialService
	registry    *pkgcore.SeamRegistry[ChatProvider]
	routes      map[string]ModelRoute

	// entitlements and usage are both optional -- see Entitlements' and
	// UsageRecorder's own doc comments for exactly what shipping without
	// either means.
	entitlements Entitlements
	usage        UsageRecorder

	// imageRegistry is the package-level ImageProviderRegistry a Gateway
	// resolves ImageProvider implementations from, by default -- see
	// image_registry.go. WithImageProviderRegistry overrides it, mirroring
	// WithChatProviderRegistry above.
	imageRegistry *pkgcore.SeamRegistry[ImageProvider]

	// imageQueue and objectService are the two seams Gateway.GenerateImage
	// needs to run at all: the jobs.Queue the generated task is enqueued on,
	// and the go/storage ObjectService the job handler reads input images
	// from and writes generated output images to. Both are nil until
	// WithImageGeneration is applied -- see image_gateway.go's own doc
	// comment for why importing storage and jobs directly here is an
	// ordinary downward dependency, unlike Entitlements/UsageRecorder.
	imageQueue    jobs.Queue
	objectService *storage.ObjectService

	// imageJobs is the per-job idempotency marker store
	// imageGenerateHandler.Handle consults before ever calling an
	// ImageProvider (image_job_store.go) -- built once, in NewGateway,
	// from credentials' own database connection (never a second db
	// parameter of its own: an ai-gateway Gateway already has exactly one
	// database, the one backing credentials, and this table is this
	// module's second use of it). nil only when NewGateway was given nil
	// credentials, a combination image_gateway.go's own WithImageGeneration
	// doc comment already treats as unsupported.
	imageJobs *imageJobRepository

	// host and limiter back checkRateLimit's per-tenant rate limiting
	// (ratelimit.go): host is attached by Module.Register (mirroring
	// go/sharing's identical hostSeams wiring), and limiter is the
	// test-injection point ratelimit_test.go uses to isolate a Gateway
	// under test from a real KVStore -- see rateLimiter's own doc comment.
	host    hostSeams
	limiter ratelimit.Limiter

	// Metric instruments (metrics.go): the 09-table AI-gateway row's
	// per-provider calls/errors/duration plus the rate-limit-hit
	// counter, registered by NewGateway; nil for a bare struct literal,
	// which the record sites guard.
	providerCalls      metric.Int64Counter
	providerErrors     metric.Int64Counter
	providerDuration   metric.Float64Histogram
	rateLimitedCounter metric.Int64Counter
}

// GatewayOption configures a Gateway at construction time.
type GatewayOption func(*Gateway)

// WithEntitlements wires the model-access-control seam Gateway.Chat/
// ChatStream check before resolving a credential or calling a provider.
// Without this option, the Gateway enforces NO quota at all -- see
// Entitlements' own doc comment.
func WithEntitlements(e Entitlements) GatewayOption {
	return func(g *Gateway) { g.entitlements = e }
}

// WithUsageRecorder wires the automatic usage-reporting seam Gateway.Chat/
// ChatStream call after a successful response. Without this option, no
// usage is reported anywhere -- see UsageRecorder's own doc comment.
func WithUsageRecorder(r UsageRecorder) GatewayOption {
	return func(g *Gateway) { g.usage = r }
}

// WithChatProviderRegistry overrides the package-level ChatProviderRegistry
// a Gateway resolves providers from. Tests use this to isolate a Gateway
// under test from the process-global registry's real registrations;
// production code has no reason to call it.
func WithChatProviderRegistry(registry *pkgcore.SeamRegistry[ChatProvider]) GatewayOption {
	return func(g *Gateway) {
		if registry != nil {
			g.registry = registry
		}
	}
}

// NewGateway returns a Gateway resolving credentials through credentials
// and, by default, providers through the package-level ChatProviderRegistry
// -- apply WithModelRoute at least once per logical model key a caller will
// use, or every call for that key fails with ErrUnroutedModel.
func NewGateway(credentials *CredentialService, opts ...GatewayOption) *Gateway {
	calls, errors, duration, rateLimited := registerAIGatewayMetrics()
	g := &Gateway{
		credentials:        credentials,
		registry:           ChatProviderRegistry,
		routes:             make(map[string]ModelRoute),
		imageRegistry:      ImageProviderRegistry,
		providerCalls:      calls,
		providerErrors:     errors,
		providerDuration:   duration,
		rateLimitedCounter: rateLimited,
	}
	if credentials != nil {
		g.imageJobs = newImageJobRepository(credentials.store.db)
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// resolve runs the routing and credential-resolution legs of the pipeline
// shared by Chat and ChatStream: look up logicalModel's ModelRoute, resolve
// its credential, and build a fresh ChatProvider instance from the two.
func (g *Gateway) resolve(ctx context.Context, logicalModel string) (ChatProvider, ModelRoute, error) {
	route, ok := g.routes[logicalModel]
	if !ok {
		return nil, ModelRoute{}, ErrUnroutedModel.WithParam("model", logicalModel)
	}

	cred, err := g.credentials.Resolve(ctx, route.Provider)
	if err != nil {
		return nil, route, err
	}

	provider, _, err := g.registry.Build(route.Provider, pkgcore.Config{
		"base_url": cred.BaseURL,
		"api_key":  cred.APIKey,
	})
	if err != nil {
		return nil, route, fmt.Errorf("aigateway: resolve provider %q for model %q: %w", route.Provider, logicalModel, err)
	}
	// A credential that resolved at the tenant tier is the caller's own
	// influence over where this call dials, so its provider must dial
	// through the SSRF-guarded client -- the dial-time re-check that closes
	// the DNS-rebinding window between this credential's validated write
	// and this call (ssrf.go's file header). A provider that cannot carry
	// the guarded client (it does not implement httpClientSettable) is
	// refused here rather than silently dialing unguarded -- the failure
	// mode guardTenantScopeDial's own doc comment describes. A platform-
	// tier row is the operator's own default and is left on the provider's
	// ordinary client.
	if err := guardTenantScopeDial(provider, cred.Scope); err != nil {
		// The one error guardTenantScopeDial returns is the coded
		// ErrProviderNotSSRFGuardable; apperr.As picks it out of the error
		// interface so the route context the guard itself does not have can
		// be attached as params (WithParam derives a new *apperr.Error; the
		// shared sentinel stays untouched).
		if appErr, ok := apperr.As(err); ok {
			err = appErr.WithParam("provider", route.Provider).WithParam("model", logicalModel)
		}
		return nil, route, err
	}
	return provider, route, nil
}

// checkEntitlement runs the entitlement-gate leg of the pipeline. It is a
// no-op (nil error) when no Entitlements seam is wired.
func (g *Gateway) checkEntitlement(ctx context.Context, logicalModel string) error {
	if g.entitlements == nil {
		return nil
	}
	decision, err := g.entitlements.Check(ctx, "model:"+logicalModel, 1)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return ErrEntitlementDenied.WithParam("model", logicalModel).WithParam("reason", decision.Reason)
	}
	return nil
}

// recordUsage runs the automatic-metering leg of the pipeline. It is a
// no-op when no UsageRecorder is wired, and it never fails the call it is
// reporting for: a recording failure is logged and swallowed, per
// UsageRecorder's own doc comment.
//
// The recorded Quantity is usage.TotalTokens, with one honest fallback:
// a vendor-reported total of zero alongside nonzero prompt or completion
// parts is self-contradictory (a genuinely token-free call would report
// zero for all three), and some OpenAI-compatible hosts produce exactly
// that shape by omitting total_tokens from a partial usage object -- the
// parts' sum is recorded then, with a warning that the correction
// happened, mirroring warnIfNoUsage's identical make-the-gap-visible
// stance. The Usage the caller's ChatResponse/ChatChunk carries is never
// rewritten: this correction is metering policy, applied at the one place
// usage is turned into a billable quantity.
func (g *Gateway) recordUsage(ctx context.Context, logicalModel string, usage Usage) {
	if g.usage == nil {
		return
	}
	// A context carrying no tenant has no meaningful UsageEvent.TenantID
	// to attribute usage to; this is not an error path -- an entitlement-
	// less, tenant-less call is legal (a system-context caller, or simply
	// no Entitlements wired), it just produces no usage report.
	tenant, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return
	}

	quantity := float64(usage.TotalTokens)
	if usage.TotalTokens == 0 && (usage.PromptTokens > 0 || usage.CompletionTokens > 0) {
		obs.FromContext(ctx).Warn("aigateway: vendor reported zero total tokens with nonzero prompt or completion tokens; recording their sum",
			"prompt_tokens", usage.PromptTokens,
			"completion_tokens", usage.CompletionTokens,
		)
		quantity = float64(usage.PromptTokens + usage.CompletionTokens)
	}

	event := UsageEvent{
		TenantID:       string(tenant),
		Feature:        usageFeatureChatTokens,
		Quantity:       quantity,
		IdempotencyKey: newIdempotencyKey(),
		Metadata:       map[string]string{"model": logicalModel},
	}
	if err := g.usage.Record(ctx, event); err != nil {
		obs.FromContext(ctx).Warn("aigateway: usage recording failed",
			"model", logicalModel, "error", err)
	}
}

// newIdempotencyKey returns a fresh, random idempotency key for one
// UsageEvent. See UsageEvent.IdempotencyKey's own doc comment for why this
// is not derived from any caller-supplied business operation id.
func newIdempotencyKey() string {
	var buf [16]byte
	// crypto/rand.Read never returns an error on this toolchain: it always
	// fills b entirely, and a genuine underlying failure crashes the process
	// irrecoverably rather than surfacing as an error value. Discarding its
	// result is therefore the correct way to write against this API -- there
	// is no surviving path on which this buffer stays zero-filled -- not the
	// swallowing of a real error. Do not copy this shape onto a reader that
	// does return errors; there, the same discard would silently produce a
	// constant, non-random key.
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// Chat runs the full pipeline (entitlement check, credential resolution,
// provider resolution, the provider call, then automatic usage reporting)
// for one non-streaming chat request. req.Model is a logical model key --
// see ChatRequest's own doc comment.
func (g *Gateway) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if err := req.validate(); err != nil {
		return ChatResponse{}, err
	}
	logicalModel := req.Model

	// The rate limiter is per-tenant: a tenantless call (a system-context
	// caller -- CredentialService.Resolve's own doc comment names tenantless
	// calls legal) has no tenant dimension for it, so the check is skipped
	// rather than silently keyed on the empty string and shared with every
	// other tenantless caller -- see checkRateLimit's own doc comment. The
	// entitlement and resolve legs below run for tenantless callers exactly
	// as they always did.
	if tenant, ok := pkgcore.TenantFromContext(ctx); ok {
		if err := g.checkRateLimit(ctx, string(tenant)); err != nil {
			return ChatResponse{}, err
		}
	}

	if err := g.checkEntitlement(ctx, logicalModel); err != nil {
		return ChatResponse{}, err
	}

	provider, route, err := g.resolve(ctx, logicalModel)
	if err != nil {
		return ChatResponse{}, err
	}

	vendorReq := req
	vendorReq.Model = route.VendorModel

	// aigateway.provider.* (metrics.go): one invocation of the resolved
	// provider, counted with its duration and, on failure, its error.
	start := time.Now()
	resp, err := provider.Chat(ctx, vendorReq)
	g.recordProviderCall(ctx, route.Provider, providerCallChat, start, err)
	if err != nil {
		return ChatResponse{}, err
	}

	obs.FromContext(ctx).Info("aigateway: chat completed",
		"model", logicalModel,
		"provider", route.Provider,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
	)
	g.recordUsage(ctx, logicalModel, resp.Usage)
	return resp, nil
}

// ChatStream runs the same pipeline as Chat for a streaming request, and
// returns a channel of incremental chunks honoring ChatChunk's own channel
// contract. Usage is reported to the wired UsageRecorder only once the
// stream's terminal success chunk (real Usage) is observed -- never
// speculatively at stream start, and never on the terminal error chunk --
// and at most once per response: a provider that sets Usage on several
// chunks is recorded once, never per chunk (relayStream's own doc
// comment).
func (g *Gateway) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	logicalModel := req.Model

	// The identical per-tenant limiter gating Chat's own call site applies
	// here -- see the comment there for why a tenantless context skips the
	// check rather than feeding it the empty string.
	if tenant, ok := pkgcore.TenantFromContext(ctx); ok {
		if err := g.checkRateLimit(ctx, string(tenant)); err != nil {
			return nil, err
		}
	}

	if err := g.checkEntitlement(ctx, logicalModel); err != nil {
		return nil, err
	}

	provider, route, err := g.resolve(ctx, logicalModel)
	if err != nil {
		return nil, err
	}

	vendorReq := req
	vendorReq.Model = route.VendorModel

	// aigateway.provider.* (metrics.go): the stream's establishment is
	// one provider invocation -- counted with its duration here; a
	// stream that fails AFTER establishment is counted by relayStream
	// (recordProviderStreamError) when its terminal chunk carries Err.
	start := time.Now()
	upstream, err := provider.ChatStream(ctx, vendorReq)
	g.recordProviderCall(ctx, route.Provider, providerCallChatStream, start, err)
	if err != nil {
		return nil, err
	}

	out := make(chan ChatChunk)
	go g.relayStream(ctx, logicalModel, route.Provider, upstream, out)
	return out, nil
}

// relayStream forwards every chunk from upstream to out unchanged,
// intercepting the terminal success chunk (Usage != nil) to log completion
// and report usage before forwarding it -- the "only after the final chunk
// carries real usage" rule the design doc states explicitly. It closes out
// exactly once, whenever upstream closes or ctx is done.
//
// Usage is reported at most once per response, whatever upstream sends:
// ChatChunk's own doc contract promises Usage is non-nil only on the
// terminal success chunk, but relayStream is the billing boundary and does
// not trust a provider implementation to keep that promise -- a provider
// setting Usage on many chunks must never multiply the tenant's metering
// events, so the once-flag below admits only the first usage-bearing chunk
// to the completion log and the recorder. Every chunk is still forwarded
// unchanged; only the recording is gated.
func (g *Gateway) relayStream(ctx context.Context, logicalModel, provider string, upstream <-chan ChatChunk, out chan<- ChatChunk) {
	defer close(out)

	// usageReported guards the completion log and usage recording below --
	// see this method's own doc comment.
	usageReported := false

	for chunk := range upstream {
		if chunk.Usage != nil && !usageReported {
			usageReported = true
			obs.FromContext(ctx).Info("aigateway: chat stream completed",
				"model", logicalModel,
				"provider", provider,
				"prompt_tokens", chunk.Usage.PromptTokens,
				"completion_tokens", chunk.Usage.CompletionTokens,
			)
			g.recordUsage(ctx, logicalModel, *chunk.Usage)
		}

		select {
		case out <- chunk:
		case <-ctx.Done():
			return
		}
		if chunk.Err != nil {
			// aigateway.provider.errors (metrics.go): the stream failed
			// after establishment -- the error that arrives on the
			// terminal chunk, never through ChatStream's own return.
			g.recordProviderStreamError(ctx, provider)
			return
		}
	}
}
