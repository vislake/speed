package admin

import (
	"context"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/metering"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// UsageSummaryRow is one tenant's row of D9's cross-tenant usage/billing
// dashboard: whatever go/metering's and go/billing's own per-tenant query
// methods can currently answer for that tenant, stitched into one row --
// no new aggregate of admin's own.
type UsageSummaryRow struct {
	TenantID    string
	DisplayName string

	// MeteringSummaries is every metering_usage_summaries row recorded
	// for this tenant (one per feature x calendar period), read through
	// metering.SummaryRepository.List -- nil when go/metering was never
	// wired through WithMetering, distinct from a non-nil empty slice,
	// which means the module IS wired but this tenant has recorded no
	// usage at all yet.
	MeteringSummaries []metering.UsageSummary

	// CreditBalance is the tenant's current credits-ledger balance, read
	// through billing.CreditService.Balance -- nil when go/billing was
	// never wired through WithBilling, and non-nil whenever it WAS wired,
	// with no third case: Balance's documented materialize-on-first-read
	// contract (go/billing/credit_service.go) guarantees a row even for a
	// tenant that has never touched credits, and that materialization --
	// one zero-valued billing_credit_balances row per ledger tenant
	// lacking one -- is the one write UsageService.Summary performs (see
	// UsageService's own doc comment).
	CreditBalance *billing.CreditBalance

	// ActiveSubscription is the tenant's currently active subscription,
	// if any, read through billing.SubscriptionService.Active -- nil both
	// when go/billing was never wired AND when the tenant simply has none
	// active right now (Active's own (nil, nil) "no active subscription"
	// answer); UsageService.Summary does not attempt to distinguish the
	// two reasons for a nil value here, since CreditBalance (non-nil
	// whenever go/billing is wired at all -- see its own comment above)
	// already tells a caller which case applies.
	ActiveSubscription *billing.Subscription
}

// UsageService is D9's runtime: admin's own tenant-by-tenant stitching of
// go/metering's and go/billing's ALREADY-REAL, per-tenant query methods --
// no new database table, no new aggregate of admin's own, exactly D9's own
// design (docs/internal/23-admin.md).
//
// The surface is read-only against go/metering's own tables and admin's
// own ledger, but deliberately NOT against go/billing's credits ledger:
// the billing leg reads each tenant's balance through
// billing.CreditService.Balance, whose documented materialize-on-first-
// read contract (go/billing/credit_service.go) creates the row it reads
// back for a tenant that has none yet -- billing's composed surface offers
// no balance read that would answer "no row" for a tenant that has never
// touched credits without first materializing it. Summary therefore
// performs exactly one kind of write: one zero-valued
// billing_credit_balances row per ledger tenant lacking a row, nothing for
// a tenant that already has one, nothing on any other table, and nothing
// further when the same tenant is summarized again. The claim and the call
// agree because this doc says so: it is not a read-only surface, and that
// materialization is the whole of what it writes.
type UsageService struct {
	metering *metering.Module // nil when WithMetering was never applied
	billing  *billing.Module  // nil when WithBilling was never applied
	tenants  *TenantService

	// bus backs the tenancy.WithSystemContext grant Summary takes out per
	// candidate tenant (D2's mechanism). Nil until Module.Register calls
	// attach.
	bus pkgcore.EventBus
}

// NewUsageService returns a UsageService reading metering/billing data
// through meteringModule/billingModule (either or both may be nil -- see
// their own doc comments on Module's WithMetering/WithBilling), with
// candidate tenants drawn from tenants (admin's own D3 ledger).
func NewUsageService(meteringModule *metering.Module, billingModule *billing.Module, tenants *TenantService) *UsageService {
	return &UsageService{metering: meteringModule, billing: billingModule, tenants: tenants}
}

// attach gives the service the bus it needs for tenancy.WithSystemContext.
func (s *UsageService) attach(bus pkgcore.EventBus) { s.bus = bus }

// Summary returns D9's row for every tenant in admin's own ledger (D3),
// under D2's mechanism -- looping tenancy.WithSystemContext per tenant,
// exactly like SearchService.MembershipsOf and AuditService's cross-
// tenant path.
//
// Writes: none against admin's own tables, go/metering's tables, or any
// subscription or credit-transaction table; exactly one zero-valued
// billing_credit_balances row per ledger tenant that has no balance row
// yet, created by the billing leg's Balance call (its documented
// materialize-on-first-read contract -- see UsageService's own doc
// comment). A tenant that already has a row is never written by Summary,
// and a repeated Summary writes nothing further.
//
// actorUserID identifies the platform operator making this cross-tenant
// read, for pkgcore.SystemReason.Actor. When neither go/metering nor
// go/billing was ever wired, this refuses outright with
// ErrUsageModulesNotWired before touching the ledger at all: a dashboard
// with nothing to stitch is not a partial answer, it is a wiring gap. A
// per-tenant read failing for any OTHER reason (a real storage error, an
// audited-escape-hatch failure) aborts the whole call rather than
// silently omitting that tenant, the same no-silent-omission discipline
// SearchService.MembershipsOf documents for its own identical loop.
func (s *UsageService) Summary(ctx context.Context, actorUserID string) ([]UsageSummaryRow, error) {
	if s.metering == nil && s.billing == nil {
		return nil, ErrUsageModulesNotWired
	}

	ledger, err := s.tenants.ListAllIDs(ctx)
	if err != nil {
		return nil, err
	}

	rows := make([]UsageSummaryRow, 0, len(ledger))
	for _, id := range ledger {
		tenantID := pkgcore.TenantID(id)
		tenantCtx, err := tenancy.WithSystemContext(
			pkgcore.WithTenant(ctx, tenantID),
			s.bus,
			pkgcore.SystemReason{
				Actor:   actorUserID,
				Purpose: SystemPurposeAdminCrossTenant,
			},
		)
		if err != nil {
			return nil, err
		}

		row := UsageSummaryRow{TenantID: id}
		if t, getErr := s.tenants.Get(ctx, id); getErr == nil {
			row.DisplayName = t.DisplayName
		} else {
			// DisplayName is cosmetic (the row's TenantID is already
			// authoritative -- it came from the ledger's own ListAllIDs a
			// moment ago), so a failure here does not abort the whole
			// call the way a real metering/billing read failure does
			// below -- but this file's own no-silent-omission discipline
			// (see this method's doc comment) means the failure must
			// still be SURFACED, not silently swallowed into an
			// unexplained blank name. A Warn log is the honest middle
			// ground: the row still renders, and an operator staring at
			// a blank DisplayName can find out why in the logs instead
			// of assuming the ledger genuinely has none recorded.
			obs.FromContext(ctx).Warn("admin could not read a tenant's display name for the usage summary row",
				"tenant_id", id, "error", getErr)
		}

		if s.metering != nil {
			summaries, err := s.metering.Summaries().List(tenantCtx)
			if err != nil {
				return nil, err
			}
			row.MeteringSummaries = summaries
		}

		if s.billing != nil {
			balance, err := s.billing.Credits().Balance(tenantCtx)
			if err != nil {
				return nil, err
			}
			row.CreditBalance = balance

			sub, err := s.billing.Subscriptions().Active(tenantCtx)
			if err != nil {
				return nil, err
			}
			row.ActiveSubscription = sub
		}

		rows = append(rows, row)
	}
	return rows, nil
}
