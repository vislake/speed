package metering

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/metering/internal/testutil"
	"github.com/vislake/speed/go/metering/migrations"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// capturedEvents records every pkgcore.Event a subscribed handler sees, the
// same minimal test double go/config's own service_test.go uses against
// pkgcore.NewMemoryEventBus() -- whose Publish is synchronous, so no wait
// is needed between a Publish call and reading c.events.
type capturedEvents struct {
	events []pkgcore.Event
}

func (c *capturedEvents) handler(_ context.Context, evt pkgcore.Event) error {
	c.events = append(c.events, evt)
	return nil
}

func newTestAggregator(t *testing.T) *Aggregator {
	t.Helper()
	return NewAggregator(NewSummaryRepository(newTestDB(t)))
}

func TestAggregator_Ingest_IncrementsRealtimeCounter(t *testing.T) {
	agg := newTestAggregator(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 2, IdempotencyKey: idem(i), OccurredAt: at}
		if err := agg.Ingest(ctx, event); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}

	got, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 6 {
		t.Errorf("RealtimeCount = %v, want 6", got)
	}
}

func TestAggregator_RealtimeCount_UnknownBucket_ReturnsZero(t *testing.T) {
	agg := newTestAggregator(t)
	got, err := agg.RealtimeCount("tenant-a", "never-recorded", time.Now())
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 0 {
		t.Errorf("RealtimeCount(never recorded) = %v, want 0", got)
	}
}

// TestAggregator_Ingest_DifferentPeriodsDoNotShareACounter proves
// realtimeKey embedding the period start actually isolates one calendar
// bucket's counter from the next -- a new month starts a fresh counter
// rather than continuing the previous one's running total.
func TestAggregator_Ingest_DifferentPeriodsDoNotShareACounter(t *testing.T) {
	agg := newTestAggregator(t)
	ctx := context.Background()

	sept := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)

	if err := agg.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 5, IdempotencyKey: "idem-sept", OccurredAt: sept}); err != nil {
		t.Fatalf("Ingest(sept): %v", err)
	}
	if err := agg.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 3, IdempotencyKey: "idem-oct", OccurredAt: oct}); err != nil {
		t.Fatalf("Ingest(oct): %v", err)
	}

	gotSept, err := agg.RealtimeCount("tenant-a", "ai.generation", sept)
	if err != nil {
		t.Fatalf("RealtimeCount(sept): %v", err)
	}
	if gotSept != 5 {
		t.Errorf("RealtimeCount(sept) = %v, want 5", gotSept)
	}
	gotOct, err := agg.RealtimeCount("tenant-a", "ai.generation", oct)
	if err != nil {
		t.Fatalf("RealtimeCount(oct): %v", err)
	}
	if gotOct != 3 {
		t.Errorf("RealtimeCount(oct) = %v, want 3", gotOct)
	}
}

// TestAggregator_Ingest_SeparatorInTenantOrFeature_KeepsBucketsDistinct
// pins the separator-ambiguity hazard of the real-time counter key: the
// key must not be a concatenation of its segments around an unescaped
// "|". That encoding is safe for the durable summary id -- there the
// tenant rides in its own primary-key column (see summaryID's doc
// comment) -- but the real-time counter map is flat, so its key carries
// the tenant in-band, and tenantID and feature are both variable-length
// values neither this module nor the layers beneath it restrict against
// "|" (UsageEvent.validate bounds length only; pkgcore.TenantID is an
// unrestricted string). Whenever either segment contains the separator,
// the boundary shifts: the two distinct buckets ("a", "b|c") and ("a|b",
// "c") would both concatenate to "a|b|c|" + the period, sharing one
// counter entry -- each bucket's RealtimeCount would answer with the
// other bucket's quantity folded in too, a silent misattribution across
// (tenant, feature) boundaries in the billing-grade counter. Each bucket
// must keep its own running total.
func TestAggregator_Ingest_SeparatorInTenantOrFeature_KeepsBucketsDistinct(t *testing.T) {
	agg := newTestAggregator(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	if err := agg.Ingest(ctx, UsageEvent{TenantID: "a", Feature: "b|c", Quantity: 1, IdempotencyKey: "idem-bucket-1", OccurredAt: at}); err != nil {
		t.Fatalf("Ingest((a, b|c)): %v", err)
	}
	if err := agg.Ingest(ctx, UsageEvent{TenantID: "a|b", Feature: "c", Quantity: 2, IdempotencyKey: "idem-bucket-2", OccurredAt: at}); err != nil {
		t.Fatalf("Ingest((a|b, c)): %v", err)
	}

	if n := lenCounters(t, agg); n != 2 {
		t.Errorf("resident counter entries = %d, want 2 -- pre-fix the two distinct (tenant, feature) buckets shared one entry", n)
	}

	got, err := agg.RealtimeCount("a", "b|c", at)
	if err != nil {
		t.Fatalf("RealtimeCount(a, b|c): %v", err)
	}
	if got != 1 {
		t.Errorf("RealtimeCount(a, b|c) = %v, want 1 -- the (a, b|c) bucket must not absorb the (a|b, c) bucket's quantity", got)
	}
	got, err = agg.RealtimeCount("a|b", "c", at)
	if err != nil {
		t.Fatalf("RealtimeCount(a|b, c): %v", err)
	}
	if got != 2 {
		t.Errorf("RealtimeCount(a|b, c) = %v, want 2 -- the (a|b, c) bucket must not absorb the (a, b|c) bucket's quantity", got)
	}
}

func TestAggregator_Ingest_UpsertsSummaryRow(t *testing.T) {
	summaries := NewSummaryRepository(newTestDB(t))
	agg := NewAggregator(summaries)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 2, IdempotencyKey: idem(i), OccurredAt: at}
		if err := agg.Ingest(ctx, event); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}

	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	start, end, err := periodBounds(at, defaultPeriodBucket)
	if err != nil {
		t.Fatalf("periodBounds: %v", err)
	}
	got, err := summaries.FindByID(tenantCtx, summaryID("ai.generation", start))
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Quantity != 6 {
		t.Errorf("summary Quantity = %v, want 6", got.Quantity)
	}
	if !got.PeriodEnd.Equal(end) {
		t.Errorf("summary PeriodEnd = %v, want %v", got.PeriodEnd, end)
	}
}

