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
// every demo grant seedDemoCredits issues carries, so a read of the ledger
// (CreditService.Balance's own transaction history, or a later round's
// billing-history UI) can tell a demo seed apart from a real purchase or an
// in-app spend at a glance.
const demoCreditGrantReason = "demo:seed"

// seedDemoCredits grants demoSimulationCreditGrant credits to every tenant
// named in tenants (cfg.HostTenants), via a real billing.CreditService.Grant
// call under that tenant's own context -- never a direct database write.
// Two demo hosts mapping to the same tenant are granted once, mirroring
// seedDemoGrants' own dedup (demo_subject.go).
//
// This call is idempotent only up to a point, and the point is deliberate,
// the identical stance seedDemoUsers' own doc comment (demo_users.go)
// takes for its own restart limitation: CreditService.Grant is NOT
// idempotent under retry (its CreditTransaction.ID is a fresh
// uuid.NewString() every call, unlike PreDeduct's caller-supplied
// IdempotencyKey -- credit_service.go's own Grant doc comment), so calling
// it on every boot against a persistent database would keep adding credits
// forever. The guard below -- grant only when the tenant's balance is
// still genuinely untouched (both Available and Reserved are zero) --
// makes a second boot against the SAME database a no-op for a tenant that
// has already been seeded and not yet spent anything, at the cost of
// re-seeding a tenant that has genuinely spent its way down to exactly
// zero on its own. That trade-off is acceptable for a reference app's own
// demo feature; it would not be for a real deployment's own credit-pack
// purchase flow, which this file is explicitly NOT.
func seedDemoCredits(ctx context.Context, credits *billing.CreditService, tenants map[string]pkgcore.TenantID) error {
	seeded := make(map[pkgcore.TenantID]struct{}, len(tenants))
	for _, tenantID := range tenants {
		if _, done := seeded[tenantID]; done {
			continue
		}
		seeded[tenantID] = struct{}{}

		tenantCtx := pkgcore.WithTenant(ctx, tenantID)
		bal, err := credits.Balance(tenantCtx)
		if err != nil {
			return fmt.Errorf("reference-app: read the demo credit balance of %q: %w", tenantID, err)
		}
		if bal.Available != 0 || bal.Reserved != 0 {
			// Already seeded (or already in genuine use) from an earlier
			// boot against the same database file -- see this function's
			// own doc comment for why that is the deliberate, accepted
			// limit of this seed's idempotence.
			continue
		}
		if _, err := credits.Grant(tenantCtx, billing.GrantInput{
			Amount: demoSimulationCreditGrant,
			Reason: demoCreditGrantReason,
		}); err != nil {
			return fmt.Errorf("reference-app: grant demo credits to %q: %w", tenantID, err)
		}
	}
	return nil
}
