package billing

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// fakeUsageReader answers a fixed count for every RealtimeCount call,
// regardless of tenant/feature/at -- entitlements_test.go's own cases each
// construct one with exactly the count their scenario needs.
type fakeUsageReader struct{ count float64 }

func (f fakeUsageReader) RealtimeCount(tenantID, feature string, at time.Time) (float64, error) {
	return f.count, nil
}

// newEntitlementsFixture wires a fresh Plan (with the given grants),
// SubscriptionService and EntitlementsService, plus an ACTIVE subscription
// for tenant onto that plan -- the common setup every case below shares.
func newEntitlementsFixture(t *testing.T, grants []Grant, usage float64) (*EntitlementsService, context.Context) {
	t.Helper()
	db := newTestDB(t)
	plans := NewPlanStore(db)
	plan := &Plan{Key: "pro", Name: "Pro"}
	if err := plan.SetGrants(grants); err != nil {
		t.Fatalf("SetGrants: %v", err)
	}
	if err := plans.Create(context.Background(), plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	subs := NewSubscriptionService(NewSubscriptionRepository(db), plans, nil)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	sub, err := subs.Create(ctx, CreateInput{PlanID: plan.ID})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if _, err := subs.Activate(ctx, sub.ID); err != nil {
		t.Fatalf("activate subscription: %v", err)
	}

	return NewEntitlementsService(subs, plans, fakeUsageReader{count: usage}), ctx
}

func TestEntitlementsService_Check_NoSubscription(t *testing.T) {
	db := newTestDB(t)
	plans := NewPlanStore(db)
	subs := NewSubscriptionService(NewSubscriptionRepository(db), plans, nil)
	svc := NewEntitlementsService(subs, plans, fakeUsageReader{})
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	decision, err := svc.Check(ctx, "anything", 1)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if decision.Allowed || decision.Reason != DecisionReasonNoSubscription {
		t.Errorf("decision = %+v, want Allowed=false Reason=no_subscription", decision)
	}
}

func TestEntitlementsService_Check_Boolean(t *testing.T) {
	svc, ctx := newEntitlementsFixture(t, []Grant{
		{FeatureKey: "priority_support", Value: true},
		{FeatureKey: "beta_features", Value: false},
	}, 0)

	allowed, err := svc.Check(ctx, "priority_support", 1)
	if err != nil {
		t.Fatalf("Check(priority_support): %v", err)
	}
	if !allowed.Allowed || allowed.Reason != DecisionReasonOK {
		t.Errorf("priority_support decision = %+v, want Allowed=true Reason=ok", allowed)
	}
	if allowed.Remaining != DecisionRemainingUnbounded {
		t.Errorf("priority_support Remaining = %d, want %d (unbounded)", allowed.Remaining, DecisionRemainingUnbounded)
	}

	denied, err := svc.Check(ctx, "beta_features", 1)
	if err != nil {
		t.Fatalf("Check(beta_features): %v", err)
	}
	if denied.Allowed || denied.Reason != DecisionReasonFeatureDisabled {
		t.Errorf("beta_features decision = %+v, want Allowed=false Reason=feature_disabled", denied)
	}
}

func TestEntitlementsService_Check_Unlimited(t *testing.T) {
	svc, ctx := newEntitlementsFixture(t, []Grant{
		{FeatureKey: "storage", Value: GrantValueUnlimited},
	}, 0)

	decision, err := svc.Check(ctx, "storage", 1_000_000)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !decision.Allowed || decision.Remaining != DecisionRemainingUnbounded {
		t.Errorf("decision = %+v, want Allowed=true Remaining=%d", decision, DecisionRemainingUnbounded)
	}
}

func TestEntitlementsService_Check_FeatureNotGranted(t *testing.T) {
	svc, ctx := newEntitlementsFixture(t, []Grant{{FeatureKey: "seats", Value: int64(5)}}, 0)

	decision, err := svc.Check(ctx, "not_a_real_feature", 1)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if decision.Allowed || decision.Reason != DecisionReasonFeatureDisabled {
		t.Errorf("decision = %+v, want Allowed=false Reason=feature_disabled", decision)
	}
}

func TestEntitlementsService_Check_Quota_WithinLimit(t *testing.T) {
	svc, ctx := newEntitlementsFixture(t, []Grant{
		{FeatureKey: "api_calls", Value: int64(10), Period: ResetPeriodMonthly, OverageMode: OverageModeBlock},
	}, 4) // 4 already used

	decision, err := svc.Check(ctx, "api_calls", 3)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !decision.Allowed || decision.Reason != DecisionReasonOK {
		t.Errorf("decision = %+v, want Allowed=true Reason=ok", decision)
	}
	if decision.Remaining != 3 { // limit 10 - used 4 - requested 3 = 3
		t.Errorf("Remaining = %d, want 3", decision.Remaining)
	}
}

func TestEntitlementsService_Check_Quota_ExceedsLimit_Block(t *testing.T) {
	svc, ctx := newEntitlementsFixture(t, []Grant{
		{FeatureKey: "api_calls", Value: int64(10), Period: ResetPeriodMonthly, OverageMode: OverageModeBlock},
	}, 9)

	decision, err := svc.Check(ctx, "api_calls", 5)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if decision.Allowed || decision.Reason != DecisionReasonQuotaExceeded {
		t.Errorf("decision = %+v, want Allowed=false Reason=quota_exceeded", decision)
	}
}

func TestEntitlementsService_Check_Quota_ExceedsLimit_AllowAndBill(t *testing.T) {
	svc, ctx := newEntitlementsFixture(t, []Grant{
		{FeatureKey: "api_calls", Value: int64(10), Period: ResetPeriodMonthly, OverageMode: OverageModeAllowAndBill},
	}, 9)

	decision, err := svc.Check(ctx, "api_calls", 5)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !decision.Allowed || decision.Reason != DecisionReasonOK {
		t.Errorf("decision = %+v, want Allowed=true Reason=ok (overage billed separately)", decision)
	}
	if decision.Remaining >= 0 {
		t.Errorf("Remaining = %d, want negative (the magnitude of the overage)", decision.Remaining)
	}
}

func TestEntitlementsService_Check_Quota_UsesRealTimeCounter_NeverASummaryTable(t *testing.T) {
	// This module ships no summary-table reader anywhere for
	// EntitlementsService to accidentally reach for -- UsageReader's own
	// shape is exactly go/metering's RealtimeCount, and this test simply
	// pins that the fake's returned value drives the decision, proving
	// Check consults it (as opposed to, say, always answering "ok"
	// regardless of usage).
	svc, ctx := newEntitlementsFixture(t, []Grant{
		{FeatureKey: "api_calls", Value: int64(10), OverageMode: OverageModeBlock},
	}, 10) // already at the limit

	decision, err := svc.Check(ctx, "api_calls", 1)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if decision.Allowed {
		t.Errorf("decision = %+v, want Allowed=false once usage already equals the limit", decision)
	}
}
