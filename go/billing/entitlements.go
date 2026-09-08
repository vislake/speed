package billing

import (
	"context"
	"math"
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
// go/billing's go.mod still requires go/metering directly -- sanctioned
// by this codebase's dependency direction, since metering sits below
// billing in the module dependency graph -- and module.go's compile-time
// assertion proves *metering.Aggregator satisfies UsageReader
// structurally, so a host wires the real thing with no adapter to write.
//
// Quota decisions read the real-time counter, never a summary table: a
// summary table has aggregation delay, and deciding against it would let
// an over-quota request through.
type UsageReader interface {
	RealtimeCount(tenantID, feature string, at time.Time) (float64, error)
}

// EntitlementsService implements Entitlements. It is the single judgment
// entry point business code -- go/ai-gateway's checkEntitlement gate
// included -- calls to learn whether a tenant's current subscription
// permits a feature. See the Entitlements interface's own doc comment for
// the full
// contract, in particular that Check never decides anything about
// credits (CreditService is the separate, synchronous path for that).
//
// usage may be nil only for a service that will never be asked about a
// FeatureKindQuota feature: Boolean and Unlimited features never consult
// the UsageReader, but a Quota decision cannot be made without it, and a
// nil reader answers ErrUsageReaderUnconfigured on the first such Check
// (checkQuota) rather than panicking on a nil interface call or guessing
// an allowance -- fail closed, never fail open.
type EntitlementsService struct {
	subscriptions *SubscriptionService
	plans         *PlanStore
	usage         UsageReader
	now           func() time.Time
}

// NewEntitlementsService returns an EntitlementsService reading
// subscriptions through subscriptions, plans through plans, and real-time
// quota counters through usage. Pass a non-nil usage when any caller may
// Check a Quota feature -- see the type's own doc comment for what a nil
// usage answers then.
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
		return Decision{Allowed: false, Reason: DecisionReasonNoSubscription}, nil
	}

	// A subscription may reference either a tenant-custom Plan of its own
	// tenant or the platform-wide Plan it was created against -- never
	// another tenant's custom Plan (SubscriptionService.Create's own scope
	// guard). Resolve the reference the same way Create admitted it: the
	// tenant's own scope first (PlanStore.Get), then the platform-wide
	// catalog (PlanStore.GetPlatformPlan, whose doc comment explains why a
	// platform-wide row is not in any tenant's own scope). A PlanID that
	// names neither -- the plan was deleted out from under the
	// subscription, or the row references a scope no lookup may reach --
	// is treated exactly like "no subscription", since there is nothing
	// left to grant against.
	plan, err := s.plans.Get(ctx, tenant, sub.PlanID)
	if hasCode(err, ErrPlanNotFound.Code) {
		plan, err = s.plans.GetPlatformPlan(ctx, sub.PlanID)
	}
	if err != nil {
		if hasCode(err, ErrPlanNotFound.Code) {
			return Decision{Allowed: false, Reason: DecisionReasonNoSubscription}, nil
		}
		return Decision{}, err
	}

	grant, ok := plan.Grant(featureKey)
	if !ok {
		return Decision{Allowed: false, Reason: DecisionReasonFeatureDisabled}, nil
	}

	kind, ok := grantKind(grant)
	if !ok {
		// A Grant whose Value no Feature kind interprets -- a string that is
		// not the GrantValueUnlimited sentinel, e.g. a quota limit encoded
		// as "1000" by a config/import error -- is refused fail-closed with
		// the same feature_disabled answer checkQuota's own malformed-value
		// branch gives, never granted unlimited: a malformed grant must
		// deny, exactly like a feature that is not granted at all.
		return Decision{Allowed: false, Reason: DecisionReasonFeatureDisabled}, nil
	}

	switch kind {
	case FeatureKindBoolean:
		return s.checkBoolean(grant)
	case FeatureKindUnlimited:
		return Decision{Allowed: true, Reason: DecisionReasonOK}, nil
	default: // FeatureKindQuota
		return s.checkQuota(ctx, string(tenant), featureKey, requested, grant)
	}
}