func TestAggregator_Ingest_InvalidEvent_ReturnsValidationError(t *testing.T) {
	agg := newTestAggregator(t)
	err := agg.Ingest(context.Background(), UsageEvent{})
	if err == nil {
		t.Fatal("Ingest(invalid event) = nil error, want a validation error")
	}
}

// TestAggregator_Ingest_ConcurrentSameKey_NoLostUpdates is the -race
// concurrency proof this codebase's testing standard requires for a
// metering counter: many goroutines incrementing the same (tenant,
// feature, period) key concurrently must not lose a single increment,
// in either the real-time counter or the database summary row.
func TestAggregator_Ingest_ConcurrentSameKey_NoLostUpdates(t *testing.T) {
	agg := newTestAggregator(t)
	at := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = agg.Ingest(context.Background(), UsageEvent{
				TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1,
				IdempotencyKey: idem(i), OccurredAt: at,
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}

	gotRealtime, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if gotRealtime != n {
		t.Errorf("RealtimeCount = %v, want %d", gotRealtime, n)
	}

	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-a")
	start, _, err := periodBounds(at, defaultPeriodBucket)
	if err != nil {
		t.Fatalf("periodBounds: %v", err)
	}
	summary, err := agg.summaries.FindByID(tenantCtx, summaryID("ai.generation", start))
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if summary.Quantity != n {
		t.Errorf("summary Quantity = %v, want %d", summary.Quantity, n)
	}
}

// TestAggregator_Ingest_PublishesOverageEventOnlyOnce proves the edge-
// triggered contract: the event that first reaches the threshold
// publishes EventOverageThresholdCrossed, and every subsequent event
// within the same period does not publish a second one.
func TestAggregator_Ingest_PublishesOverageEventOnlyOnce(t *testing.T) {
	agg := newTestAggregator(t)
	threshold := 5.0
	agg.thresholds = OverageThresholds{Default: &threshold}
	bus := pkgcore.NewMemoryEventBus()
	var captured capturedEvents
	bus.Subscribe(EventOverageThresholdCrossed, captured.handler)
	agg.bus = bus

	ctx := context.Background()
	at := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

	// Three events of quantity 2 each: 2, 4, 6 -- the third crosses the
	// threshold of 5.
	for i := 0; i < 3; i++ {
		if err := agg.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 2, IdempotencyKey: idem(i), OccurredAt: at}); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}
	// A fourth event, still within the same period, must not publish a
	// second crossing event.
	if err := agg.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: idem(3), OccurredAt: at}); err != nil {
		t.Fatalf("Ingest(3): %v", err)
	}

	if len(captured.events) != 1 {
		t.Fatalf("published %d overage event(s), want exactly 1", len(captured.events))
	}
	payload, ok := captured.events[0].Payload.(OverageThresholdCrossedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want OverageThresholdCrossedEvent", captured.events[0].Payload)
	}
	if payload.Quantity != 6 {
		t.Errorf("payload.Quantity = %v, want 6 (the value at the moment of crossing)", payload.Quantity)
	}
	if payload.Threshold != threshold {
		t.Errorf("payload.Threshold = %v, want %v", payload.Threshold, threshold)
	}
	if payload.TenantID != "tenant-a" || payload.Feature != "ai.generation" {
		t.Errorf("payload = %+v, want TenantID=tenant-a Feature=ai.generation", payload)
	}
}

func TestAggregator_Ingest_NoThresholdConfigured_NeverPublishes(t *testing.T) {
	agg := newTestAggregator(t)
	bus := pkgcore.NewMemoryEventBus()
	var captured capturedEvents
	bus.Subscribe(EventOverageThresholdCrossed, captured.handler)
	agg.bus = bus

	if err := agg.Ingest(context.Background(), UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1000, IdempotencyKey: "idem-1"}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(captured.events) != 0 {
		t.Errorf("published %d overage event(s) with no threshold configured, want 0", len(captured.events))
	}
}

func TestAggregator_Ingest_PerFeatureThresholdOverridesDefault(t *testing.T) {
	agg := newTestAggregator(t)
	defaultThreshold := 100.0
	agg.thresholds = OverageThresholds{
		Default:    &defaultThreshold,
		PerFeature: map[string]float64{"ai.generation": 2},
	}
	bus := pkgcore.NewMemoryEventBus()
	var captured capturedEvents
	bus.Subscribe(EventOverageThresholdCrossed, captured.handler)
	agg.bus = bus

	if err := agg.Ingest(context.Background(), UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 3, IdempotencyKey: "idem-1"}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(captured.events) != 1 {
		t.Fatalf("published %d overage event(s), want 1 (per-feature threshold of 2 was crossed by quantity 3)", len(captured.events))
	}
}

