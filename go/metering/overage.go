package metering

import (
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// EventOverageThresholdCrossed is the pkgcore.Event.Type Aggregator.Ingest
// publishes the moment a tenant's real-time counter for a feature first
// reaches or exceeds a configured threshold within the current period --
// the overage signal, published on the event bus as a threshold event.
// metering publishes the signal only: it does not import go/notification
// (notification delivery is the host's wiring concern, not a metering
// import) and it does not block or otherwise enforce the crossing itself
// -- the Block/AllowAndBill/Notify decision on Plan.Grants.OverageMode is
// go/billing's, to make from this event and its own domain model.
// go/billing is this module's first real consumer, but its relationship
// to THIS event is comment-only so far: billing/model.go's
// OverageModeNotify and billing/entitlements.go name it as the signal
// those modes rely on, while no code in either module subscribes to it --
// a host whose overage mode needs the event delivered wires that
// subscription itself (the compile-asserted consumption runs the other
// way, billing reading RealtimeCount through its UsageReader seam).
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
// declares: the declaration is schema-only, the same simplification
// go/pki's module doc records for its own validity periods, and no code
// path reads either item through config.Service.
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
