package billing

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// billingCreditBalancesTableName is billingCreditBalancesTable, spelled out
// again here because credit_service.go's raw SQL (applyBalanceDelta) names
// the table as a literal string rather than through GORM's schema
// inference (see that function's own doc comment for why).
const billingCreditBalancesTableName = billingCreditBalancesTable

// CreditService is the credits ledger's one write surface: PreDeduct,
// Confirm, Refund, Grant and Expire, implementing the reserve ->
// confirm/refund pattern for "pay-per-use that might fail" business
// operations, plus the two single-phase paths (Grant, Expire).
//
// # The Reason contract
//
// Every one of the three inputs that accepts a Reason (PreDeductInput,
// GrantInput, ExpireInput) declares it a bounded phrase, enforced by
// validateReason before anything is written: a non-empty reason must be a
// phrase of ASCII letters and digits joined by ":" "_" or "-" (the shape
// of this module's own documented vocabulary -- "ai_generation:job_123",
// "plan:pro:monthly_included", "expiry:2026-09-policy"), at most 255
// characters. The constraint is not stylistic: the same reason is copied
// verbatim into the audit trail's changes column (emitCreditAudit), and
// go/dbkit/audit's Diff content contract (its emit.go doc comment)
// forbids free text there -- a reason is machine-readable annotation with
// a declared shape, never prose a caller could slip PII into, so nothing
// that reaches the permanent audit column was ever free-form.
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
// EntitlementsService never touches CreditBalance -- the explicit split of
// the credits path from the entitlements judgment (see model.go's
// Entitlements doc comment).
type CreditService struct {
	db           *gorm.DB
	balances     *CreditBalanceRepository
	transactions *CreditTransactionRepository
	now          func() time.Time

	// events and auditActions are wired post-construction by module.go's
	// Register, exactly the way PlanService.events and
	// SubscriptionService.events already are (plan.go, subscription.go) --
	// NewCreditService's own signature stays untouched so every existing
	// call site (every unit test in this package, plus module.go's own
	// construction, which happens before Register ever runs) keeps
	// compiling unchanged. events is nil until Register runs, which is
	// also every unit test's own condition in this file: emitCreditAudit's
	// nil check below means a bare *CreditService built directly through
	// NewCreditService for a test never attempts to call audit.Emit at
	// all, mirroring examples/reference-app/internal/notes.Handler's
	// identical bus == nil short-circuit for the same reason.
	events       pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar

	// testHookAfterBalanceDelta, when non-nil, is invoked synchronously by
	// Grant right after applyBalanceDelta has applied this call's own
	// delta and before readBalanceForAudit reads it back -- still inside
	// the same open, not-yet-committed transaction. Nil in every
	// production path (module.go's Register never sets it) and in every
	// pre-existing test; it exists purely so
	// TestCreditService_Grant_ConcurrentGrantForSameTenant_BlocksUntilPriorTransactionCommits
	// can deterministically pause one Grant mid-transaction and prove a
	// second, concurrent Grant for the SAME tenant cannot commit inside
	// that window -- the exact property that makes reading the resulting
	// balance INSIDE the transaction (readBalanceForAudit) safe, where a
	// separate query issued only after commit would not be.
	testHookAfterBalanceDelta func()
}

// reasonMaxRunes bounds a non-empty credit-operation reason at the ledger
// column's own width (CreditTransaction.Reason -- gorm size:255, and the
// migrations' VARCHAR(255); see go/dbkit/audit's column-bounds discussion
// for why a width the schema declares is enforced in Go too, on both
// dialects alike, rather than left to PostgreSQL's 22001 to refuse
// mid-transaction). The reason pattern below is ASCII-only, so byte length
// and rune count are the same value.
const reasonMaxRunes = 255

// reasonPhrasePattern is the declared shape of a valid credit-operation
// reason: one or more ASCII letter-or-digit segments joined by ":", "_" or
// "-" -- "ai_generation:job_123", "plan:pro:monthly_included",
// "expiry:2026-09-policy". The shape admits this module's whole documented
// vocabulary (domain tags, job ids, policy periods) and nothing prose-like:
// no whitespace, no punctuation outside the three separators, no way to
// write an email address, a name or a sentence.
var reasonPhrasePattern = regexp.MustCompile(`^[A-Za-z0-9]+(?:[:_-][A-Za-z0-9]+)*$`)

