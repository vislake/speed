package billing

import "github.com/vislake/speed/go/pkgcore"

// Event type names, following the backend coding standard's
// <module>.<entity>.<action> convention.
const (
	// EventPlanChanged is published whenever a Plan's Grants change --
	// created, updated or deleted -- so that any later consumer holding
	// its own cached view of a Plan's grants can invalidate it, matching
	// docs/internal/06-billing-and-metering.md's rule that entitlement
	// changes take effect immediately, broadcast over the event bus. This
	// round's own Entitlements.Check needs no such
	// cache (it loads the Plan fresh on every call -- see Subscription's
	// own doc comment), so nothing in this module subscribes to its own
	// event; it exists for future consumers.
	EventPlanChanged = "billing.plan.changed"

	// EventSubscriptionStatusChanged is published on every Subscription
	// lifecycle transition (SubscriptionService.Activate/MarkPastDue/
	// Cancel).
	EventSubscriptionStatusChanged = "billing.subscription.status_changed"
)

// eventDecls is the catalog entry for each event this module publishes,
// declared in Register.
var eventDecls = []pkgcore.EventDecl{
	{
		Type:        EventPlanChanged,
		PayloadType: "billing.PlanChangedEvent",
		Description: "A Plan's Grants were created, updated or deleted.",
	},
	{
		Type:        EventSubscriptionStatusChanged,
		PayloadType: "billing.SubscriptionStatusChangedEvent",
		Description: "A Subscription moved from one lifecycle status to another.",
	},
}

// PlanChangedEvent is EventPlanChanged's payload.
type PlanChangedEvent struct {
	PlanID   string
	TenantID string // platformScopeSentinel ("") for a platform-wide Plan.
	Key      string
	Action   string // "created", "updated" or "deleted".
}

// SubscriptionStatusChangedEvent is EventSubscriptionStatusChanged's
// payload.
type SubscriptionStatusChangedEvent struct {
	SubscriptionID string
	TenantID       string
	PlanID         string
	FromStatus     string
	ToStatus       string
}
