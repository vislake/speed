package metering

import (
	"context"
	"sync"
	"time"

	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// counterEntry is one real-time quota counter: the running Quantity within
// one (tenant, feature, period) bucket, guarded by its own mutex so
// concurrent Ingest calls for the same key never lose an increment.
// notifiedOverage latches once EventOverageThresholdCrossed has fired for
// this bucket, so a threshold that stays crossed for the rest of the
// period publishes exactly one event rather than one per subsequent
// event -- resetting only implicitly, when a new period's counterEntry is
// created fresh (see realtimeKey embedding the period start in its key).
type counterEntry struct {
	mu              sync.Mutex
	quantity        float64
	notifiedOverage bool
}

// Aggregator is the in-process aggregation backend
// (docs/internal/06-billing-and-metering.md's backend-comparison table,
// in-process implementation column): the one place both reliability tiers
// (AnalyticsRecorder's flush loop and Dispatcher's delivery loop) funnel
// measured usage into. It maintains:
//
//   - A real-time quota counter per (tenant, feature, period), held in an
//     in-process sync.Map -- RealtimeCount reads it back.
//   - A database-backed UsageSummary row per (tenant, feature, period),
//     upserted through SummaryRepository.
//   - The overage-threshold check that publishes
//     EventOverageThresholdCrossed on the wired pkgcore.EventBus.
//
// # Summary writes are serialized by a single process-wide mutex
//
// dbkit.Repository[T]'s Create/FindByID/Update each open their OWN
// transaction (dbkit.WithTenantSession), so a naive
// "FindByID, then Create-or-Update" sequence run without external
// coordination is a lost-update race under concurrent Ingest calls for
// the same (tenant, feature, period) key. Aggregator closes that race
// with mu: every summary read-modify-write runs under it, so at most one
// goroutine performs the sequence at a time, in this process. That is a
// deliberate, round-1 simplification, not an oversight -- this round
// ships the in-process aggregation backend only
// (docs/internal/06-billing-and-metering.md's own "MVP now, split into its
// own container once volume grows" framing), so "this process" is the
// whole deployment. A Redis- or
// PostgreSQL-backed aggregation backend (a later round, per AGENTS.md's
// Known limitations) would replace this mutex with a real atomic upsert
// (Redis INCRBYFLOAT, or a PostgreSQL INSERT ... ON CONFLICT) that holds
// across processes; it does not need to reuse this one.
//
// # Real-time counters are exact; summary rows are eventually applied
//
// The in-process sync.Map counter is updated synchronously and atomically
// under counterEntry's own mutex before Ingest returns, so RealtimeCount
// always reflects every event Ingest has accepted. The database summary
// row is updated in the same call, under mu -- both effects land together,
// there is no separate async flush for the summary half within
// Aggregator itself (any staleness in a caller's view of usage comes from
// where in AnalyticsRecorder's or Dispatcher's own queue an event is
// sitting, not from Aggregator internals).
type Aggregator struct {
	summaries *SummaryRepository

	bucket     string
	thresholds OverageThresholds
	bus        pkgcore.EventBus

	counters sync.Map // string -> *counterEntry
	mu       sync.Mutex
}

// NewAggregator returns an Aggregator over summaries, with the default
// period bucket (PeriodBucketMonthly) and no overage thresholds
// configured. Module wires the period bucket and thresholds a host
// selected via its own Option functions, which mutate the fields below
// directly (same package, see module.go).
func NewAggregator(summaries *SummaryRepository) *Aggregator {
	return &Aggregator{
		summaries: summaries,
		bucket:    defaultPeriodBucket,
	}
}

// realtimeKey identifies one counterEntry. It embeds periodStart (not just
// tenant and feature) so a new calendar period gets a fresh zero-valued
// counter automatically, with no explicit reset logic needed anywhere --
// LoadOrStore below simply finds nothing for the new key and creates one.
func realtimeKey(tenantID, feature string, periodStart time.Time) string {
	return tenantID + "|" + feature + "|" + periodStart.UTC().Format(time.RFC3339)
}

// Ingest is the one place both reliability tiers deliver a validated
// UsageEvent into: it increments the real-time counter, upserts the
// database summary row, and -- on the event that first crosses a
// configured overage threshold within the current period -- publishes
// EventOverageThresholdCrossed. ctx need not carry a tenant (Ingest builds
// its own tenant context from event.TenantID before touching
// SummaryRepository, since both callers are asynchronous paths that
// cannot assume ctx carries the original request's tenant -- see this
// codebase's "workers do not inherit tenant context" trap).
//
// A zero event.OccurredAt is treated as time.Now().
//
// Publishing the overage event, if one fires, is best-effort: a publish
// failure is logged and does NOT fail Ingest. The usage measurement
// itself (the counter increment, the summary row) has already committed
// by that point; failing the whole call over a secondary notification
// signal would make a real, already-durable measurement look like it was
// lost, which is worse than a missed notification.
func (a *Aggregator) Ingest(ctx context.Context, event UsageEvent) error {
	if err := event.validate(); err != nil {
		return err
	}
	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	start, end, err := periodBounds(occurredAt, a.bucket)
	if err != nil {
		return err
	}

	quantity, crossed := a.ingestRealtime(event.TenantID, event.Feature, start, event.Quantity)

	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID(event.TenantID))
	if err := a.upsertSummary(tenantCtx, event.Feature, start, end, event.Quantity); err != nil {
		return err
	}

	if crossed {
		a.publishOverageCrossed(ctx, event, start, end, quantity, occurredAt)
	}
	return nil
}

