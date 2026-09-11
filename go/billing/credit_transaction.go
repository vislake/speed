package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// billingCreditTransactionsTable names the shared
// billing_credit_transactions table.
const billingCreditTransactionsTable = "billing_credit_transactions"

// CreditTransactionType is one credit_transaction row's kind: grant,
// deduct, refund or expire.
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
	// CreditService.Expire). A scheduler that calls Expire on its own
	// initiative is not shipped -- Expire is the at-most-once write such
	// a scheduler needs, not the schedule. The row's ID is a fresh UUID
	// for an unkeyed Expire or the caller's own IdempotencyKey for a
	// keyed one -- CreditService.Expire's doc comment has that contract.
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
// history: it must be reconstructable/auditable from the transaction log
// alone, mirroring the append-only rigor go/dbkit/audit's AuditEvent
// already establishes for this codebase's other financial/compliance
// ledger.
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
	// an unkeyed Expire row, or the caller's own IdempotencyKey for a
	// Deduct row or a keyed Expire row -- see CreditService.PreDeduct's
	// and CreditService.Expire's doc comments for why those rows' primary
	// keys ARE their idempotency keys rather than a second, unrelated
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
	// "promo:welcome_2026"), declared a bounded phrase: every write path
	// (CreditService's PreDeduct/Grant/Expire) validates it through
	// validateReason before anything is written, so a row here can only
	// ever carry the module's declared phrase shape -- ASCII letters and
	// digits joined by ":" "_" or "-", at most 255 characters (the column's
	// own width, size:255). The constraint is not closed to an enum --
	// consumers name their own tags, job ids and policy periods -- but it
	// is closed to prose; see CreditService's own doc comment for the full
	// rationale (the same text travels verbatim into the audit trail).
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
// reconcile. It is the strict insert (the idempotent-retry callers --
// PreDeduct's reserve half and a keyed Expire -- go through the separate
// insertIdempotent core, which reports a duplicate as a no-op answer
// rather than an error -- see that method's doc comment); every other
// caller's rows (Grant, an unkeyed Expire) carry fresh uuid.NewString()
// ids that must never silently collide.
func (r *CreditTransactionRepository) Insert(ctx context.Context, tx *CreditTransaction) error {
	return dbkit.WithTenantSession(ctx, r.db, func(session *gorm.DB) error {
		return r.insert(ctx, session, tx)
	})
}

// insert is Insert's transaction-scoped core, used directly by
// CreditService's own multi-step transactions (which already hold a
// dbkit.WithTenantSession session and must not open a second, nested one).
// It is the strict core: a duplicate (id, tenant_id) is a genuine
// primary-key conflict, returned as an error, never reconciled -- the
// right answer for Grant and for an unkeyed Expire, whose rows carry
// fresh uuid.NewString() ids with no idempotent-retry contract of their
// own. A KEYED Expire (CreditService.Expire with PreDeductInput.IdempotencyKey
// set) inserts through insertIdempotent instead, exactly like PreDeduct's
// reserve half.
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

// insertIdempotent is insert's idempotent-retry twin, used by
// CreditService.PreDeduct's reserve half -- the one insert whose duplicate
// is a normal, expected outcome rather than an error (a retried PreDeduct
// with the same caller-supplied IdempotencyKey meets its own earlier
// attempt's row). It reports a duplicate (id, tenant_id) as inserted==false
// with NO error, because the insert runs as ON CONFLICT DO NOTHING -- never
// as a unique-constraint error. That matters because session is PreDeduct's
// own still-open dbkit.WithTenantSession transaction: on PostgreSQL a
// unique-violation error would leave that transaction aborted, and neither
// this method's own read-back of the existing row nor anything else in the
// transaction could run another statement on it (SQLSTATE 25P02, and a
// COMMIT would be turned into a ROLLBACK). SQLite tolerates a failed
// statement inside an open transaction; PostgreSQL does not, which is
// exactly why the insert must never raise a unique-violation error there
// -- the identical poisoned-transaction hazard go/metering's outbox.go
// documents and closes the same way (see that file's own insertOutboxRecord
// doc comment).
func (r *CreditTransactionRepository) insertIdempotent(ctx context.Context, session *gorm.DB, tx *CreditTransaction) (inserted bool, err error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return false, err
	}
	tx.TenantID = string(tenant)
	res := session.
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(tx)
	if res.Error != nil {
		return false, fmt.Errorf("billing: insert credit transaction: %w", res.Error)
	}
	// RowsAffected distinguishes the fresh insert (1) from the no-op the
	// ON CONFLICT DO NOTHING became for a duplicate (0) -- on both
	// dialects, without ever having raised a statement error.
	return res.RowsAffected == 1, nil
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
// newest first -- the read surface an account/billing-history UI calls,
// and the reconstruction path the reconstructable/auditable requirement
// names. Like Get, the tenant filter is the isolation plugin's own
// automatic injection, never hand-written here.
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
