package metering

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"

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

	pending, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending rows = %d, want 1 (the row must not be lost)", len(pending))
	}
	if pending[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", pending[0].Attempts)
	}
	if pending[0].LastError == "" {
		t.Error("LastError is empty, want the delivery failure's message")
	}
}

// TestDispatcher_CrashMidDelivery_RowIsRecoveredOnTheNextRun is the
// round's mandated crash-recovery proof: it "kills" the delivery path
// mid-flight -- an Aggregator whose database connection has been closed,
// simulating a process crash between claiming a row and finishing its
// delivery -- confirms the outbox row is NOT lost (still present, still
// pending, in the SAME durable table Enqueue wrote it to), and then
// confirms a fresh, healthy Dispatcher recovers and delivers it
// successfully. This is what makes Enqueue's "write, then async deliver"
// promise real rather than aspirational: nothing about a mid-delivery
// crash can make an enqueued event disappear.
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

	waitFor(t, func() bool {
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