// grantKind infers the Feature.Kind a Grant was issued for, from the Go
// type of its own Value -- the Grant carries no Kind field of its own --
// so the value's shape is the only signal Check has. The three
// legitimate shapes are model.go's own documented vocabulary: a bool for
// FeatureKindBoolean, the GrantValueUnlimited sentinel string for
// FeatureKindUnlimited, any numeric for FeatureKindQuota (an int64 value
// round-tripped through Plan.GrantsJSON decodes as float64 -- see
// grantQuotaLimit's own doc comment). Any OTHER shape -- most importantly
// a string that is not the sentinel, since a quota limit encoded as
// "1000" is precisely the kind of config/import error Check must fail
// closed on -- is malformed, reported through ok=false, never guessed at.
func grantKind(g Grant) (kind FeatureKind, ok bool) {
	switch g.Value.(type) {
	case bool:
		return FeatureKindBoolean, true
	case string:
		if g.Value == GrantValueUnlimited {
			return FeatureKindUnlimited, true
		}
		return "", false
	default:
		return FeatureKindQuota, true
	}
}

func (s *EntitlementsService) checkBoolean(g Grant) (Decision, error) {
	allowed, _ := g.Value.(bool)
	if !allowed {
		return Decision{Allowed: false, Reason: DecisionReasonFeatureDisabled}, nil
	}
	return Decision{Allowed: true, Reason: DecisionReasonOK}, nil
}

func (s *EntitlementsService) checkQuota(ctx context.Context, tenantID, featureKey string, requested int64, g Grant) (Decision, error) {
	limit, ok := grantQuotaLimit(g.Value)
	if !ok {
		// A Quota grant whose Value is not a usable integer is treated as
		// disabled rather than panicking or silently allowing unlimited
		// use -- a malformed Grant must never fail open.
		return Decision{Allowed: false, Reason: DecisionReasonFeatureDisabled}, nil
	}

	if s.usage == nil {
		// The service was built without a UsageReader and a Quota decision
		// needs one -- the honest answer is the coded configuration error,
		// never a panic on the nil interface call and never a guessed
		// allowance (zero usage would fail OPEN for an over-quota tenant).
		// See the EntitlementsService type's own doc comment.
		return Decision{}, ErrUsageReaderUnconfigured
	}
	used, err := s.usage.RealtimeCount(tenantID, featureKey, s.now())
	if err != nil {
		return Decision{}, err
	}

	// Usage counts are float64 (go/metering's real-time counters are) while
	// the quota limit is an integer. Truncating the used count toward zero
	// at the decision point -- int64(used) -- would round 99.9 down to 99
	// and let a request of 1 past a limit of 100 even though 99.9 + 1 is
	// already over. The decision rounds the used count UP to the next whole
	// unit instead -- the tenant-hostile direction, the one that can only
	// refuse a request that was within quota by a hair of a unit, never
	// admit one that is genuinely over.
	remaining := limit - int64(math.Ceil(used))
	if requested <= remaining {
		return Decision{Allowed: true, Remaining: quotaRemaining(remaining - requested), Reason: DecisionReasonOK}, nil
	}

	switch g.OverageMode {
	case OverageModeAllowAndBill, OverageModeNotify:
		// Allowed past the limit; go/metering's own Aggregator publishes
		// EventOverageThresholdCrossed independently of this call (see
		// UsageReader's own doc comment) -- Check does not publish
		// anything itself.
		return Decision{Allowed: true, Remaining: quotaRemaining(remaining - requested), Reason: DecisionReasonOK}, nil
	default: // OverageModeBlock, or an unrecognized mode -- fail closed.
		return Decision{Allowed: false, Remaining: quotaRemaining(remaining), Reason: DecisionReasonQuotaExceeded}, nil
	}
}

// quotaRemaining returns a pointer to n for a bounded Decision.Remaining:
// nil is the unbounded marker (see Decision's own doc comment), so only
// genuinely bounded quota answers ever carry a non-nil pointer.
func quotaRemaining(n int64) *int64 { return &n }

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
