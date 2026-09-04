package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// billingPaymentEventsTable names the shared billing_payment_events table.
const billingPaymentEventsTable = "billing_payment_events"

// PaymentEvent is one durable, deduplicated record of an inbound webhook
// delivery, the storage half of
// docs/internal/06-billing-and-metering.md's mandatory rule: use the
// channel's own event id as the unique key into the payment_events table,
// inserting FIRST to dedup, before any processing runs. A row's (Channel, ProviderEventID) pair is unique
// (uq_billing_payment_events_channel_event, applied in the migration) --
// PaymentEventRepository.InsertIfNew is the sanctioned way to observe that
// constraint: a caller that gets inserted=false back knows this exact event
// was already recorded by an earlier delivery attempt (the same event
// redelivered after a network retry or a provider timeout, per the design
// doc's own rationale) and must not repeat whatever side effect processing
// it the first time would have caused.
//
// # This table is genuinely per-tenant, unlike Plan's dual-domain shape
//
// Plan (plan.go) is deliberately NOT tenant-scoped because a platform-wide
// row must be visible to every tenant's lookup -- there is no such
// platform-wide face to a payment event. Every row here belongs to exactly
// one tenant's payment history (docs/internal/04-data-and-tenancy.md's
// tenant-data domain), decoded from the channel-side object's own metadata
// at verification time (NormalizedEvent.TenantID's own doc comment), so
// PaymentEvent implements dbkit.TenantScoped and is reached through
// dbkit.Repository[PaymentEvent] -- the ordinary shape every other genuinely
// tenant-owned table in this module (Subscription, Invoice, CreditBalance)
// already uses, proven with tenancytest.AssertIsolated
// (payment_event_test.go), never AssertNotTenantScoped.
//
// The uniqueness constraint on (Channel, ProviderEventID) is deliberately
// NOT also scoped by tenant_id: a channel's own event id (a Stripe event
// id, an Alipay/WeChat notify id) is already globally unique across that
// channel's entire account, so widening the key to include tenant_id would
// only let a bug that mis-decodes TenantID silently create two rows for the
// one event it actually was -- the exact double-processing dedup exists to
// prevent.
type PaymentEvent struct {
	// ID is an application-generated UUID (uuid.NewString), the table's own
	// primary key -- distinct from ProviderEventID, which is the channel's
	// identifier, not this row's.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID method
	// (satisfying dbkit.TenantScoped). ID above is already globally
	// unique (a UUID), so -- exactly like Subscription and Invoice in this
	// same module -- TenantModel's own non-primary-key tenant_id column is
	// enough; no composite key is needed.
	dbkit.TenantModel

	// Channel names which provider produced this event -- "stripe",
	// "alipay" or "wechat" this round -- matching NormalizedEvent.Channel.
	Channel string `gorm:"column:channel;size:32;not null"`

	// ProviderEventID is the channel's own event id. Together with
	// Channel, this is the dedup key -- see the type's own doc comment.
	ProviderEventID string `gorm:"column:provider_event_id;size:255;not null"`

	// ChannelReference is the channel-side object this event concerns (a
	// Stripe Checkout Session id, an Alipay/WeChat out_trade_no).
	ChannelReference string `gorm:"column:channel_reference;size:255;not null"`

	// SubscriptionID and InvoiceID are the internal ids
	// NormalizedEvent.SubscriptionID/InvoiceID decoded from the channel's
	// own metadata -- ID references only, never an embedded Subscription
	// or Invoice (backend-coding-standards: no cross-module, and no
	// intra-module, foreign keys).
	SubscriptionID string `gorm:"column:subscription_id;size:36;not null"`
	InvoiceID      string `gorm:"column:invoice_id;size:36;not null"`

	// EventType is a NormalizedEventType value.
	EventType string `gorm:"column:event_type;size:32;not null"`
	// Status is a ChannelStatus value.
	Status string `gorm:"column:status;size:16;not null"`

	// AmountCents and Currency are Money's flattened storage columns; use
	// Amount/SetAmount to convert. Zero-valued for an event type that
	// carries no amount (NormalizedEventSubscriptionCanceled).
	AmountCents int64  `gorm:"column:amount_cents;not null"`
	Currency    string `gorm:"column:currency;size:3;not null"`

	// OccurredAt is when the channel says this event happened, decoded
	// from the payload -- not when this row was inserted.
	OccurredAt time.Time `gorm:"column:occurred_at;not null"`

	// RawPayload is the exact, already signature-verified webhook body --
	// the audit trail docs/internal/06-billing-and-metering.md's own
	// callbacks-cannot-be-trusted section implies is necessary: if a later
	// dispute needs to
	// know exactly what a channel sent, this is the record, independent of
	// however this row's own typed columns interpreted it.
	RawPayload []byte `gorm:"column:raw_payload;not null"`

	// ProcessedAt is nil until whatever later round drives Subscription/
	// Invoice transitions from this row marks it processed. This round
	// never sets it -- see AGENTS.md's Known limitations: this round ships
	// the insert-first-dedup half of the rule and the audit row, not the
	// processing loop that would consume it (no HTTP surface exists yet to
	// receive a live webhook in the first place).
	ProcessedAt *time.Time `gorm:"column:processed_at"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime;not null"`
}

// TableName pins PaymentEvent to the billing_payment_events table.
func (PaymentEvent) TableName() string { return billingPaymentEventsTable }

