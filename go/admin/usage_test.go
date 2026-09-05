package admin

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/pkgcore"
)

// TestUsageService_NeitherModuleWired_Refused pins D9's one boot-shaped
// refusal at request time: a dashboard with nothing at all to stitch
// (neither go/metering nor go/billing ever wired) is a wiring gap, not a
// partial answer.
func TestUsageService_NeitherModuleWired_Refused(t *testing.T) {
	db := testutil.NewDB(t)
	svc := NewUsageService(nil, nil, NewTenantService(NewTenantRepository(db)))
	svc.attach(newTestRegistry().EventBus())

	_, err := svc.Summary(context.Background(), "operator-1")
	if !isCode(err, ErrUsageModulesNotWired.Code) {
		t.Fatalf("Summary() with neither module wired error = %v, want %s", err, ErrUsageModulesNotWired.Code)
	}
}

// TestUsageService_Summary_StitchesMeteringAndBilling is D9's core proof:
// real go/metering and go/billing data for a real, ledger-registered
// tenant is stitched into that tenant's own row, with no new aggregate
// table of admin's own -- Summary reads straight through to
// metering.SummaryRepository.List and billing.CreditService.Balance/
// billing.SubscriptionService.Active.
func TestUsageService_Summary_StitchesMeteringAndBilling(t *testing.T) {
	env := buildTestAdminModule(t)

	const tenant = pkgcore.TenantID("tenant-usage-flow")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Usage Flow Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	// A real metering event, folded into a real UsageSummary row.
	if err := env.Metering.Aggregator().Ingest(context.Background(), metering.UsageEvent{
		TenantID:       string(tenant),
		Feature:        "ai.generation",
		Quantity:       3,
		IdempotencyKey: "usage-flow-key-1",
	}); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	// A real credit grant, materializing a real CreditBalance row.
	billingCtx := pkgcore.WithTenant(context.Background(), tenant)
	if _, err := env.Billing.Credits().Grant(billingCtx, billing.GrantInput{Amount: 500, Reason: "usage-flow-test"}); err != nil {
		t.Fatalf("Grant() error = %v", err)
	}

	svc := NewUsageService(env.Metering, env.Billing, env.Admin.Tenants())
	svc.attach(env.Registry.EventBus())

	rows, err := svc.Summary(context.Background(), "operator-1")
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}

	var row *UsageSummaryRow
	for i := range rows {
		if rows[i].TenantID == string(tenant) {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("Summary() rows = %+v, want a row for %q", rows, tenant)
	}

	if len(row.MeteringSummaries) != 1 || row.MeteringSummaries[0].Feature != "ai.generation" || row.MeteringSummaries[0].Quantity != 3 {
		t.Errorf("row.MeteringSummaries = %+v, want exactly one ai.generation row of quantity 3", row.MeteringSummaries)
	}
	if row.CreditBalance == nil || row.CreditBalance.Available != 500 {
		t.Errorf("row.CreditBalance = %+v, want Available=500", row.CreditBalance)
	}
	// No subscription was ever created for this tenant.
	if row.ActiveSubscription != nil {
		t.Errorf("row.ActiveSubscription = %+v, want nil (none created)", row.ActiveSubscription)
	}
}

// TestUsageService_Summary_OnlyMeteringWired proves the two modules are
// independently optional: wiring only go/metering leaves every row's
// CreditBalance/ActiveSubscription absent (nil), never refusing the
// whole call, and never fabricating billing data that was never wired.
func TestUsageService_Summary_OnlyMeteringWired(t *testing.T) {
	env := buildTestAdminModule(t)

	const tenant = pkgcore.TenantID("tenant-usage-metering-only")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Metering Only Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	svc := NewUsageService(env.Metering, nil, env.Admin.Tenants())
	svc.attach(env.Registry.EventBus())

	rows, err := svc.Summary(context.Background(), "operator-1")
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}

	found := false
	for _, row := range rows {
		if row.TenantID != string(tenant) {
			continue
		}
		found = true
		if row.MeteringSummaries == nil {
			t.Errorf("row.MeteringSummaries = nil, want a non-nil (possibly empty) slice since go/metering is wired")
		}
		if row.CreditBalance != nil {
			t.Errorf("row.CreditBalance = %+v, want nil: go/billing was never wired", row.CreditBalance)
		}
		if row.ActiveSubscription != nil {
			t.Errorf("row.ActiveSubscription = %+v, want nil: go/billing was never wired", row.ActiveSubscription)
		}
	}
	if !found {
		t.Fatalf("Summary() rows = %+v, want a row for %q", rows, tenant)
	}
}
