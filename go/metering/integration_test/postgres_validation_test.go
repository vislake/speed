//go:build integration

// PostgreSQL regression for UsageEvent.validate's missing length bounds --
// see the package doc comment in postgres_isolation_test.go for the
// defect this test reproduces and the fix it guards.
package metering_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TestPostgres_Enqueue_OverlongField_RefusedBeforeAnyWrite is the
// PostgreSQL leg of the unit tier's own length-bound rows in
// TestUsageEvent_Validate, in the shape the defect actually lives in: an
// over-long identifier field on an event enqueued inside a caller
// transaction. SQLite never enforces a VARCHAR column's declared length,
// so on SQLite an over-long Feature (or TenantID or IdempotencyKey) is
// accepted silently and the unit tier could not see the divergence;
// PostgreSQL rejects the INSERT with SQLSTATE 22001 -- inside the
// caller's own transaction, aborting it exactly like the outbox retry
// defect does. validate must refuse the event with the coded
// metering.field_too_long error before any statement runs, leaving the
// caller's transaction untouched.
func TestPostgres_Enqueue_OverlongField_RefusedBeforeAnyWrite(t *testing.T) {
	cases := []struct {
		name  string
		field string
		event func(idempotencyKey string) metering.UsageEvent
	}{
		{
			name:  "tenant_id beyond 64 bytes",
			field: "tenant_id",
			event: func(idempotencyKey string) metering.UsageEvent {
				return metering.UsageEvent{TenantID: strings.Repeat("t", 65), Feature: "ai.generation", Quantity: 1, IdempotencyKey: idempotencyKey}
			},
		},
		{
			name:  "feature beyond 128 bytes",
			field: "feature",
			event: func(idempotencyKey string) metering.UsageEvent {
				return metering.UsageEvent{TenantID: "tenant-a", Feature: strings.Repeat("f", 129), Quantity: 1, IdempotencyKey: idempotencyKey}
			},
		},
		{
			name:  "idempotency_key beyond 200 bytes",
			field: "idempotency_key",
			event: func(idempotencyKey string) metering.UsageEvent {
				return metering.UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: strings.Repeat("k", 201)}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newPostgres(t)
			ctx := context.Background()
			valid := metering.UsageEvent{TenantID: "tenant-a", Feature: "ai.generation", Quantity: 1, IdempotencyKey: "idem-pg-valid-after-" + tc.field}

			err := db.Transaction(func(tx *gorm.DB) error {
				if _, enqErr := metering.Enqueue(ctx, tx, tc.event("idem-pg-overlong-"+tc.field)); enqErr == nil {
					return errors.New("Enqueue(overlong field) = nil error, want metering.field_too_long refused before any write")
				} else {
					appErr, ok := apperr.As(enqErr)
					if !ok || appErr.Code != metering.ErrFieldTooLong.Code {
						return fmt.Errorf("Enqueue(overlong field) = %v, want %s (on PostgreSQL the pre-fix code let the INSERT run and failed with a raw 22001, poisoning this transaction)", enqErr, metering.ErrFieldTooLong.Code)
					}
				}
				// The refusal must have left the caller's transaction
				// healthy: a valid enqueue right after it still lands when
				// the transaction commits.
				if _, enqErr := metering.Enqueue(ctx, tx, valid); enqErr != nil {
					return fmt.Errorf("Enqueue(valid event after the refusal) = %v, want nil: the refusal poisoned the caller's transaction", enqErr)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("db.Transaction: %v", err)
			}
			assertOutboxRowCount(t, db, "tenant-a", "idem-pg-valid-after-"+tc.field, 1)
		})
	}
}
