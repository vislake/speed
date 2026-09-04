package aigateway

import "context"

// Decision is the answer Entitlements.Check gives Gateway.Chat/ChatStream.
// Only Allowed and Reason matter to this package -- this is a deliberately
// minimal, local type, not a copy of go/billing's own Decision struct (which
// also carries a Remaining quota counter this package never reads or
// reports).
type Decision struct {
	// Allowed reports whether the call may proceed.
	Allowed bool
	// Reason is a short, host-defined explanation (for example
	// "no_subscription", "quota_exceeded") the Gateway attaches to
	// ErrEntitlementDenied when Allowed is false. Purely informational --
	// this package never branches on its value.
	Reason string
}

// Entitlements is the model-access-control check
// docs/internal/08-ai-gateway.md names as reusing
// "billing.Entitlements.Check(ctx, tenant, "model:xxx", 1)'s boolean
// entitlement" -- without importing go/billing to get it.
//
// # Why this is a seam rather than an import of go/billing
//
// ai-gateway sits at the same dependency tier as billing, sharing and
// integration (root CLAUDE.md's module graph: "... -> authn/rbac/org/
// metering -> billing/ai-gateway/sharing/integration -> ..."), and none of
// the three peers may import one another. go/billing's real
// *billing.EntitlementsService already exposes exactly the call this
// interface mirrors -- Check(ctx context.Context, featureKey string,
// requested int64) (Decision, error), with the tenant read from ctx via
// pkgcore.MustTenantFromContext internally rather than taken as a parameter
// (go/billing/entitlements.go's own Check) -- so this is the identical
// no-import-edge seam pattern go/integration/seams.go's PermissionLister
// documents at length for the same reason against go/rbac: this package
// declares the shape it needs structurally, and a host wires the real
// implementation in with a one-line assignment, since Go's structural
// typing lets *billing.EntitlementsService satisfy this interface directly
// with no adapter to write (its Decision differs from this package's own,
// but the method SIGNATURE -- what satisfies an interface -- does not care
// about a different named return type's extra fields; a host that wants
// the assignment to type-check writes a two-line closure returning
// aigateway.Decision built from the billing.Decision fields it needs,
// exactly the EntitlementsFunc shape below, mirroring
// go/integration/seams.go's own WithPermissionLister closure example for
// the identical situation).
//
// # Optional, unlike PermissionLister
//
// Entitlements is optional at the interface level: a Gateway built with no
// Entitlements wired (WithEntitlements never called) enforces NO quota at
// all -- every Chat/ChatStream call proceeds unconditionally. This matches
// how a host with no billing module simply has nothing to check against. A
// REAL production deployment always wires this; shipping without it is a
// deliberate, documented "no gating" choice, never a silent default a host
// might not notice it made.
type Entitlements interface {
	// Check answers whether requested units of featureKey are permitted
	// right now, for whatever tenant ctx carries. Gateway.Chat/ChatStream
	// call it with featureKey "model:<logical-key>" and requested 1, BEFORE
	// resolving a credential or calling any provider -- so a refused caller
	// is never billed.
	Check(ctx context.Context, featureKey string, requested int64) (Decision, error)
}

// EntitlementsFunc adapts a plain function to Entitlements, the same
// func-to-interface adapter shape http.HandlerFunc popularized and
// go/integration/seams.go's PermissionListerFunc already applies in this
// codebase -- so a host can wire the real billing.EntitlementsService with a
// short closure instead of declaring a named adapter type of its own:
//
//	aigateway.WithEntitlements(aigateway.EntitlementsFunc(
//	    func(ctx context.Context, featureKey string, requested int64) (aigateway.Decision, error) {
//	        d, err := entitlementsService.Check(ctx, featureKey, requested)
//	        if err != nil {
//	            return aigateway.Decision{}, err
//	        }
//	        return aigateway.Decision{Allowed: d.Allowed, Reason: string(d.Reason)}, nil
//	    },
//	))
type EntitlementsFunc func(ctx context.Context, featureKey string, requested int64) (Decision, error)

// Check implements Entitlements.
func (f EntitlementsFunc) Check(ctx context.Context, featureKey string, requested int64) (Decision, error) {
	return f(ctx, featureKey, requested)
}

