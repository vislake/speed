package metering

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

func TestAnalyticsRecorder_Record_InvalidEvent_ReturnsValidationError(t *testing.T) {
	r := NewAnalyticsRecorder(newTestAggregator(t))
	err := r.Record(context.Background(), UsageEvent{})
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrMissingTenantID.Code {
		t.Fatalf("Record(invalid event) = %v, want %s", err, ErrMissingTenantID.Code)
	}
}

// TestAnalyticsRecorder_Record_FlushesIntoTheAggregator drives one event
// through the real background flush loop (Start/Stop), proving Record
// really reaches Aggregator.Ingest asynchronously rather than only
// buffering.
func TestAnalyticsRecorder_Record_FlushesIntoTheAggregator(t *testing.T) {
	agg := newTestAggregator(t)
	r := NewAnalyticsRecorder(agg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer r.Stop()

	at := time.Now()
	if err := r.Record(context.Background(), UsageEvent{
		TenantID: "tenant-a", Feature: "ai.generation", Quantity: 4,
		IdempotencyKey: "idem-1", OccurredAt: at,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	waitFor(t, func() bool {
		got, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
		return err == nil && got == 4
	})
}

// TestAnalyticsRecorder_Record_FullBuffer_DropsRatherThanBlocks is the
// core fail-open contract: once the channel is full, Record returns
// immediately with a nil error rather than blocking the caller, and
// Dropped() counts the drop.
func TestAnalyticsRecorder_Record_FullBuffer_DropsRatherThanBlocks(t *testing.T) {
	agg := newTestAggregator(t)
	r := NewAnalyticsRecorder(agg)
	r.events = make(chan UsageEvent, 1) // tiny buffer, and the flush loop is never Started, so it never drains.

	ctx := context.Background()
	if err := r.Record(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-1"}); err != nil {
		t.Fatalf("Record(1st, fills the buffer): %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- r.Record(ctx, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-2"})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Record(2nd, over capacity) = %v, want nil (dropped, not errored)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on a full buffer instead of dropping")
	}

	if got := r.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1", got)
	}
}

func TestAnalyticsRecorder_StartStop_IsIdempotent(t *testing.T) {
	r := NewAnalyticsRecorder(newTestAggregator(t))
	ctx := context.Background()
	r.Start(ctx)
	r.Start(ctx) // must not panic or deadlock
	r.Stop()
	r.Stop() // must not panic or deadlock
}

func TestAnalyticsRecorder_Stop_BeforeStart_IsSafe(t *testing.T) {
	r := NewAnalyticsRecorder(newTestAggregator(t))
	r.Stop() // must not block or panic
}

// waitFor polls cond until it reports true or the test times out, the
// same small helper go/pkgcore/eventbustest's own conformance suite uses
// for async delivery.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition was never satisfied before the deadline")
	}
}
