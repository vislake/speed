package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// billingCreditBalancesTableName is billingCreditBalancesTable, spelled out
// again here because credit_service.go's raw SQL (applyBalanceDelta) names
// the table as a literal string rather than through GORM's schema
// inference (see that function's own doc comment for why).
const billingCreditBalancesTableName = billingCreditBalancesTable

// CreditService is the credits ledger's one write surface: PreDeduct,
// Confirm, Refund, Grant and Expire, implementing
// docs/internal/06-billing-and-metering.md's reserve -> confirm/refund
// pattern for "pay-per-use that might fail" business operations, plus the
// two single-phase paths (Grant, Expire).
//
// # Concurrency safety
//
// Every balance mutation is a single database-arbitrated UPDATE with an
// arithmetic WHERE guard (applyBalanceDelta below) -- never a
// read-modify-write. Two concurrent PreDeduct calls against a balance too
// small for both therefore cannot both succeed: the database itself
// serializes the two UPDATE statements, the first to commit satisfies the
// WHERE guard and the second sees the already-reduced Available and fails
// it, RowsAffected reporting 0 -- proved by
// TestCreditService_PreDeduct_ConcurrentOverBalance_OnlyOneSucceeds under
// -race.
//
// # Credits are a separate path from Entitlements.Check
//
// CreditService never consults a Plan or a Subscription, and
// EntitlementsService never touches CreditBalance --
// docs/internal/06-billing-and-metering.md's own explicit split (see
// model.go's Entitlements doc comment).
type CreditService struct {
	db           *gorm.DB
	balances     *CreditBalanceRepository
	transactions *CreditTransactionRepository
	now          func() time.Time
}

// NewCreditService returns a CreditService over db. db is expected to come
// from dbkit.Open with this module's migrations applied.
func NewCreditService(db *gorm.DB) *CreditService {
	return &CreditService{
		db:           db,
		balances:     NewCreditBalanceRepository(db),
		transactions: NewCreditTransactionRepository(db),
		now:          time.Now,
	}
}

// Balance returns the tenant's current CreditBalance, materializing a
// fresh zero-valued row on first read for a tenant that has never been
// granted or charged credits -- a caller never sees ErrRecordNotFound for
// a tenant that simply has not touched credits yet. The read itself goes
// through CreditBalanceRepository.FindByID (dbkit.Repository[T]'s own,
// fully tenant-isolated read path) once ensureBalance has guaranteed a
// row exists; only the balance-mutating methods below bypass it for
// applyBalanceDelta's raw arithmetic CAS (see that function's own doc
// comment for why).
func (s *CreditService) Balance(ctx context.Context) (*CreditBalance, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}
	ensureErr := dbkit.WithTenantSession(ctx, s.db, func(session *gorm.DB) error {
		return s.ensureBalance(session, tenant)
	})
	if ensureErr != nil {
		return nil, fmt.Errorf("billing: ensure credit balance for tenant: %w", ensureErr)
	}
	bal, err := s.balances.FindByID(ctx, string(tenant))
	if err != nil {
		return nil, fmt.Errorf("billing: read credit balance: %w", err)
	}
	return bal, nil
}

// PreDeductInput names one reservation request.
type PreDeductInput struct {
	// Amount is the credit count to reserve. Must be strictly positive.
	Amount int64
	// IdempotencyKey identifies this reservation attempt, mandatory for
	// the same reason go/metering's UsageEvent.IdempotencyKey is: it is
	// what lets a retried PreDeduct call (a caller that timed out not
	// knowing whether its first attempt committed) be told apart from a
	// second, genuinely new reservation. It becomes the resulting
	// CreditTransaction's own ID -- see PreDeduct's doc comment.
	IdempotencyKey string
	// Reason is a short, free-text note on the resulting ledger entry
	// (e.g. "ai_generation:job_123").
	Reason string
}

