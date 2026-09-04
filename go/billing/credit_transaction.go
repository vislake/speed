package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// billingCreditTransactionsTable names the shared
// billing_credit_transactions table.
const billingCreditTransactionsTable = "billing_credit_transactions"

// CreditTransactionType is one credit_transaction row's kind, per
// docs/internal/06-billing-and-metering.md's grant/deduct/refund/expire
// vocabulary.
type CreditTransactionType string

const (
	// CreditTransactionGrant is a top-up: credits added to Available
	// directly (a plan's included credits, an admin top-up, a promotion).
	CreditTransactionGrant CreditTransactionType = "grant"
	// CreditTransactionDeduct is the reserve half of the two-phase
	// pattern: CreditService.PreDeduct moves credits from Available to
	// Reserved and inserts one row at CreditTransactionStatusPending.
	CreditTransactionDeduct CreditTransactionType = "deduct"
	// CreditTransactionRefund is not its own row: a Deduct row's
	// resolution to CreditTransactionStatusRefunded IS the refund record
	// -- see CreditService.Refund. This constant exists so the type's
	// closed vocabulary matches the design doc's four-way split even
	// though no row is ever inserted carrying it directly.
	CreditTransactionRefund CreditTransactionType = "refund"
	// CreditTransactionExpire is a single-phase deduction driven by an
	// expiry policy rather than a business operation: credits removed
	// from Available directly, confirmed immediately (see
	// CreditService.Expire). See AGENTS.md's Known limitations for what
	// this round does NOT ship: a scheduler that calls Expire on its own
	// initiative.
	CreditTransactionExpire CreditTransactionType = "expire"
)

// CreditTransactionStatus is a credit_transaction row's resolution state.
type CreditTransactionStatus string

const (
	// CreditTransactionStatusPending is a Deduct row whose reservation
	// has not yet been confirmed or refunded.
	CreditTransactionStatusPending CreditTransactionStatus = "pending"
	// CreditTransactionStatusConfirmed is a Deduct row whose reservation
	// became a permanent spend (CreditService.Confirm), or a Grant/Expire
	// row, which are single-phase and therefore always created already
	// Confirmed.
	CreditTransactionStatusConfirmed CreditTransactionStatus = "confirmed"
	// CreditTransactionStatusRefunded is a Deduct row whose reservation
	// was released back to Available (CreditService.Refund) -- the
	// refund IS this status, not a separate row (see
	// CreditTransactionRefund's own doc comment).
	CreditTransactionStatusRefunded CreditTransactionStatus = "refunded"
)

// CreditTransaction is one append-only entry in a tenant's credit ledger.
// The ledger, and only the ledger, is the authority on a tenant's credit
// history: docs/internal/06-billing-and-metering.md requires it be
// reconstructable/auditable from the transaction log alone,
// mirroring the append-only rigor go/dbkit/audit's AuditEvent already
// establishes for this codebase's other financial/compliance ledger.
//
// Unlike AuditEvent, CreditTransaction genuinely IS tenant data (every row
// belongs to exactly one tenant, with no cross-tenant reader the way
// Dispatcher needs for go/metering's OutboxRecord), so it is
// dbkit.TenantScoped and reached through dbkit.Repository[CreditTransaction]
// -- but CreditTransactionRepository does not embed that Repository the
// way every other tenant repository in this codebase does. Embedding would
// promote Repository[T]'s own Update and Delete methods onto this type,
// which is exactly the mutability an append-only ledger must not offer:
// CreditService's own compare-and-swap UPDATEs (see credit_service.go)
// touch this table directly through dbkit.WithTenantSession, the same
// sanctioned raw-SQL-escape-hatch shape go/notification's
// ContactService.consumePendingCode already uses for an identical
// CAS-not-a-plain-Update need -- never through a promoted Repository.Update
// call, and CreditTransactionRepository itself exposes only Insert and two
// read methods, mirroring audit.Repository's own "Insert, Get,
// ListByTenant, and NO Update or Delete method at all" shape.
// TestCreditTransactionRepository_HasNoUpdateOrDeleteMethod proves this
// with the identical reflection check go/dbkit/audit/model_test.go's own
// TestRepository_HasNoUpdateOrDeleteMethod uses.
type CreditTransaction struct {
	// ID is an application-generated UUID (uuid.NewString) for a Grant or
	// Expire row, or the caller's own IdempotencyKey for a Deduct row --
	// see CreditService.PreDeduct's doc comment for why a Deduct row's
	// primary key IS its idempotency key rather than a second, unrelated
	// generated id.
	//
	// Because a Deduct row's ID comes from the CALLER (its
	// IdempotencyKey), it is not globally unique across tenants -- two
	// different tenants may reasonably reuse the same idempotency-key
	// string for their own, unrelated operations. TenantID is therefore a
	// genuine second primary-key column below, exactly the composite-key
	// shape go/metering's UsageSummary and go/dbkit/audit's IngestReceipt
	// both already use for the identical reason -- NOT dbkit.TenantModel's
	// embeddable, non-primary-key field (see TenantModel's own doc
	// comment for why a globally-unique-ID model can use that shortcut
	// and this one cannot).
	ID string `gorm:"column:id;primaryKey;size:100"`

	// TenantID is a genuine composite-primary-key column -- see ID's own
	// doc comment for why.
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`

	// Type is a CreditTransactionType value.
	Type string `gorm:"column:type;size:16;not null"`

	// Status is a CreditTransactionStatus value.
	Status string `gorm:"column:status;size:16;not null"`

	// Amount is always positive: the credit count this entry moves. Which
	// direction it moves is implied by Type and Status together (a
	// Confirmed Deduct permanently removed Amount from the tenant's
	// balance; a Refunded Deduct removed nothing, net; a Grant added
	// Amount; an Expire removed Amount) -- never a signed value, so the
	// ledger's own arithmetic can never accidentally cancel two entries
	// that should not cancel.
	Amount int64 `gorm:"column:amount;not null"`

	// Reason is a short, caller-supplied note (e.g. "ai_generation:job_123",
	// "promo:welcome_2026") -- free text for now; see AGENTS.md's Known
	// limitations for why this round does not close it to an enum.
	Reason string `gorm:"column:reason;size:255;not null"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime;not null"`
}