// TestAggregator_Ingest_OverageBusPublishFailure_DoesNotFailIngest proves
// the "best-effort" contract Ingest's own doc comment promises: a usage
// measurement that has already committed must not be reported as failed
// merely because the secondary overage-notification publish failed. The
// second half of the test copies the property assertions of
// TestAggregator_Ingest_SummaryWriteFailure_DoesNotSilentlyLoseOverage
// onto the publish-failure side -- the overage-latch invariant: the
// notifiedOverage latch must be set only AFTER the publish succeeds. A
// latch set before an unconfirmed publish would be consumed by a publish
// failure, and no later fold in the same period could ever fire the
// crossing again. What that would lose is not a log line but the trigger
// go/billing's OverageModeNotify (billing/model.go) is built on. The
// latch is set only once the publish has succeeded; a failed publish
// leaves the latch open, so the next fold that finds the bucket still
// above the threshold is the crossing event again and retries the
// delivery -- the signal must not be lost to a transient publish failure,
// any more than to a transient summary-write failure.
func TestAggregator_Ingest_OverageBusPublishFailure_DoesNotFailIngest(t *testing.T) {
	agg := newTestAggregator(t)
	threshold := 1.0
	agg.thresholds = OverageThresholds{Default: &threshold}
	bus := pkgcore.NewMemoryEventBus()
	var captured capturedEvents
	bus.Subscribe(EventOverageThresholdCrossed, captured.handler)
	agg.bus = failingEventBus{}

	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	// Event A is the crossing event (quantity 5, threshold 1) -- but its
	// publish fails. Ingest must still succeed: the usage measurement
	// itself has committed, and failing the call over a secondary signal
	// would make real, durable usage look lost.
	err := agg.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 5, IdempotencyKey: "idem-pubfail-a", OccurredAt: at})
	if err != nil {
		t.Fatalf("Ingest = %v, want nil even though the overage publish failed", err)
	}
	got, rtErr := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if rtErr != nil {
		t.Fatalf("RealtimeCount: %v", rtErr)
	}
	if got != 5 {
		t.Errorf("RealtimeCount = %v, want 5 (the measurement itself must still have landed)", got)
	}

	// The bus recovers. Event B, still within the same period, folds the
	// bucket to 6 -- still above the threshold -- so it must now be the
	// crossing event: a failed publish leaves the latch open (the latch is
	// set only once a publish succeeds), so the overage signal cannot be
	// lost for the rest of the period to a transient publish failure.
	agg.bus = bus
	if err = agg.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-pubfail-b", OccurredAt: at}); err != nil {
		t.Fatalf("Ingest(after the bus recovered): %v", err)
	}

	if len(captured.events) != 1 {
		t.Fatalf("published %d overage event(s), want exactly 1: the overage signal must not be lost to a transient publish failure", len(captured.events))
	}
	payload, ok := captured.events[0].Payload.(OverageThresholdCrossedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want OverageThresholdCrossedEvent", captured.events[0].Payload)
	}
	if payload.Quantity != 6 {
		t.Errorf("payload.Quantity = %v, want 6 (event B's fold was the retried crossing)", payload.Quantity)
	}
	if payload.Threshold != threshold {
		t.Errorf("payload.Threshold = %v, want %v", payload.Threshold, threshold)
	}

	// Reconciliation: both events' measurements landed, and exactly one
	// overage signal was delivered -- on the retry, once the bus was back.
	got, rtErr = agg.RealtimeCount("tenant-a", "ai.generation", at)
	if rtErr != nil {
		t.Fatalf("RealtimeCount: %v", rtErr)
	}
	if got != 6 {
		t.Errorf("RealtimeCount = %v, want 6 (both events counted)", got)
	}
}

// TestAggregator_Ingest_SummaryWriteFailure_DoesNotSilentlyLoseOverage
// pins the persist-then-count ordering of the overage path: a crossing
// event whose UsageSummary write fails must leave the notifiedOverage
// latch and the real-time counter untouched, and Ingest must refuse the
// event entirely -- a fold that latched or incremented before surfacing
// its persistence error would lose the crossing for the whole period:
// every later event's crossed check would come back false (the latch
// already set) while the in-memory counter held a delta the database
// never received. Ingest persists the summary row before touching the
// counter, mirroring IngestBillingGrade's own persist-then-count order,
// so the next successful crossing event in the same period still
// publishes EventOverageThresholdCrossed, and real-time counter and
// summary row agree on what was actually accepted.
func TestAggregator_Ingest_SummaryWriteFailure_DoesNotSilentlyLoseOverage(t *testing.T) {
	db := newTestDB(t)
	agg := NewAggregator(NewSummaryRepository(db))
	threshold := 5.0
	agg.thresholds = OverageThresholds{Default: &threshold}
	bus := pkgcore.NewMemoryEventBus()
	var captured capturedEvents
	bus.Subscribe(EventOverageThresholdCrossed, captured.handler)
	agg.bus = bus

	// Arm a one-shot failure on the next summary-row insert, the same
	// GORM-callback fault injection go/admin's own failingSingleRowTenantDB
	// uses: the callback fires for the very next Create on this db -- the
	// first event's summary upsert, whose FindByID finds no row yet, so the
	// Create branch runs -- fails it, and disarms itself.
	failNextSummaryCreate := true
	const cbName = "metering_test:fail_next_summary_create"
	if err := db.Callback().Create().Before("gorm:create").Register(cbName, func(tx *gorm.DB) {
		if !failNextSummaryCreate {
			return
		}
		failNextSummaryCreate = false
		tx.Error = errors.New("forced usage_summaries create failure (test)")
	}); err != nil {
		t.Fatalf("register one-shot create failure callback: %v", err)
	}
	t.Cleanup(func() { db.Callback().Create().Remove(cbName) })

	ctx := context.Background()
	at := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

	// Event A would cross the threshold of 5 all on its own -- but its
	// summary write fails, so Ingest must refuse it entirely.
	err := agg.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 5, IdempotencyKey: idem(0), OccurredAt: at})
	if err == nil {
		t.Fatal("Ingest(crossing event with forced summary failure) = nil error, want the persistence failure to surface")
	}

	// Event B, still within the same period, must now be the crossing
	// event: A's failed write left the counter and the latch untouched.
	if err = agg.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 5, IdempotencyKey: idem(1), OccurredAt: at}); err != nil {
		t.Fatalf("Ingest(after the failed write): %v", err)
	}

	if len(captured.events) != 1 {
		t.Fatalf("published %d overage event(s), want exactly 1: the overage signal must not be lost to a transient summary-write failure", len(captured.events))
	}
	payload, ok := captured.events[0].Payload.(OverageThresholdCrossedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want OverageThresholdCrossedEvent", captured.events[0].Payload)
	}
	if payload.Quantity != 5 {
		t.Errorf("payload.Quantity = %v, want 5 (event B alone crossed)", payload.Quantity)
	}

	// Reconciliation: event A was refused, so only B's 5 may appear
	// anywhere -- in the real-time counter and in the summary row alike.
	gotRealtime, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if gotRealtime != 5 {
		t.Errorf("RealtimeCount = %v, want 5 (only the successfully persisted event counted)", gotRealtime)
	}
	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	start, _, err := periodBounds(at, defaultPeriodBucket)
	if err != nil {
		t.Fatalf("periodBounds: %v", err)
	}
	summary, err := agg.summaries.FindByID(tenantCtx, summaryID("ai.generation", start))
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if summary.Quantity != 5 {
		t.Errorf("summary Quantity = %v, want 5 (event A's failed write left no delta behind)", summary.Quantity)
	}
}

