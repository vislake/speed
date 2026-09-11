package metering

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/testkit"

	"github.com/vislake/speed/go/metering/internal/testutil"
	"github.com/vislake/speed/go/metering/migrations"
)

func newTestDispatcher(t *testing.T) (*Dispatcher, *Aggregator, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	agg := NewAggregator(NewSummaryRepository(db))
	return NewDispatcher(db, agg), agg, db
}

func TestDispatcher_RunOnce_NoRows_IsANoOp(t *testing.T) {
	d, _, _ := newTestDispatcher(t)
	delivered, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}
}

// TestDispatcher_RunOnce_DeliversAnEnqueuedRow drives the outbox pattern's
// full happy path end to end: Enqueue writes a pending row, RunOnce
// delivers it into the aggregator, and the row is marked delivered.
func TestDispatcher_RunOnce_DeliversAnEnqueuedRow(t *testing.T) {
	d, agg, db := newTestDispatcher(t)
	ctx := context.Background()

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 3, IdempotencyKey: "idem-1", OccurredAt: time.Now()}
	if _, err := Enqueue(ctx, db, event); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	delivered, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}

	got, err := agg.RealtimeCount("tenant-a", "ai.generation", event.OccurredAt)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 3 {
		t.Errorf("RealtimeCount after delivery = %v, want 3", got)
	}

	pending, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("still-pending rows after delivery = %d, want 0", len(pending))
	}
}

func TestDispatcher_RunOnce_RespectsBatchSize(t *testing.T) {
	d, _, db := newTestDispatcher(t)
	d.batchSize = 2
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: idem(i), OccurredAt: time.Now()}
		if _, err := Enqueue(ctx, db, event); err != nil {
			t.Fatalf("Enqueue(%d): %v", i, err)
		}
	}

	delivered, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != 2 {
		t.Fatalf("delivered = %d, want 2 (batchSize)", delivered)
	}

	pending, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(pending) != 3 {
		t.Errorf("still-pending rows = %d, want 3 (5 enqueued - 2 delivered)", len(pending))
	}
}

// TestDispatcher_RunOnce_DeliveryFailure_LeavesRowPendingWithAttemptRecorded
// proves the indefinite-retry contract at the single-cycle level: a row
// whose delivery fails is neither dropped nor marked delivered -- it stays
// pending, with Attempts and LastError recorded, ready for the next cycle.
func TestDispatcher_RunOnce_DeliveryFailure_LeavesRowPendingWithAttemptRecorded(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "metering_dispatcher_failure.sqlite")
	db := openAndMigrate(t, dsn) // the dispatcher's own, healthy connection
	brokenConn := closedDB(t, openAndMigrate(t, dsn))
	brokenAggregator := NewAggregator(NewSummaryRepository(brokenConn))
	d := NewDispatcher(db, brokenAggregator)
	ctx := context.Background()

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-1", OccurredAt: time.Now()}
	if _, err := Enqueue(ctx, db, event); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	delivered, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 (the aggregator's own database connection is closed)", delivered)
	}

	// Read the row back directly rather than through the claim query: a
	// failed row sits inside its re-claim window (RetryAfter is in the
	// future), which is exactly what the claim query must refuse to
	// return.
	got, found, err := findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-1")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if !found {
		t.Fatal("the outbox row is gone after the failed delivery -- the row must not be lost")
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got.Attempts)
	}
	if got.LastError == "" {
		t.Error("LastError is empty, want the delivery failure's message")
	}
	if got.RetryAfter == nil || !got.RetryAfter.After(time.Now()) {
		t.Errorf("RetryAfter = %v, want a future moment (the failed row is scheduled for a later re-claim, not left claimable immediately)", got.RetryAfter)
	}
}

