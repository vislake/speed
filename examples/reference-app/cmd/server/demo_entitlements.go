// The reference app's demo glue for go/billing's entitlement half: a
// boot-time seed that resolves a platform-wide demo Plan granting the exact
// model features this app's real AI routes gate on, and gives every demo
// tenant an Active subscription to it -- standing in for
// docs/internal/06-billing-and-metering.md's full pay-per-use-AND-
// subscription flow's own "tenant buys a subscription, the payment channel
// confirms it" leg WITHOUT performing a real one.
//
// This is deliberately, explicitly NOT a real purchase: go/billing/gateway/
// AGENTS.md records that no live credentials exist for any of the three
// providers in this environment, so a genuine "charge the customer and
// confirm the subscription" flow cannot be built here without fabricating
// one. What this file ships instead is the mechanism's OTHER end -- a tenant
// whose subscription is Active, exactly the state a real payment-channel
// confirmation would have left behind -- so cmd/server's wiring of
// aigateway.WithEntitlements over billingModule.Entitlements() (see
// server.go's own construction comment) has something real to judge
// against: a demo tenant's chat and image routes pass the gateway's
// entitlement gate (go/ai-gateway's checkEntitlement, key
// "model:"+logicalModel), while a tenant with no Active subscription is
// refused with ErrEntitlementDenied before any provider is reached --
// cmd/server/entitlements_flow_test.go drives both directions through the
// real composed HTTP stack. The pay-and-confirm leg itself stays a named,
// tracked gap: see go/billing/AGENTS.md's own Scope table.
package main

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/examples/reference-app/internal/consult"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// demoEntitlementPlanKey is the platform-wide Plan key seedDemoEntitlements
// resolves-else-creates. Every demo tenant subscribes to this one platform
// Plan; nothing in this file ever creates a tenant-custom Plan, since the
// demo story needs no custom deal.
const demoEntitlementPlanKey = "demo"

// demoEntitlementGrants is the Grant set seedDemoEntitlements stamps on the
// demo Plan, one Boolean grant (Value: true) per feature key this app's real
// routes actually resolve through the wired entitlement seam. The keys are
// derived the same way go/ai-gateway's own checkEntitlement derives them --
// "model:" + the LogicalModel the route asks for -- by importing the very
// constants the routes run on (consult.LogicalModel,
// smilesim.LogicalModel), never hand-picked strings that could drift:
//
//   - "model:chat:default"           -- every consult Suggest call's key
//     (gateway.Chat under consult.LogicalModel).
//   - "model:image:smile-simulation" -- every smile-simulation job's key
//     (Gateway.GenerateImage under smilesim.LogicalModel).
//
// Boolean grants rather than Quota ones is likewise deliberate: go/billing's
// EntitlementsService reads quota consumption through the UsageReader a
// host injects, and this app wires billing.NewModule(db, nil) -- no usage
// reader -- so a Quota grant here would panic the moment Check tried to
// consult it (go/billing/entitlements.go's own checkQuota). "Can this tenant
// use model X at all" is exactly the Boolean-shaped question
// docs/internal/06-billing-and-metering.md's per-model gate semantics name
// for these grants, so no quota machinery is being skipped: this seed's
// grants are Boolean by design, and quota-kind demo grants would need a real
// usage reader first (see go/billing/AGENTS.md's Known limitations).
var demoEntitlementGrants = []billing.Grant{
	{FeatureKey: "model:" + consult.LogicalModel, Value: true},
	{FeatureKey: "model:" + smilesim.LogicalModel, Value: true},
}

