package billing

import "context"

// FeatureKind classifies how a Feature's Grant.Value is interpreted.
type FeatureKind string

const (
	// FeatureKindBoolean is an on/off feature: Grant.Value is a bool.
	FeatureKindBoolean FeatureKind = "boolean"
	// FeatureKindQuota is a metered feature with a numeric limit per
	// ResetPeriod: Grant.Value is an int64. Entitlements.Check reads
	// go/metering's real-time counter (never a summary table) to compare
	// usage-so-far against the limit.
	FeatureKindQuota FeatureKind = "quota"
	// FeatureKindUnlimited is a feature with no numeric ceiling: presence
	// of the Grant is the whole answer. Grant.Value is conventionally
	// GrantValueUnlimited ("unlimited"), never consulted for its content.
	FeatureKindUnlimited FeatureKind = "unlimited"
)

// GrantValueUnlimited is the sentinel Grant.Value string for a
// FeatureKindUnlimited grant, matching
// docs/internal/06-billing-and-metering.md's sketch (Value any // bool /
// int64 / "unlimited") literally.
const GrantValueUnlimited = "unlimited"

// Feature is one capability a Plan can grant. Kind decides how Grant.Value
// is interpreted for it: Boolean features let "can this tenant use model
// X" and "does this tenant have feature Y at all" reuse the identical
// mechanism a numeric quota uses, so business code never needs a second,
// bespoke on/off switch.
type Feature struct {
	// Key identifies the feature, e.g. "seats", "api_calls", "ai_tokens",
	// or "model:gpt-4o" for a per-model access gate.
	Key string
	// Kind is FeatureKindBoolean, FeatureKindQuota or FeatureKindUnlimited.
	Kind FeatureKind
	// Unit is the measurement unit for a FeatureKindQuota feature (e.g.
	// "tokens", "calls", "seats"). Empty for the other two kinds.
	Unit string
}

// ResetPeriod names when a Quota grant's consumption resets.
type ResetPeriod string

const (
	// ResetPeriodNever means the quota never resets: it is a lifetime
	// ceiling.
	ResetPeriodNever ResetPeriod = "never"
	// ResetPeriodMonthly resets the quota at the start of each calendar
	// month, matching go/metering's PeriodBucketMonthly bucket.
	ResetPeriodMonthly ResetPeriod = "monthly"
	// ResetPeriodBillingCycle resets the quota at the start of the
	// tenant's own subscription billing cycle, which need not align with
	// the calendar month.
	ResetPeriodBillingCycle ResetPeriod = "billing_cycle"
)

// OverageMode decides what happens when a Quota feature's usage would
// exceed its Grant's limit.
type OverageMode string

const (
	// OverageModeBlock refuses a request that would exceed the limit.
	// Entitlements.Check reports Decision{Allowed: false, Reason:
	// "quota_exceeded"}.
	OverageModeBlock OverageMode = "block"
	// OverageModeAllowAndBill allows the request past the limit; the
	// overage is expected to be billed separately (credits, or a later
	// invoice line item -- neither is this package's concern inside
	// Check itself).
	OverageModeAllowAndBill OverageMode = "allow_and_bill"
	// OverageModeNotify allows the request past the limit and relies on
	// go/metering's own EventOverageThresholdCrossed (published by the
	// Aggregator this Grant's usage counter already feeds) for the
	// notification signal -- Check does not publish anything itself.
	OverageModeNotify OverageMode = "notify"
)

// Grant is one Feature a Plan awards, with how much and what happens on
// overage.
type Grant struct {
	// FeatureKey is the Feature.Key this grant is for.
	FeatureKey string
	// Value is the grant's value, interpreted per the Feature's Kind:
	// bool for FeatureKindBoolean, int64 for FeatureKindQuota, and the
	// GrantValueUnlimited sentinel string for FeatureKindUnlimited.
	Value any
	// Period is the Quota grant's reset cadence. Meaningless (left at its
	// zero value) for Boolean and Unlimited grants.
	Period ResetPeriod
	// OverageMode decides what happens when a Quota grant's usage would
	// exceed Value. Meaningless for Boolean and Unlimited grants.
	OverageMode OverageMode
}

// BillingInterval names how often a Plan's Price recurs.
type BillingInterval string

const (
	// BillingIntervalMonth is a monthly recurring charge.
	BillingIntervalMonth BillingInterval = "month"
	// BillingIntervalYear is a yearly recurring charge.
	BillingIntervalYear BillingInterval = "year"
	// BillingIntervalOneTime is a single, non-recurring charge.
	BillingIntervalOneTime BillingInterval = "one_time"
)

// Money is an exact monetary amount: an integer minor-unit count (cents for
// a two-decimal currency) plus its ISO 4217 currency code, never a
// floating-point amount. Cents is the field name this codebase's own
// logging convention already expects (backend-coding-standards §11's
// "amount_cents" key).
type Money struct {
	Cents    int64
	Currency string
}

// Decision is Entitlements.Check's answer: whether the request is allowed,
// how much of the feature's quota remains, and why.
type Decision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool
	// Remaining is the quota units left in the current period, for a
	// FeatureKindQuota feature. It is DecisionRemainingUnbounded for
	// Boolean and Unlimited features, since "remaining" has no meaning
	// for either. It may be negative for an OverageModeAllowAndBill /
	// OverageModeNotify grant whose usage has gone past its limit -- the
	// magnitude of the overage.
	Remaining int64
	// Reason names why Allowed has its value: "ok", "feature_disabled",
	// "quota_exceeded" or "no_subscription".
	Reason string
}

// DecisionRemainingUnbounded is Decision.Remaining's sentinel value for a
// feature with no numeric ceiling to be "remaining" of (Boolean and
// Unlimited kinds).
const DecisionRemainingUnbounded int64 = -1

// Decision.Reason's closed vocabulary, matching
// docs/internal/06-billing-and-metering.md's Decision sketch exactly.
const (
	DecisionReasonOK              = "ok"
	DecisionReasonFeatureDisabled = "feature_disabled"
	DecisionReasonQuotaExceeded   = "quota_exceeded"
	DecisionReasonNoSubscription  = "no_subscription"
)

// Entitlements is the single judgment entry point business code -- including
// the not-yet-built AI gateway -- calls to learn whether a tenant's current
// subscription permits a feature. Callers never read the subscription or
// plan tables themselves, and never compute quota consumption by hand.
//
// Check answers "does the plan allow this"; it never decides anything about
// credits (docs/internal/06-billing-and-metering.md's own explicit split --
// see CreditService for the separate, synchronous reserve/confirm/refund
// path). A per-use operation typically consults both: Check to learn
// whether the plan permits the call at all, then CreditService.PreDeduct to
// reserve the credits it costs.
type Entitlements interface {
	// Check reports whether featureKey may be consumed for requested
	// additional units, for the tenant found in ctx
	// (pkgcore.TenantFromContext). requested is typically 1 for a
	// Boolean or Unlimited feature, and the unit count being consumed
	// (e.g. tokens about to be spent) for a Quota feature.
	Check(ctx context.Context, featureKey string, requested int64) (Decision, error)
}
