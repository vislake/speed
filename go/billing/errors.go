package billing

import "github.com/vislake/speed/go/pkgcore/apperr"

// The error index of the billing module. Every exported error is an
// *apperr.Error builder whose Code follows the <module>.<reason> convention
// the backend coding standard requires: match a decorated error with
// apperr.As(err) and compare its Code, never with == or errors.Is against
// the var below, since WithParam/WithCause derive a new *apperr.Error
// rather than mutating the receiver -- the same convention dbkit, tenancy,
// org, pki and metering already document.
//
// Only the codes this round's own code paths can actually return are
// declared here. In particular there is no code for a payment-channel or
// webhook failure: that machinery is billing/gateway's, a later round, and
// a code for a check nothing performs would be dead catalog weight, not
// forward compatibility -- the same discipline go/pki's and go/metering's
// error indexes document for their own round boundaries.
var (
	// ErrPlanNotFound reports that neither a tenant-custom nor a
	// platform-wide Plan exists for the requested key.
	ErrPlanNotFound = apperr.NotFound("billing.plan_not_found")

	// ErrPlanKeyRequired reports that a Plan was saved with an empty Key.
	ErrPlanKeyRequired = apperr.Invalid("billing.plan_key_required")

	// ErrDuplicatePlanKey reports that a Plan with the same (TenantID,
	// Key) pair already exists -- the unique index PlanStore.Create
	// relies on to keep lookup precedence well-defined.
	ErrDuplicatePlanKey = apperr.Conflict("billing.duplicate_plan_key")

	// ErrSubscriptionNotFound reports that no Subscription exists with
	// the requested id, for the requesting tenant.
	ErrSubscriptionNotFound = apperr.NotFound("billing.subscription_not_found")

	// ErrInvalidSubscriptionTransition reports a lifecycle transition
	// that is not legal from the Subscription's current Status --
	// e.g. activating an already-canceled subscription.
	ErrInvalidSubscriptionTransition = apperr.Invalid("billing.invalid_subscription_transition")

	// ErrInvoiceNotFound reports that no Invoice exists with the
	// requested id, for the requesting tenant.
	ErrInvoiceNotFound = apperr.NotFound("billing.invoice_not_found")

	// ErrInvalidAmount reports a credit amount that is zero or negative,
	// where CreditService requires a strictly positive one.
	ErrInvalidAmount = apperr.Invalid("billing.invalid_amount")

	// ErrIdempotencyKeyRequired reports a credit operation submitted
	// with no idempotency key -- mandatory, the same way
	// UsageEvent.IdempotencyKey is in go/metering, since it is what
	// lets a retried PreDeduct/Confirm/Refund call be told apart from a
	// second, genuinely new operation.
	ErrIdempotencyKeyRequired = apperr.Invalid("billing.idempotency_key_required")

	// ErrInsufficientCredits reports that a PreDeduct's requested amount
	// exceeds the tenant's available credit balance. The reservation
	// was NOT made; no credit_transaction row exists for this attempt.
	ErrInsufficientCredits = apperr.Conflict("billing.insufficient_credits")

	// ErrCreditTransactionNotFound reports that Confirm or Refund named
	// an idempotency key with no matching pending credit_transaction row
	// for the requesting tenant.
	ErrCreditTransactionNotFound = apperr.NotFound("billing.credit_transaction_not_found")

	// ErrCreditTransactionAlreadyResolved reports that Confirm or Refund
	// was called for a credit_transaction row already moved to a
	// terminal status (confirmed or refunded) OTHER than the one the
	// caller is now asking for -- e.g. calling Refund on a transaction
	// Confirm already settled. Calling the SAME resolution again (a
	// genuine retry) is a no-op success instead; see CreditService.Confirm
	// and .Refund for the exact idempotent-retry contract.
	ErrCreditTransactionAlreadyResolved = apperr.Conflict("billing.credit_transaction_already_resolved")

	// ErrCreditBalanceInconsistent reports that a Confirm or Refund's own
	// ledger-row transition succeeded but the paired balance CAS it
	// depends on did not -- a bookkeeping inconsistency between Reserved
	// and the outstanding pending transactions, not a caller error.
	// Declared here rather than left as a bare inline apperr.Internal(...)
	// call so the module's own error index and locale bundles stay
	// complete, the same discipline go/metering's ErrMetadataEncodeFailed
	// documents for its own unreachable-in-practice defensive branch.
	ErrCreditBalanceInconsistent = apperr.Internal("billing.credit_balance_inconsistent")
)

// hasCode reports whether err is (or wraps, via apperr.As's Unwrap chain
// walk) an *apperr.Error whose Code equals code. This is the standard way
// this codebase compares against a dbkit or apperr sentinel once it may
// have been decorated with WithParam/WithCause -- see go/metering/errors.go's
// and go/org/tree.go's identical helper.
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
