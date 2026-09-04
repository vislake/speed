package metering

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Enqueue is the billing-grade tier's write half of the outbox pattern
// (docs/internal/06-billing-and-metering.md's billing-grade row: written
// in the same transaction as the business operation). The caller passes
// its OWN transaction as tx -- typically the
// tx argument of a db.Transaction(func(tx *gorm.DB) error { ... }) call
// that also performs the business write Enqueue's event measures --
// exactly like the shape go/dbkit's audit_capture.go plugin achieves
// automatically for its own GORM-callback-driven capture, except here it
// is an explicit call the caller makes rather than an automatic hook,
// since metering has no write to attach a callback to (the caller's
// business write is not a call into this module at all).
//
// Because the outbox row and the business write share one transaction,
// "the business write commits but its metering silently vanishes" is
// physically impossible: either both land when tx commits, or neither
// does when it rolls back. This is the property Enqueue exists to
// guarantee -- see Dispatcher's doc comment for the asynchronous delivery
// half that later moves the row into the aggregation pipeline.
//
// Enqueue is idempotent under retry: if event.IdempotencyKey was already
// enqueued for event.TenantID (the database's own unique index on
// (tenant_id, idempotency_key) catches this even under concurrent
// callers), Enqueue returns the EXISTING row rather than erroring or
// creating a duplicate -- a caller that retries an Enqueue call after a
// timeout with no visible response, not knowing whether the first attempt
// committed, gets the same durable outcome either way.
func Enqueue(ctx context.Context, tx *gorm.DB, event UsageEvent) (*OutboxRecord, error) {
	if err := event.validate(); err != nil {
		return nil, err
	}

	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	metadata, err := encodeMetadata(event.Metadata)
	if err != nil {
		return nil, err
	}

	rec := &OutboxRecord{
		ID:             uuid.NewString(),
		TenantID:       event.TenantID,
		Feature:        event.Feature,
		Quantity:       event.Quantity,
		IdempotencyKey: event.IdempotencyKey,
		OccurredAt:     occurredAt,
		Metadata:       metadata,
		Status:         outboxStatusPending,
		CreatedAt:      time.Now(),
	}

	insertErr := insertOutboxRecord(ctx, tx, rec)
	if insertErr == nil {
		return rec, nil
	}
	if !isUniqueViolation(insertErr) {
		return nil, insertErr
	}

	// Idempotent retry: a row for this (tenant, idempotency_key) already
	// exists -- return it instead of the constraint-violation error. If
	// the lookup itself fails, the original insert error is more useful
	// to the caller than a lookup failure would be.
	existing, found, findErr := findOutboxByIdempotencyKey(ctx, tx, event.TenantID, event.IdempotencyKey)
	if findErr != nil || !found {
		return nil, insertErr
	}
	return existing, nil
}

// isUniqueViolation reports whether err is a unique-constraint violation.
// dbkit.Open sets gorm.Config.TranslateError: true, so both dialects'
// drivers already translate their own raw error (SQLite's "UNIQUE
// constraint failed", PostgreSQL's "duplicate key value violates unique
// constraint") into gorm's portable gorm.ErrDuplicatedKey sentinel before
// this function ever sees it -- there is no dialect-specific string
// matching to get right here.
func isUniqueViolation(err error) bool {
	return errors.Is(err, gorm.ErrDuplicatedKey)
}

// encodeMetadata JSON-encodes m for storage in OutboxRecord.Metadata,
// returning "" for a nil or empty map so the column's NOT NULL DEFAULT ”
// is satisfied without a caller ever seeing the literal "{}" or "null".
func encodeMetadata(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", ErrMetadataEncodeFailed.WithCause(err)
	}
	return string(b), nil
}

// decodeMetadata reverses encodeMetadata. A stored value that fails to
// decode (which nothing in this module's own write path can produce)
// decodes to nil rather than propagating an error: Dispatcher's delivery
// path must not get stuck retrying an outbox row forever over a metadata
// field it cannot parse, when the quantity and idempotency key it
// actually needs to deliver are intact.
func decodeMetadata(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}
