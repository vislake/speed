package metering

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
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
// merely because the secondary overage-notification publish failed.
func TestAggregator_Ingest_OverageBusPublishFailure_DoesNotFailIngest(t *testing.T) {
	agg := newTestAggregator(t)
	threshold := 1.0
	agg.thresholds = OverageThresholds{Default: &threshold}
	agg.bus = failingEventBus{}

	err := agg.Ingest(context.Background(), UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 5, IdempotencyKey: "idem-1"})
	if err != nil {
		t.Fatalf("Ingest = %v, want nil even though the overage publish failed", err)
	}
	got, rtErr := agg.RealtimeCount("tenant-a", "ai.generation", time.Now())
	if rtErr != nil {
		t.Fatalf("RealtimeCount: %v", rtErr)
	}
	if got != 5 {
		t.Errorf("RealtimeCount = %v, want 5 (the measurement itself must still have landed)", got)
	}
}

// TestAggregator_Ingest_SummaryWriteFailure_DoesNotSilentlyLoseOverage is
// the regression proof for the overage-latch ordering bug: the event that
// first crosses a configured threshold is also the event whose UsageSummary
// write fails, and the old count-then-persist order had already latched
// notifiedOverage and incremented the real-time counter before Ingest
// returned the persistence error. The crossing was never published, and no
// later event in the same period could publish it either -- the latch was
// already set, so every subsequent event's crossed came back false while
// the in-memory counter held a delta the database never received. The fix
// persists the summary row before touching the counter, mirroring
// IngestBillingGrade's own persist-then-count order: a failed write leaves
// counter and latch untouched, so the next successful crossing event in
// the same period still publishes EventOverageThresholdCrossed, and
// real-time counter and summary row agree on what was actually accepted.
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

// TestAggregator_RealtimeCount_AfterRestart_ReflectsSummaryHistory is the
// P2-metering-12 regression in its read form: the real-time counters are
// in-process state, so a fresh Aggregator over the same database (a
// mid-period restart) used to answer RealtimeCount from an empty map --
// zero until enough new events arrived -- under-reporting the period's
// history to every quota check and dashboard read that runs before the
// first post-restart event. ensureSeeded reconstructs a counter from its
// bucket's durable UsageSummary row on the key's first touch, so the
// restarted aggregator reflects the full period history immediately.
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
// is the P2-metering-12 regression in its billing-grade form, pinning the
// alreadyIngested path's interplay with the reconstruction: after a
// restart, a redelivered event (its receipt and summary fold committed
// before the "crash") must leave the reconstructed counter exactly where
// the summary says -- no double count, no under-count -- and a genuinely
// new event must then apply on top of that reconstructed base. The crash
// window between a fold's commit and its counter increment is closed by
// the seed read itself: the summary row that fold wrote is what the
// fresh process reconstructs from.
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

// TestAggregator_Restart_ReconstructsOverageLatch_NoDoubleFire is the
// P2-metering-12 regression for the overage half: the notifiedOverage
// latch is in-process state too, so a restarted aggregator used to start
// with every latch open -- and a tenant whose durable usage already
// crossed a threshold before the restart crossed "again" on the first
// post-restart event, publishing a second EventOverageThresholdCrossed
// for one period. The reconstruction latches from the summary row (a
// quantity at or above the threshold means the crossing durably
// happened), so the edge fires exactly once per period whatever the
// restart count.
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

// TestAggregator_Ingest_ExpiredPeriodEntriesAreEvicted is the
// P2-metering-13 regression: counter entries live per
// (tenant, feature, period), and periods end -- without eviction the
// in-process map kept every period's entries for the process lifetime,
// growing without bound. Ingest and IngestBillingGrade sweep, at most
// once per period boundary crossed, every resident entry whose period
// predates the event's own (sweepExpiredCountersLocked), so advancing
// past a period retires the old entries instead of letting them sit
// forever.
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