// Amount decodes AmountCents/Currency into a Money value.
func (e PaymentEvent) Amount() Money { return Money{Cents: e.AmountCents, Currency: e.Currency} }

// SetAmount encodes m into AmountCents/Currency.
func (e *PaymentEvent) SetAmount(m Money) {
	e.AmountCents = m.Cents
	e.Currency = m.Currency
}

// PaymentEventRepository is the tenant-scoped accessor for
// billing_payment_events. PaymentEvent is tenant data, so this embeds
// dbkit.Repository[PaymentEvent] and inherits all three tenant-isolation
// layers, exactly like every other tenant-owned repository in this
// codebase.
type PaymentEventRepository struct {
	*dbkit.Repository[PaymentEvent]

	// db is the same connection the embedded Repository[PaymentEvent] was
	// built on, kept only so listPending below can compose its own
	// filtered query on it -- the identical shape go/storage's
	// ObjectRepository uses for its own listExpiredUploads/
	// listExpiredCompleted queries. Every use routes through
	// dbkit.WithTenantSession and a dbkit.TenantScoped destination; nothing
	// in this file issues an unprotected statement.
	db *gorm.DB
}

// NewPaymentEventRepository returns a PaymentEventRepository over db. db is
// expected to come from dbkit.Open with this module's migrations applied.
func NewPaymentEventRepository(db *gorm.DB) *PaymentEventRepository {
	return &PaymentEventRepository{Repository: dbkit.NewRepository[PaymentEvent](db), db: db}
}

// Get returns the PaymentEvent with the given id, for the tenant in ctx --
// the read surface a later round's payment-history or admin surface calls,
// translating dbkit's own not-found sentinel into ErrPaymentEventNotFound
// exactly like Subscription.Get and InvoiceRepository.setStatus already do
// for their own tables.
func (r *PaymentEventRepository) Get(ctx context.Context, id string) (*PaymentEvent, error) {
	evt, err := r.FindByID(ctx, id)
	if err != nil {
		if isDBKitNotFound(err) {
			return nil, ErrPaymentEventNotFound.WithParam("id", id)
		}
		return nil, err
	}
	return evt, nil
}

// listPending returns every ChannelStatusPending PaymentEvent row of the
// caller's tenant whose OccurredAt is older than before -- the rows the
// active-polling fallback (job.go's PollingService) re-queries. Ordered
// oldest first so a bounded run (limit below) makes progress on the
// longest-stuck rows before the newest ones.
func (r *PaymentEventRepository) listPending(ctx context.Context, before time.Time, limit int) ([]PaymentEvent, error) {
	var rows []PaymentEvent
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("status = ? AND occurred_at < ?", string(ChannelStatusPending), before).
			Order("occurred_at ASC").
			Limit(limit).
			Find(&rows).Error
	})
	if err != nil {
		return nil, fmt.Errorf("billing: list pending payment events: %w", err)
	}
	return rows, nil
}

// markStatus updates one PaymentEvent row's Status to status, for the
// caller's tenant. Used by the polling fallback to record what QueryStatus
// found -- see PollingService.Poll's own doc comment for why this is the
// row's own Status the poll updates, never Subscription or Invoice
// directly (this round's own scope boundary: no processing loop exists yet
// that would drive those transitions from a PaymentEvent row -- see
// AGENTS.md's Known limitations).
func (r *PaymentEventRepository) markStatus(ctx context.Context, id string, status ChannelStatus) error {
	// The tenant filter is never hand-written here (backend-coding-
	// standards §3.2): PaymentEvent implements dbkit.TenantScoped, so the
	// isolation plugin injects "WHERE tenant_id = ?" from ctx
	// automatically. Updates is called with a struct pointer directly,
	// never through .Model/.Table/.Raw -- the identical sanctioned shape
	// CreditService's own CAS transitions use (credit_service.go).
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("id = ?", id).Updates(&PaymentEvent{Status: string(status)}).Error
	})
	if err != nil {
		return fmt.Errorf("billing: mark payment event %q status: %w", id, err)
	}
	return nil
}

// InsertIfNew inserts evt and reports (true, nil), unless a row already
// exists for evt's (Channel, ProviderEventID) pair -- the unique index the
// migration applies -- in which case it reports (false, nil) and leaves the
// existing row untouched. evt.ID is generated when left empty.
//
// This is the sanctioned way to satisfy
// docs/internal/06-billing-and-metering.md's insert-first-to-dedup rule: a
// caller MUST call this before running any side effect the event implies
// (driving a Subscription/Invoice transition, granting credits), and must
// treat inserted=false exactly like "already handled, answer success and do
// nothing else" -- every channel redelivers the same event on retry or
// timeout, and re-running the side effect on a redelivery is the double-
// charge/double-grant bug the rule exists to prevent.
//
// ctx must carry the tenant NormalizedEvent.TenantID decoded to (see that
// field's own doc comment) -- dbkit.Repository[T].Create resolves and
// stamps the tenant from ctx exactly like every other Repository[T].Create
// call in this codebase, never from evt.TenantID as given.
func (r *PaymentEventRepository) InsertIfNew(ctx context.Context, evt *PaymentEvent) (bool, error) {
	if evt.ID == "" {
		evt.ID = uuid.NewString()
	}
	err := r.Create(ctx, evt)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return false, nil
	}
	return false, fmt.Errorf("billing: insert payment event: %w", err)
}