// TestDispatcher_CrashMidDelivery_RowIsRecoveredOnTheNextRun is the
// crash-recovery proof: it "kills" the delivery path mid-flight -- an
// Aggregator whose database connection has been closed, simulating a
// process crash between claiming a row and finishing its delivery --
// confirms the outbox row is NOT lost (still present, still pending, in
// the SAME durable table Enqueue wrote it to), and then confirms a
// fresh, healthy Dispatcher recovers and delivers it successfully. This
// is what makes Enqueue's "write, then async deliver" promise real
// rather than aspirational: nothing about a mid-delivery crash can make
// an enqueued event disappear.
func TestDispatcher_CrashMidDelivery_RowIsRecoveredOnTheNextRun(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "metering_dispatcher_crash.sqlite")

	// The dispatcher's own connection: this is what durably holds the
	// outbox bookkeeping (metering_outbox_records), and it stays open and
	// healthy throughout -- a crash in DELIVERY does not imply a crash in
	// the outbox table's own storage.
	dispatcherDB := openAndMigrate(t, dsn)

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 7, IdempotencyKey: "idem-crash", OccurredAt: time.Now()}
	ctx := context.Background()
	enqueued, err := Enqueue(ctx, dispatcherDB, event)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Simulate the delivery path dying mid-flight: a second connection to
	// the SAME file, opened and then immediately killed, standing in for
	// "the process crashed while this aggregator was mid-Ingest".
	deadConn := closedDB(t, openAndMigrate(t, dsn))
	brokenAggregator := NewAggregator(NewSummaryRepository(deadConn))
	crashedDispatcher := NewDispatcher(dispatcherDB, brokenAggregator)

	delivered, err := crashedDispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce (simulated crash): %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d during the simulated crash, want 0", delivered)
	}

	// The event must still be recoverable from the outbox table -- not
	// lost -- exactly as enqueued.
	recovered, found, err := findOutboxByIdempotencyKey(ctx, dispatcherDB, "tenant-a", "idem-crash")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey after the simulated crash: %v", err)
	}
	if !found {
		t.Fatal("the outbox row was lost after the simulated mid-delivery crash")
	}
	if recovered.ID != enqueued.ID || recovered.Status != outboxStatusPending {
		t.Fatalf("recovered row = %+v, want ID=%q Status=%q", recovered, enqueued.ID, outboxStatusPending)
	}
	if recovered.Attempts < 1 {
		t.Errorf("recovered.Attempts = %d, want at least 1 (the failed attempt was recorded)", recovered.Attempts)
	}

	// The crashed attempt scheduled the row's re-claim at the failure time
	// plus the retry delay -- in real time, the poll interval must elapse
	// before the row is claimable again. The recovery dispatcher below
	// stands in for the process restarting after that interval, so the
	// row's retry_after is moved into the past first, deterministically,
	// in place of waiting out the delay.
	backdated := time.Now().Add(-time.Second)
	if updateErr := dispatcherDB.Model(&OutboxRecord{}).Where("id = ?", enqueued.ID).Update("retry_after", backdated).Error; updateErr != nil {
		t.Fatalf("backdate retry_after: %v", updateErr)
	}

	// A fresh, healthy dispatcher -- standing in for the process restarting
	// -- recovers and completes the delivery on its very next run.
	healthyAggregator := NewAggregator(NewSummaryRepository(dispatcherDB))
	recoveredDispatcher := NewDispatcher(dispatcherDB, healthyAggregator)

	delivered, err = recoveredDispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce (recovery): %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d on recovery, want 1", delivered)
	}

	final, found, err := findOutboxByIdempotencyKey(ctx, dispatcherDB, "tenant-a", "idem-crash")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey after recovery: %v", err)
	}
	if !found || final.Status != outboxStatusDelivered {
		t.Fatalf("final row = %+v (found=%v), want Status=%q", final, found, outboxStatusDelivered)
	}

	got, err := healthyAggregator.RealtimeCount("tenant-a", "ai.generation", event.OccurredAt)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 7 {
		t.Errorf("RealtimeCount after recovery = %v, want 7 (the event was delivered exactly once)", got)
	}
}

// TestDispatcher_RunOnce_RedeliveredRow_DoesNotDoubleCount is the
// Dispatcher-level regression proof for the crash-recovery double-count
// bug deliverOne's own doc comment (and IngestReceipt's) describes:
// Aggregator.IngestBillingGrade's own database transaction can commit
// while the SEPARATE markOutboxDelivered write that should follow it
// fails or the process dies -- leaving the outbox row "pending" so the
// next RunOnce cycle reclaims and redelivers it. This test reproduces
// exactly that window without a second broken connection: it calls
// IngestBillingGrade directly (simulating "the first delivery attempt's
// aggregation already committed") while deliberately never calling
// markOutboxDelivered (simulating "...but the write that should have
// retired the row did not"), leaving the row genuinely still pending in
// the database. RunOnce is then driven normally and must complete the
// interrupted delivery (marking the row delivered) WITHOUT re-applying
// the event's quantity a second time.
func TestDispatcher_RunOnce_RedeliveredRow_DoesNotDoubleCount(t *testing.T) {
	d, agg, db := newTestDispatcher(t)
	ctx := context.Background()

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 9, IdempotencyKey: "idem-interrupted", OccurredAt: time.Now()}
	enqueued, err := Enqueue(ctx, db, event)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Simulate "the first delivery attempt's IngestBillingGrade call
	// already committed" by calling it directly -- deliberately skipping
	// markOutboxDelivered, so enqueued stays "pending" exactly as it would
	// after a crash (or a merely transient failure of that write) right
	// after IngestBillingGrade's own transaction landed.
	if ingestErr := agg.IngestBillingGrade(ctx, event); ingestErr != nil {
		t.Fatalf("IngestBillingGrade (simulating the interrupted first attempt): %v", ingestErr)
	}
	stillPending, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(stillPending) != 1 || stillPending[0].ID != enqueued.ID {
		t.Fatalf("outbox rows before RunOnce = %+v, want exactly enqueued row still pending", stillPending)
	}

	// The real recovery path: RunOnce reclaims the still-pending row and
	// redelivers it.
	delivered, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (the interrupted row completes delivery)", delivered)
	}

	final, found, err := findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-interrupted")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if !found || final.Status != outboxStatusDelivered {
		t.Fatalf("final row = %+v (found=%v), want Status=%q", final, found, outboxStatusDelivered)
	}

	gotRealtime, err := agg.RealtimeCount("tenant-a", "ai.generation", event.OccurredAt)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if gotRealtime != 9 {
		t.Errorf("RealtimeCount after redelivery = %v, want 9 (applied exactly once, not 18)", gotRealtime)
	}
}