// PreDeduct is the reserve half of the two-phase pattern: it moves
// in.Amount credits from the tenant's Available balance to Reserved and
// inserts one CreditTransaction at CreditTransactionStatusPending, keyed
// by in.IdempotencyKey (the transaction's own ID, not a second, unrelated
// generated id -- so a retried PreDeduct with the same key can recognize
// its own earlier attempt at the database's own primary-key level, the
// same idempotency shape go/metering's IngestReceipt and Enqueue use for
// theirs).
//
// If the tenant's Available balance cannot cover in.Amount,
// ErrInsufficientCredits is returned and NOTHING is written: no
// CreditTransaction row exists for this attempt, and the balance is
// unchanged -- both the ledger insert and the balance CAS run inside one
// database transaction, so a failed reservation leaves no trace to clean
// up.
//
// A retried call with the same IdempotencyKey (the row already exists,
// whatever its current Status) returns that existing row rather than
// erroring or reserving a second time -- PreDeduct is itself idempotent
// under retry.
func (s *CreditService) PreDeduct(ctx context.Context, in PreDeductInput) (*CreditTransaction, error) {
	if in.Amount <= 0 {
		return nil, ErrInvalidAmount.WithParam("amount", in.Amount)
	}
	if in.IdempotencyKey == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var result *CreditTransaction
	txErr := dbkit.WithTenantSession(ctx, s.db, func(session *gorm.DB) error {
		row := &CreditTransaction{
			ID:     in.IdempotencyKey,
			Type:   string(CreditTransactionDeduct),
			Status: string(CreditTransactionStatusPending),
			Amount: in.Amount,
			Reason: in.Reason,
		}
		insertErr := s.transactions.insert(ctx, session, row)
		if insertErr == nil {
			if err := s.ensureBalance(session, tenant); err != nil {
				return err
			}
			ok, err := applyBalanceDelta(session, string(tenant), -in.Amount, in.Amount, s.now())
			if err != nil {
				return err
			}
			if !ok {
				return ErrInsufficientCredits.WithParam("amount", in.Amount)
			}
			result = row
			return nil
		}
		if !isUniqueViolationErr(insertErr) {
			return insertErr
		}
		// Idempotent retry: a row for this IdempotencyKey already exists.
		existing, err := s.findTransaction(session, in.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing == nil {
			// Unreachable in practice (the unique-key violation just
			// proved a row exists), but never silently swallow it into a
			// nil result.
			return insertErr
		}
		result = existing
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return result, nil
}

// Confirm settles a pending reservation permanently: the reserved credits
// are removed from Reserved (they were already removed from Available at
// PreDeduct time), and the CreditTransaction moves to
// CreditTransactionStatusConfirmed.
//
// Calling Confirm again for the same, already-confirmed idempotencyKey is
// a no-op success (the same idempotent-retry contract PreDeduct itself
// has). Calling it for a transaction already CreditTransactionStatusRefunded
// is ErrCreditTransactionAlreadyResolved: the reservation was already
// resolved the other way, and reversing that silently would double-spend
// or double-release credits.
func (s *CreditService) Confirm(ctx context.Context, idempotencyKey string) (*CreditTransaction, error) {
	return s.resolve(ctx, idempotencyKey, CreditTransactionStatusConfirmed, func(session *gorm.DB, tenantID string, amount int64, at time.Time) (bool, error) {
		return applyBalanceDelta(session, tenantID, 0, -amount, at)
	})
}

// Refund releases a pending reservation back to the tenant's Available
// balance, moving the CreditTransaction to
// CreditTransactionStatusRefunded. Its idempotent-retry and
// already-resolved contracts mirror Confirm's exactly (see that method's
// doc comment) with the two terminal statuses swapped.
func (s *CreditService) Refund(ctx context.Context, idempotencyKey string) (*CreditTransaction, error) {
	return s.resolve(ctx, idempotencyKey, CreditTransactionStatusRefunded, func(session *gorm.DB, tenantID string, amount int64, at time.Time) (bool, error) {
		return applyBalanceDelta(session, tenantID, amount, -amount, at)
	})
}

// resolve is Confirm's and Refund's shared core: compare-and-swap the
// pending transaction's Status to `to`, then apply the caller's balance
// delta. See Confirm's doc comment for the full idempotent-retry and
// already-resolved contract this implements for both callers.
func (s *CreditService) resolve(
	ctx context.Context,
	idempotencyKey string,
	to CreditTransactionStatus,
	applyDelta func(session *gorm.DB, tenantID string, amount int64, at time.Time) (bool, error),
) (*CreditTransaction, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var result *CreditTransaction
	txErr := dbkit.WithTenantSession(ctx, s.db, func(session *gorm.DB) error {
		// The tenant filter is never hand-written here (backend-coding-
		// standards §3.2): CreditTransaction implements dbkit.TenantScoped,
		// so the isolation plugin injects "WHERE tenant_id = ?" from ctx
		// automatically, the same way it would for any dbkit.Repository[T]
		// mutation.
		res := session.
			Where("id = ? AND type = ? AND status = ?",
				idempotencyKey, string(CreditTransactionDeduct), string(CreditTransactionStatusPending)).
			Updates(&CreditTransaction{Status: string(to)})
		if res.Error != nil {
			return fmt.Errorf("billing: transition credit transaction %q: %w", idempotencyKey, res.Error)
		}

		if res.RowsAffected == 1 {
			row, err := s.findTransaction(session, idempotencyKey)
			if err != nil {
				return err
			}
			if row == nil {
				return fmt.Errorf("billing: credit transaction %q vanished mid-transaction", idempotencyKey)
			}
			ok, err := applyDelta(session, string(tenant), row.Amount, s.now())
			if err != nil {
				return err
			}
			if !ok {
				// The ledger row transitioned but the balance guard did
				// not hold -- a bookkeeping inconsistency between
				// Reserved and the outstanding pending transactions, not
				// a caller error. Round 1 has no compensating recovery
				// path for this beyond surfacing it loudly; see AGENTS.md's
				// Known limitations.
				return ErrCreditBalanceInconsistent.WithParam("idempotency_key", idempotencyKey)
			}
			result = row
			return nil
		}

		// RowsAffected == 0: either no such transaction, or it is not in
		// the pending/deduct state this CAS requires. Disambiguate.
		existing, err := s.findTransaction(session, idempotencyKey)
		if err != nil {
			return err
		}
		if existing == nil || existing.Type != string(CreditTransactionDeduct) {
			return ErrCreditTransactionNotFound.WithParam("idempotency_key", idempotencyKey)
		}
		if existing.Status == string(to) {
			// Idempotent retry of a call that already succeeded.
			result = existing
			return nil
		}
		return ErrCreditTransactionAlreadyResolved.
			WithParam("idempotency_key", idempotencyKey).
			WithParam("status", existing.Status)
	})
	if txErr != nil {
		return nil, txErr
	}
	return result, nil
}

// GrantInput names one top-up.
type GrantInput struct {
	// Amount is the credit count to add. Must be strictly positive.
	Amount int64
	// Reason is a short, free-text note (e.g. "plan:pro:monthly_included",
	// "promo:welcome_2026").
	Reason string
}

// Grant adds in.Amount credits directly to the tenant's Available balance
// -- a plan's included credits, an admin top-up, a promotion. Single-phase:
// the CreditTransaction is inserted already CreditTransactionStatusConfirmed,
// there is no reservation to resolve later.
func (s *CreditService) Grant(ctx context.Context, in GrantInput) (*CreditTransaction, error) {
	if in.Amount <= 0 {
		return nil, ErrInvalidAmount.WithParam("amount", in.Amount)
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	row := &CreditTransaction{
		ID:     uuid.NewString(),
		Type:   string(CreditTransactionGrant),
		Status: string(CreditTransactionStatusConfirmed),
		Amount: in.Amount,
		Reason: in.Reason,
	}
	txErr := dbkit.WithTenantSession(ctx, s.db, func(session *gorm.DB) error {
		if err := s.ensureBalance(session, tenant); err != nil {
			return err
		}
		// A Grant's own delta can never be refused by the CAS guard (both
		// resulting buckets only grow), so its ok result is not checked
		// against a caller-facing error the way PreDeduct's and Expire's
		// are -- a false result here could only mean the balance row
		// itself vanished between ensureBalance and this call, which
		// nothing in this package's own write paths can do.
		if _, err := applyBalanceDelta(session, string(tenant), in.Amount, 0, s.now()); err != nil {
			return err
		}
		return s.transactions.insert(ctx, session, row)
	})
	if txErr != nil {
		return nil, txErr
	}
	return row, nil
}

// ExpireInput names one expiry deduction.
type ExpireInput struct {
	// Amount is the credit count to remove from Available. Must be
	// strictly positive.
	Amount int64
	// Reason is a short, free-text note (e.g. "expiry:2026-09-policy").
	Reason string
}

// Expire removes in.Amount credits from the tenant's Available balance
// directly -- a single-phase deduction driven by an expiry policy rather
// than a business operation that might fail and need refunding. The
// CreditTransaction is inserted already CreditTransactionStatusConfirmed.
//
// If Available cannot cover in.Amount, ErrInsufficientCredits is returned
// and nothing is written -- expiring more than a tenant actually holds is
// refused, not clamped to zero, so a caller's own accounting error is
// never silently absorbed.
//
// Expire is NOT idempotent under retry (its CreditTransaction.ID is a
// fresh uuid.NewString() every call, unlike PreDeduct's caller-supplied
// IdempotencyKey): this round ships the mechanism a future expiry
// scheduler would call, not the scheduler itself -- see AGENTS.md's Known
// limitations for what that later round would need to add (its own
// idempotency key, e.g. deterministic per tenant+period, the same shape
// go/storage's EnqueueExpirySweep already establishes for an identical
// "scheduled sweep must not double-apply" need).
func (s *CreditService) Expire(ctx context.Context, in ExpireInput) (*CreditTransaction, error) {
	if in.Amount <= 0 {
		return nil, ErrInvalidAmount.WithParam("amount", in.Amount)
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	row := &CreditTransaction{
		ID:     uuid.NewString(),
		Type:   string(CreditTransactionExpire),
		Status: string(CreditTransactionStatusConfirmed),
		Amount: in.Amount,
		Reason: in.Reason,
	}
	txErr := dbkit.WithTenantSession(ctx, s.db, func(session *gorm.DB) error {
		if err := s.ensureBalance(session, tenant); err != nil {
			return err
		}
		ok, err := applyBalanceDelta(session, string(tenant), -in.Amount, 0, s.now())
		if err != nil {
			return err
		}
		if !ok {
			return ErrInsufficientCredits.WithParam("amount", in.Amount)
		}
		return s.transactions.insert(ctx, session, row)
	})
	if txErr != nil {
		return nil, txErr
	}
	return row, nil
}

// ensureBalance materializes tenant's CreditBalance row at zero if it does
// not exist yet, using an INSERT ... ON CONFLICT DO NOTHING -- the same
// dialect-neutral upsert clause go/config's store.put already uses for its
// own (key, scope, tenant_id) row -- so two callers racing to touch a
// brand-new tenant's balance for the first time never both try to Create
// the same row (one wins, the other's DO NOTHING is silently a no-op,
// exactly like the balance already being there).
func (s *CreditService) ensureBalance(session *gorm.DB, tenant pkgcore.TenantID) error {
	err := session.
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoNothing: true,
		}).
		Create(&CreditBalance{
			ID:          string(tenant),
			TenantModel: dbkitTenantModel(tenant),
		}).Error
	if err != nil {
		return fmt.Errorf("billing: ensure credit balance for tenant: %w", err)
	}
	return nil
}

// findTransaction is CreditTransactionRepository.Get's transaction-scoped
// core, used by PreDeduct/Confirm/Refund so the lookup runs inside the
// SAME database transaction as the mutation around it, rather than a
// second, separate session. The tenant filter is never hand-written here
// -- see resolve's identical note -- session already carries ctx, so the
// isolation plugin injects it.
func (s *CreditService) findTransaction(session *gorm.DB, id string) (*CreditTransaction, error) {
	var out CreditTransaction
	err := session.Where("id = ?", id).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("billing: find credit transaction %q: %w", id, err)
	}
	return &out, nil
}

// applyBalanceDelta is the single primitive every CreditService balance
// mutation goes through: one database-arbitrated UPDATE that adds
// availableDelta to Available and reservedDelta to Reserved, GUARDED so
// neither bucket can go negative -- WHERE available + availableDelta >= 0
// AND reserved + reservedDelta >= 0 -- in the SAME statement that applies
// the change. This is what makes concurrent callers safe: two UPDATEs
// against the same row are serialized by the database itself (ordinary row
// locking, no dialect-specific atomic-increment feature required -- this
// is plain, portable SQL that runs identically on SQLite and PostgreSQL),
// so the second to run sees the first's already-applied change and its own
// guard evaluates against the POST-first-update values, never a stale read
// a Go-level read-modify-write could race on.
//
// A caller passes 0 for whichever delta it does not touch (PreDeduct moves
// both; Confirm/Refund move only Reserved's counterpart plus Available;
// Grant/Expire move only Available) -- see each call site.
//
// ok is false when the guard refused the update (the resulting bucket
// would have gone negative): RowsAffected is 0 in that case, and the
// caller is responsible for turning that into whatever domain error fits
// its own operation (ErrInsufficientCredits for PreDeduct/Expire; an
// apperr.Internal bookkeeping-inconsistency error for Confirm/Refund,
// which should never actually see ok=false under correct bookkeeping).
//
// # Why this is a raw Exec, not a dbkit.Repository[T] or struct-based Updates call
//
// CreditBalance implements dbkit.TenantScoped (embeds dbkit.TenantModel),
// so the isolation plugin normally auto-injects a tenant filter and forces
// tenant_id on every ORM-level Create/Update/Delete/Query -- but this
// specific mutation needs genuine server-side arithmetic
// ("available = available + ?"), which neither a struct passed to
// .Updates() (GORM's struct-based partial update silently OMITS any field
// left at its Go zero value from the SET clause -- fatal here, since
// Available or Reserved legitimately reaching exactly zero must still be
// written) nor dbkit.Repository[T]'s own Update (a full-row Save, not an
// arithmetic delta) can express. The three GORM entry points this
// codebase's raw-gorm-bypass semgrep rule flags as Repository workarounds
// are .Table/.Model/.Raw; a plain .Exec(sql, args...) call is a different,
// narrower surface that rule does not (and, per its own "Residual gaps"
// note, deliberately cannot) catch -- the exact "raw SQL escape hatch"
// backend-coding-standards SKILL.md §3.2 sanctions for a genuine need like
// this one, PROVIDED the tenant is passed explicitly (this function's own
// tenantID parameter, bound into the WHERE clause below) and the call
// carries an isolation test -- both true here
// (TestApplyBalanceDelta_ScopedToOneTenant).
//
// Because .Exec bypasses the ORM callback chain entirely, the isolation
// plugin does NOT auto-filter this statement -- unlike every other query in
// this file, tenant_id is hand-written into the WHERE clause here because
// it MUST be, not merely as defense in depth. Every call site runs inside
// dbkit.WithTenantSession, so on PostgreSQL the row-level-security GUC is
// also engaged as a second, database-level backstop underneath this
// application-level guard, the same layering every other tenant-scoped
// write in this codebase gets.
func applyBalanceDelta(session *gorm.DB, tenantID string, availableDelta, reservedDelta int64, at time.Time) (ok bool, err error) {
	res := session.Exec(
		`UPDATE `+billingCreditBalancesTableName+` `+
			`SET available = available + ?, reserved = reserved + ?, updated_at = ? `+
			`WHERE id = ? AND tenant_id = ? AND available + ? >= 0 AND reserved + ? >= 0`,
		availableDelta, reservedDelta, at,
		tenantID, tenantID, availableDelta, reservedDelta,
	)
	if res.Error != nil {
		return false, fmt.Errorf("billing: apply credit balance delta: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// isUniqueViolationErr reports whether err is a unique-constraint
// violation. dbkit.Open sets gorm.Config.TranslateError: true, so both
// dialects' drivers already translate their own raw error into gorm's
// portable gorm.ErrDuplicatedKey sentinel before this function ever sees
// it -- the identical helper go/metering's outbox.go documents in full.
func isUniqueViolationErr(err error) bool {
	return errors.Is(err, gorm.ErrDuplicatedKey)
}

// dbkitTenantModel returns a dbkit.TenantModel carrying tenant, so
// ensureBalance can populate CreditBalance's embedded field directly --
// the isolation plugin would overwrite it to the same value from ctx
// regardless (defense in depth, the identical belt-and-suspenders shape
// dbkit.Repository[T].Create itself uses), so this is never load-bearing
// on its own, only consistent with how every other Create call site in
// this package populates the field it is about to hand to a plugin that
// will re-derive it anyway.
func dbkitTenantModel(tenant pkgcore.TenantID) dbkit.TenantModel {
	return dbkit.TenantModel{TenantID: string(tenant)}
}
