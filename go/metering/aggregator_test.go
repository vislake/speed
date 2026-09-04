package metering

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