// TestAggregator_IngestBillingGrade_UpsertsSummaryAndRealtimeCounter proves
// IngestBillingGrade's happy path behaves exactly like Ingest: one call
// folds the event into both the real-time counter and the persisted
// UsageSummary row.
func TestAggregator_IngestBillingGrade_UpsertsSummaryAndRealtimeCounter(t *testing.T) {
	summaries := NewSummaryRepository(newTestDB(t))
	agg := NewAggregator(summaries)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 4, IdempotencyKey: "idem-billing-1", OccurredAt: at}
	if err := agg.IngestBillingGrade(ctx, event); err != nil {
		t.Fatalf("IngestBillingGrade: %v", err)
	}

	gotRealtime, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if gotRealtime != 4 {
		t.Errorf("RealtimeCount = %v, want 4", gotRealtime)
	}

	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	start, _, err := periodBounds(at, defaultPeriodBucket)
	if err != nil {
		t.Fatalf("periodBounds: %v", err)
	}
	summary, err := summaries.FindByID(tenantCtx, summaryID("ai.generation", start))
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if summary.Quantity != 4 {
		t.Errorf("summary Quantity = %v, want 4", summary.Quantity)
	}
}

// TestAggregator_IngestBillingGrade_RedeliveredEvent_DoesNotDoubleCount is
// the regression proof for the crash-recovery double-count bug
// IngestReceipt exists to close: Dispatcher.deliverOne's own doc comment
// (and IngestReceipt's) describes a process crash, or a merely transient
// failure of markOutboxDelivered, landing between a successful delivery's
// aggregation commit and the outbox row's own mark-delivered write --
// which leaves the row "pending" and gets it redelivered by the next
// Dispatcher.RunOnce cycle, calling the ingest path a SECOND time for the
// identical event. This test reproduces that redelivery directly at the
// Aggregator level: the SAME UsageEvent is handed to IngestBillingGrade
// twice, standing in for "the outbox row was reclaimed and redelivered
// after its first attempt already committed" -- and proves the second call
// is a safe no-op rather than a second application, in both the
// persisted UsageSummary row and the in-process real-time counter.
func TestAggregator_IngestBillingGrade_RedeliveredEvent_DoesNotDoubleCount(t *testing.T) {
	summaries := NewSummaryRepository(newTestDB(t))
	agg := NewAggregator(summaries)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 7, IdempotencyKey: "idem-redelivered", OccurredAt: at}

	if err := agg.IngestBillingGrade(ctx, event); err != nil {
		t.Fatalf("IngestBillingGrade (first delivery): %v", err)
	}
	// The redelivery: same event, same IdempotencyKey, standing in for the
	// next Dispatcher.RunOnce cycle reclaiming the still-"pending" outbox
	// row after the first delivery's own mark-delivered write failed or
	// the process crashed.
	if err := agg.IngestBillingGrade(ctx, event); err != nil {
		t.Fatalf("IngestBillingGrade (redelivery): %v", err)
	}

	gotRealtime, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if gotRealtime != 7 {
		t.Errorf("RealtimeCount after redelivery = %v, want 7 (exactly one application, not 14)", gotRealtime)
	}

	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	start, _, err := periodBounds(at, defaultPeriodBucket)
	if err != nil {
		t.Fatalf("periodBounds: %v", err)
	}
	summary, err := summaries.FindByID(tenantCtx, summaryID("ai.generation", start))
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if summary.Quantity != 7 {
		t.Errorf("summary Quantity after redelivery = %v, want 7 (exactly one application, not 14)", summary.Quantity)
	}
}

// TestAggregator_IngestBillingGrade_RedeliveredEvent_DoesNotRepublishOverage
// proves the redelivery no-op extends to the overage-crossing side effect
// too: a threshold that was already reported crossed by the first,
// genuine delivery must not fire a second EventOverageThresholdCrossed
// merely because the same event was redelivered.
func TestAggregator_IngestBillingGrade_RedeliveredEvent_DoesNotRepublishOverage(t *testing.T) {
	agg := newTestAggregator(t)
	threshold := 5.0
	agg.thresholds = OverageThresholds{Default: &threshold}
	bus := pkgcore.NewMemoryEventBus()
	var captured capturedEvents
	bus.Subscribe(EventOverageThresholdCrossed, captured.handler)
	agg.bus = bus

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 7, IdempotencyKey: "idem-overage-redelivered"}
	if err := agg.IngestBillingGrade(context.Background(), event); err != nil {
		t.Fatalf("IngestBillingGrade (first delivery): %v", err)
	}
	if err := agg.IngestBillingGrade(context.Background(), event); err != nil {
		t.Fatalf("IngestBillingGrade (redelivery): %v", err)
	}

	if len(captured.events) != 1 {
		t.Fatalf("published %d overage event(s) across delivery+redelivery, want exactly 1", len(captured.events))
	}
}

// TestAggregator_IngestBillingGrade_DifferentEvents_BothApply proves the
// idempotency check is keyed by IdempotencyKey, not merely "has this
// tenant/feature ever been ingested": two genuinely different events for
// the same (tenant, feature, period) must both apply.
func TestAggregator_IngestBillingGrade_DifferentEvents_BothApply(t *testing.T) {
	agg := newTestAggregator(t)
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	if err := agg.IngestBillingGrade(context.Background(), UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 3, IdempotencyKey: "idem-x", OccurredAt: at}); err != nil {
		t.Fatalf("IngestBillingGrade(idem-x): %v", err)
	}
	if err := agg.IngestBillingGrade(context.Background(), UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 4, IdempotencyKey: "idem-y", OccurredAt: at}); err != nil {
		t.Fatalf("IngestBillingGrade(idem-y): %v", err)
	}

	got, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 7 {
		t.Errorf("RealtimeCount = %v, want 7 (both distinct events applied)", got)
	}
}

// TestAggregator_IngestBillingGrade_InvalidEvent_ReturnsValidationError
// mirrors Ingest's own identical proof: validation runs before anything
// else, including the idempotency-receipt insert.
func TestAggregator_IngestBillingGrade_InvalidEvent_ReturnsValidationError(t *testing.T) {
	agg := newTestAggregator(t)
	if err := agg.IngestBillingGrade(context.Background(), UsageEvent{}); err == nil {
		t.Fatal("IngestBillingGrade(invalid event) = nil error, want a validation error")
	}
}

