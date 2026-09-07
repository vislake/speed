package metering

import (
	"context"
	"sync"
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

// TestAnalyticsRecorder_Stop_DeliversEventsBufferedAtStopTime pins the
// recorder's "only lost when full, and counted" promise across the
// shutdown boundary: events sitting in the buffer when Stop is called are
// delivered into the aggregator before Stop returns (drained by Stop
// itself, since the flush goroutine may already be exiting and must never
// be the only deliverer), rather than silently vanishing with Dropped()
// none the wiser. Before the fix Stop simply returned, leaving every
// buffered event unaccounted for.
func TestAnalyticsRecorder_Stop_DeliversEventsBufferedAtStopTime(t *testing.T) {
	agg := newTestAggregator(t)
	r := NewAnalyticsRecorder(agg)
	r.events = make(chan UsageEvent, 8) // roomy buffer, and the flush loop is never Started, so nothing drains it but Stop itself

	at := time.Now()
	total := 0.0
	for i := 0; i < 3; i++ {
		q := float64(i + 2)
		total += q
		event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: q, IdempotencyKey: idem(i), OccurredAt: at}
		if err := r.Record(context.Background(), event); err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}

	r.Stop()

	got, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != total {
		t.Errorf("RealtimeCount after Stop = %v, want %v (every buffered event delivered by Stop, none silently lost)", got, total)
	}
	if dropped := r.Dropped(); dropped != 0 {
		t.Errorf("Dropped() = %d, want 0 (nothing overflowed; the buffer was drained, not dropped)", dropped)
	}
}

// TestAnalyticsRecorder_Record_AfterStop_DropsAndCounts pins the other
// half of the shutdown contract: once Stop has been called, a Record can
// no longer be buffered for delivery (Stop's drain has already run or is
// about to), so it is dropped and counted exactly like a full-buffer drop
// -- an event recorded into a stopped recorder is never silently
// buffered into oblivion.
func TestAnalyticsRecorder_Record_AfterStop_DropsAndCounts(t *testing.T) {
	agg := newTestAggregator(t)
	r := NewAnalyticsRecorder(agg)

	at := time.Now()
	if err := r.Record(context.Background(), UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-before", OccurredAt: at}); err != nil {
		t.Fatalf("Record(before Stop): %v", err)
	}
	r.Stop() // delivers the buffered event, then latches the recorder closed

	for i := 0; i < 2; i++ {
		if err := r.Record(context.Background(), UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: idem(i), OccurredAt: at}); err != nil {
			t.Fatalf("Record(after Stop, %d): %v", i, err)
		}
	}

	if dropped := r.Dropped(); dropped != 2 {
		t.Errorf("Dropped() = %d, want 2 (post-Stop Records are counted drops, never silent buffer enqueues)", dropped)
	}
	got, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 1 {
		t.Errorf("RealtimeCount = %v, want 1 (only the pre-Stop event was delivered)", got)
	}
}

// TestAnalyticsRecorder_Stop_BeforeStart_DoesNotPreventStoppingALaterLoop
// pins the lifecycle finding: an early Stop (before any Start) consumed
// the stop signal, so a loop Started afterwards could never be stopped
// and the later Stop blocked forever on the never-closed done channel --
// a goroutine leak plus a hang. Stop before Start must leave a later
// Start's loop fully stoppable.
func TestAnalyticsRecorder_Stop_BeforeStart_DoesNotPreventStoppingALaterLoop(t *testing.T) {
	agg := newTestAggregator(t)
	r := NewAnalyticsRecorder(agg)
	r.Stop() // before Start -- must not consume the ability to stop a later loop

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)

	at := time.Now()
	if err := r.Record(context.Background(), UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-lifecycle", OccurredAt: at}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		r.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after a Start that followed an earlier Stop: the started loop can never be stopped")
	}

	got, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 1 {
		t.Errorf("RealtimeCount = %v, want 1 (the event recorded while the loop ran was delivered, by the loop or by Stop's own drain)", got)
	}
}