// openAndMigrate opens a fresh *gorm.DB connection to the SQLite file at
// dsn and applies this module's migrations through it (a no-op if they
// were already applied by an earlier connection to the same file).
func openAndMigrate(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     dsn,
	})
	if err != nil {
		t.Fatalf("dbkit.Open(%q): %v", dsn, err)
	}
	t.Cleanup(func() {
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	testutil.Migrate(t, db, dbkit.DialectSQLite, moduleName, migrations.FS)
	return db
}

// closedDB returns a *gorm.DB over the same connection template as db but
// with its underlying *sql.DB immediately closed, so every subsequent
// call against it fails -- a stand-in for "this connection died".
func closedDB(t *testing.T, db *gorm.DB) *gorm.DB {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	return db
}

func TestDispatcher_StartStop_DrivesRunOnceOnASchedule(t *testing.T) {
	d, agg, db := newTestDispatcher(t)
	d.interval = 10 * time.Millisecond

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-1", OccurredAt: time.Now()}
	if _, err := Enqueue(context.Background(), db, event); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	testkit.EventuallyWithin(t, 2*time.Second, "the realtime counter to count the delivered event", func() bool {
		got, err := agg.RealtimeCount("tenant-a", "ai.generation", event.OccurredAt)
		return err == nil && got == 1
	})
}

func TestDispatcher_StartStop_IsIdempotent(t *testing.T) {
	d, _, _ := newTestDispatcher(t)
	ctx := context.Background()
	d.Start(ctx)
	d.Start(ctx) // must not panic or deadlock
	d.Stop()
	d.Stop() // must not panic or deadlock
}

func TestDispatcher_Stop_BeforeStart_IsSafe(t *testing.T) {
	d, _, _ := newTestDispatcher(t)
	d.Stop() // must not block or panic
}

// TestDispatcher_Stop_BeforeStart_DoesNotPreventStoppingALaterLoop pins
// the stop-signal lifecycle contract in its Dispatcher form: Stop before
// any Start must not consume the ability to stop a later loop -- a Stop
// that consumed the signal would leave the poll loop Started afterwards
// unstoppable, the later Stop blocking forever on the never-closed done
// channel: a goroutine leak plus a hang. Stop before Start leaves a later
// Start's loop fully stoppable.
func TestDispatcher_Stop_BeforeStart_DoesNotPreventStoppingALaterLoop(t *testing.T) {
	d, _, _ := newTestDispatcher(t)
	d.Stop() // before Start -- must not consume the ability to stop a later loop

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	stopped := make(chan struct{})
	go func() {
		d.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after a Start that followed an earlier Stop: the started poll loop can never be stopped")
	}
}

// TestDispatcher_ConcurrentStartAndStop_NoDataRace drives Start and Stop
// from racing goroutines -- one Start racing two Stops, so a Stop can
// also land while another Stop is mid-wait and a Start has already
// replaced the loop generation. Every lifecycle field is guarded by the
// lifecycle mutex (or passed to the goroutine by value) -- never written
// and read without synchronization between the racing goroutines, which
// the race detector would see when the calls actually overlap -- and a
// Stop only clears the started flag for the generation it actually
// waited on, so any interleaving is race-free and every order converges.
func TestDispatcher_ConcurrentStartAndStop_NoDataRace(t *testing.T) {
	for i := 0; i < 10; i++ {
		d, _, _ := newTestDispatcher(t)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			d.Start(ctx)
		}()
		go func() {
			defer wg.Done()
			d.Stop()
		}()
		go func() {
			defer wg.Done()
			d.Stop()
		}()
		wg.Wait()
		cancel()
		d.Stop() // whichever order the race resolved in, this returns and stops any started loop
	}
}

