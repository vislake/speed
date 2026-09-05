package aigateway

import (
	"context"
	"net/http"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/ratelimit"
)

// This file closes the known gap docs/internal/11-cross-cutting.md's
// ratelimit consumer table records for this module: per-tenant request-rate
// limiting, independent of the credit-based cost quota Entitlements
// already enforces. It applies go/ratelimit to Gateway's three call
// sites -- Chat, ChatStream and GenerateImage -- the same one-dimension
// shape go/sharing's checkCreateRateLimit applies to its own single
// per-tenant dimension, with the underlying Limiter built lazily over the
// host's KVStore (rateLimiter, below) so it always reads whichever
// implementation the running deployment mode actually resolved, never one
// captured before Bootstrap ran.
//
// Positioned as the FIRST check in the pipeline, before checkEntitlement:
// a request-rate limit protects the gateway (and the vendor credentials it
// holds) from raw call volume regardless of whether the caller would go on
// to pass or fail the credit-based entitlement gate, so it must not wait
// behind a check that itself calls out to a host-wired Entitlements
// implementation.

// perTenantRate and perTenantWindow bound how many Gateway calls
// (Chat/ChatStream/GenerateImage combined -- one shared budget, since all
// three consume the same underlying vendor-credential and provider-call
// capacity) one tenant may make per window. Package constants, not dynamic
// configuration: this module declares no config schema of its own (see
// route.go's doc comment on why model routing is a construction-time Go
// option rather than a tenant-tunable config value), and a declared config
// schema this module would then ignore would be a lying schema, worse than
// a constant -- the identical reasoning go/sharing/ratelimit.go's own
// const block gives for its budgets.
const (
	perTenantRate   = 60
	perTenantWindow = time.Minute
)

// ErrRateLimited reports that Gateway.Chat, Gateway.ChatStream or
// Gateway.GenerateImage denied a request under this module's per-tenant
// rate-limit budget. Status is 429, not one of apperr's five builder
// shapes, matching go/sharing's and go/integration's identical
// ErrRateLimited -- a struct literal rather than apperr.Forbidden or
// apperr.Invalid, since neither status fits a rate-limit refusal.
var ErrRateLimited = &apperr.Error{Code: "aigateway.rate_limited", Status: http.StatusTooManyRequests}

// hostSeams is the slice of *pkgcore.Registry Gateway reads at call time,
// mirroring go/sharing's and go/org's identically-named, identically-
// shaped interface for the same reason: declaring it as its own interface,
// rather than holding a *pkgcore.Registry directly, keeps Gateway testable
// against a fake and honest about the one thing it actually needs from the
// host for rate limiting -- a KVStore to build a go/ratelimit.Limiter over.
type hostSeams interface {
	// KVStore returns the resolved KVStore implementation the registry's
	// seam wiring picked for the running deployment mode. Reading it here,
	// rather than caching a Limiter at construction, keeps rate limiting
	// bound to whichever KVStore the deployment mode actually resolved
	// (in-memory standalone, Redis distributed) instead of one captured
	// before Bootstrap ever ran.
	KVStore() pkgcore.KVStore
}

// compile-time check that the concrete registry satisfies the seam.
var _ hostSeams = (*pkgcore.Registry)(nil)

// rateLimiter returns the injected limiter (set by a test through
// Gateway.limiter), or builds one over the host's KVStore. Building it here
// rather than caching it at construction keeps host seams read at call
// time, the same rule go/sharing's identically-named method follows.
//
// A Gateway whose Module.Register was never called (host is nil -- the
// zero value NewGateway leaves it at) enforces NO rate limit at all, the
// same "no gating without wiring" default Entitlements and UsageRecorder
// already document: a Gateway is usable standalone in tests and examples
// without a full Bootstrap.
func (g *Gateway) rateLimiter() (ratelimit.Limiter, bool) {
	if g.limiter != nil {
		return g.limiter, true
	}
	if g.host == nil {
		return nil, false
	}
	kv := g.host.KVStore()
	if kv == nil {
		return nil, false
	}
	return ratelimit.New(kv), true
}

// checkRateLimit guards every Gateway entry point's one dimension: how many
// calls tenant has made recently, across Chat, ChatStream and
// GenerateImage combined. tenant is read by the caller via
// pkgcore.TenantFromContext, which returns "" when ctx carries none (a
// system-context caller, or a chat-only Gateway used with no tenant
// middleware at all) -- every such caller shares one counter, exactly the
// "no better identifier, one shared bucket" consequence
// go/integration/seams.go's Extractor doc comment already establishes for
// its own optional dimensions, rather than a refusal that would change
// existing tenant-less-caller behavior.
//
// A limiter that cannot be built at all (no host wired) is a no-op --
// ErrRateLimited is never returned in that case -- mirroring Entitlements'
// and UsageRecorder's own "not wired means not enforced" contract, unlike
// go/sharing's checkCreateRateLimit (whose Service is never usable at all
// without a host attached, so an unwired host there is itself an error).
// A limiter that IS wired but fails to answer (a KVStore outage) fails
// closed with ErrInternal-shaped apperr, never silently allowing the call
// through -- the same fail-closed rule go/ratelimit.Limiter's own doc
// comment states.
func (g *Gateway) checkRateLimit(ctx context.Context, tenant string) error {
	limiter, ok := g.rateLimiter()
	if !ok {
		return nil
	}
	decision, err := limiter.Allow(ctx, "aigateway:tenant:"+tenant, ratelimit.Limit{
		Rate: perTenantRate, Per: perTenantWindow,
	})
	if err != nil {
		return ErrRateLimitCheckFailed.WithCause(err)
	}
	if !decision.Allowed {
		return ErrRateLimited.WithParam("retry_after_seconds", int(decision.ResetAfter.Seconds()))
	}
	return nil
}
