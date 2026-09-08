package metering

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
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
//
// The insert carries clause.OnConflict{DoNothing: true}, so a duplicate
// (tenant_id, idempotency_key) -- caught by the table's own unique index
// -- is reported back as inserted == false with no error, rather than as
// a unique-constraint error. That distinction is the whole point: a
// unique-violation error leaves the transaction ABORTED on PostgreSQL
// (every later statement fails with SQLSTATE 25P02 until ROLLBACK), and
// Enqueue's idempotent-retry recovery needs the transaction to stay
// healthy so it can read the pre-existing row back and so the caller's
// own transaction can go on committing its business write. An ON CONFLICT
// DO NOTHING statement never aborts anything, on either dialect; see
// Enqueue's doc comment for the full poisoned-transaction argument.
func insertOutboxRecord(ctx context.Context, tx *gorm.DB, rec *OutboxRecord) (inserted bool, err error) {
	res := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(rec)
	return res.RowsAffected > 0, res.Error
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

// claimPendingOutboxRecords returns up to limit pending outbox rows whose
// retry_after has arrived, in schedule order: the row whose RetryAfter is
// oldest goes first, with CreatedAt breaking ties. RetryAfter is
// CreatedAt for a never-failed row and the failure time plus the
// dispatcher's retry delay for an already-failed one (see
// markOutboxAttemptFailed), so this is exactly go/jobs' scheduled_at
// discipline: a failed row re-enters the candidate set at a moment in the
// future rather than re-joining the queue head. That single property
// delivers both fairness directions at once, which an attempts-based class
// ordering could not (see Dispatcher's "Retry is scheduled, not
// priority-classed" doc comment):
//
//   - A pile of permanently failing rows cannot occupy batch after batch
//     ahead of healthy rows: each pile row is ineligible for the retry
//     delay after every failure, and every row enqueued while it waits
//     sorts ahead of it.
//   - A row that failed once is reached the moment its re-claim window
//     opens, whatever the arrival rate of never-failed rows behind it: no
//     number of new rows can push its schedule slot later than the delay
//     itself.
//
// It is a read only -- it does not mark anything as in-flight -- which is
// safe because the shipped composition runs exactly one in-process
// Dispatcher; see Dispatcher's and OutboxRecord's doc comments for what a
// second concurrent dispatcher process would need. The now cutoff is
// time.Now(), computed here rather than by the database so a caller -- or
// a test seeding rows around the boundary -- reasons about the same clock
// the query compares against.
//
// # Ordering and the index serve each other
//
// retry_after is NOT NULL in the schema (migration 0007), so NULL is
// structurally impossible here: this query carries no COALESCE and no
// IS NULL escape. That matters twice over. NULL would sort differently
// on the two dialects -- SQLite orders NULLs first in ASC, PostgreSQL's
// ASC default is NULLS LAST -- so a single stray NULL row would jump the
// head of every claim poll on one engine and sit at its tail on the
// other; the schema removes the state instead of betting the ordering on
// code discipline. And with NULL gone, the ORDER BY is the bare column,
// retry_after ASC, created_at ASC -- an order key idx_
// metering_outbox_records_status_retry_after's own second column
// supplies: both engines scan the (status, retry_after) index for
// pending rows whose retry_after has arrived and stream them out in
// retry_after order, stopping at the limit, with only the created_at
// tie-break sorted among rows that share one retry_after instant (the
// never-failed majority carry retry_after == created_at, so schedule
// order and creation order coincide there). A COALESCE-wrapped order key
// could not be served by that index at all: every claim poll would
// materialize and fully sort the entire eligible set before the limit
// could return, on both dialects.
func claimPendingOutboxRecords(ctx context.Context, db *gorm.DB, limit int) ([]OutboxRecord, error) {
	now := time.Now()
	var recs []OutboxRecord
	err := db.WithContext(ctx).
		Where("status = ?", outboxStatusPending).
		Where("retry_after <= ?", now).
		Order("retry_after ASC, created_at ASC").
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

// markOutboxAttemptFailed increments id's Attempts, records cause as its
// LastError, and schedules the row's next claim at retryAfter, leaving
// Status untouched (still outboxStatusPending) so the row is retried --
// billing-grade delivery retries indefinitely, per Dispatcher's own doc
// comment. retryAfter is the caller's re-claim policy (Dispatcher passes
// the failure time plus its own retry delay): the claim query
// (claimPendingOutboxRecords) refuses the row until that moment, which is
// what spaces retries honestly and keeps a re-failed row from re-joining
// the queue head (see that function's doc comment). cause is truncated to
// fit OutboxRecord.LastError's column width, always valid UTF-8 and never
// splitting a multi-byte rune (see truncateError).
func markOutboxAttemptFailed(ctx context.Context, db *gorm.DB, id string, cause string, retryAfter time.Time) error {
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
	rec.RetryAfter = &retryAfter
	return db.WithContext(ctx).Save(&rec).Error
}

// retireDeliveredOutboxRecords deletes up to limit outbox rows that have
// been delivered and have stayed delivered past olderThan, together with
// the ingest receipt each delivered row's fold created -- the retention
// half of the outbox lifecycle (see Dispatcher's "Retention: delivered
// rows and receipts are retired" doc comment): without it both tables
// would grow without bound, delivered rows forever and receipts with
// them.
//
// What the sweep can safely delete, and only that: a row is retired only
// in the outboxStatusDelivered state (never a pending row -- pending rows
// are the retry queue, and their receipts are the dedup guard that makes
// their redelivery safe), only once it has stayed delivered for the
// whole retention window (a host's caller retrying an Enqueue whose
// answer it never saw resolves against the existing row while it lives;
// after the window both the row and its receipt are gone and a
// re-enqueued key is a genuinely new event -- the same horizon every
// outbox retention policy assumes, documented in Dispatcher's doc
// comment), and only a bounded batch per call (limit), so a large
// backlog of dead rows costs one bounded slice of one poll cycle rather
// than a full-table purge.
//
// Each row's two deletes run in one dbkit.WithTenantSession transaction
// under the row's own tenant -- the outbox delete by row id plus a
// status guard (the delete is refused when the row is no longer
// delivered the moment it runs), the receipt delete keyed by the row's
// (tenant, idempotency_key), with the tenant-scoping plugin scoping it
// to the session's tenant exactly as it scopes every other write in this
// module.
//
// The status guard is defensive redundancy, not the defense of a
// reachable race: no path in this module can stop a delivered row being
// delivered between the read and the delete. Delivery is one-way --
// markOutboxDelivered moves pending -> delivered and its own WHERE
// refuses every other state, markOutboxAttemptFailed never writes Status
// at all, and Enqueue's conflict path only reads the existing row back,
// never writes it -- so a row the sweep read as delivered is still
// delivered when the delete runs, in the shipped single-Dispatcher
// composition or under any writer this module owns. The guard stays
// because it costs
// one status predicate on the delete and keeps the delete honest should
// a future writer ever change that: a row that no longer matches is
// left alone, and its receipt with it, which is the conservative
// direction in every case. Rows are selected oldest-delivered first so
// the same rows are not re-read across calls.
//
// Returns how many rows were retired (both deletes committed). On a
// per-row error it stops and surfaces the error -- the rows that were
// not yet retired still match on the next call, so an interrupted sweep
// is convergent rather than duplicated.
func retireDeliveredOutboxRecords(ctx context.Context, db *gorm.DB, olderThan time.Time, limit int) (int, error) {
	var recs []OutboxRecord
	err := db.WithContext(ctx).
		Where("status = ? AND delivered_at <= ?", outboxStatusDelivered, olderThan).
		Order("delivered_at ASC, id ASC").
		Limit(limit).
		Find(&recs).Error
	if err != nil {
		return 0, err
	}
	retired := 0
	for _, rec := range recs {
		tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID(rec.TenantID))
		// txRetired records, inside the transaction, that this row's two
		// deletes both succeeded. The count itself accumulates only
		// OUTSIDE the transaction, once it has genuinely committed: a
		// closure error rolls the outbox delete back, and a row the
		// rollback resurrected was not retired and must not be counted.
		var txRetired bool
		err := dbkit.WithTenantSession(tenantCtx, db, func(tx *gorm.DB) error {
			res := tx.Where("id = ? AND status = ?", rec.ID, outboxStatusDelivered).Delete(&OutboxRecord{})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				// The row is no longer delivered at delete time --
				// defensive redundancy only, per the method doc's writer
				// trace. If it ever tripped, leaving the receipt is the
				// conservative direction either way: a row deleted
				// concurrently has its receipt deleted with it, and a row
				// that merely stopped being delivered still needs its
				// receipt as the dedup guard for a future delivery.
				return nil
			}
			txRetired = true
			return tx.Where("id = ?", rec.IdempotencyKey).Delete(&IngestReceipt{}).Error
		})
		if err != nil {
			return retired, err
		}
		if txRetired {
			retired++
		}
	}
	return retired, nil
}