// TestDispatcher_RunOnce_FailedRowsAtTheHead_DoNotStarveNewerRows pins
// the pile half of the claim query's schedule-ordering fairness: rows
// that can never deliver -- here: an empty Feature, which delivery-time
// validation refuses forever, a shape current Enqueue validation never
// produces, so only a corrupt or foreign row can carry it -- fail every
// attempt, and a pile of them at the head of the queue must not keep a
// healthy row enqueued behind them from being claimed. The claim query
// this test pins orders by the re-claim schedule, retry_after (see
// claimPendingOutboxRecords): after one poison cycle the whole pile sits
// inside its retry delay, ineligible, so the healthy row is claimed on
// the very next cycle. Ranking never-failed rows ahead of every
// already-failed row as a strict class would buy the same headway at the
// price of starving failed rows under a flood -- the other half of the
// same fairness contract, pinned by
// TestDispatcher_RunOnce_OnceFailedRow_IsStillRetriedUnderSteadyArrivals.
func TestDispatcher_RunOnce_FailedRowsAtTheHead_DoNotStarveNewerRows(t *testing.T) {
	d, agg, db := newTestDispatcher(t)
	d.batchSize = 50
	ctx := context.Background()

	const poisonCount = 50 // fills exactly one full batch
	poisonAt := time.Now()
	for i := 0; i < poisonCount; i++ {
		rec := newTestOutboxRecord(fmt.Sprintf("poison-%02d", i), "tenant-p", fmt.Sprintf("idem-poison-%02d", i))
		rec.Feature = "" // validation poison: delivery can never succeed
		// Backdate the pile a full hour (both timestamps: RetryAfter is
		// the claim's schedule key, CreatedAt its tiebreak): the fresh
		// row's CreatedAt is the wall clock at Enqueue, so it must be
		// strictly newer than every poison row at any execution speed -- a
		// fast setup would otherwise land it inside the pile's timestamp
		// window and a created_at-only claim (the ordering this test pins
		// against) would deliver it by accident, a false green in plain
		// mode.
		at := poisonAt.Add(-time.Hour).Add(time.Duration(i) * time.Millisecond)
		rec.CreatedAt = at
		rec.RetryAfter = &at
		if _, err := insertOutboxRecord(ctx, db, rec); err != nil {
			t.Fatalf("insertOutboxRecord(poison-%02d): %v", i, err)
		}
	}

	// One full cycle: every poison row fails once and stays pending, now
	// carrying Attempts = 1 and a RetryAfter a retry delay away -- the
	// state a real pile of failing rows has.
	delivered, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce (poison cycle): %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered during the poison cycle = %d, want 0 (every poison row must fail)", delivered)
	}
	poisonRows := pendingRowsForTest(t, ctx, db, poisonCount)
	if len(poisonRows) != poisonCount {
		t.Fatalf("pending rows after the poison cycle = %d, want %d", len(poisonRows), poisonCount)
	}
	for _, rec := range poisonRows {
		if rec.Attempts != 1 {
			t.Fatalf("poison row %s Attempts = %d, want 1", rec.ID, rec.Attempts)
		}
		if rec.RetryAfter == nil || !rec.RetryAfter.After(time.Now()) {
			t.Fatalf("poison row %s RetryAfter = %v, want a future moment (each failed row waits out its re-claim window)", rec.ID, rec.RetryAfter)
		}
	}

	// A fresh, healthy row enqueued behind the pile.
	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 5, IdempotencyKey: "idem-fresh", OccurredAt: time.Now()}
	if _, enqueueErr := Enqueue(ctx, db, event); enqueueErr != nil {
		t.Fatalf("Enqueue: %v", enqueueErr)
	}

	delivered, err = d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce (fresh cycle): %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (the fresh row must be claimed while the %d already-failed head rows wait out their re-claim windows)", delivered, poisonCount)
	}

	got, err := agg.RealtimeCount("tenant-a", "ai.generation", event.OccurredAt)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 5 {
		t.Errorf("RealtimeCount = %v, want 5 (the fresh row was delivered exactly once)", got)
	}

	// The pile is untouched by the fresh cycle: still pending, still on
	// Attempts = 1 -- each row waits out its re-claim window instead of
	// being retried every cycle, and is never dropped.
	remaining := pendingRowsForTest(t, ctx, db, poisonCount)
	if len(remaining) != poisonCount {
		t.Errorf("pending rows after the fresh cycle = %d, want %d (the poison pile is still retried, never dropped)", len(remaining), poisonCount)
	}
	for _, rec := range remaining {
		if rec.Attempts != 1 {
			t.Errorf("poison row %s Attempts = %d, want 1 (no pile row may be retried before its re-claim window -- the retry delay spaces attempts honestly)", rec.ID, rec.Attempts)
		}
	}
}

