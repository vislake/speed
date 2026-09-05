package notification

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// tableSendRecords is the send_records table name, shared by the model's
// TableName and by the migration's own header comments.
const tableSendRecords = "send_records"

// Recipient-class values, the closed vocabulary of the
// send_records.recipient_class column and of a delivery's recipient kind.
//
// A delivery targets either a user of the platform (whose addresses the
// host resolves through its UserAddressResolver seam and whose preferences
// govern the channels) or an external contact of one tenant (whose address
// and consent live in the tenant's own verified_contacts ledger). The same
// two words travel in Dispatch.Recipient, in the job payload, and in the
// send record -- never re-typed at any boundary.
const (
	RecipientClassUser     = "user"
	RecipientClassExternal = "external"
)

// Send-record status values, the closed vocabulary of the
// send_records.status column.
//
// The record exists to make an outbound delivery's outcome observable and
// replay-safe: succeeded is written only after the transport accepted the
// send, failed after a failure exhausted a delivery attempt, and skipped
// after a deliberate non-send -- no address on file, an external contact
// whose consent lapsed (unsubscribed or bounced) -- whose reason will not
// change by retrying. The record is upserted on every attempt, so its final
// status reflects the last attempt; replay convergence is best-effort
// at-most-once -- the delivery job checks this record's succeeded state
// before any attempt, and the UNIQUE (tenant_id, idempotency_key) index
// keeps the record set under one key singular while attempts race -- but a
// crash between the transport's accept and this record's settle, or two
// attempts probing before either settles, can still double-send
// (delivery.go's deliverUserChannel doc spells the windows out).
const (
	SendRecordStatusSucceeded = "succeeded"
	SendRecordStatusFailed    = "failed"
	SendRecordStatusSkipped   = "skipped"
)