// maxLastErrorLength mirrors OutboxRecord.LastError's column size.
const maxLastErrorLength = 500

// truncateError makes cause safe for OutboxRecord.LastError's column at
// ANY length, the rune-safe-helper shape go/sharing's
// truncateAccessLogValue and go/authn's truncateClientField already
// established: it never writes more bytes than the column can hold, and
// the stored value is always valid UTF-8. A value that is short AND valid
// passes
// through untouched; anything else is first sanitized -- invalid byte
// sequences rendered as the Unicode replacement character via
// strings.ToValidUTF8, exactly the sharing helper's choice, never
// silently dropped, since dropping bytes could concatenate two arbitrary
// byte runs into a different valid value -- and the sanitized result is
// then byte-bounded. The byte bound is the conservative direction on
// PostgreSQL, where VARCHAR(n) counts characters, and it is enforced on
// a UTF-8 boundary: a byte-level cut through a rune would leave invalid
// UTF-8 in the string, which SQLite stores happily but PostgreSQL
// refuses on the very write this truncation feeds (SQLSTATE 22021,
// invalid byte sequence for encoding "UTF8") -- taking the
// failure-record write down with the failure it was recording. The cut
// backs off to the nearest rune boundary; only the final, partial rune
// can straddle the cut, so at most three bytes ever come off.
func truncateError(cause string) string {
	if len(cause) <= maxLastErrorLength && utf8.ValidString(cause) {
		return cause
	}
	sanitized := strings.ToValidUTF8(cause, "\uFFFD")
	if len(sanitized) <= maxLastErrorLength {
		return sanitized
	}
	cut := sanitized[:maxLastErrorLength]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
