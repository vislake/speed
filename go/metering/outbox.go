package metering

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Enqueue is the billing-grade tier's write half of the outbox pattern:
// the row is written in the same transaction as the business operation
// it measures. The caller passes its OWN transaction as tx -- typically
// the tx argument of a db.Transaction(func(tx *gorm.DB) error { ... })
// call that also performs the business write Enqueue's event measures --
// the same-transaction shape go/dbkit's audit_capture.go plugin
// achieves automatically for its own GORM-callback-driven capture, except
// here it is an explicit call the caller makes rather than an automatic
// hook, since metering has no write to attach a callback to (the caller's
// business write is not a call into this module at all).
//
// Because the outbox row and the business write share one transaction,
// "the business write commits but its metering silently vanishes" is
// physically impossible: either both land when tx commits, or neither
// does when it rolls back. This is the property Enqueue exists to
// guarantee -- see Dispatcher's doc comment for the asynchronous delivery
// half that moves the row into the aggregation pipeline in the
// background.
//
// Enqueue is idempotent under retry: if event.IdempotencyKey was already
// enqueued for event.TenantID (the database's own unique index on
// (tenant_id, idempotency_key) catches this even under concurrent
// callers), Enqueue returns the EXISTING row rather than erroring or
// creating a duplicate -- a caller that retries an Enqueue call after a
// timeout with no visible response, not knowing whether the first attempt
// committed, gets the same durable outcome either way. The duplicate is
// detected without ever leaving tx in the aborted state a unique-violation
// error would put a PostgreSQL transaction in: the insert runs as ON
// CONFLICT DO NOTHING, so the caller's own still-open transaction -- whose
// business write this outbox row accompanies -- stays healthy and commits
// normally (see the recovery comment on insertOutboxRecord's call below
// for the full poisoned-transaction argument).
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

	now := time.Now()
	rec := &OutboxRecord{
		ID:             uuid.NewString(),
		TenantID:       event.TenantID,
		Feature:        event.Feature,
		Quantity:       event.Quantity,
		IdempotencyKey: event.IdempotencyKey,
		OccurredAt:     occurredAt,
		Metadata:       metadata,
		Status:         outboxStatusPending,
		// RetryAfter starts at CreatedAt: a never-failed row is claimable
		// from birth. Only a failed delivery attempt moves it (see
		// markOutboxAttemptFailed).
		RetryAfter: &now,
		CreatedAt:  now,
	}

	// insertOutboxRecord reports a duplicate (tenant_id, idempotency_key)
	// as inserted == false with NO error -- its insert ran as ON CONFLICT
	// DO NOTHING -- never as a unique-constraint error. That matters
	// because tx is the caller's own transaction: on PostgreSQL a
	// unique-violation error would leave that transaction aborted, and
	// neither this recovery nor the caller's own still-pending business
	// write could run another statement on it (SQLSTATE 25P02). SQLite
	// tolerates a failed statement inside an open transaction; PostgreSQL
	// does not, which is exactly why the insert must never raise a
	// unique-violation error there -- see insertOutboxRecord's own doc
	// comment for the full poisoned-transaction argument.
	inserted, insertErr := insertOutboxRecord(ctx, tx, rec)
	if insertErr != nil {
		return nil, insertErr
	}
	if inserted {
		return rec, nil
	}

	// Idempotent retry: a row for this (tenant, idempotency_key) already
	// exists -- the transaction is still healthy (nothing aborted), so
	// read the existing row back on it and return that instead of the
	// constraint-violation error a plain insert would have produced.
	existing, found, findErr := findOutboxByIdempotencyKey(ctx, tx, event.TenantID, event.IdempotencyKey)
	if findErr != nil {
		return nil, findErr
	}
	if !found {
		// The conflicting row vanished between the no-op insert and this
		// read-back -- only a concurrent deleter could do that. The
		// retention sweep is the sole deleter of outbox rows and it
		// removes delivered rows only, never the pending row this read-back
		// runs against, so this branch is unreachable in practice. Enqueue
		// is retry-safe either way, so the honest answer to the caller is
		// a coded error it can retry on, not a fabricated row.
		return nil, errOutboxConflictRowVanished
	}
	return existing, nil
}

// errOutboxConflictRowVanished reports the unreachable-in-practice corner
// where a duplicate (tenant_id, idempotency_key) insert skipped via ON
// CONFLICT DO NOTHING but the read-back that follows finds no row: some
// other writer deleted it between the two statements. Nothing can: only
// the retention sweep deletes outbox rows at all, it deletes delivered
// rows only (never pending ones, which is the state this read-back runs
// against), and the sweep is driven by the same process's Dispatcher
// loop rather than a concurrent writer on the caller's transaction. The
// branch exists for completeness only; a caller retrying Enqueue gets the
// correct outcome either way, since a vanished row makes the retry a
// plain first insert again. It is deliberately a plain package-internal
// sentinel rather than an *apperr.Error: it is not reachable through any
// user-facing surface, so it earns no error-index or locale entry.
var errOutboxConflictRowVanished = errors.New("metering: conflicting outbox row vanished between insert and read-back; retry Enqueue")

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
