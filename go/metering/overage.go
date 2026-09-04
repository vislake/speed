package metering

import (
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// EventOverageThresholdCrossed is the pkgcore.Event.Type Aggregator.Ingest
// publishes the moment a tenant's real-time counter for a feature first
// reaches or exceeds a configured threshold within the current period --
// docs/internal/06-billing-and-metering.md's overage-policy bullet: a
// threshold event is published on the event bus, for the notification
// module to subscribe to. metering publishes the signal only; it does not
// import go/notification (this codebase's events-not-imports rule for
// cross-module notification, root CLAUDE.md's "Notifications are
// event-driven" trap) and it does not block or otherwise enforce the
// crossing itself -- Plan.Grants.OverageMode's Block/AllowAndBill/Notify
// decision is go/billing's, once it exists, to make from this event and
// its own domain model.
//
// Its Payload is an OverageThresholdCrossedEvent.
const EventOverageThresholdCrossed = "metering.overage_threshold.crossed"

// eventOverageThresholdCrossedPayloadType names OverageThresholdCrossedEvent
// for pkgcore.EventDecl.PayloadType.
const eventOverageThresholdCrossedPayloadType = "metering.OverageThresholdCrossedEvent"

// overageEventDecl is the catalog entry Module.Register declares on
// reg.Events.
var overageEventDecl = pkgcore.EventDecl{
	Type:        EventOverageThresholdCrossed,
	PayloadType: eventOverageThresholdCrossedPayloadType,
	Description: "A tenant's real-time usage counter for a feature reached or exceeded its configured overage threshold within the current period.",
}

// OverageThresholdCrossedEvent is the concrete type carried in the
// pkgcore.Event.Payload of every EventOverageThresholdCrossed event.
type OverageThresholdCrossedEvent struct {
	// TenantID is the tenant whose counter crossed.
	TenantID string
	// Feature is the dimension that crossed, the same vocabulary as
	// UsageEvent.Feature.
	Feature string
	// Threshold is the configured limit that was crossed.
	Threshold float64
	// Quantity is the real-time counter's value at the moment of
	// crossing -- greater than or equal to Threshold, and, because this
	// event fires only on the edge (the first event that reaches or
	// exceeds the threshold within a period, never every event
	// afterwards), the smallest value the counter held while at or above
	// Threshold.
	Quantity float64
	// PeriodStart and PeriodEnd bound the calendar bucket the crossing
	// happened within.
	PeriodStart time.Time
	PeriodEnd   time.Time
	// OccurredAt is when the crossing was detected.
	OccurredAt time.Time
}

// OverageThresholds is the per-feature (or default) limit Aggregator
// checks a real-time counter against after every Ingest. It is a Go-level
// value a host supplies via Module's WithOverageThresholds option, not a
// live read of the ConfigDefaultOverageThreshold config item Module.Register
// declares -- see AGENTS.md's "Overage thresholds: declared config schema,
// Go-level values" section for why, mirroring go/pki's own identical,
// documented simplification for its CA/certificate validity periods
// (go/pki/AGENTS.md: "Register only declares the schema... wiring these
// declared config keys into that decision is left to the host, or to a
// later round").
type OverageThresholds struct {
	// Default is the limit applied to any feature with no entry of its own
	// in PerFeature. A nil Default means a feature with no PerFeature entry
	// has no threshold at all -- Aggregator never publishes an overage
	// event for it.
	Default *float64
	// PerFeature overrides Default for specific feature keys.
	PerFeature map[string]float64
}

// resolve returns the threshold that applies to feature and whether one
// applies at all.
func (o OverageThresholds) resolve(feature string) (float64, bool) {
	if v, ok := o.PerFeature[feature]; ok {
		return v, true
	}
	if o.Default != nil {
		return *o.Default, true
	}
	return 0, false
}