// SendRecord is one row of the outbound-delivery log: what was sent, to
// whom, through which channel, and with what outcome.
//
// # Data domain
//
// Platform data (docs/internal/04-data-and-tenancy.md's data-domain table):
// the log exists so that delivery outcomes can be replayed and audited
// across the platform -- the same row that proves to a retry that a
// delivery already succeeded must be readable by the worker whatever
// tenant context the retry carries, and a platform-level deliverability
// report reads across tenants. SendRecord therefore deliberately does NOT
// implement dbkit.TenantScoped -- no GetTenantID, no embedded
// dbkit.TenantModel -- exactly as PlatformBlacklist does not, for the
// reasons blacklist.go's doc comment spells out. Its isolation is proven
// by tenancytest.AssertNotTenantScoped.
//
// TenantID is nevertheless a real column, written with the tenant whose
// send produced the record, and unenforced: it exists so an operator can
// see each tenant's sending behaviour at a glance, the same treatment
// jobs and audit give their own real tenant columns. The schema defaults
// it to the empty-string sentinel (the audit convention); exactly one read
// filters on it -- ByTenantAndKey, whose hand-written tenant filter mirrors
// the UNIQUE index's scoped-uniqueness semantics (see that method's doc).
// The UNIQUE (tenant_id, idempotency_key) index the migration creates is
// scoped uniqueness: the same delivery key may legitimately recur across
// tenants, never within one.
//
// # Shape
//
// RecipientClass is RecipientClassUser or RecipientClassExternal, exactly
// one of RecipientUserID / ContactID names the recipient (the other stays
// the empty-string sentinel), Channel is a channel key, Status one of the
// SendRecordStatus* values, DurationMs the transport call's wall time, and
// Error the attempt's outcome text -- the raw cause text on failed
// records, a short reason on skipped ones, the empty-string sentinel on
// succeeded ones (the field's comment spells the contents out).
// IdempotencyKey is the derived delivery key (delivery.go's
// deriveDeliveryKey) that makes the whole record a replay-checkable unit.
// ProviderReceiptID is reserved for the transport provider's own message
// id (SES's message id, say), which no transport in this round returns;
// the column exists so a later transport round does not migrate.
type SendRecord struct {
	// ID is an application-generated UUID, never a database-generated one.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantID is the owning tenant of the delivery that produced the
	// record, unenforced and filtered on only by the ByTenantAndKey probe
	// -- see the doc comment above.
	TenantID string `gorm:"column:tenant_id;size:64;not null"`

	// TypeKey is the notification type key the delivery was rendered from.
	TypeKey string `gorm:"column:type_key;size:128;not null"`

	// Channel is the delivery channel the record describes.
	Channel string `gorm:"column:channel;size:16;not null"`

	// RecipientClass is RecipientClassUser or RecipientClassExternal.
	RecipientClass string `gorm:"column:recipient_class;size:16;not null"`

	// RecipientUserID names the recipient when RecipientClass is
	// RecipientClassUser; the empty-string sentinel otherwise.
	RecipientUserID string `gorm:"column:recipient_user_id;size:64;not null"`

	// ContactID names the recipient when RecipientClass is
	// RecipientClassExternal; the empty-string sentinel otherwise.
	ContactID string `gorm:"column:contact_id;size:36;not null"`

	// Status is one of the SendRecordStatus* values.
	Status string `gorm:"column:status;size:16;not null"`

	// DurationMs is the transport call's wall-clock duration, 0 when the
	// attempt never reached the transport.
	DurationMs int64 `gorm:"column:duration_ms;not null"`

	// Error is the attempt's outcome text: the raw text of the cause error
	// on failed records (a transport message, a render failure, a host-seam
	// error -- the wrap sites may interpolate identifiers such as a user
	// id, so the text is not a sanitized payload and operators treat it as
	// untrusted diagnostic text), a short reason on skipped records
	// (delivery.go's skipReason* constants), the empty-string sentinel
	// otherwise -- never a stack trace. Truncation happens at the write
	// site, to the column's 4000-char budget.
	Error string `gorm:"column:error;size:4000;not null"`

	// ProviderReceiptID is the transport provider's own message id, empty
	// until a transport returns one.
	ProviderReceiptID string `gorm:"column:provider_receipt_id;size:128;not null"`

	// IdempotencyKey is the derived delivery key the delivery job checks
	// before any attempt and writes with every outcome.
	IdempotencyKey string `gorm:"column:idempotency_key;size:128;not null"`

	// CreatedAt and UpdatedAt are populated by gorm's autoCreateTime /
	// autoUpdateTime, never by a database default (SQLite has no NOW()).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the send_records table.
func (SendRecord) TableName() string { return tableSendRecords }

// SendRecordRepository is the send_records data path.
//
// It is deliberately NOT built on dbkit.Repository[T], whose generic
// constraint requires dbkit.TenantScoped and therefore cannot compile
// against SendRecord even by accident -- the compile-time guarantee that
// this platform-domain table never acquires tenant scoping. It queries the
// plain *gorm.DB dbkit.Open returns directly, the documented pattern for
// identity and platform data (see go/dbkit/AGENTS.md's "Known
// limitations"), and never reaches for db.Table, db.Model or db.Raw.
type SendRecordRepository struct {
	db *gorm.DB
}

// NewSendRecordRepository returns a SendRecordRepository backed by db. db
// is expected to come from dbkit.Open, already migrated with this module's
// Migrations().
func NewSendRecordRepository(db *gorm.DB) *SendRecordRepository {
	return &SendRecordRepository{db: db}
}

// ByTenantAndKey reads the record for one (tenant, idempotency key) pair,
// the pair the delivery job checks before every attempt and after every
// transport call. An absent record is (nil, nil); a database failure
// surfaces raw for the caller to wrap.
//
// The tenant filter is written by hand here -- and is the one hand-written
// tenant filter in the module -- because the query targets a platform
// table whose model carries no tenant scoping, so no plugin injects the
// filter; scoping by hand is what makes the UNIQUE index's scoped-uniqueness
// semantics match the lookup that must honour them.
func (r *SendRecordRepository) ByTenantAndKey(ctx context.Context, tenantID, key string) (*SendRecord, error) {
	var row SendRecord
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND idempotency_key = ?", tenantID, key).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// Create inserts one record. The caller derives the id and the
// idempotency key; a duplicate (tenant, idempotency key) pair surfaces as
// the database's unique-index violation for the caller to reconcile.
func (r *SendRecordRepository) Create(ctx context.Context, rec *SendRecord) error {
	return r.db.WithContext(ctx).Create(rec).Error
}

// Save upserts one record by its primary key: gorm's Save updates every
// column of the row whose id matches, and inserts when none does. The
// delivery job's settle uses it so a retry's attempt overwrites the
// previous attempt's row in place -- the record keeps one id per (tenant,
// idempotency key) for the life of the delivery.
func (r *SendRecordRepository) Save(ctx context.Context, rec *SendRecord) error {
	return r.db.WithContext(ctx).Save(rec).Error
}

// SendRecordFilter narrows a ListByFilter read to one tenant's own send
// records, newest first. TenantID is required (ErrSendRecordTenantRequired
// otherwise) -- mirroring ByTenantAndKey's own explicit, hand-written
// tenant filter, since this platform table carries no plugin-injected
// tenant scoping. Every other field is optional: its zero value matches
// everything.
//
// Limit must be positive and Offset non-negative -- the identical contract
// repository.go's ListForRecipient documents for its own list surface: the
// caller resolves the spec's default and cap before calling ListByFilter,
// and nothing is silently clamped here.
type SendRecordFilter struct {
	// TenantID is the tenant to search. Required.
	TenantID string

	// Channel, when non-empty, matches SendRecord.Channel exactly.
	Channel string

	// Status, when non-empty, matches SendRecord.Status exactly (one of
	// the SendRecordStatus* values).
	Status string

	// From, when non-zero, excludes records created strictly before it.
	From time.Time

	// To, when non-zero, excludes records created strictly after it.
	To time.Time

	// Limit bounds the number of rows returned. See the type's own doc
	// comment: the caller resolves this before calling, ListByFilter never
	// clamps it.
	Limit int

	// Offset skips this many matching rows before the page ListByFilter
	// returns starts.
	Offset int
}

// ListByFilter returns the page of filter.TenantID's own send records
// matching filter, newest first (created_at DESC with id DESC as the
// tiebreak -- ListForRecipient's identical stable-paging convention, so
// two records written in the same instant still page deterministically).
// This is D10's operator-facing search (docs/internal/23-admin.md):
// "did this delivery actually go out, and what happened".
//
// This is a single-tenant read, matching D2's established mechanism for
// every other cross-tenant admin read in this codebase: a caller needing
// every tenant's records (go/admin's own HTTP handler) loops this method
// once per tenant under tenancy.WithSystemContext, rather than
// notification growing a ListAcrossTenants bypass of its own.
func (r *SendRecordRepository) ListByFilter(ctx context.Context, filter SendRecordFilter) ([]SendRecord, error) {
	if filter.TenantID == "" {
		return nil, ErrSendRecordTenantRequired
	}

	q := r.db.WithContext(ctx).Where("tenant_id = ?", filter.TenantID)
	if filter.Channel != "" {
		q = q.Where("channel = ?", filter.Channel)
	}
	if filter.Status != "" {
		q = q.Where("status = ?", filter.Status)
	}
	if !filter.From.IsZero() {
		q = q.Where("created_at >= ?", filter.From)
	}
	if !filter.To.IsZero() {
		q = q.Where("created_at <= ?", filter.To)
	}

	var rows []SendRecord
	err := q.Order("created_at DESC, id DESC").Limit(filter.Limit).Offset(filter.Offset).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}
