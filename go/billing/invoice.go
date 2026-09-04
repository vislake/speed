package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// billingInvoicesTable names the shared billing_invoices table.
const billingInvoicesTable = "billing_invoices"

// InvoiceStatus is an Invoice's lifecycle state.
type InvoiceStatus string

const (
	// InvoiceStatusOpen is an invoice awaiting payment.
	InvoiceStatusOpen InvoiceStatus = "open"
	// InvoiceStatusPaid is an invoice that has been settled in full.
	InvoiceStatusPaid InvoiceStatus = "paid"
	// InvoiceStatusVoid is an invoice that was canceled before payment
	// (e.g. a subscription canceled before its invoice was paid).
	InvoiceStatusVoid InvoiceStatus = "void"
)

// Invoice is one billing document for a Subscription's billing cycle.
// Exactly like Subscription, it is channel-agnostic: it knows nothing
// about which payment channel, if any, collected it -- no gateway
// reference, no external transaction id. A later round's billing/gateway
// package settles an Invoice from a real payment event; this round's
// InvoiceRepository is a plain Create/FindByID/Update accessor, with the
// status transition (Open -> Paid or Open -> Void) left to the caller,
// exactly like Subscription's own round-1 simplification.
type Invoice struct {
	dbkit.TenantModel

	// ID is an application-generated UUID (uuid.NewString), never a
	// database-generated one -- the backend coding standard forbids
	// gen_random_uuid().
	ID string `gorm:"column:id;primaryKey;size:36"`

	// SubscriptionID is the Subscription this invoice bills -- an ID
	// reference, never an embedded Subscription (no cross-module foreign
	// keys; this is an in-module reference, but the same discipline
	// applies for the same reason: independently evolvable rows).
	SubscriptionID string `gorm:"column:subscription_id;size:36;not null"`

	// AmountCents and Currency are Money's flattened storage columns;
	// use Amount/SetAmount to convert.
	AmountCents int64  `gorm:"column:amount_cents;not null"`
	Currency    string `gorm:"column:currency;size:3;not null"`

	// Status is an InvoiceStatus value.
	Status string `gorm:"column:status;size:16;not null"`

	// PeriodStart and PeriodEnd bound the billing cycle this invoice
	// covers.
	PeriodStart time.Time `gorm:"column:period_start;not null"`
	PeriodEnd   time.Time `gorm:"column:period_end;not null"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime;not null"`
}

// TableName pins Invoice to the billing_invoices table.
func (Invoice) TableName() string { return billingInvoicesTable }

// Amount decodes AmountCents/Currency into a Money value.
func (i Invoice) Amount() Money { return Money{Cents: i.AmountCents, Currency: i.Currency} }

// SetAmount encodes m into AmountCents/Currency.
func (i *Invoice) SetAmount(m Money) {
	i.AmountCents = m.Cents
	i.Currency = m.Currency
}

// InvoiceRepository is the tenant-scoped accessor for billing_invoices.
// Invoice is tenant data, so this embeds dbkit.Repository[Invoice] and
// inherits all three tenant-isolation layers, exactly like every other
// tenant-owned repository in this codebase.
type InvoiceRepository struct {
	*dbkit.Repository[Invoice]
}

// NewInvoiceRepository returns an InvoiceRepository over db. db is expected
// to come from dbkit.Open with this module's migrations applied.
func NewInvoiceRepository(db *gorm.DB) *InvoiceRepository {
	return &InvoiceRepository{Repository: dbkit.NewRepository[Invoice](db)}
}

// CreateInvoiceInput names a new Invoice's starting shape. It is always
// created at InvoiceStatusOpen.
type CreateInvoiceInput struct {
	SubscriptionID string
	Amount         Money
	PeriodStart    time.Time
	PeriodEnd      time.Time
}

// CreateInvoice inserts a new Invoice for the tenant in ctx, at
// InvoiceStatusOpen.
func (r *InvoiceRepository) CreateInvoice(ctx context.Context, in CreateInvoiceInput) (*Invoice, error) {
	inv := &Invoice{
		ID:             uuid.NewString(),
		SubscriptionID: in.SubscriptionID,
		Status:         string(InvoiceStatusOpen),
		PeriodStart:    in.PeriodStart,
		PeriodEnd:      in.PeriodEnd,
	}
	inv.SetAmount(in.Amount)
	if err := r.Create(ctx, inv); err != nil {
		return nil, fmt.Errorf("billing: create invoice: %w", err)
	}
	return inv, nil
}

// MarkPaid transitions id to InvoiceStatusPaid.
func (r *InvoiceRepository) MarkPaid(ctx context.Context, id string) (*Invoice, error) {
	return r.setStatus(ctx, id, InvoiceStatusPaid)
}

// Void transitions id to InvoiceStatusVoid.
func (r *InvoiceRepository) Void(ctx context.Context, id string) (*Invoice, error) {
	return r.setStatus(ctx, id, InvoiceStatusVoid)
}

func (r *InvoiceRepository) setStatus(ctx context.Context, id string, status InvoiceStatus) (*Invoice, error) {
	inv, err := r.FindByID(ctx, id)
	if err != nil {
		if isDBKitNotFound(err) {
			return nil, ErrInvoiceNotFound.WithParam("id", id)
		}
		return nil, err
	}
	inv.Status = string(status)
	if err := r.Update(ctx, inv); err != nil {
		return nil, fmt.Errorf("billing: update invoice %q: %w", id, err)
	}
	return inv, nil
}