// pendingRowsForTest reads every pending outbox row directly from the
// table, bypassing the claim query's eligibility filter: the claim query
// exists to select the rows whose re-claim window has arrived, so tests
// asserting on rows still inside their window (a freshly failed pile, a
// row awaiting its first retry) must read the table itself.
func pendingRowsForTest(t *testing.T, ctx context.Context, db *gorm.DB, limit int) []OutboxRecord {
	t.Helper()
	var recs []OutboxRecord
	if err := db.WithContext(ctx).Where("status = ?", outboxStatusPending).Limit(limit).Find(&recs).Error; err != nil {
		t.Fatalf("find pending rows: %v", err)
	}
	return recs
}

// TestDispatcher_RunOnce_OnceFailedRow_IsStillRetriedUnderSteadyArrivals
// pins the anti-starvation property at the Dispatcher level: ranking
// never-failed rows (Attempts 0) as a strict class ahead of every
// already-failed row would, under a sustained enqueue rate -- every
// batch filled with never-failed rows -- leave a row that had failed
// ONCE unclaimed forever: permanent starvation of exactly the rows
// retry exists to reach. Under the schedule ordering (retry_after, see
// claimPendingOutboxRecords) the once-failed row is claimable again the
// moment its re-claim window opens, and because its schedule slot
// predates every row enqueued afterwards, no flood of new arrivals can
// push it out of the batch: it is retried -- and here, recovered -- on
// the very next cycle.
func TestDispatcher_RunOnce_OnceFailedRow_IsStillRetriedUnderSteadyArrivals(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "metering_dispatcher_starvation.sqlite")
	db := openAndMigrate(t, dsn)
	brokenConn := closedDB(t, openAndMigrate(t, dsn))
	ctx := context.Background()

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-once-failed", OccurredAt: time.Now()}
	enqueued, err := Enqueue(ctx, db, event)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Cycle 1: delivery fails once -- a transient hiccup, not a poison
	// row: the aggregator's database connection is closed for this one
	// cycle only, exactly the failure a recovery is meant to retry.
	dBroken := NewDispatcher(db, NewAggregator(NewSummaryRepository(brokenConn)))
	dBroken.batchSize = 5
	delivered, err := dBroken.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce (failure cycle): %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered during the failure cycle = %d, want 0", delivered)
	}

	// The row's re-claim window opens one retry delay after the failure;
	// move its retry_after into the past deterministically, in place of
	// waiting the delay out.
	backdated := time.Now().Add(-time.Second)
	if updateErr := db.Model(&OutboxRecord{}).Where("id = ?", enqueued.ID).Update("retry_after", backdated).Error; updateErr != nil {
		t.Fatalf("backdate retry_after: %v", updateErr)
	}

	// From cycle 2 on, a healthy dispatcher races the once-failed row
	// against a steady flood: every cycle enqueues one full batch of
	// fresh rows before RunOnce claims one batch, so each batch could
	// fill entirely with never-failed rows -- which is exactly what an
	// attempts-class ordering would do, every cycle, forever.
	dHealthy := NewDispatcher(db, NewAggregator(NewSummaryRepository(db)))
	dHealthy.batchSize = 5
	const floodCycles = 10
	for cycle := 0; cycle < floodCycles; cycle++ {
		for i := 0; i < dHealthy.batchSize; i++ {
			fresh := UsageEvent{
				TenantID:       "tenant-a",
				Feature:        "ai.generation",
				Quantity:       1,
				IdempotencyKey: fmt.Sprintf("idem-flood-%02d-%d", cycle, i),
				OccurredAt:     time.Now(),
			}
			if _, enqueueErr := Enqueue(ctx, db, fresh); enqueueErr != nil {
				t.Fatalf("Enqueue(flood %d/%d): %v", cycle, i, enqueueErr)
			}
		}
		delivered, err = dHealthy.RunOnce(ctx)
		if err != nil {
			t.Fatalf("RunOnce (flood cycle %d): %v", cycle, err)
		}
		if delivered != dHealthy.batchSize {
			t.Fatalf("flood cycle %d delivered %d, want %d (every claimed row delivers; the once-failed row recovered on cycle 0)", cycle, delivered, dHealthy.batchSize)
		}
	}

	// The once-failed row: retried on the very first flood cycle and
	// delivered exactly once, its single failed attempt recorded.
	got, found, err := findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-once-failed")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if !found {
		t.Fatal("the once-failed outbox row is gone without ever being delivered")
	}
	if got.Status != outboxStatusDelivered {
		t.Fatalf("once-failed row Status = %q after %d flood cycles, want %q -- the row that failed once must still be reached and delivered under steady new arrivals", got.Status, floodCycles, outboxStatusDelivered)
	}
	if got.Attempts != 1 {
		t.Errorf("once-failed row Attempts = %d, want 1 (one failed attempt, then the recovery delivery)", got.Attempts)
	}
}