// ingestRealtime increments the in-process counter for (tenantID, feature,
// periodStart) by delta, returning the counter's new value and whether
// this call is the one that first crossed a configured overage threshold
// within this period.
func (a *Aggregator) ingestRealtime(tenantID, feature string, periodStart time.Time, delta float64) (quantity float64, crossed bool) {
	key := realtimeKey(tenantID, feature, periodStart)
	entryAny, _ := a.counters.LoadOrStore(key, &counterEntry{})
	entry, ok := entryAny.(*counterEntry)
	if !ok {
		// Unreachable: this Aggregator is the only writer of a.counters,
		// and every value it ever stores is a *counterEntry.
		panic("metering: a.counters holds a value that is not a *counterEntry")
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.quantity += delta
	quantity = entry.quantity

	threshold, hasThreshold := a.thresholds.resolve(feature)
	if hasThreshold && !entry.notifiedOverage && quantity >= threshold {
		entry.notifiedOverage = true
		crossed = true
	}
	return quantity, crossed
}

// RealtimeCount returns the real-time counter's current value for
// (tenantID, feature) within the calendar period at contains, or (0, nil)
// when no event has been ingested for that bucket yet.
func (a *Aggregator) RealtimeCount(tenantID, feature string, at time.Time) (float64, error) {
	start, _, err := periodBounds(at, a.bucket)
	if err != nil {
		return 0, err
	}
	v, ok := a.counters.Load(realtimeKey(tenantID, feature, start))
	if !ok {
		return 0, nil
	}
	entry, ok := v.(*counterEntry)
	if !ok {
		// Unreachable: see ingestRealtime's identical guard.
		panic("metering: a.counters holds a value that is not a *counterEntry")
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.quantity, nil
}

// upsertSummary folds delta into the UsageSummary row for (feature,
// start) under tenantCtx's tenant, creating the row on its first event
// within the period. See the Aggregator type's own doc comment for why
// this runs under a.mu.
func (a *Aggregator) upsertSummary(tenantCtx context.Context, feature string, start, end time.Time, delta float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	id := summaryID(feature, start)
	existing, err := a.summaries.FindByID(tenantCtx, id)
	if err != nil {
		if !hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return err
		}
		return a.summaries.Create(tenantCtx, &UsageSummary{
			ID:          id,
			Feature:     feature,
			PeriodStart: start,
			PeriodEnd:   end,
			Quantity:    delta,
		})
	}
	existing.Quantity += delta
	existing.PeriodEnd = end
	return a.summaries.Update(tenantCtx, existing)
}

// publishOverageCrossed publishes an EventOverageThresholdCrossed for
// event, best-effort -- see Ingest's own doc comment for why a publish
// failure here is logged, not returned.
func (a *Aggregator) publishOverageCrossed(ctx context.Context, event UsageEvent, start, end time.Time, quantity float64, occurredAt time.Time) {
	if a.bus == nil {
		return
	}
	threshold, _ := a.thresholds.resolve(event.Feature)
	err := a.bus.Publish(ctx, pkgcore.Event{
		Type:     EventOverageThresholdCrossed,
		TenantID: pkgcore.TenantID(event.TenantID),
		Payload: OverageThresholdCrossedEvent{
			TenantID:    event.TenantID,
			Feature:     event.Feature,
			Threshold:   threshold,
			Quantity:    quantity,
			PeriodStart: start,
			PeriodEnd:   end,
			OccurredAt:  occurredAt,
		},
	})
	if err != nil {
		obs.FromContext(ctx).Warn("metering.overage_event_publish_failed",
			"error", err,
			"tenant_id", event.TenantID,
			"feature", event.Feature,
		)
	}
}
