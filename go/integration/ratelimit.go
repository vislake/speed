package integration

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/ratelimit"
)

// Layer names LayeredDecision.Layer reports for the one that denied a
// request, and the "layer" parameter httpguard.go's translateDecision
// attaches to ErrRateLimited.
const (
	LayerGlobal = "global"
	LayerTenant = "tenant"
	LayerKey    = "key"
)

// layerOrder is the fixed evaluation sequence LayeredLimiter.Allow follows:
// global first, then tenant, then key, matching
// docs/internal/07-platform-services.md's own layering -- each layer maps
// to one go/ratelimit.Allow call, and integration itself composes the
// three -- and go/ratelimit.Limiter's own doc comment, which names this
// exact three-layer composition as its motivating multi-dimension example.
// Evaluating global first means a
// platform-wide incident denies every tenant and key uniformly before either
// of the narrower counters is even touched -- the cheapest layer to exhaust
// fails fastest, and every call this package makes is still one hit per
// layer regardless of order, so no ordering choice changes how many
// go/ratelimit.Allow calls a request costs.
var layerOrder = [...]string{LayerGlobal, LayerTenant, LayerKey}

// LayeredLimits configures LayeredLimiter's three independent
// go/ratelimit.Limit windows. A zero-value Limit (Rate == 0) on any one
// field disables that layer entirely -- LayeredLimiter.Allow skips calling
// Limiter.Allow for a disabled layer altogether, rather than passing a
// Limit that go/ratelimit.Limit.validate would reject as ErrInvalidLimit
// (Rate must be strictly positive; see go/ratelimit/limiter.go). A
// LayeredLimits zero value therefore disables all three layers, which is a
// legal, if unusual, configuration: every request is allowed, with
// LayeredDecision.Layer left empty.
type LayeredLimits struct {
	// Global bounds every request this module's caller routes through
	// LayeredLimiter, regardless of tenant or key -- the platform-wide
	// ceiling.
	Global ratelimit.Limit
	// Tenant bounds every request from one tenant, summed across every key
	// that tenant holds.
	Tenant ratelimit.Limit
	// Key bounds every request authenticated by one specific API key.
	Key ratelimit.Limit
}

// LayeredDecision is LayeredLimiter.Allow's result: which single
// go/ratelimit.Decision decided the overall outcome, and which layer it
// came from. Allowed is true only when every enabled layer's own Decision
// was itself Allowed; the moment one layer denies, evaluation stops (the
// remaining, narrower layers are never even called -- see Allow's own doc
// comment for why that is deliberate, not merely an optimization).
type LayeredDecision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool
	// Layer names which layer produced this Decision: LayerGlobal,
	// LayerTenant or LayerKey when Allowed is false, or -- when Allowed is
	// true -- the last layer actually evaluated (LayerKey, unless a
	// narrower layer was disabled, in which case the last enabled one),
	// since every enabled layer had to allow for the overall decision to be
	// allowed at all.
	Layer string
	// Decision is the raw go/ratelimit.Decision the reported Layer
	// produced, carrying ResetAfter and Remaining for httpguard.go's header
	// translation.
	Decision ratelimit.Decision
}

// LayeredLimiter composes three independent go/ratelimit.Limiter.Allow
// calls -- global, tenant, key -- into the one three-layer decision
// docs/internal/07-platform-services.md's rate-limiting section describes,
// entirely from go/ratelimit's existing public API: this package never
// modifies go/ratelimit itself, and holds no state of its own beyond the
// Limiter and LayeredLimits it was built with.
type LayeredLimiter struct {
	limiter ratelimit.Limiter
	limits  LayeredLimits
}

// NewLayeredLimiter returns a LayeredLimiter backed by limiter (typically
// ratelimit.New(store) over the host's pkgcore.KVStore), evaluating limits'
// three windows on every Allow call.
func NewLayeredLimiter(limiter ratelimit.Limiter, limits LayeredLimits) *LayeredLimiter {
	return &LayeredLimiter{limiter: limiter, limits: limits}
}

// Allow evaluates the global, tenant and key layers in that fixed order
// (layerOrder) against globalKey, tenantKey and apiKeyID respectively,
// short-circuiting on the first denial: the request that fails the global
// layer never touches the tenant or key counters at all, and one that fails
// the tenant layer never touches the key counter. This is both cheaper (a
// request already known to be denied does not pay for two more store round
// trips) and correct for the "any single layer's rejection rejects the
// whole request" contract this module's task names -- there being no answer
// this function could ever return other than "denied" once any layer says
// so, evaluating the rest would only spend more of THEIR quota against a
// request that was never going through, artificially starving legitimate
// traffic sharing those narrower counters.
//
// tenantKey and apiKeyID are the caller's own storage-key construction for
// those two dimensions (typically the tenant id and the API key's id or
// hash) -- this method treats both as opaque strings, exactly as
// go/ratelimit.Limiter.Allow itself treats its own key parameter (see that
// interface's doc comment). globalKey exists as a parameter, rather than a
// hard-coded literal, only so a caller sharing one KVStore across several
// LayeredLimiter instances (a highly unusual composition this module does
// not itself build) can namespace them; a caller with a single global
// limiter passes any fixed constant, such as "global".
//
// A disabled layer (LayeredLimits' zero Limit on that field) is skipped
// entirely -- Allow never calls the underlying Limiter for it -- so a
// LayeredLimiter built with, say, only Limits.Key set evaluates exactly one
// layer per call.
//
// An error from the underlying Limiter (a KVStore failure, or
// ErrInvalidLimit for a misconfigured, non-zero-but-otherwise-invalid
// Limit) is returned unmodified, exactly as go/ratelimit.Limiter.Allow's own
// doc comment promises for that call -- LayeredLimiter adds no fail-open or
// fail-closed policy of its own on top.
func (l *LayeredLimiter) Allow(ctx context.Context, globalKey, tenantKey, apiKeyID string) (LayeredDecision, error) {
	keys := map[string]string{
		LayerGlobal: globalKey,
		LayerTenant: tenantKey,
		LayerKey:    apiKeyID,
	}
	limits := map[string]ratelimit.Limit{
		LayerGlobal: l.limits.Global,
		LayerTenant: l.limits.Tenant,
		LayerKey:    l.limits.Key,
	}

	last := LayeredDecision{Allowed: true}
	for _, layer := range layerOrder {
		limit := limits[layer]
		if limit.Rate <= 0 {
			// Disabled layer -- see LayeredLimits' own doc comment.
			continue
		}

		decision, err := l.limiter.Allow(ctx, layerStorageKey(layer, keys[layer]), limit)
		if err != nil {
			return LayeredDecision{}, fmt.Errorf("integration: %s layer rate limit: %w", layer, err)
		}

		last = LayeredDecision{Allowed: decision.Allowed, Layer: layer, Decision: decision}
		if !decision.Allowed {
			return last, nil
		}
	}
	return last, nil
}

// layerStorageKey namespaces key by layer, so the same tenant id or API key
// id used as both the tenant-layer and key-layer identifier -- an unlikely
// but not impossible caller choice -- never collides into one shared
// go/ratelimit counter.
func layerStorageKey(layer, key string) string {
	return "integration:" + layer + ":" + key
}
