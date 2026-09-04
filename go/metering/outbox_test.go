package metering

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

func TestEnqueue_WritesAPendingRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	event := UsageEvent{
		TenantID:       "tenant-a",
		Feature:        "ai.generation",
		Quantity:       2,
		IdempotencyKey: "idem-1",
		OccurredAt:     time.Now(),
		Metadata:       map[string]string{"job_id": "job-1"},
	}

	rec, err := Enqueue(ctx, db, event)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if rec.Status != outboxStatusPending {
		t.Errorf("Status = %q, want %q", rec.Status, outboxStatusPending)
	}
	if rec.Quantity != 2 {
		t.Errorf("Quantity = %v, want 2", rec.Quantity)
	}

	got, found, err := findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-1")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if !found {
		t.Fatal("row was not found after Enqueue")
	}
	if got.ID != rec.ID {
		t.Errorf("stored row ID = %q, want %q", got.ID, rec.ID)
	}
	if decodeMetadata(got.Metadata)["job_id"] != "job-1" {
		t.Errorf("stored metadata = %v, want job_id=job-1", decodeMetadata(got.Metadata))
	}
}

func TestEnqueue_InvalidEvent_ReturnsValidationError(t *testing.T) {
	db := newTestDB(t)
	_, err := Enqueue(context.Background(), db, UsageEvent{})
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrMissingTenantID.Code {
		t.Fatalf("Enqueue(empty event) = %v, want %s", err, ErrMissingTenantID.Code)
	}
}

// TestEnqueue_IdempotentRetry_ReturnsTheExistingRow proves the core
// billing-grade dedup guarantee: a second Enqueue call for the same
// (tenant, idempotency_key) -- a caller retrying after a timeout with no
// visible response -- returns the SAME row rather than creating a
// duplicate or erroring.
func TestEnqueue_IdempotentRetry_ReturnsTheExistingRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	event := UsageEvent{
		TenantID:       "tenant-a",
		Feature:        "ai.generation",
		Quantity:       1,
		IdempotencyKey: "idem-retry",
		OccurredAt:     time.Now(),
	}

	first, err := Enqueue(ctx, db, event)
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	second, err := Enqueue(ctx, db, event)
	if err != nil {
		t.Fatalf("second Enqueue (retry): %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("retried Enqueue returned a different row: first ID %q, second ID %q", first.ID, second.ID)
	}

	all, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("outbox holds %d row(s) after a retried Enqueue, want exactly 1 (no duplicate)", len(all))
	}
}

// TestEnqueue_DifferentTenants_SameIdempotencyKey_BothLand proves the
// unique index is scoped to (tenant_id, idempotency_key) together: two
// different tenants reusing the same idempotency key value must not
// collide with each other.
func TestEnqueue_DifferentTenants_SameIdempotencyKey_BothLand(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	a, err := Enqueue(ctx, db, UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-shared"})
	if err != nil {
		t.Fatalf("Enqueue(tenant-a): %v", err)
	}
	b, err := Enqueue(ctx, db, UsageEvent{TenantID: "tenant-b", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-shared"})
	if err != nil {
		t.Fatalf("Enqueue(tenant-b): %v", err)
	}
	if a.ID == b.ID {
		t.Error("two different tenants' Enqueue calls collided onto the same row")
	}
}

// TestEnqueue_SharesTheCallerTransaction_CommitLandsTheRow proves the
// core outbox-pattern guarantee's happy path: Enqueue called with the tx
// argument of a db.Transaction closure lands once that transaction
// commits, alongside whatever business write shared it.
func TestEnqueue_SharesTheCallerTransaction_CommitLandsTheRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-commit"}

	var enqueuedID string
	err := db.Transaction(func(tx *gorm.DB) error {
		rec, enqErr := Enqueue(ctx, tx, event)
		if enqErr != nil {
			return enqErr
		}
		enqueuedID = rec.ID
		return nil
	})
	if err != nil {
		t.Fatalf("db.Transaction: %v", err)
	}

	_, found, err := findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-commit")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if !found {
		t.Fatalf("outbox row %q was not visible after the transaction committed", enqueuedID)
	}
}

// TestEnqueue_SharesTheCallerTransaction_RollbackDiscardsTheRow proves the
// other half of the same guarantee: when the caller's transaction rolls
// back (because the business write it shared failed), the outbox row
// enqueued inside it must NOT survive either -- "the metering row exists
// but the business write never happened" is exactly as forbidden as the
// reverse.
func TestEnqueue_SharesTheCallerTransaction_RollbackDiscardsTheRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	event := UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-rollback"}

	businessWriteFailed := errors.New("simulated business write failure")
	err := db.Transaction(func(tx *gorm.DB) error {
		if _, enqErr := Enqueue(ctx, tx, event); enqErr != nil {
			return enqErr
		}
		// Simulate the business write this outbox row was supposed to
		// accompany failing after Enqueue already ran -- the whole
		// transaction must roll back.
		return businessWriteFailed
	})
	if !errors.Is(err, businessWriteFailed) {
		t.Fatalf("db.Transaction error = %v, want %v", err, businessWriteFailed)
	}

	_, found, findErr := findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-rollback")
	if findErr != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", findErr)
	}
	if found {
		t.Error("outbox row survived a rolled-back transaction")
	}
}

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "gorm.ErrDuplicatedKey directly", err: gorm.ErrDuplicatedKey, want: true},
		{name: "gorm.ErrDuplicatedKey wrapped", err: fmt.Errorf("insert: %w", gorm.ErrDuplicatedKey), want: true},
		{name: "unrelated error", err: errors.New("connection refused"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueViolation(tc.err); got != tc.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestEncodeDecodeMetadata_RoundTrip(t *testing.T) {
	m := map[string]string{"job_id": "job-1", "note": "hello"}
	encoded, err := encodeMetadata(m)
	if err != nil {
		t.Fatalf("encodeMetadata: %v", err)
	}
	if encoded == "" {
		t.Fatal("encodeMetadata(non-empty map) = \"\", want non-empty JSON")
	}
	decoded := decodeMetadata(encoded)
	if len(decoded) != len(m) || decoded["job_id"] != "job-1" || decoded["note"] != "hello" {
		t.Errorf("decodeMetadata(encodeMetadata(m)) = %v, want %v", decoded, m)
	}
}

func TestEncodeMetadata_Empty(t *testing.T) {
	encoded, err := encodeMetadata(nil)
	if err != nil {
		t.Fatalf("encodeMetadata(nil): %v", err)
	}
	if encoded != "" {
		t.Errorf("encodeMetadata(nil) = %q, want \"\"", encoded)
	}
	encoded, err = encodeMetadata(map[string]string{})
	if err != nil {
		t.Fatalf("encodeMetadata(empty map): %v", err)
	}
	if encoded != "" {
		t.Errorf("encodeMetadata(empty map) = %q, want \"\"", encoded)
	}
}

func TestDecodeMetadata_EmptyAndMalformed(t *testing.T) {
	if got := decodeMetadata(""); got != nil {
		t.Errorf("decodeMetadata(\"\") = %v, want nil", got)
	}
	if got := decodeMetadata("not json"); got != nil {
		t.Errorf("decodeMetadata(malformed) = %v, want nil (best-effort, never panics or errors)", got)
	}
}
