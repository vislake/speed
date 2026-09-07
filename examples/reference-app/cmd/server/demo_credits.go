// The reference app's demo glue for go/billing's CreditService: a
// boot-time seed that grants each demo tenant a starting credit balance,
// standing in for docs/internal/15-roadmap.md's M2 exit condition's own
// buy-a-credit-pack leg -- a real Stripe/Alipay/WeChat sandbox charge --
// WITHOUT actually performing one.
//
// This is deliberately, explicitly NOT a real payment: go/billing/gateway/
// AGENTS.md records that no live credentials exist for any of the three
// providers in this environment, so a genuine "purchase a credit pack"
// flow cannot be built here without fabricating one. What this file ships
// instead is the mechanism's OTHER end -- a tenant that already holds
// credits, exactly the state a successful purchase would have left behind
// -- so internal/smilesim's real reserve/confirm/refund wiring
// (internal/smilesim/service.go's own "Credit accounting" section) has
// something real to reserve against. The credit-pack-purchase leg itself
// stays a named, tracked gap: see go/billing/AGENTS.md's own Scope table
// and this round's PR description.
package main

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/pkgcore"
)

// demoSimulationCreditGrant is the number of credits seedDemoCredits grants
// each demo tenant at boot -- enough for many multiples of
// smilesim.CreditsPerSimulation (internal/smilesim/service.go) worth of smile
// simulations, a round, memorable demo number rather than anything tied to
// a real credit-pack SKU (no such SKU exists yet -- see this file's own
// package doc comment).
const demoSimulationCreditGrant int64 = 1000

// demoCreditGrantReason is the free-text Reason (billing.GrantInput.Reason)
// every grant demo_credits.go's shared per-tenant grant (grantDemoCredits)
// issues -- the boot-time demo seed's (seedDemoCredits) and the
// self-service clinic provisioning path's (self_service.go's provision)
// alike -- so a read of the ledger (CreditService.Balance's own
// transaction history, or a later round's billing-history UI) can tell
// this app's seed grant apart from a real purchase or an in-app spend at
// a glance.
const demoCreditGrantReason = "demo:seed"

// grantDemoCredits grants demoSimulationCreditGrant credits to ONE tenant,
// via a real billing.CreditService.Grant call under the tenant's own
// context -- never a direct database write. Both of this app's paths that
// give a tenant a starting balance converge on this call:
// seedDemoCredits' boot-time loop grants every demo tenant (cfg.HostTenants)
// this way, and self_service.go's provision grants each newly created
// clinic the same amount, for the same reason, right after subscribing it
// -- a clinic whose registration seeded no balance would find its first
// smile simulation refused at the credit reservation (smilesim's own
// "Credit accounting" section) the moment its first request arrived.
//
// The grant is idempotent only up to a point, and the point is deliberate,
// the identical stance seedDemoUsers' own doc comment (demo_users.go)
// takes for its own restart limitation: CreditService.Grant is NOT
// idempotent under retry (its CreditTransaction.ID is a fresh
// uuid.NewString() every call, unlike PreDeduct's caller-supplied
// IdempotencyKey -- credit_service.go's own Grant doc comment), so calling
// it on every boot against a persistent database would keep adding credits
// forever. The guard below -- grant only when the tenant's balance is
// still genuinely untouched (both Available and Reserved are zero) -- is
// also what makes a repeated provisioning a no-op for a clinic whose
// earlier attempt already seeded it (a retry or a redelivered event
// re-runs the whole provision): the balance-zero guard converges the
// repeat instead of double-granting, exactly as a second demo boot
// against the SAME database leaves an already-seeded demo tenant alone.
// The cost is re-seeding a tenant that has genuinely spent its way down
// to exactly zero on its own -- for a clinic that happens only when a
// provisioning retry outlives the clinic's own first spend, a corner
// this reference app accepts, and a trade-off that would not be
// acceptable for a real deployment's own credit-pack purchase flow,
// which this file is explicitly NOT.
func grantDemoCredits(ctx context.Context, credits *billing.CreditService, tenantID pkgcore.TenantID) error {
	tenantCtx := pkgcore.WithTenant(ctx, tenantID)
	bal, err := credits.Balance(tenantCtx)
	if err != nil {
		return fmt.Errorf("reference-app: read the credit balance of %q before the seed grant: %w", tenantID, err)
	}
	if bal.Available != 0 || bal.Reserved != 0 {
		// Already seeded (or already in genuine use) from an earlier
		// boot against the same database file or an earlier provisioning
		// attempt -- see this function's own doc comment for why that is
		// the deliberate, accepted limit of this seed's idempotence.
		return nil
	}
	if _, err := credits.Grant(tenantCtx, billing.GrantInput{
		Amount: demoSimulationCreditGrant,
		Reason: demoCreditGrantReason,
	}); err != nil {
		return fmt.Errorf("reference-app: grant demo credits to %q: %w", tenantID, err)
	}
	return nil
}

// seedDemoCredits grants demoSimulationCreditGrant credits to every tenant
// named in tenants (cfg.HostTenants), one grantDemoCredits call per tenant
// -- the same per-tenant grant self_service.go's clinic provisioning uses,
// so a clinic's starting balance is the SAME grant a demo tenant's boot
// seed gives it. Two demo hosts mapping to the same tenant are granted
// once, mirroring seedDemoGrants' own dedup (demo_subject.go).
func seedDemoCredits(ctx context.Context, credits *billing.CreditService, tenants map[string]pkgcore.TenantID) error {
	seeded := make(map[pkgcore.TenantID]struct{}, len(tenants))
	for _, tenantID := range tenants {
		if _, done := seeded[tenantID]; done {
			continue
		}
		seeded[tenantID] = struct{}{}

		if err := grantDemoCredits(ctx, credits, tenantID); err != nil {
			return err
		}
	}
	return nil
}
