package metering

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// Table names, shared between the model TableName methods and the
// migrations' own header comments.
const (
	tableUsageSummaries = "metering_usage_summaries"
	tableOutboxRecords  = "metering_outbox_records"
)

// The OutboxRecord.Status vocabulary. There is no "processing" transitional
// state this round: Dispatcher.RunOnce claims a batch, attempts each row's
// delivery synchronously within that call, and either marks it
// outboxStatusDelivered or leaves it outboxStatusPending for the next
// cycle -- see Dispatcher's doc comment for why a single in-process
// dispatcher makes that safe this round, and what a second concurrent
// dispatcher process would need that this round does not build.
const (
	outboxStatusPending   = "pending"
	outboxStatusDelivered = "delivered"
)

// UsageSummary is one row of metering_usage_summaries: the aggregated
// quantity for one tenant's one feature within one calendar period
// (docs/internal/06-billing-and-metering.md's summary-storage row, the
// SQLite/PostgreSQL usage-summary table the design doc names "usage_*_summary").
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md): a usage summary is
// meaningful only inside the tenant it measures, so UsageSummary
// implements dbkit.TenantScoped, is reached only through SummaryRepository
// (which embeds dbkit.Repository[UsageSummary]), and its isolation is
// proven by tenancytest.AssertIsolated.
//
// # ID is deterministic, and TenantID is a genuine second primary-key column
//
// ID is summaryID(Feature, PeriodStart) -- deterministic, not a random
// ULID/UUID -- so Aggregator.upsertSummary can reach the one row for a
// given (tenant, feature, period) through dbkit.Repository[T].FindByID
// rather than a hand-written query, which this codebase's raw-GORM-bypass
// discipline forbids outside dbkit's own internals (root CLAUDE.md,
// "Do not use db.Table / db.Model / db.Raw to work around the
// Repository"). Because ID is derived from Feature and PeriodStart alone
// (not TenantID), it is NOT globally unique across tenants -- two
// different tenants both measuring "ai.generation" in the same calendar
// month get the same ID string -- so, unlike go/pki's Certificate (whose
// ID is an application-generated UUID and needs no help), UsageSummary
// declares TenantID as a second `primaryKey` column directly rather than
// embedding dbkit.TenantModel, giving the table a true composite primary
// key (id, tenant_id) the same way go/config's row does for its own
// non-globally-unique key. See dbkit.TenantModel's own doc comment for
// exactly this decision rule.
type UsageSummary struct {
	// ID is summaryID(Feature, PeriodStart).
	ID string `gorm:"column:id;primaryKey;size:200"`
	// TenantID is a genuine composite-primary-key column -- see the type's
	// own doc comment for why ID alone is not enough here.
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
	// Feature is the quota/billing dimension this row aggregates, the same
	// vocabulary as UsageEvent.Feature.
	Feature string `gorm:"column:feature;size:128;not null"`
	// PeriodStart and PeriodEnd bound the calendar bucket this row
	// aggregates -- the [start, end) window periodBounds computed for
	// every event folded into it.
	PeriodStart time.Time `gorm:"column:period_start;not null"`
	PeriodEnd   time.Time `gorm:"column:period_end;not null"`
	// Quantity is the sum of every UsageEvent.Quantity folded into this
	// row so far.
	Quantity  float64   `gorm:"column:quantity;not null"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// GetTenantID returns s's tenant, satisfying dbkit.TenantScoped.
func (s UsageSummary) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(s.TenantID) }

// TableName names the metering_usage_summaries table.
func (UsageSummary) TableName() string { return tableUsageSummaries }

// compile-time check that UsageSummary satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = UsageSummary{}

// OutboxRecord is one row of metering_outbox_records: the billing-grade
// tier's durable, not-yet-delivered (or already-delivered) usage event --
// see Enqueue's and Dispatcher's doc comments for the write and delivery
// halves of the outbox pattern this table exists for.
//
// # Data domain: platform data, deliberately NOT dbkit.TenantScoped
//
// This is a deliberate departure from a literal "tenant-owned table"
// reading: OutboxRecord does NOT implement dbkit.TenantScoped, is reached
// through plain *gorm.DB functions (repository.go's outbox-prefixed
// functions), and its isolation is proven by
// tenancytest.AssertNotTenantScoped, not AssertIsolated. tenant_id is a
// real, populated column -- every row genuinely belongs to one tenant --
// it is simply not the database-enforced kind.
//
// The reason is Dispatcher: outbox delivery is a background process that
// must find every tenant's pending rows to retry them, the same shape
// go/jobs' own jobRecord is platform data for (its dispatch query "must
// scan candidate Jobs across every tenant at once", per
// tools/semgrep_rules/raw-gorm-bypass.yml's own allowlist header for
// go/jobs/store.go) and the same shape go/config's row and go/dbkit's
// AuditEvent already get, per root CLAUDE.md's Repository Status census
// ("AuditEvent is platform data with a real, non-enforced tenant_id
// column, the same treatment go/jobs's jobRecord and go/config's row
// already get, never dbkit.TenantScoped"). dbkit.Repository[T] has no
// cross-tenant read path (tenancy.WithSystemContext elevates who may ask,
// not what dbkit.Repository[T] itself can see -- see that function's own
// doc comment), so a genuinely tenant-scoped OutboxRecord would leave
// Dispatcher with no sanctioned way to find pending rows across tenants at
// all. See AGENTS.md's "Outbox table: platform data, not tenant-scoped"
// section for the full argument.
type OutboxRecord struct {
	ID             string    `gorm:"column:id;primaryKey;size:36"`
	TenantID       string    `gorm:"column:tenant_id;size:64;not null"`
	Feature        string    `gorm:"column:feature;size:128;not null"`
	Quantity       float64   `gorm:"column:quantity;not null"`
	IdempotencyKey string    `gorm:"column:idempotency_key;size:200;not null"`
	OccurredAt     time.Time `gorm:"column:occurred_at;not null"`
	// Metadata is the JSON encoding of UsageEvent.Metadata, or "" when the
	// event carried none. Plain TEXT on both dialects, never a native
	// PostgreSQL array or JSONB operator target -- this column is written
	// and read whole, never filtered into.
	Metadata string `gorm:"column:metadata;not null;default:''"`
	// Status is outboxStatusPending or outboxStatusDelivered.
	Status string `gorm:"column:status;size:16;not null"`
	// Attempts counts failed delivery attempts, incremented by
	// markOutboxAttemptFailed. It never causes a row to stop being
	// retried -- see Dispatcher's doc comment: billing-grade delivery
	// retries indefinitely, it does not dead-letter.
	Attempts int `gorm:"column:attempts;not null;default:0"`
	// LastError is the most recent delivery failure's message, truncated
	// to fit the column -- never a stack trace or internal detail beyond
	// what Aggregator.Ingest's own error already reports.
	LastError   string     `gorm:"column:last_error;size:500;not null;default:''"`
	CreatedAt   time.Time  `gorm:"column:created_at;not null"`
	DeliveredAt *time.Time `gorm:"column:delivered_at"`
}

// TableName names the metering_outbox_records table.
func (OutboxRecord) TableName() string { return tableOutboxRecords }