// TestUpsertSummaryTx_ConcurrentSameRow_NoLostUpdate pins the
// summary-fold race: the fold must not be a Go-level read-modify-write
// -- read the existing UsageSummary row, add the delta in memory, write
// the mutated value back -- serialized only by Aggregator's in-process
// mu, a lock two replicas (or, as exercised here, two real database
// connections to one database file) do not share. Two folds of the same
// summary row would each read the pre-other value, and the loser's write
// would clobber the winner's delta: already-metered usage silently lost,
// after the outbox receipt was already committed, with no compensation
// path (see the Aggregator type's "Summary folds are database-arbitrated"
// doc comment).
//
// The fold is therefore ONE database-arbitrated statement -- an
// INSERT ... ON CONFLICT DO UPDATE whose conflict branch does the
// addition server-side (see upsertSummaryTx's own doc comment) -- so the
// database itself serializes concurrent folds of the same row: both
// deltas land whatever the interleaving. The regression drives
// upsertSummaryTx directly, under two real connections (two dbkit.Open
// handles over one temp-file database), with no Aggregator in the picture
// to serialize the calls: leg 1 races two folds against a bucket that has
// no row yet -- a bucket's first touch, where a naive implementation
// would see one of the two concurrent first folds fail (busy or
// duplicate key) or get lost; leg 2 races a batch of folds against the
// row leg 1 created -- where the loser's read-modify-write would either
// fail busy or silently clobber a delta. Both legs must return no error
// and leave the row holding the exact sum.
func TestUpsertSummaryTx_ConcurrentSameRow_NoLostUpdate(t *testing.T) {
	ctx := context.Background()
	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	start, end, err := periodBounds(at, defaultPeriodBucket)
	if err != nil {
		t.Fatalf("periodBounds: %v", err)
	}
	const feature = "ai.generation"

	// Two real connections to one temp-file database: the file is shared,
	// the pools are not, so concurrent folds below always travel through
	// distinct physical connections -- the multi-replica shape the
	// in-process mutex never covered.
	dsn := filepath.Join(t.TempDir(), "metering-upsert-race.sqlite")
	dbA, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("open connection A: %v", err)
	}
	t.Cleanup(func() { sqlDB, _ := dbA.DB(); _ = sqlDB.Close() })
	testutil.Migrate(t, dbA, dbkit.DialectSQLite, moduleName, migrations.FS)
	dbB, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("open connection B: %v", err)
	}
	t.Cleanup(func() { sqlDB, _ := dbB.DB(); _ = sqlDB.Close() })

	// No thresholds are configured in this test, so every fold records a
	// nil OverageThreshold -- the fold-time in-force threshold parameter
	// rides the same statement as the delta (see upsertSummaryTx's doc
	// comment) and is not what this race is about.
	fold := func(db *gorm.DB, delta float64, errs chan<- error) {
		errs <- dbkit.WithTenantSession(tenantCtx, db, func(tx *gorm.DB) error {
			return upsertSummaryTx(tx, feature, start, end, delta, nil)
		})
	}

	// Leg 1: two concurrent first-touch folds of one bucket (no row yet).
	// Both deltas must land.
	leg1 := make(chan error, 2)
	go fold(dbA, 5, leg1)
	go fold(dbB, 7, leg1)
	for i := 0; i < 2; i++ {
		if foldErr := <-leg1; foldErr != nil {
			t.Fatalf("first-touch fold: %v -- pre-fix one of the two concurrent first folds failed or was lost", foldErr)
		}
	}

	// Leg 2: a batch of concurrent folds against the row leg 1 created,
	// alternating connections. Every fold must land on top of the others'.
	const racers = 10
	dbs := []*gorm.DB{dbA, dbB}
	leg2 := make(chan error, racers)
	for i := 0; i < racers; i++ {
		go fold(dbs[i%2], 1, leg2)
	}
	for i := 0; i < racers; i++ {
		if foldErr := <-leg2; foldErr != nil {
			t.Fatalf("accumulating fold %d: %v -- pre-fix a concurrent fold failed busy or was lost", i, foldErr)
		}
	}

	summaries := NewSummaryRepository(dbA)
	summary, err := summaries.FindByID(tenantCtx, summaryID(feature, start))
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	want := float64(5 + 7 + racers)
	if summary.Quantity != want {
		t.Errorf("summary Quantity = %v, want %v: every concurrent fold must land, whatever the interleaving (pre-fix the loser's read-modify-write dropped its delta)", summary.Quantity, want)
	}
}

// idem returns a distinct idempotency key for test index i.
func idem(i int) string { return "idem-" + string(rune('a'+i)) }

// failingEventBus is a pkgcore.EventBus whose Publish always fails, used
// only to prove Ingest's best-effort overage-publish contract.
type failingEventBus struct{}

func (failingEventBus) Publish(context.Context, pkgcore.Event) error {
	return errors.New("simulated bus failure")
}
func (failingEventBus) Subscribe(string, pkgcore.EventHandler) {}

var _ pkgcore.EventBus = failingEventBus{}

// TestAggregator_RealtimeCount_AfterRestart_ReflectsSummaryHistory pins
// the restart-reconstruction contract in its read form: the real-time
// counters are in-process state, so a fresh Aggregator over the same
// database (a mid-period restart) must never answer RealtimeCount from an
// empty map -- zero until enough new events arrived -- under-reporting
// the period's history to every quota check and dashboard read that runs
// before the first post-restart event. ensureSeeded reconstructs a
// counter from its bucket's durable UsageSummary row on the key's first
// touch, so the restarted aggregator reflects the full period history
// immediately.
func TestAggregator_RealtimeCount_AfterRestart_ReflectsSummaryHistory(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	first := NewAggregator(NewSummaryRepository(db))
	for i := 0; i < 3; i++ {
		if err := first.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 2, IdempotencyKey: idem(i), OccurredAt: at}); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}

	// The simulated restart: a fresh Aggregator over the SAME database,
	// whose in-process counters start empty.
	restarted := NewAggregator(NewSummaryRepository(db))
	got, err := restarted.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount after restart: %v", err)
	}
	if got != 6 {
		t.Errorf("RealtimeCount after restart = %v, want 6 (the period history reconstructed from the summary row, not 0)", got)
	}

	// A key with no usage at all still answers zero, and a read only ever
	// reconstructs the key it was asked about.
	never, err := restarted.RealtimeCount("tenant-a", "never-recorded", at)
	if err != nil {
		t.Fatalf("RealtimeCount(never recorded): %v", err)
	}
	if never != 0 {
		t.Errorf("RealtimeCount(never recorded) = %v, want 0", never)
	}
}