// TestDispatcher_RetentionSweep_RetiresDeliveredRowsOlderThanRetention
// pins the retention contract: delivered outbox rows and the ingest
// receipts their deliveries created must not stay on their tables
// forever -- monotonic growth with every delivered event. Dispatcher's
// poll loop runs a bounded retention pass each cycle
// (retireDeliveredOutboxRecords) that retires delivered rows which have
// stayed delivered past the retention window, together with each row's
// receipt, in one transaction. A delivered row younger than the window --
// and every pending row, whose receipt is what makes its redelivery
// idempotent -- is never touched.
func TestDispatcher_RetentionSweep_RetiresDeliveredRowsOlderThanRetention(t *testing.T) {
	d, _, db := newTestDispatcher(t)
	d.interval = 10 * time.Millisecond
	ctx := context.Background()

	const n = 3
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = fmt.Sprintf("idem-retire-%d", i)
		event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: keys[i], OccurredAt: time.Now()}
		if _, err := Enqueue(ctx, db, event); err != nil {
			t.Fatalf("Enqueue(%d): %v", i, err)
		}
	}
	delivered, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != n {
		t.Fatalf("delivered = %d, want %d", delivered, n)
	}

	// Every delivery wrote an ingest receipt (IngestBillingGrade's fold):
	// the guard whose retirement must wait for the row's own retirement.
	receipts := NewIngestReceiptRepository(db)
	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	rows, err := receipts.List(tenantCtx)
	if err != nil {
		t.Fatalf("receipts List: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("receipts after delivery = %d, want %d", len(rows), n)
	}

	// Backdate the delivered rows past the retention window, the
	// deterministic stand-in for the window elapsing.
	past := time.Now().Add(-defaultOutboxRetention - time.Hour)
	if updateErr := db.Model(&OutboxRecord{}).
		Where("status = ?", outboxStatusDelivered).
		Update("delivered_at", past).Error; updateErr != nil {
		t.Fatalf("backdate delivered_at: %v", updateErr)
	}

	// The running poll loop's retention pass retires the rows and their
	// receipts within a few cycles; without the pass the delivered rows
	// would stay on their tables and grow without bound.
	d.Start(ctx)
	defer d.Stop()
	testkit.EventuallyWithin(t, 2*time.Second, "the delivered outbox rows to be retired", func() bool {
		var remaining int64
		if err := db.Model(&OutboxRecord{}).Where("status = ?", outboxStatusDelivered).Count(&remaining).Error; err != nil {
			return false
		}
		return remaining == 0
	})
	testkit.EventuallyWithin(t, 2*time.Second, "the delivered receipts to be retired with them", func() bool {
		rows, err := receipts.List(tenantCtx)
		return err == nil && len(rows) == 0
	})
}

// TestDispatcher_CancelThenStart_RestartsThePollLoop pins the
// cancel-restart contract in its Dispatcher form: when the poll loop
// exits because its ctx was canceled -- not because Stop closed the stop
// channel -- the started flag must not stay set forever, or a later
// Start would be a permanent no-op and the dispatcher would never poll
// again. run clears the started flag for its own loop generation on
// exit, so a canceled ctx leaves Start restartable.
func TestDispatcher_CancelThenStart_RestartsThePollLoop(t *testing.T) {
	d, agg, db := newTestDispatcher(t)
	d.interval = 10 * time.Millisecond

	ctx1, cancel1 := context.WithCancel(context.Background())
	d.Start(ctx1)
	cancel1()

	// Wait for the canceled loop to actually exit: the loop's exit clears
	// the started flag for its own generation (see poll_loop.go), so a
	// canceled ctx leaves Start restartable -- a flag left set forever
	// would make the next Start a permanent no-op.
	testkit.EventuallyWithin(t, 2*time.Second, "the dispatcher's poll loop to stop", func() bool { return !pollLoopStarted(&d.loop) })

	// Start must run a fresh loop that genuinely polls again.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	d.Start(ctx2)

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-restart", OccurredAt: time.Now()}
	if _, err := Enqueue(context.Background(), db, event); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	testkit.EventuallyWithin(t, 2*time.Second, "the realtime counter to count the delivered event", func() bool {
		got, err := agg.RealtimeCount("tenant-a", "ai.generation", event.OccurredAt)
		return err == nil && got == 1
	})
}