// validateReason enforces the module's declared bounded-phrase constraint
// on a credit-operation reason (see CreditService's own doc comment for
// the contract's full rationale). An empty reason is legal -- Reason is
// optional on every input. A non-empty reason violating the phrase shape
// or the reasonMaxRunes bound is refused with ErrInvalidReason, before any
// database work: the same text would otherwise land verbatim in the audit
// trail's changes column, which dbkit/audit's Diff content contract
// (go/dbkit/audit/emit.go) holds to a no-free-text standard.
func validateReason(reason string) error {
	if reason == "" {
		return nil
	}
	if len(reason) > reasonMaxRunes || !reasonPhrasePattern.MatchString(reason) {
		return ErrInvalidReason
	}
	return nil
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

// Transactions returns the tenant's credit ledger rows, newest first --
// every CreditTransaction the tenant's balance movements ever wrote, the
// reconstructable/auditable ledger that is the authority on the tenant's
// credit history (Balance above answers the number those rows produced;
// this answers the rows themselves). The read is tenant-scoped exactly
// like Balance's: it goes through CreditTransactionRepository.ListByTenant
// (dbkit's isolation plugin injecting the tenant filter from ctx, never a
// hand-written WHERE), so a caller can only ever see its own tenant's
// rows.
//
// The method is this module's credit-history read surface, added for the
// HTTP layer (handler.go's BillingListCreditTransactions serves its
// newest-first result, narrowed to the fragment's recent window) and the
// read side a billing-history UI would call directly in-process. It
// performs no write and no ensure-balance materialization: a tenant that
// has never touched credits answers an empty list, never a missing-row
// error.
func (s *CreditService) Transactions(ctx context.Context) ([]CreditTransaction, error) {
	rows, err := s.transactions.ListByTenant(ctx)
	if err != nil {
		return nil, err
	}
	return rows, nil
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
	// Reason is a short, machine-readable note on the resulting ledger
	// entry (e.g. "ai_generation:job_123"), declared a bounded phrase and
	// validated by validateReason -- see CreditService's own doc comment
	// for the declared shape and why the constraint exists. Optional.
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
//
// Keeping a reservation settleable is the caller's obligation: the module
// performs no library-side reclamation of stuck pending reservations -- a
// Pending row whose owner never Confirms or Refunds it (a caller that
// died mid-flight, a job that was never reaped) sits in Reserved forever,
// with no expiry and no sweep of this module's own. The reference app
// meets that obligation with its own durable job-id-to-key mapping and a
// scheduled settlement sweep (examples/reference-app/internal/smilesim's
// ReservationStore/ReconcileOutstandingCredits), and every consumer that
// reserves credits must arrange an equivalent path of its own.
func (s *CreditService) PreDeduct(ctx context.Context, in PreDeductInput) (*CreditTransaction, error) {
	if in.Amount <= 0 {
		return nil, ErrInvalidAmount.WithParam("amount", in.Amount)
	}
	if in.IdempotencyKey == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	if err := validateReason(in.Reason); err != nil {
		return nil, err
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var result *CreditTransaction
	// reserved is set true only inside the fresh-insert branch below --
	// never on the idempotent-retry branch that finds an already-existing
	// row. This is what lets the audit.Emit call after the transaction
	// commits fire exactly once per genuine reservation, never a second
	// time for a retried call that reserved nothing new: an audit record
	// must never be written for something that did not actually happen
	// this call, and a harmless no-op retry is as much "nothing happened"
	// as an outright failure.
	var reserved bool
	var resultBalance *CreditBalance
	txErr := dbkit.WithTenantSession(ctx, s.db, func(session *gorm.DB) error {
		row := &CreditTransaction{
			ID:     in.IdempotencyKey,
			Type:   string(CreditTransactionDeduct),
			Status: string(CreditTransactionStatusPending),
			Amount: in.Amount,
			Reason: in.Reason,
		}
		// The insert runs as ON CONFLICT DO NOTHING and reports a duplicate
		// (id, tenant_id) as inserted==false with NO error (see
		// insertIdempotent's own doc comment for why that matters): the
		// transaction stays healthy, and the read-back below -- the
		// idempotent retry's answer -- runs on it either way. The insert
		// must never raise a unique-violation error: on PostgreSQL the
		// violation aborts the whole transaction (SQLSTATE 25P02), the
		// read-back could not run on it, and a retried PreDeduct whose
		// first attempt had already committed would return an error
		// instead of its own earlier reservation -- the money path failing
		// on the one dialect SQLite's tolerance of a failed statement
		// inside a transaction never exposes (proven against real
		// PostgreSQL by
		// go/billing/integration_test/postgres_credit_transactions_test.go).
		inserted, insertErr := s.transactions.insertIdempotent(ctx, session, row)
		if insertErr != nil {
			return insertErr
		}
		if inserted {
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
			reserved = true
			resultBalance = s.readBalanceForAudit(ctx, session, string(tenant))
			return nil
		}
		// Idempotent retry: a row for this IdempotencyKey already exists --
		// the transaction is still healthy (nothing aborted), so read the
		// existing row back on it and return that instead of erroring or
		// reserving a second time.
		existing, err := s.findTransaction(session, in.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing == nil {
			// The conflicting row vanished between the no-op insert and
			// this read-back -- only a concurrent deleter could do that,
			// and nothing in this module deletes credit-transaction rows
			// (the ledger is append-only; this repository offers no Delete
			// at all). Unreachable in practice, but never silently swallow
			// it into a nil result: the honest answer is a coded error a
			// caller can retry on, exactly like go/metering's own
			// errOutboxConflictRowVanished corner.
			return errCreditLedgerRowVanished
		}
		result = existing
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	if reserved {
		s.emitCreditAudit(ctx, AuditActionCreditDeductReserve, tenant, result.ID, in.Amount, in.Reason, resultBalance)
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
	return s.resolve(ctx, idempotencyKey, CreditTransactionStatusConfirmed, AuditActionCreditDeductConfirm, func(session *gorm.DB, tenantID string, amount int64, at time.Time) (bool, error) {
		return applyBalanceDelta(session, tenantID, 0, -amount, at)
	})
}

// Refund releases a pending reservation back to the tenant's Available
// balance, moving the CreditTransaction to
// CreditTransactionStatusRefunded. Its idempotent-retry and
// already-resolved contracts mirror Confirm's exactly (see that method's
// doc comment) with the two terminal statuses swapped.
func (s *CreditService) Refund(ctx context.Context, idempotencyKey string) (*CreditTransaction, error) {
	return s.resolve(ctx, idempotencyKey, CreditTransactionStatusRefunded, AuditActionCreditRefund, func(session *gorm.DB, tenantID string, amount int64, at time.Time) (bool, error) {
		return applyBalanceDelta(session, tenantID, amount, -amount, at)
	})
}

// resolve is Confirm's and Refund's shared core: compare-and-swap the
// pending transaction's Status to `to`, then apply the caller's balance
// delta. See Confirm's doc comment for the full idempotent-retry and
// already-resolved contract this implements for both callers. auditAction
// is the caller-specific action name (AuditActionCreditDeductConfirm or
// AuditActionCreditRefund) recorded through audit.Emit -- but only when
// this call is the one that genuinely performed the CAS transition (see
// the resolved flag below); a retried call that lands on the "already in
// state `to`" idempotent branch changed nothing this time and must not
// produce a second audit record for the same underlying operation.
func (s *CreditService) resolve(
	ctx context.Context,
	idempotencyKey string,
	to CreditTransactionStatus,
	auditAction string,
	applyDelta func(session *gorm.DB, tenantID string, amount int64, at time.Time) (bool, error),
) (*CreditTransaction, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var result *CreditTransaction
	var resolved bool
	var resultBalance *CreditBalance
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
				// a caller error. No compensating recovery path exists
				// for this beyond surfacing it loudly.
				return ErrCreditBalanceInconsistent.WithParam("idempotency_key", idempotencyKey)
			}
			result = row
			resolved = true
			resultBalance = s.readBalanceForAudit(ctx, session, string(tenant))
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
	if resolved {
		s.emitCreditAudit(ctx, auditAction, tenant, result.ID, result.Amount, result.Reason, resultBalance)
	}
	return result, nil
}

// GrantInput names one top-up.
type GrantInput struct {
	// Amount is the credit count to add. Must be strictly positive.
	Amount int64
	// Reason is a short, machine-readable note (e.g.
	// "plan:pro:monthly_included", "promo:welcome_2026"), declared a
	// bounded phrase and validated by validateReason -- see CreditService's
	// own doc comment for the declared shape and why the constraint
	// exists. Optional.
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
	if err := validateReason(in.Reason); err != nil {
		return nil, err
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
	var resultBalance *CreditBalance
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
		if s.testHookAfterBalanceDelta != nil {
			s.testHookAfterBalanceDelta()
		}
		if err := s.transactions.insert(ctx, session, row); err != nil {
			return err
		}
		resultBalance = s.readBalanceForAudit(ctx, session, string(tenant))
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	s.emitCreditAudit(ctx, AuditActionCreditGrant, tenant, row.ID, in.Amount, in.Reason, resultBalance)
	return row, nil
}

// ExpireInput names one expiry deduction.
type ExpireInput struct {
	// Amount is the credit count to remove from Available. Must be
	// strictly positive.
	Amount int64
	// IdempotencyKey optionally names this expiry run. When set, it
	// becomes the resulting CreditTransaction's own ID -- the identical
	// ledger-ID-as-idempotency-key shape PreDeduct's mandatory
	// IdempotencyKey uses -- and Expire's retry contract becomes
	// idempotent: a retried call with the same key (a jobs-driven expiry
	// sweep rerunning its own window after a crash or a timeout, deriving
	// a deterministic per-tenant+period key the way go/storage's
	// EnqueueExpirySweep does) is answered with the first call's own row
	// and applies NO second deduction (see Expire's own doc comment for
	// the full contract, including which existing rows count as the
	// retry's own earlier run). Empty (the default) keeps Expire's legacy
	// single-phase shape: a fresh uuid.NewString() transaction id per
	// call, not idempotent under retry -- the right mode for a one-off,
	// operator-driven expiry, wrong for any caller that may retry.
	IdempotencyKey string
	// Reason is a short, machine-readable note (e.g.
	// "expiry:2026-09-policy"), declared a bounded phrase and validated by
	// validateReason -- see CreditService's own doc comment for the
	// declared shape and why the constraint exists. Optional.
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
// # Idempotency: the caller's choice, keyed by ExpireInput.IdempotencyKey
//
// An unkeyed Expire (IdempotencyKey empty) is NOT idempotent under retry:
// its CreditTransaction.ID is a fresh uuid.NewString() every call, so a
// retried call -- a scheduler that crashed after its first attempt
// committed, rerunning without knowing -- would deduct a second time.
// That is the one-off, operator-driven shape: an ad-hoc policy deduction
// naming its own reason, deliberately never the shape a retrying caller
// may use.
//
// A keyed Expire is the contract a jobs-driven expiry sweep needs: the
// key becomes the row's own ID, and the insert runs through the same ON
// CONFLICT DO NOTHING core PreDeduct's reserve half uses
// (insertIdempotent) -- never a unique-constraint error, so the retry's
// transaction stays healthy on both dialects, the identical reasoning
// PreDeduct documents for its own insert-first shape. A retried call with
// the same key -- the deterministic per-tenant+period key a sweep derives
// for the window it is rerunning, the shape go/storage's EnqueueExpirySweep
// establishes for an identical "scheduled sweep must not double-apply"
// need -- finds its first call's row already present and returns it
// unchanged, applying NO second deduction and recording NO second audit
// event. Only an existing CreditTransactionExpire row can be the retry's
// own earlier run: the ledger's row-ID namespace is shared across every
// row type, and a key that names a row of another kind (a Grant, a Deduct,
// an unkeyed row's UUID) is refused with ErrIdempotencyKeyCollision rather
// than reported as a successful expiry it was not. A keyed retry whose
// FIRST attempt was refused with ErrInsufficientCredits is a fresh attempt
// again, not a retry: the refused call's whole transaction rolled back,
// its inserted row included, so the key was never burned and a later,
// better-funded run with the same key proceeds normally.
//
// What this method deliberately does NOT ship is the sweep itself --
// deciding WHICH credits expire, when, in what amount, on what schedule is
// product policy this module's data model cannot anchor (no
// grant-vintage or expiry-window data exists in the ledger), and the
// scheduler that runs such a policy is a host's jobs wiring. This method
// supplies the at-most-once write that scheduler needs, never the policy.
func (s *CreditService) Expire(ctx context.Context, in ExpireInput) (*CreditTransaction, error) {
	if in.Amount <= 0 {
		return nil, ErrInvalidAmount.WithParam("amount", in.Amount)
	}
	if err := validateReason(in.Reason); err != nil {
		return nil, err
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var result *CreditTransaction
	// expired is set true only on the branch that genuinely appended this
	// call's own expiry row: the keyed path's duplicate branch (a retried
	// sweep run finding its first call's row already present) changed
	// nothing THIS call, and emitCreditAudit below must not record a
	// second event for an operation that did not happen again -- the
	// identical reserved-flag discipline PreDeduct's reserve half
	// documents for its own retry branch.
	var expired bool
	var resultBalance *CreditBalance
	txErr := dbkit.WithTenantSession(ctx, s.db, func(session *gorm.DB) error {
		if in.IdempotencyKey == "" {
			// Unkeyed: the legacy single-phase shape -- a fresh UUID per
			// call, the balance CAS followed by a strict insert, all in
			// one transaction. Not idempotent under retry by design; see
			// Expire's own doc comment for which caller that fits.
			row := &CreditTransaction{
				ID:     uuid.NewString(),
				Type:   string(CreditTransactionExpire),
				Status: string(CreditTransactionStatusConfirmed),
				Amount: in.Amount,
				Reason: in.Reason,
			}
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
			if err := s.transactions.insert(ctx, session, row); err != nil {
				return err
			}
			result = row
			expired = true
			resultBalance = s.readBalanceForAudit(ctx, session, string(tenant))
			return nil
		}

		// Keyed: the row's ID is the caller's own deterministic key, and
		// the insert runs as ON CONFLICT DO NOTHING -- reporting a
		// duplicate as inserted==false with NO error (see
		// insertIdempotent's own doc comment for why that matters on
		// PostgreSQL): the transaction stays healthy, and the retry's
		// read-back below runs on it either way. The balance CAS runs only
		// on the fresh-insert branch -- never on the retry branch, whose
		// deduction already happened inside its first call's transaction.
		row := &CreditTransaction{
			ID:     in.IdempotencyKey,
			Type:   string(CreditTransactionExpire),
			Status: string(CreditTransactionStatusConfirmed),
			Amount: in.Amount,
			Reason: in.Reason,
		}
		inserted, insertErr := s.transactions.insertIdempotent(ctx, session, row)
		if insertErr != nil {
			return insertErr
		}
		if inserted {
			if err := s.ensureBalance(session, tenant); err != nil {
				return err
			}
			ok, err := applyBalanceDelta(session, string(tenant), -in.Amount, 0, s.now())
			if err != nil {
				return err
			}
			if !ok {
				// The guard refused, so the whole transaction rolls back --
				// this call's own just-inserted row included: the key is
				// not burned, and a later, better-funded run with the same
				// key is a fresh attempt again.
				return ErrInsufficientCredits.WithParam("amount", in.Amount)
			}
			result = row
			expired = true
			resultBalance = s.readBalanceForAudit(ctx, session, string(tenant))
			return nil
		}

		// Idempotent retry: a row for this IdempotencyKey already exists --
		// the transaction is still healthy (nothing aborted), so read the
		// existing row back and decide whether it is this retry's own
		// earlier run. Only an expire row can be: the ledger's row-ID
		// namespace is shared across every row type, and reporting a
		// different kind's row as a successful expiry would be the wrong
		// answer, so any other kind is refused loudly
		// (ErrIdempotencyKeyCollision) rather than guessed at.
		existing, err := s.findTransaction(session, in.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing == nil {
			// The conflicting row vanished between the no-op insert and
			// this read-back -- the same unreachable-in-practice corner
			// PreDeduct's reserve half documents for its own read-back
			// (nothing in this module deletes credit-transaction rows, and
			// a retry would get the correct outcome either way).
			return errCreditLedgerRowVanished
		}
		if existing.Type != string(CreditTransactionExpire) {
			return ErrIdempotencyKeyCollision.
				WithParam("idempotency_key", in.IdempotencyKey).
				WithParam("type", existing.Type)
		}
		result = existing
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	if expired {
		s.emitCreditAudit(ctx, AuditActionCreditExpire, tenant, result.ID, result.Amount, result.Reason, resultBalance)
	}
	return result, nil
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

// errCreditLedgerRowVanished reports the unreachable-in-practice corner
// where a duplicate (id, tenant_id) insert -- PreDeduct's reserve half or
// a keyed Expire's -- skipped via ON CONFLICT DO NOTHING but the read-back
// that follows finds no row: some other writer deleted it between the two
// statements. Nothing in this module deletes credit-transaction rows (the
// ledger is append-only, and CreditTransactionRepository offers no Delete
// at all), so the branch exists for completeness only; a caller retrying
// the operation gets the correct outcome either way, since a vanished row
// makes the retry a plain first insert again. It is deliberately a plain
// package-internal sentinel rather than an *apperr.Error: it is not
// reachable through any user-facing surface, so it earns no error-index or
// locale entry -- the identical choice go/metering's
// errOutboxConflictRowVanished makes for the same corner in its own
// idempotent-insert path.
var errCreditLedgerRowVanished = errors.New("billing: conflicting credit-ledger row vanished between insert and read-back; retry the operation")

// readBalanceForAudit reads tenant's CreditBalance row through session --
// the SAME *gorm.DB passed to the mutating transaction's own
// dbkit.WithTenantSession closure, called only immediately after
// applyBalanceDelta has just applied this operation's own delta inside
// that transaction, and before it commits.
//
// This is deliberately NOT a fresh, post-commit read (what this method
// replaced): a separate query opened after commit has no synchronization
// with a concurrently-committing operation against the same tenant's
// balance, so under concurrent mutations for one tenant it could observe
// a LATER state than the one this specific operation actually produced
// -- e.g. two concurrent Grants committing available 0->100 and
// 100->200 could have the FIRST one's post-commit read land after the
// SECOND'S commit and wrongly report resulting_available=200 for an
// operation that itself only ever moved 0->100. Reading inside the same
// transaction instead, before commit, ties the read to exactly this
// operation's own applied delta: the row is exclusively locked by this
// transaction's own prior UPDATE until it commits, so no concurrent
// transaction's write can land in between the UPDATE and this read, and
// the read sees this transaction's own uncommitted change via ordinary
// transaction-local visibility, on both dialects.
//
// A read failure here is logged and nil returned, mirroring
// emitCreditAudit's own "log, never fail the write" contract below: it
// must never turn an already-decided balance mutation into a rolled-back
// transaction merely because this follow-up read hit an error.
func (s *CreditService) readBalanceForAudit(ctx context.Context, session *gorm.DB, tenantID string) *CreditBalance {
	var out CreditBalance
	err := session.Where("id = ?", tenantID).First(&out).Error
	if err != nil {
		obs.FromContext(ctx).Error("billing.credit audit: resulting balance read failed",
			"tenant_id", tenantID, "error", err)
		return nil
	}
	return &out
}

// emitCreditAudit records action against the credit ledger transaction
// txID for tenant, following the exact declarative-Emit pattern
// examples/reference-app/internal/notes/handler.go's recordNoteCreatedAudit
// established as this codebase's real, working answer to "a business
// module explicitly calls audit.Emit after a state-changing operation
// succeeds": every call site above invokes this only AFTER its own
// dbkit.WithTenantSession transaction has already committed, never from
// inside the closure, so audit.Emit's own write opens a fresh,
// uncontended database session rather than nesting inside one still open
// -- the same-file SQLITE_BUSY hazard of AuditBus's automatic
// write-capture plugin, sidestepped here exactly the way notes' own
// handler sidesteps it: by construction, never by avoiding the shared
// connection.
//
// The Resource this ledger's five audited actions all record is the
// CreditTransaction row itself (Type "credit_transaction", ID txID) --
// the actual append-only fact each of PreDeduct/Confirm/Refund/Grant/
// Expire adds to the ledger -- rather than the mutable CreditBalance row
// underneath it, mirroring notes' own choice to audit the created Note,
// not some other aggregate it happens to touch. Result is always
// {Success: true}: every call site below runs only once its own mutating
// transaction has already committed, so an audited action that reaches
// this method by definition succeeded -- an audit record is never written
// for something that did not happen this call, enforced by each call
// site's own reserved/resolved guard (PreDeduct, resolve) or its
// single-phase always-succeeds shape (Grant, Expire), never by branching
// inside this shared helper.
//
// Changes.After carries the delta this specific action applied (amount,
// and reason when the caller supplied one) plus, best effort, the
// tenant's resulting balance: resultingBalance, read by the caller via
// readBalanceForAudit from INSIDE the same transaction that applied this
// operation's delta (see that function's own doc comment for why it must
// be read there and not here, after commit). The reason is safe in the
// Changes copy because this module declares it a bounded phrase and
// enforces that at every reason-taking entry point (validateReason)
// before anything is written -- the audit trail receives the same
// machine-readable phrase the ledger row carries, never free text a
// caller could slip PII into, per go/dbkit/audit's Diff content contract.
// A nil resultingBalance --
// that in-transaction read having failed, already logged by
// readBalanceForAudit -- simply omits the two balance fields from the
// payload; it must never turn an already-succeeded credit mutation into
// a reported failure, matching this method's own "log, never return"
// contract for a downstream audit.Emit failure below.
//
// s.events is nil for a bare CreditService built directly through
// NewCreditService (every unit test in this package) and non-nil only
// once module.go's Register has wired it from the host's
// pkgcore.Registry -- exactly mirroring notes' own Handler.bus == nil
// short-circuit, so emitCreditAudit is a no-op for every pre-existing
// call site and test that constructs a CreditService without going
// through a full Kernel.Bootstrap.
func (s *CreditService) emitCreditAudit(ctx context.Context, action string, tenant pkgcore.TenantID, txID string, amount int64, reason string, resultingBalance *CreditBalance) {
	if s.events == nil {
		return
	}

	payload := map[string]any{"amount": amount}
	if reason != "" {
		payload["reason"] = reason
	}
	if resultingBalance != nil {
		payload["resulting_available"] = resultingBalance.Available
		payload["resulting_reserved"] = resultingBalance.Reserved
	}

	err := audit.Emit(ctx, s.events, s.auditActions, audit.Input{
		Action:   action,
		Resource: audit.Resource{Type: "credit_transaction", ID: txID},
		Result:   audit.Result{Success: true},
		Changes:  &audit.Diff{After: payload},
	})
	if err != nil {
		// See this method's own doc comment above for why the operation
		// that produced txID is never turned into a failure by this: the
		// underlying credit mutation already committed by the time this
		// runs. An Error-level structured log line is the whole "must
		// alert" mechanism for the lost-audit-write window -- the same
		// choice notes' own recordNoteCreatedAudit makes for the identical
		// situation.
		obs.FromContext(ctx).Error("billing.credit audit event emit failed",
			"action", action, "credit_transaction_id", txID, "tenant_id", string(tenant), "error", err)
	}
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
