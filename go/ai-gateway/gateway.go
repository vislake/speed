package aigateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
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
	g := &Gateway{
		credentials:   credentials,
		registry:      ChatProviderRegistry,
		routes:        make(map[string]ModelRoute),
		imageRegistry: ImageProviderRegistry,
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

	event := UsageEvent{
		TenantID:       string(tenant),
		Feature:        usageFeatureChatTokens,
		Quantity:       float64(usage.TotalTokens),
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
	// A crypto/rand failure here is exceptionally rare (an exhausted
	// entropy source) and never worth failing an already-successful chat
	// call over; the zero-value buffer still yields a syntactically valid,
	// merely non-random key in that vanishingly unlikely case.
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

	if err := g.checkEntitlement(ctx, logicalModel); err != nil {
		return ChatResponse{}, err
	}

	provider, route, err := g.resolve(ctx, logicalModel)
	if err != nil {
		return ChatResponse{}, err
	}

	vendorReq := req
	vendorReq.Model = route.VendorModel

	resp, err := provider.Chat(ctx, vendorReq)
	if err != nil {
		return ChatResponse{}, err
	}

	// The attribute keys below are deliberately "prompt_units"/
	// "completion_units", never "prompt_tokens"/"completion_tokens":
	// go/observability's on-by-default redaction (redact.go's
	// sensitiveStems) redacts any attribute key containing the substring
	// "token" -- including as part of a longer word -- wholesale, which
	// would silently mask these integer counts as "[REDACTED]" in every
	// deployment. See relayStream's identical rename below for the
	// streaming path.
	obs.FromContext(ctx).Info("aigateway: chat completed",
		"model", logicalModel,
		"provider", route.Provider,
		"prompt_units", resp.Usage.PromptTokens,
		"completion_units", resp.Usage.CompletionTokens,
	)
	g.recordUsage(ctx, logicalModel, resp.Usage)
	return resp, nil
}

// ChatStream runs the same pipeline as Chat for a streaming request, and
// returns a channel of incremental chunks honoring ChatChunk's own channel
// contract. Usage is reported to the wired UsageRecorder only once the
// stream's terminal success chunk (real Usage) is observed -- never
// speculatively at stream start, and never on the terminal error chunk.
func (g *Gateway) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	logicalModel := req.Model

	if err := g.checkEntitlement(ctx, logicalModel); err != nil {
		return nil, err
	}

	provider, route, err := g.resolve(ctx, logicalModel)
	if err != nil {
		return nil, err
	}

	vendorReq := req
	vendorReq.Model = route.VendorModel

	upstream, err := provider.ChatStream(ctx, vendorReq)
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
func (g *Gateway) relayStream(ctx context.Context, logicalModel, provider string, upstream <-chan ChatChunk, out chan<- ChatChunk) {
	defer close(out)

	for chunk := range upstream {
		if chunk.Usage != nil {
			// See the identical rename note in Chat above: "_units", never
			// "_tokens", to dodge go/observability's key-substring
			// redaction of anything containing "token".
			obs.FromContext(ctx).Info("aigateway: chat stream completed",
				"model", logicalModel,
				"provider", provider,
				"prompt_units", chunk.Usage.PromptTokens,
				"completion_units", chunk.Usage.CompletionTokens,
			)
			g.recordUsage(ctx, logicalModel, *chunk.Usage)
		}

		select {
		case out <- chunk:
		case <-ctx.Done():
			return
		}
		if chunk.Err != nil {
			return
		}
	}
}
