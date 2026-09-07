package metering

import (
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// tableIngestReceipts names the metering_ingest_receipts table.
const tableIngestReceipts = "metering_ingest_receipts"

// IngestReceipt is one row of metering_ingest_receipts: a durable record
// that Aggregator.IngestBillingGrade has already folded one
// UsageEvent{TenantID, IdempotencyKey} into the aggregation pipeline (the
// real-time counter and the persisted UsageSummary row).
//
// # The crash window this closes
//
// Dispatcher's billing-grade delivery is two logically separate database
// operations: folding the event into the aggregation pipeline, then
// flipping the outbox row's status to delivered (markOutboxDelivered).
// Those two operations run against, in general, two independent database
// connections -- Dispatcher's own d.db and the Aggregator's own summaries
// connection, deliberately kept independent (see dispatcher_test.go's
// delivery-failure and crash-recovery tests, which simulate one dying
// while the other stays healthy) -- so nothing can make the two commit or
// roll back together.
//
// Without this table, a process crash (or merely a transient failure of
// markOutboxDelivered -- a busy/locked database, a dropped connection)
// landing between the aggregation commit and markOutboxDelivered's own
// commit leaves the outbox row "pending", so the next Dispatcher.RunOnce
// cycle reclaims it and delivers the SAME event a second time -- silently
// double-counting it in both the real-time counter and
// UsageSummary.Quantity. That is the opposite failure mode from the silent
// loss the outbox pattern exists to prevent, but equally wrong for a
// pipeline documented as "must not silently drop".
//
// IngestReceipt closes it: IngestBillingGrade inserts one receipt row
// keyed by (TenantID, IdempotencyKey) in the SAME database transaction as
// the UsageSummary upsert it guards -- Aggregator.foldIntoSummaryOnce's own
// one dbkit.WithTenantSession call, both writes composed as raw GORM calls
// against the tx it hands its callback (see upsertSummaryTx's doc comment
// for why a second, nested dbkit.Repository[T] transaction is not how this
// is done) -- so either both commit (the event was genuinely new) or
// neither does (the whole attempt failed and left nothing behind to redo).
// A second IngestBillingGrade call for the same event finds this row's
// primary key already taken: its insert ran as ON CONFLICT DO NOTHING and
// reported RowsAffected == 0, so the call recognizes the event as
// already-ingested and applies nothing -- a safe no-op whose transaction
// commits cleanly, never a unique-constraint error. That is what keeps
// the recovery working on PostgreSQL, where a statement error would abort
// the whole transaction (see foldIntoSummaryOnce's own doc comment for
// the full poisoned-transaction argument).
//
// # Data domain
//
// Tenant data, exactly like UsageSummary itself (see that type's own doc
// comment): meaningful only inside the tenant whose event it records,
// reached only through IngestReceiptRepository
// (dbkit.Repository[IngestReceipt]), isolation proven by
// tenancytest.AssertIsolated. ID is the ingested event's own
// IdempotencyKey -- not globally unique across tenants, since two
// different tenants may reuse the same caller-chosen key -- so TenantID is
// a genuine second primaryKey column, the same composite-key shape
// UsageSummary's own doc comment explains.
//
// # This table exists only for the billing-grade tier
//
// AnalyticsRecorder keeps calling the plain Ingest, never
// IngestBillingGrade -- see AnalyticsRecorder's own "No idempotency dedup
// this round" doc comment for why paying for a durable seen-key ledger
// contradicts that tier's cheap, in-memory, best-effort positioning. The
// table only ever grows from billing-grade traffic Dispatcher delivers,
// and its growth is bounded, not open-ended: Dispatcher's retention sweep
// retires each receipt together with its delivered outbox row once the
// row has stayed delivered past the retention window (see
// retireDeliveredOutboxRecords) -- a receipt lives exactly as long as the
// dedup answer it guards is still answerable against a live outbox row.
type IngestReceipt struct {
	// ID is the ingested UsageEvent's own IdempotencyKey.
	ID string `gorm:"column:id;primaryKey;size:200"`
	// TenantID is a genuine composite-primary-key column -- see the type's
	// own doc comment for why ID alone is not enough here.
	TenantID  string    `gorm:"column:tenant_id;primaryKey;size:64"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// GetTenantID returns r's tenant, satisfying dbkit.TenantScoped.
func (r IngestReceipt) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(r.TenantID) }

// TableName names the metering_ingest_receipts table.
func (IngestReceipt) TableName() string { return tableIngestReceipts }

// compile-time check that IngestReceipt satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = IngestReceipt{}

// IngestReceiptRepository is the tenant-scoped accessor for
// metering_ingest_receipts.
type IngestReceiptRepository struct {
	*dbkit.Repository[IngestReceipt]
}

// NewIngestReceiptRepository returns an IngestReceiptRepository over db.
func NewIngestReceiptRepository(db *gorm.DB) *IngestReceiptRepository {
	return &IngestReceiptRepository{Repository: dbkit.NewRepository[IngestReceipt](db)}
}