// TestAggregator_IngestBillingGrade_AfterRestart_RedeliveryAndNewEvents
// pins the alreadyIngested path's interplay with the reconstruction in
// its billing-grade form: after a restart, a redelivered event (its
// receipt and summary fold committed before the "crash") must leave the
// reconstructed counter exactly where the summary says -- no double
// count, no under-count -- and a genuinely new event must then apply on
// top of that reconstructed base. The crash window between a fold's
// commit and its counter increment is closed by the seed read itself:
// the summary row that fold wrote is what the fresh process reconstructs
// from.
func TestAggregator_IngestBillingGrade_AfterRestart_RedeliveryAndNewEvents(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	first := NewAggregator(NewSummaryRepository(db))
	firstEvent := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 7, IdempotencyKey: "idem-crash-window", OccurredAt: at}
	if err := first.IngestBillingGrade(ctx, firstEvent); err != nil {
		t.Fatalf("IngestBillingGrade (first delivery): %v", err)
	}

	// The simulated restart: a fresh Aggregator over the SAME database.
	restarted := NewAggregator(NewSummaryRepository(db))

	// The redelivery (the outbox row was still pending when the process
	// died): its receipt already exists, so the fold is a no-op -- and the
	// reconstructed counter must already hold the event exactly once.
	if err := restarted.IngestBillingGrade(ctx, firstEvent); err != nil {
		t.Fatalf("IngestBillingGrade (redelivery after restart): %v", err)
	}
	got, err := restarted.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 7 {
		t.Errorf("RealtimeCount after redelivery = %v, want 7 (the event applied exactly once across the restart -- pre-fix the counter stayed 0, the fold's delta never reaching it)", got)
	}

	// A genuinely new event applies on top of the reconstructed base.
	secondEvent := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 3, IdempotencyKey: "idem-new-after-restart", OccurredAt: at}
	if err = restarted.IngestBillingGrade(ctx, secondEvent); err != nil {
		t.Fatalf("IngestBillingGrade (new event): %v", err)
	}
	got, err = restarted.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 10 {
		t.Errorf("RealtimeCount after the new event = %v, want 10 (7 reconstructed + 3 applied -- pre-fix only the new event's 3 landed)", got)
	}
}

// TestAggregator_Restart_ReconstructsOverageLatch_NoDoubleFire pins the
// reconstruction's overage half: the notifiedOverage latch is in-process
// state too, so a restarted aggregator must never start with every latch
// open -- a tenant whose durable usage already crossed a threshold
// before the restart must not cross "again" on the first post-restart
// event, publishing a second EventOverageThresholdCrossed for one
// period. The reconstruction latches from the summary row (a quantity at
// or above the threshold means the crossing durably happened), so the
// edge fires exactly once per period whatever the restart count.
func TestAggregator_Restart_ReconstructsOverageLatch_NoDoubleFire(t *testing.T) {
	db := newTestDB(t)
	threshold := 5.0
	bus := pkgcore.NewMemoryEventBus()
	var captured capturedEvents
	bus.Subscribe(EventOverageThresholdCrossed, captured.handler)
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	first := NewAggregator(NewSummaryRepository(db))
	first.thresholds = OverageThresholds{Default: &threshold}
	first.bus = bus
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := first.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 2, IdempotencyKey: idem(i), OccurredAt: at}); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}
	if len(captured.events) != 1 {
		t.Fatalf("first process published %d overage event(s), want exactly 1", len(captured.events))
	}

	// The simulated restart: a fresh Aggregator over the SAME database.
	restarted := NewAggregator(NewSummaryRepository(db))
	restarted.thresholds = OverageThresholds{Default: &threshold}
	restarted.bus = bus

	// The read reconstructs the counter (6, the full period history) --
	// and latches, since the durable summary already meets the threshold.
	got, err := restarted.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount after restart: %v", err)
	}
	if got != 6 {
		t.Errorf("RealtimeCount after restart = %v, want 6 (pre-fix: 0 -- the period history under-reported)", got)
	}

	// A post-restart event large enough to cross from zero must NOT
	// publish a second crossing: the reconstruction latched.
	if err := restarted.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 5, IdempotencyKey: idem(3), OccurredAt: at}); err != nil {
		t.Fatalf("Ingest(after restart): %v", err)
	}
	if len(captured.events) != 1 {
		t.Errorf("published %d overage event(s) across the restart, want exactly 1 (pre-fix the restarted process fired the crossing a second time)", len(captured.events))
	}
}