// TestAnalyticsRecorder_ConcurrentStartAndStop_NoDataRace drives Start
// and Stop from racing goroutines -- one Start racing two Stops, so a
// Stop can also land while another Stop is mid-wait and a Start has
// already replaced the loop generation. Before the fix the two
// sync.Once critical sections wrote and read the stop/done fields
// without any synchronization between them, which the race detector can
// see when the calls actually overlap. After the fix every lifecycle
// field is guarded by the lifecycle mutex (or passed to the goroutine by
// value), and a Stop only clears the started flag for the generation it
// actually waited on, so any interleaving is race-free and every order
// converges.
func TestAnalyticsRecorder_ConcurrentStartAndStop_NoDataRace(t *testing.T) {
	agg := newTestAggregator(t)
	for i := 0; i < 10; i++ {
		r := NewAnalyticsRecorder(agg)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			r.Start(ctx)
		}()
		go func() {
			defer wg.Done()
			r.Stop()
		}()
		go func() {
			defer wg.Done()
			r.Stop()
		}()
		wg.Wait()
		cancel()
		r.Stop() // whichever order the race resolved in, this returns and stops any started loop
	}
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

// TestAnalyticsRecorder_IngestFailure_CountsIntoDropped is the
// P2-metering-11 regression: a buffered event whose delivery into the
// aggregator fails (an Ingest error) is a lost event exactly like a
// full-buffer drop -- it will never reach the summary row or the
// real-time counter -- but deliver() used to only log it, leaving
// Dropped() at zero while events vanished. Delivery failures now count
// into the same counter the explicit drops do, so a host's drop metric
// tells the whole truth about the fail-open tier. The failure is
// injected deterministically with no database involved: an aggregator
// whose period bucket is misconfigured refuses every Ingest with
// ErrInvalidPeriodBucket after validation passes.
func TestAnalyticsRecorder_IngestFailure_CountsIntoDropped(t *testing.T) {
	agg := newTestAggregator(t)
	agg.bucket = "not-a-period-bucket" // validate passes; periodBounds refuses
	r := NewAnalyticsRecorder(agg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer r.Stop()

	at := time.Now()
	if err := r.Record(context.Background(), UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-failing", OccurredAt: at}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// The flush loop delivers, Ingest fails, and the failure must be
	// counted. Pre-fix the event vanished with Dropped() still at 0, so
	// this wait times out.
	waitFor(t, func() bool { return r.Dropped() == 1 })
}

// TestAnalyticsRecorder_CancelThenStart_RestartsTheLoopAndDeliversBuffered
// is the P3-metering-14 regression: when the flush loop exits because its
// ctx was canceled -- not because Stop closed the stop channel -- the
// started flag used to stay set forever, so a later Start was a permanent
// no-op and every Record after the cancel was silently stuffed into a
// buffer nothing would ever drain. run now clears the started flag for
// its own loop generation on exit (without setting the stopped latch), so
// a canceled ctx leaves Start restartable and events recorded during the
// gap are buffered honestly -- delivered by the fresh loop, counted drops
// never, silence never.
func TestAnalyticsRecorder_CancelThenStart_RestartsTheLoopAndDeliversBuffered(t *testing.T) {
	agg := newTestAggregator(t)
	r := NewAnalyticsRecorder(agg)

	ctx1, cancel1 := context.WithCancel(context.Background())
	r.Start(ctx1)
	cancel1()

	// Wait for the canceled loop to actually exit. Pre-fix this never
	// happens: the started flag stays set forever, so waitFor fails here
	// (the defect the finding names -- Start after a cancel-driven stop
	// was a permanent no-op and Record buffered into nothing).
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return !r.started
	})

	// A Record during the dead gap is buffered, not dropped: the stopped
	// latch was not set by the cancel, so the event is honestly awaiting
	// the next loop.
	at := time.Now()
	if err := r.Record(context.Background(), UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 3, IdempotencyKey: "idem-gap", OccurredAt: at}); err != nil {
		t.Fatalf("Record(during the dead gap): %v", err)
	}
	if dropped := r.Dropped(); dropped != 0 {
		t.Fatalf("Dropped() during the dead gap = %d, want 0 (the event was buffered, not dropped)", dropped)
	}

	// Start must run a fresh loop, and the fresh loop must deliver the
	// event recorded during the gap.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	r.Start(ctx2)
	waitFor(t, func() bool {
		got, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
		return err == nil && got == 3
	})
	if dropped := r.Dropped(); dropped != 0 {
		t.Errorf("Dropped() = %d, want 0 (the buffered event was delivered by the restarted loop, never dropped)", dropped)
	}
}