// UsageEvent is one unit of AI usage Gateway reports automatically after a
// Chat/ChatStream call, the local shape UsageRecorder.Record accepts.
//
// This deliberately does NOT reuse go/metering's own UsageEvent struct --
// crossing the module boundary with a business module's struct is exactly
// what root CLAUDE.md's "do not import another business module's structs"
// rule forbids, and metering sits below ai-gateway in the dependency graph
// but importing it here would still couple this package's public API to
// metering's own evolution. A host's UsageRecorder implementation maps this
// local shape onto a real metering.UsageEvent inside its own closure -- see
// UsageRecorderFunc's doc comment.
type UsageEvent struct {
	// TenantID is the tenant this usage belongs to, read from ctx by
	// Gateway itself -- never accepted from a caller-supplied parameter.
	TenantID string
	// Feature is the quota/billing dimension this event measures. Gateway
	// always reports "ai.chat_tokens" for a chat call; the field exists
	// (rather than a hardcoded constant on UsageRecorder.Record itself) so
	// a future round can report a different dimension without changing
	// this seam's shape.
	Feature string
	// Quantity is how many units of Feature this event measures -- the
	// call's Usage.TotalTokens for the "ai.chat_tokens" dimension.
	Quantity float64
	// IdempotencyKey is a value Gateway generates fresh per call, never
	// derived from a caller-supplied business operation id (this package
	// has no such id to derive one from -- a chat call carries no request
	// id of its own). A UsageRecorder implementation that wants exactly-once
	// billing-grade semantics must therefore treat this key as
	// non-deterministic across a retried call, the same caveat
	// go/metering's own AnalyticsRecorder documents for its fail-open,
	// undeduped tier; this package makes no billing-grade guarantee.
	IdempotencyKey string
	// Metadata carries small, bounded context about the call -- currently
	// just the logical model key under "model".
	Metadata map[string]string
}

// UsageRecorder is the automatic usage-reporting seam Gateway.Chat and
// Gateway.ChatStream call after a successful response, mirroring
// go/metering's own Recorder interface (go/metering/recorder.go) shape --
// Record(ctx, event) error -- without importing go/metering for the
// identical peer-tier reason Entitlements does not import go/billing (see
// Entitlements' own doc comment). A host with go/metering wired satisfies
// this with a short closure over the real Recorder a
// *metering.Module.Recorder() returns (go/metering/module.go), mapping this
// package's local UsageEvent onto a real metering.UsageEvent -- see
// UsageRecorderFunc's doc comment.
//
// Optional, exactly like Entitlements: a Gateway built with no UsageRecorder
// wired still works, it just reports no usage anywhere. Business code never
// calls a metering API of its own for a call that went through this
// Gateway -- automatic reporting is the whole point (docs/internal/
// 08-ai-gateway.md's "AI 计量是内置行为不需手动上报" rule) -- so a host that
// wants metering for AI usage MUST wire this seam; there is no other path.
type UsageRecorder interface {
	// Record reports event. Whether a failed Record call is retried,
	// buffered, or simply dropped is entirely a property of the wired
	// implementation; Gateway itself only logs a Record failure (via
	// obs.FromContext) and otherwise ignores it -- a metering failure must
	// never fail the chat call that already succeeded and already answered
	// the caller.
	Record(ctx context.Context, event UsageEvent) error
}

// UsageRecorderFunc adapts a plain function to UsageRecorder, the same
// adapter shape EntitlementsFunc gives Entitlements:
//
//	aigateway.WithUsageRecorder(aigateway.UsageRecorderFunc(
//	    func(ctx context.Context, event aigateway.UsageEvent) error {
//	        return meteringRecorder.Record(ctx, metering.UsageEvent{
//	            TenantID:       event.TenantID,
//	            Feature:        event.Feature,
//	            Quantity:       event.Quantity,
//	            IdempotencyKey: event.IdempotencyKey,
//	            Metadata:       event.Metadata,
//	        })
//	    },
//	))
type UsageRecorderFunc func(ctx context.Context, event UsageEvent) error

// Record implements UsageRecorder.
func (f UsageRecorderFunc) Record(ctx context.Context, event UsageEvent) error {
	return f(ctx, event)
}
