package metering

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// SummaryRepository is the tenant-scoped accessor for
// metering_usage_summaries. UsageSummary is tenant data (see its own doc
// comment), so this embeds dbkit.Repository[UsageSummary] and inherits all
// three tenant-isolation layers, exactly like every other tenant-owned
// repository in this codebase.
//
// db is kept alongside the embedded dbkit.Repository[UsageSummary]
// (unexported, package-internal) so Aggregator.IngestBillingGrade can open
// its own transaction on the SAME connection this repository's writes use
// -- see IngestReceipt's doc comment for why that matters.
type SummaryRepository struct {
	*dbkit.Repository[UsageSummary]
	db *gorm.DB
}

// NewSummaryRepository returns a SummaryRepository over db. db is expected
// to come from dbkit.Open with this module's migrations applied.
func NewSummaryRepository(db *gorm.DB) *SummaryRepository {
	return &SummaryRepository{Repository: dbkit.NewRepository[UsageSummary](db), db: db}
}

// --- Outbox: plain *gorm.DB functions, platform-data pattern -------------
//
// OutboxRecord is platform data (see its own doc comment), so it is
// reached through plain functions over a *gorm.DB rather than
// dbkit.Repository[T] -- the identical shape go/jobs/store.go uses for
// jobRecord, and the reason that file is named in
// tools/semgrep_rules/raw-gorm-bypass.yml's allowlist header. None of the
// functions below uses .Table/.Model/.Raw (the three entry points that
// rule flags): inserts and reads pass a concrete *OutboxRecord or
// *[]OutboxRecord so GORM infers the schema from the argument, and updates
// load the row first (First) and call Save on the mutated struct, the same
// idiom dbkit.Repository[T].Update itself uses internally.

// insertOutboxRecord inserts rec via tx -- ordinarily the caller's own
// transaction, so committing it lands rec atomically with whatever
// business write shares that transaction. See Enqueue's doc comment.
func insertOutboxRecord(ctx context.Context, tx *gorm.DB, rec *OutboxRecord) error {
	return tx.WithContext(ctx).Create(rec).Error
}

// findOutboxByIdempotencyKey returns the row for (tenantID,
// idempotencyKey), or (nil, false, nil) when none exists.
func findOutboxByIdempotencyKey(ctx context.Context, db *gorm.DB, tenantID, idempotencyKey string) (*OutboxRecord, bool, error) {
	var rec OutboxRecord
	err := db.WithContext(ctx).
		Where("tenant_id = ? AND idempotency_key = ?", tenantID, idempotencyKey).
		First(&rec).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return &rec, true, nil
}

// claimPendingOutboxRecords returns up to limit outboxStatusPending rows,
// oldest first. It is a read only -- it does not mark anything as
// in-flight -- because this round runs exactly one in-process Dispatcher;
// see Dispatcher's and OutboxRecord's doc comments for what a second
// concurrent dispatcher process would need that this round does not build.
func claimPendingOutboxRecords(ctx context.Context, db *gorm.DB, limit int) ([]OutboxRecord, error) {
	var recs []OutboxRecord
	err := db.WithContext(ctx).
		Where("status = ?", outboxStatusPending).
		Order("created_at ASC").
		Limit(limit).
		Find(&recs).Error
	return recs, err
}

// markOutboxDelivered transitions id from outboxStatusPending to
// outboxStatusDelivered, recording deliveredAt. It is a no-op (no error)
// when id no longer exists or is no longer pending -- both mean some
// other delivery attempt already finished it.
func markOutboxDelivered(ctx context.Context, db *gorm.DB, id string, deliveredAt time.Time) error {
	var rec OutboxRecord
	err := db.WithContext(ctx).
		Where("id = ? AND status = ?", id, outboxStatusPending).
		First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	rec.Status = outboxStatusDelivered
	rec.DeliveredAt = &deliveredAt
	return db.WithContext(ctx).Save(&rec).Error
}

// markOutboxAttemptFailed increments id's Attempts and records cause as
// its LastError, leaving Status untouched (still outboxStatusPending) so
// the row is retried on the next Dispatcher cycle -- billing-grade
// delivery retries indefinitely, per Dispatcher's own doc comment. cause
// is truncated to fit OutboxRecord.LastError's column width.
func markOutboxAttemptFailed(ctx context.Context, db *gorm.DB, id string, cause string) error {
	var rec OutboxRecord
	err := db.WithContext(ctx).Where("id = ?", id).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	rec.Attempts++
	rec.LastError = truncateError(cause)
	return db.WithContext(ctx).Save(&rec).Error
}

// maxLastErrorLength mirrors OutboxRecord.LastError's column size.
const maxLastErrorLength = 500

// truncateError bounds cause to maxLastErrorLength bytes, never writing
// more into the database than the column can hold.
func truncateError(cause string) string {
	if len(cause) <= maxLastErrorLength {
		return cause
	}
	return cause[:maxLastErrorLength]
}
