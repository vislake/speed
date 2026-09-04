package billing

import (
	"context"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// UsageReader is the real-time quota-counter read this package needs --
// exactly go/metering's Aggregator.RealtimeCount shape
// (tenantID, feature string, at time.Time) (float64, error). Declaring it
// as a small interface here, rather than depending on the concrete
// *metering.Aggregator type in EntitlementsService's own field, keeps this
// package's unit tests independent of a real Aggregator/database wiring.
//
// go/billing's go.mod still requires go/metering directly -- sanctioned by
// this codebase's dependency direction, since metering sits below billing
// (docs/internal/01-architecture.md: "... -> authn/rbac/org/metering ->
// billing/...") -- and module.go's compile-time assertion proves
// *metering.Aggregator satisfies UsageReader structurally, so a host wires
// the real thing with no adapter to write.
//
// docs/internal/06-billing-and-metering.md is explicit that quota decisions
// read the real-time counter, never a summary table: a summary table has
// aggregation delay, and deciding against it would let an over-quota
// request through.
type UsageReader interface {
	RealtimeCount(tenantID, feature string, at time.Time) (float64, error)
}

// EntitlementsService implements Entitlements. It is the single judgment
// entry point business code -- including the not-yet-built AI gateway --
// calls to learn whether a tenant's current subscription permits a
// feature. See the Entitlements interface's own doc comment for the full
// contract, in particular that Check never decides anything about
// credits (CreditService is the separate, synchronous path for that).
type EntitlementsService struct {
	subscriptions *SubscriptionService
	plans         *PlanStore
	usage         UsageReader
	now           func() time.Time
}

// NewEntitlementsService returns an EntitlementsService reading
// subscriptions through subscriptions, plans through plans, and real-time
// quota counters through usage.
func NewEntitlementsService(subscriptions *SubscriptionService, plans *PlanStore, usage UsageReader) *EntitlementsService {
	return &EntitlementsService{
		subscriptions: subscriptions,
		plans:         plans,
		usage:         usage,
		now:           time.Now,
	}
}

// compile-time check that *EntitlementsService satisfies Entitlements.
var _ Entitlements = (*EntitlementsService)(nil)

// Check implements Entitlements.
func (s *EntitlementsService) Check(ctx context.Context, featureKey string, requested int64) (Decision, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return Decision{}, err
	}

	sub, err := s.subscriptions.Active(ctx)
	if err != nil {
		return Decision{}, err
	}
	if sub == nil {
		return Decision{Allowed: false, Remaining: DecisionRemainingUnbounded, Reason: DecisionReasonNoSubscription}, nil
	}

	plan, err := s.plans.Get(ctx, sub.PlanID)
	if err != nil {
		if hasCode(err, ErrPlanNotFound.Code) {
			// The subscription's own Plan was deleted out from under it --
			// treat exactly like "no subscription", since there is nothing
			// left to grant against.
			return Decision{Allowed: false, Remaining: DecisionRemainingUnbounded, Reason: DecisionReasonNoSubscription}, nil
		}
		return Decision{}, err
	}

	grant, ok := plan.Grant(featureKey)
	if !ok {
		return Decision{Allowed: false, Remaining: DecisionRemainingUnbounded, Reason: DecisionReasonFeatureDisabled}, nil
	}

	switch grantKind(grant) {
	case FeatureKindBoolean:
		return s.checkBoolean(grant)
	case FeatureKindUnlimited:
		return Decision{Allowed: true, Remaining: DecisionRemainingUnbounded, Reason: DecisionReasonOK}, nil
	default: // FeatureKindQuota
		return s.checkQuota(ctx, string(tenant), featureKey, requested, grant)
	}
}

// grantKind infers the Feature.Kind a Grant was issued for, from the Go
// type of its own Value -- Grant carries no Kind field of its own
// (docs/internal/06-billing-and-metering.md's sketch does not give it
// one), so the value's shape is the only signal Check has.
func grantKind(g Grant) FeatureKind {
	switch g.Value.(type) {
	case bool:
		return FeatureKindBoolean
	case string:
		return FeatureKindUnlimited
	default:
		return FeatureKindQuota
	}
}

func (s *EntitlementsService) checkBoolean(g Grant) (Decision, error) {
	allowed, _ := g.Value.(bool)
	if !allowed {
		return Decision{Allowed: false, Remaining: DecisionRemainingUnbounded, Reason: DecisionReasonFeatureDisabled}, nil
	}
	return Decision{Allowed: true, Remaining: DecisionRemainingUnbounded, Reason: DecisionReasonOK}, nil
}

func (s *EntitlementsService) checkQuota(ctx context.Context, tenantID, featureKey string, requested int64, g Grant) (Decision, error) {
	limit, ok := grantQuotaLimit(g.Value)
	if !ok {
		// A Quota grant whose Value is not a usable integer is treated as
		// disabled rather than panicking or silently allowing unlimited
		// use -- a malformed Grant must never fail open.
		return Decision{Allowed: false, Remaining: DecisionRemainingUnbounded, Reason: DecisionReasonFeatureDisabled}, nil
	}

	used, err := s.usage.RealtimeCount(tenantID, featureKey, s.now())
	if err != nil {
		return Decision{}, err
	}

	remaining := limit - int64(used)
	if requested <= remaining {
		return Decision{Allowed: true, Remaining: remaining - requested, Reason: DecisionReasonOK}, nil
	}

	switch g.OverageMode {
	case OverageModeAllowAndBill, OverageModeNotify:
		// Allowed past the limit; go/metering's own Aggregator publishes
		// EventOverageThresholdCrossed independently of this call (see
		// UsageReader's own doc comment) -- Check does not publish
		// anything itself.
		return Decision{Allowed: true, Remaining: remaining - requested, Reason: DecisionReasonOK}, nil
	default: // OverageModeBlock, or an unrecognized mode -- fail closed.
		return Decision{Allowed: false, Remaining: remaining, Reason: DecisionReasonQuotaExceeded}, nil
	}
}

// grantQuotaLimit extracts a Quota grant's int64 limit from its Value,
// accepting every concrete numeric type json.Unmarshal or a caller's own Go
// literal might have produced (an int64 value round-tripped through
// Plan.GrantsJSON decodes as float64 -- encoding/json's universal number
// type -- rather than int64).
func grantQuotaLimit(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
