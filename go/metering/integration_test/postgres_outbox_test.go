//go:build integration

// PostgreSQL regression for Enqueue's idempotent-retry guarantee on the
// metering_outbox_records side of the billing-grade pipeline -- see the
// package doc comment in postgres_isolation_test.go for the defect this
// test reproduces and the fix it guards.
package metering_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/metering"
)

// TestPostgres_Enqueue_IdempotentRetry_InsideOneCallerTransaction is the
// PostgreSQL leg of the unit tier's own
// TestEnqueue_IdempotentRetry_ReturnsTheExistingRow, but in the shape the
// defect actually lives in: the retried Enqueue runs inside a NEW caller
// transaction -- exactly what a host does when it retries a whole
// business transaction whose Enqueue response it never saw -- and that
// same transaction also carries a second, unrelated business write (a
// second enqueue with its own idempotency key), which must commit.
//
// Before the fix, the retry's insert hit the (tenant_id, idempotency_key)
// unique index and the recovery then read the existing row back on the
// SAME transaction. SQLite tolerates a failed statement inside an open
// transaction; PostgreSQL does not -- the transaction is aborted and the
// recovery's SELECT fails with SQLSTATE 25P02, so Enqueue returned the
// insert error, the caller's whole transaction rolled back, and the
// idempotency contract collapsed: a retried caller whose first attempt
// had already committed got an error and lost its unrelated business
// write too. After the fix the insert uses ON CONFLICT DO NOTHING (which
// never aborts the transaction), the pre-existing row is read back on the
// still-healthy transaction, and both this call and the follow-up write
// commit.
func TestPostgres_Enqueue_IdempotentRetry_InsideOneCallerTransaction(t *testing.T) {
	db := newPostgres(t)
	ctx := context.Background()

	event := metering.UsageEvent{
		TenantID:       "tenant-a",
		Feature:        "ai.generation",
		Quantity:       2,
		IdempotencyKey: "idem-pg-retry",
		OccurredAt:     time.Now(),
	}

	// Attempt 1, committed.
	first, err := metering.Enqueue(ctx, db, event)
	if err != nil {
		t.Fatalf("Enqueue (first attempt): %v", err)
	}

	// The retry, inside a fresh caller transaction that also performs a
	// second, unrelated business write.
	secondEvent := metering.UsageEvent{
		TenantID:       "tenant-a",
		Feature:        "api.calls",
		Quantity:       1,
		IdempotencyKey: "idem-pg-retry-business-write",
		OccurredAt:     time.Now(),
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		second, enqErr := metering.Enqueue(ctx, tx, event)
		if enqErr != nil {
			return enqErr
		}
		if second.ID != first.ID {
			return &retryReturnedWrongRowError{got: second.ID, want: first.ID}
		}
		// The unrelated business write sharing this transaction must
		// survive the retry's duplicate handling.
		if _, enqErr := metering.Enqueue(ctx, tx, secondEvent); enqErr != nil {
			return enqErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retried Enqueue inside the caller's transaction = %v, want nil (the existing row returned, the transaction healthy): on PostgreSQL the pre-fix recovery read the duplicate back on the aborted transaction, failed, and dragged this whole transaction down", err)
	}

	// Exactly one row per idempotency key, both committed.
	assertOutboxRowCount(t, db, "tenant-a", "idem-pg-retry", 1)
	assertOutboxRowCount(t, db, "tenant-a", "idem-pg-retry-business-write", 1)
}

// retryReturnedWrongRowError reports a retried Enqueue answering with a
// different row than the first attempt created.
type retryReturnedWrongRowError struct {
	got  string
	want string
}

func (e *retryReturnedWrongRowError) Error() string {
	return "retried Enqueue returned a different row: got ID " + e.got + ", want " + e.want
}

// assertOutboxRowCount reads metering_outbox_records directly and fails
// the test unless exactly want rows exist for (tenantID, idempotencyKey).
// The tier reads through plain *gorm.DB because the module's own outbox
// helpers are package-internal by design (platform-data functions in
// go/metering/repository.go); this is a test assertion, not production
// access.
func assertOutboxRowCount(t *testing.T, db *gorm.DB, tenantID, idempotencyKey string, want int) {
	t.Helper()
	var rows []metering.OutboxRecord
	if err := db.
		Where("tenant_id = ? AND idempotency_key = ?", tenantID, idempotencyKey).
		Find(&rows).Error; err != nil {
		t.Fatalf("read outbox rows for (%q, %q): %v", tenantID, idempotencyKey, err)
	}
	if len(rows) != want {
		t.Errorf("outbox rows for (%q, %q) = %d, want %d", tenantID, idempotencyKey, len(rows), want)
	}
}
