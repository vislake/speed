package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"go.opentelemetry.io/otel/metric"
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

// invoiceTransitions is the legal-transition table for InvoiceStatus,
// mirroring subscriptionTransitions' exact shape (subscription.go): for
// each current status, the set of statuses a transition may move to. A
// transition not listed here -- including any move out of Paid or Void,
// both terminal statuses with empty entries -- is
// ErrInvalidInvoiceTransition. Open -> Paid and Open -> Void are the two
// legal moves, and neither terminal status may ever be rewritten: an
// invoice that recorded a settled payment (Paid) or a deliberate
// cancellation (Void) is the record of that fact.
var invoiceTransitions = map[InvoiceStatus]map[InvoiceStatus]bool{
	InvoiceStatusOpen: {
		InvoiceStatusPaid: true,
		InvoiceStatusVoid: true,
	},
	InvoiceStatusPaid: {},
	InvoiceStatusVoid: {},
}

// Invoice is one billing document for a Subscription's billing cycle.
// Exactly like Subscription, it is channel-agnostic: it knows nothing
// about which payment channel, if any, collected it -- no gateway
// reference, no external transaction id. A later round's billing/gateway
// package settles an Invoice from a real payment event; this round's
// InvoiceRepository is a plain Create/FindByID/Update accessor plus the
// newest-first ListByTenant read the module's HTTP surface serves, with
// the status transition (Open -> Paid or Open -> Void) left to the
// caller, exactly like Subscription's own round-1 simplification.
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

	// db is the same connection the embedded Repository was built on, kept
	// only so setStatusIf's guarded conditional UPDATE can be composed on
	// it -- the identical dual shape SubscriptionRepository documents for
	// its own compareAndSetStatus. Every use routes through
	// dbkit.WithTenantSession against a TenantScoped destination, never a
	// raw query of any other shape.
	db *gorm.DB

	// Metric instruments (metrics.go): billing.invoice.transition and
	// billing.invoice.open_dwell, registered by NewInvoiceRepository;
	// nil for a bare struct literal, which the record site guards.
	transitionMetric metric.Int64Counter
	openDwellMetric  metric.Float64Histogram
}

// NewInvoiceRepository returns an InvoiceRepository over db. db is expected
// to come from dbkit.Open with this module's migrations applied.
func NewInvoiceRepository(db *gorm.DB) *InvoiceRepository {
	transition, openDwell := registerBillingMetrics()
	return &InvoiceRepository{
		Repository:       dbkit.NewRepository[Invoice](db),
		db:               db,
		transitionMetric: transition,
		openDwellMetric:  openDwell,
	}
}

// ListByTenant returns every invoice for the tenant in ctx, newest
// first -- the read surface the module's HTTP layer (handler.go's
// BillingListInvoices) serves its recent window from, and the read
// side a billing-history page would call directly in-process. The
// ordering key is creation order (created_at DESC), never the billed
// period: a voided document and its replacement need not share cycle
// dates, while issue order is total. Like Get, the tenant filter is
// the isolation plugin's own automatic injection from ctx, never
// hand-written here.
func (r *InvoiceRepository) ListByTenant(ctx context.Context) ([]Invoice, error) {
	var out []Invoice
	err := dbkit.WithTenantSession(ctx, r.db, func(session *gorm.DB) error {
		return session.Order("created_at DESC").Find(&out).Error
	})
	if err != nil {
		return nil, fmt.Errorf("billing: list invoices: %w", err)
	}
	return out, nil
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
	// maxTransitionAttempts bounds how many read-validate-write rounds one
	// transition may spend before giving up, exactly as
	// SubscriptionService.transition's identical constant documents for
	// subscriptions.
	const maxTransitionAttempts = 5

	for attempt := 1; attempt <= maxTransitionAttempts; attempt++ {
		inv, err := r.FindByID(ctx, id)
		if err != nil {
			if isDBKitNotFound(err) {
				return nil, ErrInvoiceNotFound.WithParam("id", id)
			}
			return nil, err
		}
		// Validate the move against the legal-transition table before the
		// invoice's Status is touched -- the identical validation
		// SubscriptionService.transition performs for subscriptions. A Void on
		// a Paid invoice (or any other move out of a terminal status) is
		// refused with ErrInvalidInvoiceTransition, never applied to the row:
		// an invoice that recorded a settled payment is the record of that
		// settlement and must not be rewritten into a voided one.
		from := InvoiceStatus(inv.Status)
		if !invoiceTransitions[from][status] {
			return nil, ErrInvalidInvoiceTransition.
				WithParam("from", string(from)).
				WithParam("to", string(status))
		}

		// The move is applied by a guarded UPDATE whose WHERE carries the
		// very status this round validated from (setStatusIf), never by a
		// whole-row save of the read. An unconditional write would let two
		// racing transitions that both validated from Open both commit -- a
		// Void landing after a MarkPaid would rewrite a settled payment's
		// record into a voided one, silently breaking the terminal-state
		// invariant above, with no error to either caller. The guard makes
		// at most one transition land; a caller whose UPDATE matched zero
		// rows has lost to a concurrent transition and loops back to
		// re-read and re-validate from the fresh status (a move that is
		// legal from it still converges; a move out of the terminal state
		// the winner committed is refused on the next round).
		applied, err := r.setStatusIf(ctx, id, from, status)
		if err != nil {
			return nil, err
		}
		if applied {
			// billing.invoice.transition / billing.invoice.open_dwell
			// (metrics.go): recorded only when the guarded UPDATE
			// genuinely applied -- a lost race applies nothing and is
			// not a transition.
			recordInvoiceTransition(ctx, r.transitionMetric, r.openDwellMetric,
				from, status, inv.CreatedAt)
			inv.Status = string(status)
			return inv, nil
		}
	}
	return nil, fmt.Errorf(
		"billing: invoice %q transition to %q did not settle after %d attempts (concurrent transitions kept winning)",
		id, status, maxTransitionAttempts)
}

// setStatusIf attempts ONE guarded status transition: an UPDATE whose WHERE
// carries both the row id and the status the move was validated from, with
// RowsAffected as the arbiter -- the identical compare-and-swap shape
// SubscriptionRepository.compareAndSetStatus uses for subscription rows
// (and CreditService's resolve for its ledger rows). It reports true only
// when the UPDATE affected exactly one row, i.e. this call is the one that
// genuinely performed the transition; a false result means the row no
// longer carried `from` by the time this UPDATE ran (a concurrent
// MarkPaid/Void won the race), never an error.
//
// The tenant filter is never hand-written here (backend-coding-standards
// §3.2): Invoice implements dbkit.TenantScoped, so the isolation plugin
// injects "WHERE tenant_id = ?" from ctx automatically, exactly like the
// identical shape SubscriptionRepository.compareAndSetStatus relies on.
func (r *InvoiceRepository) setStatusIf(ctx context.Context, id string, from, to InvoiceStatus) (bool, error) {
	applied := false
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND status = ?", id, string(from)).
			Updates(&Invoice{Status: string(to)})
		if res.Error != nil {
			return fmt.Errorf("billing: transition invoice %q: %w", id, res.Error)
		}
		applied = res.RowsAffected == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}