// TestDispatcher_StopThenStart_RestartsThePollLoop pins the restart
// contract in its Dispatcher form for a completed Stop (the ctx-canceled
// path is the sibling CancelThenStart regression above): a Start after
// Stop has returned must run a fresh loop that genuinely polls again. A
// stopped generation that left the started flag set would make every
// later Start a permanent no-op, and pending rows would sit unclaimed
// forever.
func TestDispatcher_StopThenStart_RestartsThePollLoop(t *testing.T) {
	d, agg, db := newTestDispatcher(t)
	d.interval = 10 * time.Millisecond

	ctx := context.Background()
	d.Start(ctx)
	d.Stop()

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-stop-restart", OccurredAt: time.Now()}
	if _, err := Enqueue(context.Background(), db, event); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	d.Start(ctx)
	defer d.Stop()
	testkit.EventuallyWithin(t, 2*time.Second, "the realtime counter to count the delivered event", func() bool {
		got, err := agg.RealtimeCount("tenant-a", "ai.generation", event.OccurredAt)
		return err == nil && got == 1
	})
}

// TestDispatcher_RunOnce_PermanentlyFailingRow_EscalatesToErrorPastTheStatedHorizon
// pins the escalation half of the billing-grade contract: a delivery row
// whose sink permanently fails delivery must not be retried forever with
// only a per-attempt Warn -- the contract ("retries indefinitely, plus an
// alert", restated on Dispatcher's own doc comment) promises exactly that
// alert. The contract's alert half is implemented as an escalation horizon:
// once a row's failed attempts reach the dispatcher's stated threshold
// (defaultDispatchEscalationAttempts), its failure cadence switches from the
// per-attempt Warn (metering.outbox_delivery_failed) to an Error
// (metering.outbox_delivery_escalated) naming the row, repeated on every
// subsequent failed attempt -- a permanently failing row is continuously
// visible to log-based alerting on that Error key, so operations discovers
// the stuck row within the stated horizon. Retry itself never stops and
// nothing converges to a terminal state: the row stays pending and keeps
// being retried, the escalation is a signal layered on the retry, not a cap
// under it.
func TestDispatcher_RunOnce_PermanentlyFailingRow_EscalatesToErrorPastTheStatedHorizon(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "metering_dispatcher_escalation.sqlite")
	db := openAndMigrate(t, dsn)
	brokenConn := closedDB(t, openAndMigrate(t, dsn))
	d := NewDispatcher(db, NewAggregator(NewSummaryRepository(brokenConn))) // a sink that can never deliver
	ctx := context.Background()

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-stuck", OccurredAt: time.Now()}
	enqueued, err := Enqueue(ctx, db, event)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// The module's stated default escalation horizon: the failed-attempt
	// count at which the delivery loop's per-attempt Warn becomes an
	// escalated Error (defaultDispatchEscalationAttempts' doc comment states
	// the wall-clock meaning at the default pacing).
	const horizon = defaultDispatchEscalationAttempts

	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	// One delivery cycle against the permanently broken sink: the row is
	// claimed (its re-claim window backdated below the claim cutoff, the
	// deterministic stand-in for waiting out the retry delay), delivery
	// fails, and the failed attempt is recorded -- the same cycle shape the
	// existing retry regressions drive.
	runFailureCycle := func() {
		t.Helper()
		delivered, cycleErr := d.RunOnce(ctx)
		if cycleErr != nil {
			t.Fatalf("RunOnce: %v", cycleErr)
		}
		if delivered != 0 {
			t.Fatalf("delivered = %d, want 0 (the aggregator's own database connection is closed)", delivered)
		}
		backdated := time.Now().Add(-time.Second)
		if updateErr := db.Model(&OutboxRecord{}).Where("id = ?", enqueued.ID).Update("retry_after", backdated).Error; updateErr != nil {
			t.Fatalf("backdate retry_after: %v", updateErr)
		}
	}

	// Below the stated horizon: only the ordinary per-attempt Warn cadence
	// may appear -- no escalation line, no Error level, and the row is
	// retried (never dropped, never converged to a terminal state).
	for attempt := 1; attempt < horizon; attempt++ {
		runFailureCycle()
	}
	row, found, err := findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-stuck")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if !found {
		t.Fatal("the permanently failing outbox row is gone before the stated horizon -- the row must never be dropped")
	}
	if row.Attempts != horizon-1 {
		t.Fatalf("Attempts = %d after %d failed cycles, want %d", row.Attempts, horizon-1, horizon-1)
	}
	if row.Status != outboxStatusPending {
		t.Fatalf("Status = %q before the horizon, want %q (the retry loop never converges a failing row to a terminal state)", row.Status, outboxStatusPending)
	}
	if out := buf.String(); strings.Contains(out, "metering.outbox_delivery_escalated") {
		t.Errorf("escalation fired below the stated horizon: %s", out)
	} else if got := strings.Count(out, "metering.outbox_delivery_failed"); got != horizon-1 {
		t.Errorf("metering.outbox_delivery_failed lines = %d, want %d (one Warn per failed attempt below the horizon)", got, horizon-1)
	}

	// The horizon attempt itself is where the alert fires: the failure is
	// logged at Error level under the escalation key, naming the stuck row.
	runFailureCycle()
	atHorizon := buf.String()
	if !strings.Contains(atHorizon, "metering.outbox_delivery_escalated") {
		t.Errorf("no escalation line at the stated horizon (attempt %d): %s", horizon, atHorizon)
	}
	if !strings.Contains(atHorizon, "level=ERROR") {
		t.Errorf("escalation below Error level at the stated horizon: %s", atHorizon)
	}
	if !strings.Contains(atHorizon, "outbox_id="+enqueued.ID) {
		t.Errorf("escalation line does not name the stuck row's id (%s): %s", enqueued.ID, atHorizon)
	}
	row, found, err = findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-stuck")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if !found || row.Status != outboxStatusPending {
		t.Fatalf("row at the horizon = %+v (found=%v), want Status=%q -- escalation is a signal, not a terminal transition", row, found, outboxStatusPending)
	}
	if row.Attempts != horizon {
		t.Errorf("Attempts = %d at the horizon, want %d", row.Attempts, horizon)
	}

	// The escalation keeps firing on every later failed attempt, so the
	// stuck row stays visible until it is fixed rather than alerting once
	// and falling silent.
	runFailureCycle()
	if got := strings.Count(buf.String(), "metering.outbox_delivery_escalated"); got != 2 {
		t.Errorf("escalation lines after %d failed attempts = %d, want 2 (one per failed attempt from the horizon on)", horizon+1, got)
	}
	row, found, err = findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-stuck")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if !found || row.Status != outboxStatusPending {
		t.Fatalf("row after the horizon = %+v (found=%v), want Status=%q -- still retried, never dead-lettered", row, found, outboxStatusPending)
	}
	if row.Attempts != horizon+1 {
		t.Errorf("Attempts = %d after %d failed cycles, want %d", row.Attempts, horizon+1, horizon+1)
	}
	if row.LastError == "" {
		t.Error("LastError is empty after the escalated failures, want the delivery failure's message")
	}
}

