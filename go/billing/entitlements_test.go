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
	if allowed.Remaining != nil {
		t.Errorf("priority_support Remaining = %v, want nil (the unbounded marker -- Boolean features have no remaining count)", allowed.Remaining)
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
	if !decision.Allowed || decision.Remaining != nil {
		t.Errorf("decision = %+v, want Allowed=true Remaining=nil (the unbounded marker)", decision)
	}
}

// TestEntitlementsService_Check_NonSentinelStringGrantValue_FailsClosed
// pins the string-value refusal: a string-typed Grant.Value that is NOT the
// GrantValueUnlimited sentinel -- a quota limit encoded as "1000" by a
// config/import error, a typo'd "unlimted" -- must never be read as
// FeatureKindUnlimited. grantKind must not classify every string
// as Unlimited, or such a grant would let every request through with
// Remaining=unbounded -- exactly the fail-open a malformed grant must not
// produce. Check fails closed with the same feature_disabled answer
// checkQuota's own "Value is not a usable integer" branch gives
// for a malformed Quota grant: the feature is treated as disabled, never
// as unlimited.
func TestEntitlementsService_Check_NonSentinelStringGrantValue_FailsClosed(t *testing.T) {
	svc, ctx := newEntitlementsFixture(t, []Grant{
		{FeatureKey: "ai_tokens", Value: "1000"},
	}, 0)

	decision, err := svc.Check(ctx, "ai_tokens", 1)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if decision.Allowed || decision.Remaining != nil || decision.Reason != DecisionReasonFeatureDisabled {
		t.Errorf("decision = %+v, want Allowed=false Remaining=nil Reason=feature_disabled (a string grant value must fail closed, never grant unlimited)", decision)
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
	if decision.Remaining == nil || *decision.Remaining != 3 { // limit 10 - used 4 - requested 3 = 3
		t.Errorf("Remaining = %v, want 3", decision.Remaining)
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
	if decision.Remaining == nil || *decision.Remaining >= 0 {
		t.Errorf("Remaining = %v, want negative (the magnitude of the overage)", decision.Remaining)
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

// TestEntitlementsService_Check_NilUsageReader_FailsClosed pins the
// refusal: a Quota Check on a service built without a UsageReader (nil)
// must fail closed with the coded configuration error on the very first
// call -- never a panic on the nil interface call, and never a guessed
// allowance (a zero-usage guess would fail OPEN for an over-quota tenant).
func TestEntitlementsService_Check_NilUsageReader_FailsClosed(t *testing.T) {
	svc, ctx := newEntitlementsFixture(t, []Grant{
		{FeatureKey: "api_calls", Value: int64(10), Period: ResetPeriodMonthly, OverageMode: OverageModeBlock},
	}, 0)
	svc.usage = nil // the wiring gap: NewEntitlementsService was handed no UsageReader

	_, err := svc.Check(ctx, "api_calls", 1)
	if !hasCode(err, ErrUsageReaderUnconfigured.Code) {
		t.Fatalf("Check with nil UsageReader: err = %v, want %s (fail closed, never a panic, never an allowance)", err, ErrUsageReaderUnconfigured.Code)
	}
}

// TestEntitlementsService_Check_Quota_FractionalUsage_RoundsUpNotDown is
// pins the rounding direction: usage counters are float64, and truncating the used
// count toward zero at the decision point lets 99.9 used + 1 requested
// pass a limit of 100 -- 99.9 + 1 is already over. The decision must round
// in the tenant-hostile direction (up), refusing this request.
func TestEntitlementsService_Check_Quota_FractionalUsage_RoundsUpNotDown(t *testing.T) {
	svc, ctx := newEntitlementsFixture(t, []Grant{
		{FeatureKey: "api_calls", Value: int64(100), Period: ResetPeriodMonthly, OverageMode: OverageModeBlock},
	}, 99.9) // the counter reports 99.9 units already used

	decision, err := svc.Check(ctx, "api_calls", 1)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if decision.Allowed {
		t.Errorf("decision = %+v, want Allowed=false (99.9 used + 1 requested already exceeds the 100 limit; usage must never be truncated toward zero at the decision point)", decision)
	}
	if decision.Reason != DecisionReasonQuotaExceeded {
		t.Errorf("Reason = %q, want quota_exceeded", decision.Reason)
	}
}

// TestEntitlementsService_Check_UnboundedDecision_SurfacesTheMarkerNotMinusOne
// pins the unbounded marker: an unbounded decision -- Unlimited or Boolean
// features, and the feature_disabled / no_subscription answers -- must
// surface the unbounded state as Decision.Remaining's nil marker, never as
// a -1 the answer's numeric slot would leak to a UI-shaped consumer as a
// real remaining count (indistinguishable from the genuine -1 raw headroom
// a bounded overage decision legitimately reports -- the collision the
// bare -1 sentinel would make both "unlimited" and "one unit over" read as -1).
func TestEntitlementsService_Check_UnboundedDecision_SurfacesTheMarkerNotMinusOne(t *testing.T) {
	svc, ctx := newEntitlementsFixture(t, []Grant{
		{FeatureKey: "storage", Value: GrantValueUnlimited},
		{FeatureKey: "priority_support", Value: true},
		{FeatureKey: "api_calls", Value: int64(100), Period: ResetPeriodMonthly, OverageMode: OverageModeAllowAndBill},
	}, 100)

	for _, featureKey := range []string{"storage", "priority_support"} {
		decision, err := svc.Check(ctx, featureKey, 1)
		if err != nil {
			t.Fatalf("Check(%s): %v", featureKey, err)
		}
		if !decision.Allowed || decision.Remaining != nil {
			t.Errorf("Check(%s) = %+v, want Allowed=true Remaining=nil (the distinct unbounded marker, never -1)", featureKey, decision)
		}
	}

	denied, err := svc.Check(ctx, "not_a_real_feature", 1)
	if err != nil {
		t.Fatalf("Check(disabled): %v", err)
	}
	if denied.Allowed || denied.Remaining != nil {
		t.Errorf("feature_disabled decision = %+v, want Allowed=false Remaining=nil", denied)
	}

	// The genuine negative raw headroom a bounded overage decision reports
	// still sits in the numeric slot -- nil is reserved for "no numeric
	// ceiling", so a consumer can never confuse the two: a -1 with
	// Allowed=true on a quota feature is real headroom, never the unbounded
	// marker.
	over, err := svc.Check(ctx, "api_calls", 1)
	if err != nil {
		t.Fatalf("Check(api_calls): %v", err)
	}
	if !over.Allowed || over.Remaining == nil || *over.Remaining != -1 {
		t.Errorf("overage decision = %+v, want Allowed=true Remaining=-1 (genuine raw headroom, distinct from the nil unbounded marker)", over)
	}
}