// TestAggregator_Restart_ThresholdLoweredAcrossRestart_CrossingFires pins
// the threshold-identity qualification of the reconstruction: the
// equivalence "once the durable summary holds a quantity at or above this
// bucket's threshold, the crossing has happened" holds ONLY while the
// threshold is unchanged. The thresholds are a construction-time field
// (module.go's WithOverageThresholds option mutates the Aggregator before
// Bootstrap returns), so changing one -- an operator lowering a limit is
// the routine shape -- requires a restart. After that restart, quantity
// >= threshold is true not because a crossing was ever published but
// because the threshold moved below an already-existing quantity: the
// seed must not latch notifiedOverage on the mere quantity match, or the
// crossing under the new threshold would never fire for the period and
// "the crossing has happened" would be false in that cell. Every fold
// records on the durable summary row the overage threshold that was in
// force at the row's last fold, so the rebuild latches only when the
// recorded threshold IS the current one; a row written under a different
// (here: higher) threshold leaves the latch open and the first
// post-restart fold that finds the bucket at or above the lowered
// threshold is the crossing event.
//
// The test also pins the read-form amplifier of the same rule: a pure
// RealtimeCount after the restart must not latch the open crossing away
// either -- a read reconstructs the quantity but publishes nothing, so
// it must leave the crossing for the next fold.
func TestAggregator_Restart_ThresholdLoweredAcrossRestart_CrossingFires(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	bus := pkgcore.NewMemoryEventBus()
	var captured capturedEvents
	bus.Subscribe(EventOverageThresholdCrossed, captured.handler)

	// First process, threshold 10: three events of 2 each bring the bucket
	// to 6, still below the threshold, so no crossing ever fires or is
	// published -- the summary row holds quantity 6 written under 10.
	high := 10.0
	first := NewAggregator(NewSummaryRepository(db))
	first.thresholds = OverageThresholds{Default: &high}
	first.bus = bus
	for i := 0; i < 3; i++ {
		if err := first.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 2, IdempotencyKey: idem(i), OccurredAt: at}); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}
	if len(captured.events) != 0 {
		t.Fatalf("first process published %d overage event(s) at quantity 6 under threshold 10, want 0", len(captured.events))
	}

	// The operator lowers the threshold to 5 and restarts: a fresh
	// Aggregator over the SAME database, whose counters start empty.
	low := 5.0
	restarted := NewAggregator(NewSummaryRepository(db))
	restarted.thresholds = OverageThresholds{Default: &low}
	restarted.bus = bus

	// The pure read first: it reconstructs the full period history (6) --
	// and must NOT set the overage latch on the quantity match alone, or
	// the crossing under the lowered threshold would be swallowed by a read
	// that published nothing.
	got, err := restarted.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount after restart: %v", err)
	}
	if got != 6 {
		t.Errorf("RealtimeCount after restart = %v, want 6 (the period history reconstructed from the summary row)", got)
	}
	if len(captured.events) != 0 {
		t.Fatalf("a pure read published %d overage event(s), want 0", len(captured.events))
	}

	// The first post-restart event: the bucket (6, soon 7) is already at or
	// above the lowered threshold of 5, and no crossing under 5 has ever
	// fired. The reconstruction must not latch on the quantity match alone
	// -- that would publish nothing and leave the lowered threshold's
	// signal gone for the whole period; the crossing under the
	// recorded-threshold-discerning rebuild fires exactly once.
	if err := restarted.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: idem(3), OccurredAt: at}); err != nil {
		t.Fatalf("Ingest(after restart): %v", err)
	}
	if len(captured.events) != 1 {
		t.Fatalf("published %d overage event(s) across the threshold-lowering restart, want exactly 1 (pre-fix the latch was set on a quantity match alone and the crossing under the lowered threshold never fired)", len(captured.events))
	}
	payload, ok := captured.events[0].Payload.(OverageThresholdCrossedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want OverageThresholdCrossedEvent", captured.events[0].Payload)
	}
	if payload.Threshold != low {
		t.Errorf("payload.Threshold = %v, want %v (the lowered threshold the crossing fired under)", payload.Threshold, low)
	}
	if payload.Quantity != 7 {
		t.Errorf("payload.Quantity = %v, want 7 (the counter value at the post-restart crossing fold)", payload.Quantity)
	}

	// Reconciliation: a further fold within the same period must NOT fire a
	// second crossing -- the post-restart fold above claimed and delivered
	// it, and its latch now stands.
	if err := restarted.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: idem(4), OccurredAt: at}); err != nil {
		t.Fatalf("Ingest(further fold): %v", err)
	}
	if len(captured.events) != 1 {
		t.Errorf("published %d overage event(s) after the crossing fired, want exactly 1 (the edge fires once per period per threshold)", len(captured.events))
	}

	// A second restart under the SAME (lowered) configuration: the
	// post-restart fold above recorded 5 on the row, so the rebuild now
	// attests the current threshold and latches again -- the crossing that
	// already fired must not fire a third time across the two restarts.
	again := NewAggregator(NewSummaryRepository(db))
	again.thresholds = OverageThresholds{Default: &low}
	again.bus = bus
	if _, err := again.RealtimeCount("tenant-a", "ai.generation", at); err != nil {
		t.Fatalf("RealtimeCount after the second restart: %v", err)
	}
	if err := again.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: idem(5), OccurredAt: at}); err != nil {
		t.Fatalf("Ingest(after the second restart): %v", err)
	}
	if len(captured.events) != 1 {
		t.Errorf("published %d overage event(s) across the two restarts, want exactly 1 (a same-configuration restart stays latched once the row attests the current threshold)", len(captured.events))
	}
}

// TestAggregator_Restart_ThresholdConfiguredWhereNoneApplied_CrossingFires
// pins the NULL-record arm of the threshold-identity rule: a row whose
// folds all ran under NO threshold records a nil OverageThreshold -- the
// durable state every unattested row carries -- and a restart that
// CONFIGURES a threshold below the
// existing quantity is the same "new configuration, crossing never
// fired" shape as the lowering case: the seed must not latch on the
// quantity comparison alone, or the newly configured threshold's signal
// would stay silently absent for the whole period. The unattested row
// leaves the latch open, and the first fold that finds the bucket at or
// above the threshold is the crossing event under it.
func TestAggregator_Restart_ThresholdConfiguredWhereNoneApplied_CrossingFires(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	bus := pkgcore.NewMemoryEventBus()
	var captured capturedEvents
	bus.Subscribe(EventOverageThresholdCrossed, captured.handler)

	// First process, no threshold configured at all: three events of 2
	// each bring the bucket to 6, and with no threshold nothing can ever
	// fire or be published.
	first := NewAggregator(NewSummaryRepository(db))
	first.bus = bus
	for i := 0; i < 3; i++ {
		if err := first.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 2, IdempotencyKey: idem(i), OccurredAt: at}); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}
	if len(captured.events) != 0 {
		t.Fatalf("first process published %d overage event(s) with no threshold configured, want 0", len(captured.events))
	}

	// The operator configures a threshold of 5 -- below the existing
	// quantity of 6 -- and restarts. The row's nil record attests nothing
	// about the current configuration, so the first post-restart fold is
	// the crossing event under it.
	threshold := 5.0
	restarted := NewAggregator(NewSummaryRepository(db))
	restarted.thresholds = OverageThresholds{Default: &threshold}
	restarted.bus = bus
	if err := restarted.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: idem(3), OccurredAt: at}); err != nil {
		t.Fatalf("Ingest(after restart): %v", err)
	}
	if len(captured.events) != 1 {
		t.Fatalf("published %d overage event(s) after configuring a threshold below the existing quantity, want exactly 1 (pre-fix the seed latched on the quantity match alone and the newly configured threshold's crossing never fired)", len(captured.events))
	}
	payload, ok := captured.events[0].Payload.(OverageThresholdCrossedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want OverageThresholdCrossedEvent", captured.events[0].Payload)
	}
	if payload.Threshold != threshold {
		t.Errorf("payload.Threshold = %v, want %v", payload.Threshold, threshold)
	}
}

