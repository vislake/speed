//go:build integration

// PostgreSQL regressions for the metering_ingest_receipts side of the
// billing-grade pipeline -- see the package doc comment in
// postgres_isolation_test.go for the defect class each test reproduces
// and the contract each guards.
package metering_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// TestPostgres_IngestReceiptRepository_DuplicateCreate_IsErrDuplicatedKey
// proves on the real server the one remaining place a duplicate receipt
// Create still surfaces as an error: the raw repository path (a caller
// deliberately not using IngestBillingGrade's idempotent fold). dbkit's
// TranslateError must classify PostgreSQL's own "duplicate key value
// violates unique constraint" (SQLSTATE 23505) as the portable
// gorm.ErrDuplicatedKey, the classification the unit tier proves on
// SQLite only. The idempotent fold path itself does not depend on this
// classification -- it avoids the error entirely through ON CONFLICT DO
// NOTHING (see TestPostgres_IngestBillingGrade_Redelivery_IsANoOp below)
// -- but the raw path's answer still matters for callers that use
// IngestReceiptRepository directly.
func TestPostgres_IngestReceiptRepository_DuplicateCreate_IsErrDuplicatedKey(t *testing.T) {
	repo := metering.NewIngestReceiptRepository(newPostgres(t))
	ctx := testkit.TenantCtx("tenant-a")

	if err := repo.Create(ctx, &metering.IngestReceipt{ID: "idem-1"}); err != nil {
		t.Fatalf("Create (first): %v", err)
	}
	err := repo.Create(ctx, &metering.IngestReceipt{ID: "idem-1"})
	if err == nil {
		t.Fatal("Create (duplicate) = nil error, want gorm.ErrDuplicatedKey")
	}
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Errorf("Create (duplicate) error = %v, want errors.Is(err, gorm.ErrDuplicatedKey)", err)
	}
}

// TestPostgres_IngestBillingGrade_Redelivery_IsANoOpNotAnError is the
// PostgreSQL leg of the unit tier's own
// TestAggregator_IngestBillingGrade_RedeliveredEvent_DoesNotDoubleCount:
// the SAME UsageEvent is handed to IngestBillingGrade twice, standing in
// for Dispatcher reclaiming a still-"pending" outbox row whose first
// delivery's aggregation already committed. The second call must be a
// safe no-op returning nil, with the event applied exactly once.
//
// The PostgreSQL-specific hazard the no-op must avoid: a receipt-insert
// conflict recognized by swallowing the unique-violation error would
// leave the enclosing transaction in the aborted state PostgreSQL put it
// in, and a COMMIT on an aborted transaction is turned into a ROLLBACK by
// the server, which the pgx driver surfaces as an error -- making the
// whole redelivery fail. On SQLite (no aborted-transaction semantics)
// such a swallow would be a silent, correct no-op, which is exactly why
// this regression must run against real PostgreSQL to be a regression at
// all. foldIntoSummaryOnce's receipt insert therefore runs as ON
// CONFLICT DO NOTHING: the conflict is recognized through RowsAffected ==
// 0 with the transaction healthy, and the commit is a real commit.
func TestPostgres_IngestBillingGrade_Redelivery_IsANoOpNotAnError(t *testing.T) {
	db := newPostgres(t)
	summaries := metering.NewSummaryRepository(db)
	agg := metering.NewAggregator(summaries)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	event := metering.UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 7, IdempotencyKey: "idem-pg-redelivered", OccurredAt: at}

	if err := agg.IngestBillingGrade(ctx, event); err != nil {
		t.Fatalf("IngestBillingGrade (first delivery): %v", err)
	}
	if err := agg.IngestBillingGrade(ctx, event); err != nil {
		t.Fatalf("IngestBillingGrade (redelivery) = %v, want nil: on PostgreSQL the pre-fix swallow of the receipt-insert conflict committed an aborted transaction, which the driver reports as an error and an SQLite-only suite cannot see", err)
	}

	gotRealtime, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if gotRealtime != 7 {
		t.Errorf("RealtimeCount after redelivery = %v, want 7 (exactly one application, not 14)", gotRealtime)
	}

	rows, err := summaries.List(testkit.TenantCtx("tenant-a"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List returned %d summary row(s), want exactly 1", len(rows))
	}
	if rows[0].Quantity != 7 {
		t.Errorf("summary Quantity after redelivery = %v, want 7 (exactly one application, not 14)", rows[0].Quantity)
	}
}

// TestPostgres_Dispatcher_RedeliveredRow_RetiresInsteadOfErroring is the
// end-to-end form of the same contract, driven through the real
// billing-grade delivery loop: an outbox row whose aggregation already
// committed but whose mark-delivered write never ran (the crash window
// IngestReceipt closes -- simulated here by calling IngestBillingGrade
// directly and leaving the row pending) is reclaimed by the next
// Dispatcher.RunOnce and redelivered. The redelivery must be a no-op
// that commits cleanly (see the test above), so the row retires on its
// first redelivery cycle -- a redelivery that failed would be recorded
// as a failed attempt and leave the row pending, the next cycle
// reproducing the identical failure forever even though the event was
// durably applied.
func TestPostgres_Dispatcher_RedeliveredRow_RetiresInsteadOfErroring(t *testing.T) {
	db := newPostgres(t)
	summaries := metering.NewSummaryRepository(db)
	agg := metering.NewAggregator(summaries)
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	event := metering.UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 5, IdempotencyKey: "idem-pg-dispatcher", OccurredAt: at}
	enqueued, err := metering.Enqueue(ctx, db, event)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// The crash window: the aggregation half commits...
	if err := agg.IngestBillingGrade(ctx, event); err != nil {
		t.Fatalf("IngestBillingGrade (first delivery): %v", err)
	}
	// ...while the row stays genuinely "pending" (mark-delivered never ran).

	dispatcher := metering.NewDispatcher(db, agg)
	delivered, err := dispatcher.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != 1 {
		t.Errorf("RunOnce delivered = %d, want 1: the redelivered row must retire on its first cycle; on PostgreSQL the pre-fix code failed the redelivery instead and left the row pending forever", delivered)
	}

	var rec metering.OutboxRecord
	if err := db.Where("id = ?", enqueued.ID).First(&rec).Error; err != nil {
		t.Fatalf("read back outbox row %q: %v", enqueued.ID, err)
	}
	if rec.Status != "delivered" {
		t.Errorf("outbox row status after redelivery = %q, want %q", rec.Status, "delivered")
	}
	if rec.Attempts != 0 {
		t.Errorf("outbox row Attempts = %d, want 0 (the redelivery must not have been recorded as a failed attempt)", rec.Attempts)
	}

	gotRealtime, err := agg.RealtimeCount("tenant-a", "ai.generation", at)
	if err != nil {
		t.Fatalf("RealtimeCount: %v", err)
	}
	if gotRealtime != 5 {
		t.Errorf("RealtimeCount after redelivery = %v, want 5 (exactly one application, not 10)", gotRealtime)
	}

	rows, err := summaries.List(testkit.TenantCtx("tenant-a"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Quantity != 5 {
		t.Errorf("summary rows after redelivery = %v, want exactly one row of Quantity 5", fmt.Sprintf("%+v", rows))
	}
}
