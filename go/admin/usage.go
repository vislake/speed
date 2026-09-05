package admin

import (
	"context"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/metering"
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
	// never wired through WithBilling.
	CreditBalance *billing.CreditBalance

	// ActiveSubscription is the tenant's currently active subscription,
	// if any, read through billing.SubscriptionService.Active -- nil both
	// when go/billing was never wired AND when the tenant simply has none
	// active right now (Active's own (nil, nil) "no active subscription"
	// answer); UsageService.Summary does not attempt to distinguish the
	// two reasons for a nil value here, since CreditBalance (present
	// whenever go/billing is wired at all, per Balance's own
	// materialize-on-first-read contract) already tells a caller which
	// case applies.
	ActiveSubscription *billing.Subscription
}

// UsageService is D9's runtime: admin's own tenant-by-tenant stitching of
// go/metering's and go/billing's ALREADY-REAL, per-tenant query methods --
// no new database table, no new aggregate, exactly D9's own design
// (docs/internal/23-admin.md). It is a read-only aggregation surface.
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