// TestAggregator_RealtimeCount_NilSummariesRepository_ReturnsError pins
// the nil-summaries refusal: RealtimeCount must not answer a counter miss
// on an Aggregator built with no summaries repository (NewAggregator(nil))
// with (0, nil) -- a silent zero -- while zero is a legal, MEANINGFUL
// answer in a quota context (zero usage means certainly within quota).
// "Cannot answer" must not be expressed as an exactly-legal value: the
// read cannot distinguish "this bucket has no row yet" (which zero means)
// from "there is no durable state to reconstruct from at all", so the
// miss returns the coded configuration error
// (metering.usage_summaries_unconfigured), aligned with the vocabulary of
// go/billing's own ErrUsageReaderUnconfigured -- the layer that consumes
// RealtimeCount for quota decisions and refuses the same class of
// inability with a coded error rather than a guessed allowance.
func TestAggregator_RealtimeCount_NilSummariesRepository_ReturnsError(t *testing.T) {
	agg := NewAggregator(nil)
	got, err := agg.RealtimeCount("tenant-a", "ai.generation", time.Now())
	if err == nil {
		t.Fatalf("RealtimeCount over a nil-summaries Aggregator = (%v, nil), want a coded error -- pre-fix the silent zero was an exactly-legal quota answer (zero usage means within quota) for a reader that cannot answer at all", got)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("RealtimeCount error = %v, want an *apperr.Error", err)
	}
	if appErr.Code != "metering.usage_summaries_unconfigured" {
		t.Errorf("RealtimeCount error code = %q, want %q (aligned with billing's usage_reader_unconfigured vocabulary)", appErr.Code, "metering.usage_summaries_unconfigured")
	}
}

// TestAggregator_NilSummariesRepository_IngestPathsRefuse pins the
// nil-summaries refusal on the ingest paths, the counterpart of the
// read-form test above: every operation that needs the summaries
// repository refuses with the coded ErrUsageSummariesUnconfigured, never
// half-works. Without the refusal the two ingest paths would be the crash
// half of the unconfigured object -- their seed would "succeed" seeding
// to zero, the fold would run and die on a bare nil-pointer dereference
// of the repository's connection (upsertSummaryInto's summaries.db on the
// Ingest path, foldIntoSummaryOnce's a.summaries.db on the billing-grade
// one). ensureSeeded refuses an unconfigured construction BEFORE any
// counter entry is created, so both folds short-circuit on the seed's
// error and neither crash point is reachable; and because the refusal
// happens before the map insert, the failed writes leave nothing behind
// for a later read to misreport -- the same bucket answered after them is
// refused afresh with the same coded error, never (0, nil) off an
// unseeded entry.
func TestAggregator_NilSummariesRepository_IngestPathsRefuse(t *testing.T) {
	agg := NewAggregator(nil)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: idem(0), OccurredAt: at}
	assertNilSummariesRefusal(t, "Ingest", agg.Ingest(ctx, event))
	assertNilSummariesRefusal(t, "IngestBillingGrade", agg.IngestBillingGrade(ctx, event))

	// The refused writes above must not have left an unseeded counter
	// entry behind: a read of the same bucket is refused afresh with the
	// same coded error, never answered (0, nil).
	got, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err == nil {
		t.Fatalf("RealtimeCount after the refused ingests = (%v, nil), want the coded error -- an unseeded entry a failed seed left behind must not surface as a silent zero", got)
	}
}

// assertNilSummariesRefusal is the shared assertion both ingest-path
// refusals above check: a non-nil error that is the coded
// metering.usage_summaries_unconfigured, never a nil error and never a
// bare nil-pointer panic on the nil repository's connection field.
func assertNilSummariesRefusal(t *testing.T, path string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s over a nil-summaries Aggregator succeeded, want the coded ErrUsageSummariesUnconfigured -- pre-closure the fold crashed on the nil repository with a nil-pointer dereference", path)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("%s over a nil-summaries Aggregator error = %v, want an *apperr.Error", path, err)
	}
	if appErr.Code != "metering.usage_summaries_unconfigured" {
		t.Errorf("%s over a nil-summaries Aggregator error code = %q, want %q", path, appErr.Code, "metering.usage_summaries_unconfigured")
	}
}

// TestAggregator_Ingest_ExpiredPeriodEntriesAreEvicted pins the
// expired-period eviction: counter entries live per
// (tenant, feature, period), and periods end -- without eviction the
// in-process map would keep every period's entries for the process
// lifetime, growing without bound. Ingest and IngestBillingGrade sweep,
// at most once per period boundary crossed, every resident entry whose
// period predates the event's own (sweepExpiredCountersLocked), so
// advancing past a period retires the old entries instead of letting
// them sit forever.
func TestAggregator_Ingest_ExpiredPeriodEntriesAreEvicted(t *testing.T) {
	agg := newTestAggregator(t)
	agg.bucket = PeriodBucketDaily
	ctx := context.Background()
	day1 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	// Two buckets on day 1: two distinct (tenant, feature) pairs, to make
	// the sweep retire more than one entry.
	for i := 0; i < 3; i++ {
		if err := agg.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: idem(i), OccurredAt: day1}); err != nil {
			t.Fatalf("Ingest(day1 a/%d): %v", i, err)
		}
	}
	if err := agg.Ingest(ctx, UsageEvent{TenantID: "tenant-b", Feature: "api.calls", Quantity: 1, IdempotencyKey: "idem-b-day1", OccurredAt: day1}); err != nil {
		t.Fatalf("Ingest(day1 b): %v", err)
	}
	if n := lenCounters(t, agg); n != 2 {
		t.Fatalf("resident entries during day 1 = %d, want 2", n)
	}

	// The first event of day 2 crosses the period boundary: the sweep
	// retires both day-1 entries, leaving only the new day-2 entry.
	if err := agg.Ingest(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-day2", OccurredAt: day2}); err != nil {
		t.Fatalf("Ingest(day2): %v", err)
	}
	n := lenCounters(t, agg)
	if n != 1 {
		t.Errorf("resident entries after the day-2 ingest = %d, want 1 -- pre-fix the day-1 entries stayed resident forever (permanent, unbounded residency)", n)
	}
	day2Start := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	if _, ok := agg.counters.Load(realtimeKey("tenant-a", "ai.generation", day2Start)); !ok {
		t.Errorf("the day-2 entry itself is missing after the sweep")
	}
}

// lenCounters returns how many entries the aggregator's counter map
// currently holds. Test-only: the map is package-internal, and the count
// is read only when no other goroutine is mutating the aggregator.
func lenCounters(t *testing.T, agg *Aggregator) int {
	t.Helper()
	n := 0
	agg.counters.Range(func(_, _ any) bool { n++; return true })
	return n
}