// GetTenantID returns tx's tenant, satisfying dbkit.TenantScoped.
func (tx CreditTransaction) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(tx.TenantID) }

// TableName pins CreditTransaction to the billing_credit_transactions
// table.
func (CreditTransaction) TableName() string { return billingCreditTransactionsTable }

// compile-time check that CreditTransaction satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = CreditTransaction{}

// CreditTransactionRepository is the append-only accessor for
// billing_credit_transactions. See CreditTransaction's own doc comment for
// why this deliberately does NOT embed dbkit.Repository[CreditTransaction]
// the way every other tenant repository in this codebase does.
type CreditTransactionRepository struct {
	db *gorm.DB
}

// NewCreditTransactionRepository returns a CreditTransactionRepository
// over db. db is expected to come from dbkit.Open with this module's
// migrations applied.
func NewCreditTransactionRepository(db *gorm.DB) *CreditTransactionRepository {
	return &CreditTransactionRepository{db: db}
}

// Insert appends tx to the ledger, resolving the tenant from ctx exactly
// like dbkit.Repository[T].Create does (overwriting tx.TenantID
// regardless of what it held on entry). A duplicate ID is a genuine
// primary-key conflict -- Insert never updates an existing row, since
// Repository has no notion of "the same entry happening again" to
// reconcile (CreditService.PreDeduct's own idempotent-retry handling
// checks for the duplicate BEFORE calling Insert, inside the same
// transaction -- see that method's doc comment).
func (r *CreditTransactionRepository) Insert(ctx context.Context, tx *CreditTransaction) error {
	return dbkit.WithTenantSession(ctx, r.db, func(session *gorm.DB) error {
		return r.insert(ctx, session, tx)
	})
}

// insert is Insert's transaction-scoped core, used directly by
// CreditService's own multi-step transactions (which already hold a
// dbkit.WithTenantSession session and must not open a second, nested one).
func (r *CreditTransactionRepository) insert(ctx context.Context, session *gorm.DB, tx *CreditTransaction) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}
	tx.TenantID = string(tenant)
	if err := session.Create(tx).Error; err != nil {
		return fmt.Errorf("billing: insert credit transaction: %w", err)
	}
	return nil
}

// Get returns the CreditTransaction with the given id, for the tenant in
// ctx, or (nil, nil) when none exists. The tenant filter itself is never
// hand-written here (backend-coding-standards §3.2): CreditTransaction
// implements dbkit.TenantScoped, so dbkit's own isolation plugin injects
// "WHERE tenant_id = ?" automatically from ctx the same way it would for
// any dbkit.Repository[T] read -- this method only has to run inside a
// dbkit.WithTenantSession for the identical PostgreSQL row-level-security
// GUC coverage a Repository[T] read gets.
func (r *CreditTransactionRepository) Get(ctx context.Context, id string) (*CreditTransaction, error) {
	var out CreditTransaction
	err := dbkit.WithTenantSession(ctx, r.db, func(session *gorm.DB) error {
		return session.Where("id = ?", id).First(&out).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("billing: get credit transaction %q: %w", id, err)
	}
	return &out, nil
}

// ListByTenant returns every credit transaction for the tenant in ctx,
// newest first -- the read surface a later round's account/billing-history
// UI would call, and the reconstruction path
// docs/internal/06-billing-and-metering.md's reconstructable/auditable
// requirement names. Like Get, the tenant filter is the isolation plugin's
// own automatic injection, never hand-written here.
func (r *CreditTransactionRepository) ListByTenant(ctx context.Context) ([]CreditTransaction, error) {
	var out []CreditTransaction
	err := dbkit.WithTenantSession(ctx, r.db, func(session *gorm.DB) error {
		return session.Order("created_at DESC").Find(&out).Error
	})
	if err != nil {
		return nil, fmt.Errorf("billing: list credit transactions: %w", err)
	}
	return out, nil
}
