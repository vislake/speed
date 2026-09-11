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
// Only the codes this module's own code paths can actually return are
// declared here -- the validation and lifecycle codes below, plus the
// webhook/payment-event codes PaymentGateway.VerifyWebhook/QueryStatus and
// PaymentEventRepository can actually return. A code for a check nothing
// performs would be dead catalog weight, not forward compatibility -- the
// same discipline go/pki's and go/metering's error indexes document for
// their own module boundaries.
var (
	// ErrPlanNotFound reports that no Plan exists for whatever was looked
	// up in the scope it was looked up under: a plan id in a named scope
	// (PlanStore.Get/GetPlatformPlan/Update, SubscriptionService.Create)
	// or a plan key (PlanStore.Resolve) -- a row that exists but belongs
	// to another scope answers the identical code, never a distinguishable
	// refusal. Every call site decorates the error with the looked-up
	// value under the shared "id" parameter name -- the same name the
	// locale template interpolates -- never with a site-specific name the
	// template would render as an empty slot.
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

	// ErrInvalidInvoiceTransition reports a lifecycle transition that is
	// not legal from the Invoice's current Status -- e.g. voiding an
	// already-paid invoice, or any other move out of the terminal Paid or
	// Void statuses. Mirrors ErrInvalidSubscriptionTransition for
	// subscriptions.
	ErrInvalidInvoiceTransition = apperr.Invalid("billing.invalid_invoice_transition")

	// ErrInvalidAmount reports a credit amount that is zero or negative,
	// where CreditService requires a strictly positive one.
	ErrInvalidAmount = apperr.Invalid("billing.invalid_amount")

	// ErrIdempotencyKeyRequired reports a credit operation submitted
	// with no idempotency key -- mandatory, the same way
	// UsageEvent.IdempotencyKey is in go/metering, since it is what
	// lets a retried PreDeduct/Confirm/Refund call be told apart from a
	// second, genuinely new operation.
	ErrIdempotencyKeyRequired = apperr.Invalid("billing.idempotency_key_required")

	// ErrIdempotencyKeyCollision reports that a keyed
	// CreditService.PreDeduct or CreditService.Expire call supplied an
	// IdempotencyKey that already identifies a credit_transaction row of
	// ANOTHER kind (a Grant row, an Expire row, a Deduct row, or an
	// unkeyed row's generated UUID). The ledger's row-ID namespace is
	// shared across every row type, so a deterministic key that names a
	// row which is not the caller's own earlier row of the same kind
	// cannot be answered as a successful idempotent retry -- reporting
	// success on a row of another kind would be a wrong answer the audit
	// trail would then preserve. Only an existing row of exactly the
	// caller's own type counts as the retry's own earlier run (see
	// CreditService.PreDeduct's and Expire's doc comments); anything else
	// under the key is refused with this coded Conflict.
	ErrIdempotencyKeyCollision = apperr.Conflict("billing.idempotency_key_collision")

	// ErrInvalidReason reports a credit-operation Reason that violates
	// the module's declared bounded-phrase constraint (validateReason):
	// a non-empty reason must be a phrase of ASCII letters, digits and
	// ':' '_' '-' separators, at most 255 characters long. The
	// constraint is declared and enforced because the reason is copied
	// verbatim into the audit trail's changes column (emitCreditAudit),
	// where dbkit/audit's Diff content contract forbids free text that
	// could carry PII.
	ErrInvalidReason = apperr.Invalid("billing.invalid_reason")

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

	// ErrWebhookSignatureInvalid reports that PaymentGateway.VerifyWebhook
	// could not authenticate an inbound webhook delivery against its
	// channel's own signature scheme -- the delivery must be refused, never
	// processed, since an unverified body could have been forged by anyone
	// who can reach the endpoint.
	ErrWebhookSignatureInvalid = apperr.Invalid("billing.webhook_signature_invalid")

	// ErrWebhookPayloadUnrecognized reports that a webhook delivery's
	// signature verified, but PaymentGateway.VerifyWebhook could not parse
	// its payload into a known, identifiable NormalizedEvent -- including a
	// payload missing the tenant/subscription/invoice metadata
	// PaymentGateway.CreateCharge attached when it created the channel-side
	// object.
	ErrWebhookPayloadUnrecognized = apperr.Invalid("billing.webhook_payload_unrecognized")

	// ErrChannelReferenceNotFound reports that PaymentGateway.QueryStatus
	// was asked about a ChannelReference its channel has no record of at
	// all.
	ErrChannelReferenceNotFound = apperr.NotFound("billing.channel_reference_not_found")

	// ErrPaymentEventNotFound reports that no PaymentEvent row exists for
	// the requested id, for the requesting tenant.
	ErrPaymentEventNotFound = apperr.NotFound("billing.payment_event_not_found")

	// ErrUnsupportedCurrency reports that a ChargeRequest named a currency
	// the target payment channel cannot actually collect -- Alipay and
	// WeChat Pay's Native (QR-code) product only ever settle in CNY, so a
	// request naming any other currency is refused at the CreateCharge
	// boundary rather than silently collected as if it were CNY.
	ErrUnsupportedCurrency = apperr.Invalid("billing.unsupported_currency")

	// ErrUsageReaderUnconfigured reports that EntitlementsService.Check was
	// asked to make a FeatureKindQuota decision while the service had been
	// built without a UsageReader (NewEntitlementsService's usage was nil)
	// -- a wiring gap, never a quota answer. Quota decisions read the
	// real-time counter (UsageReader's own doc comment), and with no
	// counter to read the honest answer is this loud configuration error,
	// never a nil-interface-call panic and never a guessed allowance (a
	// zero-usage guess would fail OPEN for an over-quota tenant). Boolean
	// and Unlimited features never consult the UsageReader and are
	// unaffected; the error is reachable only for FeatureKindQuota grants.
	ErrUsageReaderUnconfigured = apperr.Internal("billing.usage_reader_unconfigured")

	// ErrInvalidLimit reports a transactions-list request whose limit
	// query parameter is an integer outside the fragment's 1-100 bound --
	// the range half of the route's 400 answer (handler.go's
	// BillingListCreditTransactions, which decorates it with the limit
	// sent and the bound). The companion shape, a limit that is not an
	// integer at all, is refused by the spec-generated parameter binder
	// before the handler runs and answered as ErrInvalidRequest instead --
	// see that error's own doc comment.
	ErrInvalidLimit = apperr.Invalid("billing.invalid_limit")

	// ErrInvalidRequest reports a request the transport could not parse
	// -- on this surface, the spec-generated parameter binder rejecting a
	// query value before Handler's own method is ever called (a limit
	// that is not an integer, say), answered through the coded envelope
	// handler.go's bindingErrorHandler installs in place of oapi-codegen's
	// default plain-text http.Error. The failing parameter is carried as
	// the structured "parameter" param. It is deliberately the generic
	// request-shape code, never one naming limit's own semantics: the
	// binder never sees the bound (only the handler does, answering
	// ErrInvalidLimit for an integer outside it), and any bindable
	// parameter the spec gains is refused through this same code rather
	// than one minted per parameter -- the identical stance go/sharing's
	// own ErrInvalidRequest takes for its binder failures.
	ErrInvalidRequest = apperr.Invalid("billing.invalid_request")

	// ErrInternal reports a failure the module's HTTP layer cannot
	// classify -- a database error wrapped in a plain fmt.Errorf, say,
	// which is not an *apperr.Error -- folded to this one stable code so
	// a caller never sees raw Go error text (handler.go's writeError,
	// mirroring go/storage's own ErrInternal for the identical role).
	ErrInternal = apperr.Internal("billing.internal_error")
)