// TestDispatcher_RunOnce_RowRecoveredBelowTheHorizon_NeverEscalates pins the
// unaffected half of the escalation contract: escalation exists to surface
// rows whose sink is permanently failing, so a row that fails a few times
// below the stated horizon and then delivers normally -- the transient
// failure every retry exists to ride out -- must never produce an escalation
// line. Healthy rows and transient failures stay on the ordinary Warn
// cadence; only a row that keeps failing past the stated horizon pages
// operations.
func TestDispatcher_RunOnce_RowRecoveredBelowTheHorizon_NeverEscalates(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "metering_dispatcher_escalation_recovery.sqlite")
	db := openAndMigrate(t, dsn)
	brokenConn := closedDB(t, openAndMigrate(t, dsn))
	ctx := context.Background()

	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 5, IdempotencyKey: "idem-recovering", OccurredAt: time.Now()}
	enqueued, err := Enqueue(ctx, db, event)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	// One transient failure -- far below the stated horizon -- against the
	// broken connection, then the sink recovers.
	dBroken := NewDispatcher(db, NewAggregator(NewSummaryRepository(brokenConn)))
	delivered, err := dBroken.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce (failure cycle): %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered during the failure cycle = %d, want 0", delivered)
	}
	backdated := time.Now().Add(-time.Second)
	if updateErr := db.Model(&OutboxRecord{}).Where("id = ?", enqueued.ID).Update("retry_after", backdated).Error; updateErr != nil {
		t.Fatalf("backdate retry_after: %v", updateErr)
	}

	// The healthy dispatcher completes the delivery on its next run.
	dHealthy := NewDispatcher(db, NewAggregator(NewSummaryRepository(db)))
	delivered, err = dHealthy.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce (recovery): %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d on recovery, want 1", delivered)
	}

	// The whole journey produced ordinary Warns at most -- never an
	// escalation line.
	if out := buf.String(); strings.Contains(out, "metering.outbox_delivery_escalated") {
		t.Errorf("a row that recovered below the stated horizon escalated: %s", out)
	} else if got := strings.Count(out, "metering.outbox_delivery_failed"); got != 1 {
		t.Errorf("metering.outbox_delivery_failed lines = %d, want exactly 1 (the one transient failure)", got)
	}

	final, found, err := findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-recovering")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if !found || final.Status != outboxStatusDelivered {
		t.Fatalf("final row = %+v (found=%v), want Status=%q", final, found, outboxStatusDelivered)
	}
	got, err := dHealthy.aggregator.RealtimeCount("tenant-a", "ai.generation", event.OccurredAt)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if got != 5 {
		t.Errorf("RealtimeCount after recovery = %v, want 5 (the event was delivered exactly once)", got)
	}
}
