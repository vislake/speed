package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// billingSubscriptionsTable names the shared billing_subscriptions table.
const billingSubscriptionsTable = "billing_subscriptions"

// SubscriptionStatus is a Subscription's lifecycle state.
type SubscriptionStatus string

const (
	// SubscriptionStatusCreated is a subscription that exists but has not
	// yet been confirmed active -- e.g. awaiting its first successful
	// payment.
	SubscriptionStatusCreated SubscriptionStatus = "created"
	// SubscriptionStatusActive is a subscription in good standing.
	// Entitlements.Check only ever grants access for an Active
	// subscription.
	SubscriptionStatusActive SubscriptionStatus = "active"
	// SubscriptionStatusPastDue is a subscription whose most recent
	// payment attempt failed but has not yet been given up on.
	SubscriptionStatusPastDue SubscriptionStatus = "past_due"
	// SubscriptionStatusCanceled is a subscription that has ended.
	// Terminal: no further transition is legal from it.
	SubscriptionStatusCanceled SubscriptionStatus = "canceled"
)

// subscriptionTransitions is the legal-transition table: for each current
// status, the set of statuses a transition may move to. A transition not
// listed here (including any transition out of SubscriptionStatusCanceled,
// a terminal status with an empty entry) is
// ErrInvalidSubscriptionTransition.
var subscriptionTransitions = map[SubscriptionStatus]map[SubscriptionStatus]bool{
	SubscriptionStatusCreated: {
		SubscriptionStatusActive:   true,
		SubscriptionStatusCanceled: true,
	},
	SubscriptionStatusActive: {
		SubscriptionStatusPastDue:  true,
		SubscriptionStatusCanceled: true,
	},
	SubscriptionStatusPastDue: {
		SubscriptionStatusActive:   true,
		SubscriptionStatusCanceled: true,
	},
	SubscriptionStatusCanceled: {},
}

// Subscription is speed's channel-agnostic internal domain concept of a
// tenant's relationship to a Plan
// (docs/internal/06-billing-and-metering.md's core design principle:
// the design doc's core principle that Subscription is an internal domain
// concept and a payment channel is merely the collector). It knows nothing
// about which payment channel, if any, is behind it -- no Stripe
// subscription id, no Alipay order id, nothing gateway-shaped lives on this
// struct. A later round's billing/gateway package drives Status
// transitions from real payment events; this round drives them with a
// plain Go call (Activate/MarkPastDue/Cancel below).
type Subscription struct {
	// ID is an application-generated UUID (uuid.NewString), never a
	// database-generated one -- the backend coding standard forbids
	// gen_random_uuid().
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID method
	// (satisfying dbkit.TenantScoped). ID above is already globally
	// unique (a UUID), so -- exactly like examples/reference-app's Note --
	// TenantModel's own non-primary-key tenant_id column is enough; no
	// composite key is needed the way go/metering's UsageSummary or
	// go/config's row (both keyed by a value that repeats across tenants)
	// require. See TenantModel's own doc comment for the shadowing trap
	// this avoids by not redeclaring TenantID directly.
	dbkit.TenantModel

	// PlanID is the Plan this subscription is on -- an ID reference, not
	// an embedded Plan or a cross-module struct import (backend coding
	// standard: no cross-module foreign keys, ID references plus domain
	// events only). Entitlements.Check loads the referenced Plan fresh
	// on every call, so an in-place edit to that Plan's Grants (an
	// Update through PlanStore) takes effect immediately, with no cache
	// to invalidate for THIS package's own read path -- see AGENTS.md's
	// "Immediate effect" section for what this does and does not cover.
	PlanID string `gorm:"column:plan_id;size:36;not null"`

	// Status is a SubscriptionStatus value.
	Status string `gorm:"column:status;size:16;not null"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime;not null"`
}

// TableName pins Subscription to the billing_subscriptions table.
func (Subscription) TableName() string { return billingSubscriptionsTable }

// SubscriptionRepository is the tenant-scoped accessor for
// billing_subscriptions. Subscription is tenant data, so this embeds
// dbkit.Repository[Subscription] and inherits all three tenant-isolation
// layers, exactly like every other tenant-owned repository in this
// codebase.
type SubscriptionRepository struct {
	*dbkit.Repository[Subscription]
}

// NewSubscriptionRepository returns a SubscriptionRepository over db. db is
// expected to come from dbkit.Open with this module's migrations applied.
func NewSubscriptionRepository(db *gorm.DB) *SubscriptionRepository {
	return &SubscriptionRepository{Repository: dbkit.NewRepository[Subscription](db)}
}

// SubscriptionService drives Subscription's lifecycle and answers "what is
// this tenant's active subscription" for EntitlementsService.
type SubscriptionService struct {
	repo   *SubscriptionRepository
	plans  *PlanStore
	events pkgcore.EventBus
}

// NewSubscriptionService returns a SubscriptionService over repo, validating
// CreateInput.PlanID against plans (see Create's own doc comment), and
// publishing status-transition events on bus. bus may be nil, in which case
// transitions publish nothing (used by tests that do not wire an
// EventBus) -- Module.Register attaches the real bus (see module.go).
func NewSubscriptionService(repo *SubscriptionRepository, plans *PlanStore, bus pkgcore.EventBus) *SubscriptionService {
	return &SubscriptionService{repo: repo, plans: plans, events: bus}
}

// CreateInput names a new Subscription's starting shape. It is always
// created at SubscriptionStatusCreated; use Activate to move it live.
type CreateInput struct {
	PlanID string
}

