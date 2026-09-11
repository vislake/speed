// Package bridges holds the bridge closures that hand one module's
// concrete service to another module's structurally-typed seam. Each pair
// of modules deliberately shares no import edge (docs/internal/
// 01-architecture.md draws them as dashed seam edges), so the concrete
// end's type differs from the seam's own named types just enough that a
// direct assignment does not compile -- billing.Decision vs.
// ai-gateway's, metering's UsageEvent vs. its, each module's method set
// vs. the other's interface. The closure per pair is mechanical and
// identical in every host that wires both modules; it lives here so a
// host writes the wiring as one call and the two ends' shapes are
// reconciled in exactly one place.
//
// This package is separate from the app root so the dependency cost stays
// with the hosts that actually wire these modules: a host with no billing
// or metering module imports the root (and chain) without ever pulling
// ai-gateway, billing or metering into its build.
package bridges

import (
	"context"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/org"
)

// Entitlements adapts billing's entitlement check onto ai-gateway's
// Entitlements seam -- the closure go/ai-gateway/seams.go's own doc
// comment documents verbatim, over the real *billing.EntitlementsService:
// a host wires it with
//
//	aigateway.WithEntitlements(app.Entitlements(billingModule.Entitlements()))
//
// so both halves of one Gateway instance (chat and image generation) gate
// every request on the calling tenant's subscription -- key
// "model:"+logicalModel, requested 1 -- before any provider is reached. A
// host that also pre-flights the gate outside the gateway (before opening
// a credit reservation, say) must hand that pre-flight the same returned
// func: one closure instance, so the pre-flight and the gateway's own
// re-check can never disagree about the same state.
//
// The conversion is needed because the two modules' Decision types are
// distinct named types; only the fields ai-gateway reads cross over (the
// Remaining quota counter billing also answers belongs to billing's own
// consumers).
func Entitlements(entitlements billing.Entitlements) aigateway.EntitlementsFunc {
	return func(ctx context.Context, featureKey string, requested int64) (aigateway.Decision, error) {
		decision, err := entitlements.Check(ctx, featureKey, requested)
		if err != nil {
			return aigateway.Decision{}, err
		}
		return aigateway.Decision{Allowed: decision.Allowed, Reason: string(decision.Reason)}, nil
	}
}

// UsageRecorder adapts metering's recorder onto ai-gateway's UsageRecorder
// seam -- the closure go/ai-gateway/seams.go's own doc comment documents
// verbatim: a host wires it with
//
//	aigateway.WithUsageRecorder(app.UsageRecorder(meteringModule.Recorder()))
//
// so every successful Chat/ChatStream call (image generation records the
// same way, through the job handler go/ai-gateway registers) reports its
// token usage automatically; AI metering is a built-in behavior needing no
// manual reporting.
//
// recorder is the seam's only accepted shape: the analytics-grade
// (fail-open) tier, whose Record(ctx, event) needs no caller-owned
// transaction handle -- a recorder callback has no room for the
// billing-grade Enqueue path's transaction. The events it buffers are
// folded into real metering_usage_summaries rows by the recorder's
// background flush loop. Whether a failed Record call is retried,
// buffered, or dropped is entirely a property of the wired recorder;
// ai-gateway logs a failure and otherwise ignores it, because a metering
// failure must never fail the chat call that already succeeded.
//
// The conversion is needed because metering.UsageEvent and
// aigateway.UsageEvent are distinct named types; the fields are
// field-for-field one shape.
func UsageRecorder(recorder metering.Recorder) aigateway.UsageRecorderFunc {
	return func(ctx context.Context, event aigateway.UsageEvent) error {
		return recorder.Record(ctx, metering.UsageEvent{
			TenantID:       event.TenantID,
			Feature:        event.Feature,
			Quantity:       event.Quantity,
			IdempotencyKey: event.IdempotencyKey,
			Metadata:       event.Metadata,
		})
	}
}

// OrgFeatureGate adapts the config module's lazy read handle onto org's
// FeatureGate seam:
//
//	org.WithFeatureGate(app.OrgFeatureGate(configModule.Handle()))
//
// Both modules must be part of the single the assembly call -- so
// their permissions, audit actions, events and routes are declared there
// -- while the *config.Service is only produced by configModule.Attach,
// strictly after the assembly returns. The handle resolves the Service per
// call and reports the config module's own not-attached refusal in the
// window before Attach, so a read in that window fails closed instead of
// panicking on a nil *config.Service.
func OrgFeatureGate(handle *config.Handle) org.FeatureGate {
	return org.FeatureGateFunc(handle.IsEnabled)
}

// AuthnFeatureGate is OrgFeatureGate for authn's identically shaped
// FeatureGate seam -- authn gates its sign-in channels on flag reads the
// same way org gates its own behavior:
//
//	authn.WithFeatureGate(app.AuthnFeatureGate(configModule.Handle()))
//
// The two compile-time assertions below are the proof that the seams
// really are one shape: the same config.Handle read adapts to both, which
// is why no hand-written gate type exists anywhere. (The nil receiver is
// never called; building a method value on it proves the conversion, and
// a nil handle fails closed anyway -- its own doc comment.)
func AuthnFeatureGate(handle *config.Handle) authn.FeatureGate {
	return authn.FeatureGateFunc(handle.IsEnabled)
}

var (
	_ org.FeatureGate   = org.FeatureGateFunc((*config.Handle)(nil).IsEnabled)
	_ authn.FeatureGate = authn.FeatureGateFunc((*config.Handle)(nil).IsEnabled)
)