// seedDemoEntitlements resolves -- or, on a first boot against a fresh
// database, creates -- the platform-wide demo Plan granting
// demoEntitlementGrants, then gives every tenant named in tenants
// (cfg.HostTenants) an Active subscription to it, via real
// billing.PlanService/SubscriptionService calls under that tenant's own
// context -- never a direct database write. Two demo hosts mapping to the
// same tenant are subscribed once, mirroring seedDemoCredits' own dedup
// (demo_credits.go).
//
// Unlike seedDemoCredits, this seed is idempotent in full: Plan resolution
// is key-based (a re-boot finds the existing demo Plan and reuses it as-is,
// never clobbering a later edit to its grants), and
// SubscriptionService.Active makes a second boot against the SAME database
// a no-op for a tenant that already holds an Active subscription. The only
// state that is not re-seeded is one a re-boot should not touch anyway: a
// demo subscription that was deliberately canceled (entitlements_flow_test.go's
// refusal leg does exactly that mid-test) stays canceled, so a running demo
// cannot silently resubscribe a tenant an operator just took offline --
// re-seeding it is a fresh-database or explicit-administrator act, never a
// boot side effect.
//
// This must run before the first demo chat/image request can arrive -- the
// gateway's entitlement gate refuses every call for a tenant with no Active
// subscription -- and server.go therefore calls it during boot, next to
// seedDemoCredits, before any route can serve. A failure here fails the
// boot: a demo app whose seeded subscriptions cannot be established should
// not half-start with an entitlement seam that denies everything.
func seedDemoEntitlements(ctx context.Context, plans *billing.PlanService, subs *billing.SubscriptionService, tenants map[string]pkgcore.TenantID) error {
	// Resolve the platform-wide demo Plan first: pkgcore.TenantID("")
	// makes PlanStore.Resolve run its platform-wide lookup alone (see
	// go/billing/plan.go's Resolve doc comment). ErrPlanNotFound means no
	// demo Plan exists yet -- create one, platform-scoped (TenantID left
	// at platformScopeSentinel, the empty string), stamping exactly
	// demoEntitlementGrants onto it.
	plan, err := plans.Resolve(ctx, "", demoEntitlementPlanKey)
	if err != nil {
		if appErr, ok := apperr.As(err); !ok || appErr.Code != billing.ErrPlanNotFound.Code {
			return fmt.Errorf("reference-app: resolve the demo entitlement plan: %w", err)
		}
		demoPlan := &billing.Plan{
			TenantID: "",
			Key:      demoEntitlementPlanKey,
			Name:     "Demo plan",
		}
		if err := demoPlan.SetGrants(demoEntitlementGrants); err != nil {
			return fmt.Errorf("reference-app: encode the demo entitlement plan's grants: %w", err)
		}
		if err := plans.Create(ctx, demoPlan); err != nil {
			return fmt.Errorf("reference-app: create the demo entitlement plan: %w", err)
		}
		plan = demoPlan
	}

	subscribed := make(map[pkgcore.TenantID]struct{}, len(tenants))
	for _, tenantID := range tenants {
		if _, done := subscribed[tenantID]; done {
			continue
		}
		subscribed[tenantID] = struct{}{}

		tenantCtx := pkgcore.WithTenant(ctx, tenantID)
		active, err := subs.Active(tenantCtx)
		if err != nil {
			return fmt.Errorf("reference-app: read the active subscription of demo tenant %q: %w", tenantID, err)
		}
		if active != nil {
			// Already holds an Active subscription from an earlier boot
			// against the same database file (or from an operator's own
			// doing) -- leave it exactly as it is, never replaced.
			continue
		}
		sub, err := subs.Create(tenantCtx, billing.CreateInput{PlanID: plan.ID})
		if err != nil {
			return fmt.Errorf("reference-app: create the demo subscription for tenant %q: %w", tenantID, err)
		}
		// The plain-Go-call stand-in for the payment channel's
		// confirmation: a real deployment's subscription would arrive here
		// Active only after its first successful payment event
		// (go/billing/subscription.go's own Subscription doc comment), a
		// leg this demo deliberately does not perform -- see this file's
		// package doc comment.
		if _, err := subs.Activate(tenantCtx, sub.ID); err != nil {
			return fmt.Errorf("reference-app: activate the demo subscription for tenant %q: %w", tenantID, err)
		}
	}
	return nil
}