// Create inserts a new Subscription for the tenant in ctx, at
// SubscriptionStatusCreated.
//
// in.PlanID must resolve, through plans, to a Plan the calling tenant may
// actually subscribe to: either a platform-wide Plan (Plan.IsPlatformWide)
// or a tenant-custom Plan owned by the ctx tenant itself. Plan is a
// dual-domain table reached through a plain PlanStore with no ambient
// tenant filter of its own (Plan's own doc comment) -- nothing else in the
// write path stops a caller-supplied PlanID from naming another tenant's
// private, negotiated Plan, and EntitlementsService.Check would then
// silently apply that other tenant's Grants/quota limits to this tenant.
// A PlanID that fails this check -- because it does not exist at all, or
// exists but names a Plan outside the ctx tenant's own scope -- is refused
// with the same ErrPlanNotFound either way, so a caller cannot use this
// call to probe for another tenant's Plan ids.
func (s *SubscriptionService) Create(ctx context.Context, in CreateInput) (*Subscription, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	plan, err := s.plans.Get(ctx, in.PlanID)
	if err != nil {
		return nil, err
	}
	if !plan.IsPlatformWide() && plan.TenantID != string(tenant) {
		// Exists, but not visible to this tenant -- answer identically to
		// "does not exist" (ErrPlanNotFound), never a distinguishing
		// Forbidden, so this cannot be used to enumerate other tenants'
		// Plan ids.
		return nil, ErrPlanNotFound.WithParam("id", in.PlanID)
	}

	sub := &Subscription{
		ID:     uuid.NewString(),
		PlanID: in.PlanID,
		Status: string(SubscriptionStatusCreated),
	}
	if err := s.repo.Create(ctx, sub); err != nil {
		return nil, fmt.Errorf("billing: create subscription: %w", err)
	}
	return sub, nil
}

// Get returns the Subscription with the given id, for the tenant in ctx.
func (s *SubscriptionService) Get(ctx context.Context, id string) (*Subscription, error) {
	sub, err := s.repo.FindByID(ctx, id)
	if err != nil {
		if isDBKitNotFound(err) {
			return nil, ErrSubscriptionNotFound.WithParam("id", id)
		}
		return nil, err
	}
	return sub, nil
}

// Active returns the tenant's SubscriptionStatusActive subscription, or
// (nil, nil) when it has none. This round assumes at most one active
// subscription per tenant -- see AGENTS.md's Known limitations for what a
// genuine multi-subscription tenant would need instead.
func (s *SubscriptionService) Active(ctx context.Context) (*Subscription, error) {
	subs, err := s.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("billing: list subscriptions: %w", err)
	}
	for i := range subs {
		if subs[i].Status == string(SubscriptionStatusActive) {
			return &subs[i], nil
		}
	}
	return nil, nil
}

// Activate transitions id to SubscriptionStatusActive. Legal from Created
// or PastDue.
func (s *SubscriptionService) Activate(ctx context.Context, id string) (*Subscription, error) {
	return s.transition(ctx, id, SubscriptionStatusActive)
}

// MarkPastDue transitions id to SubscriptionStatusPastDue. Legal from
// Active.
func (s *SubscriptionService) MarkPastDue(ctx context.Context, id string) (*Subscription, error) {
	return s.transition(ctx, id, SubscriptionStatusPastDue)
}

// Cancel transitions id to SubscriptionStatusCanceled, a terminal status.
// Legal from Created, Active or PastDue.
func (s *SubscriptionService) Cancel(ctx context.Context, id string) (*Subscription, error) {
	return s.transition(ctx, id, SubscriptionStatusCanceled)
}

// transition validates and applies one lifecycle move, then publishes
// EventSubscriptionStatusChanged best-effort -- the status change itself
// has already committed by that point, matching the identical best-effort
// convention go/metering's own Aggregator.publishOverageCrossed documents.
func (s *SubscriptionService) transition(ctx context.Context, id string, to SubscriptionStatus) (*Subscription, error) {
	sub, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	from := SubscriptionStatus(sub.Status)
	if !subscriptionTransitions[from][to] {
		return nil, ErrInvalidSubscriptionTransition.
			WithParam("from", string(from)).
			WithParam("to", string(to))
	}
	sub.Status = string(to)
	if err := s.repo.Update(ctx, sub); err != nil {
		return nil, fmt.Errorf("billing: update subscription %q: %w", id, err)
	}
	s.publishStatusChanged(ctx, sub, from, to)
	return sub, nil
}

func (s *SubscriptionService) publishStatusChanged(ctx context.Context, sub *Subscription, from, to SubscriptionStatus) {
	if s.events == nil {
		return
	}
	_ = s.events.Publish(ctx, pkgcore.Event{
		Type:     EventSubscriptionStatusChanged,
		TenantID: pkgcore.TenantID(sub.TenantID),
		Payload: SubscriptionStatusChangedEvent{
			SubscriptionID: sub.ID,
			TenantID:       sub.TenantID,
			PlanID:         sub.PlanID,
			FromStatus:     string(from),
			ToStatus:       string(to),
		},
	})
}

// isDBKitNotFound reports whether err is dbkit's ErrRecordNotFound, the
// sentinel dbkit.Repository[T].FindByID/Update/Delete return for "no row
// for this id under this tenant". Matched by Code through hasCode, not
// errors.Is or == against the sentinel var, since WithParam/WithCause
// derive a new *apperr.Error rather than mutating the receiver -- see
// errors.go's own doc comment.
func isDBKitNotFound(err error) bool {
	return hasCode(err, dbkit.ErrRecordNotFound.Code)
}
