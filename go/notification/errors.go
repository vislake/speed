package notification

import (
	"net/http"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// The error index of the notification module. Every exported error is an
// *apperr.Error builder whose Code follows the <module>.<reason> convention
// the backend coding standard requires: match a decorated error with
// apperr.As(err) and compare its Code, never with == or errors.Is against
// the var below. WithParam and WithCause derive a NEW *apperr.Error rather
// than mutating the receiver, so the pointer a call returns is never the
// pointer declared here -- the same convention org, dbkit, tenancy and
// config already document.
//
// Every code in this file has a matching description entry in
// locales/{zh-CN,en-US}.toml, under the identical id. The API returns the
// code and its parameters; the text is resolved by the consumer.
//
// The errors are grouped by the surface that returns them. The preference
// group (this file's first block) belongs to the preference matrix; the
// contact group belongs to the consent ledger ContactService owns; the
// delivery group belongs to the outbound-delivery pipeline DeliveryService
// owns; the wiring group reports a Module whose required seams were not
// supplied before Register validated them -- boot-time failures a caller
// cannot trigger once the module is registered, listed here because an
// *apperr sentinel per code is this module's convention for every error it
// can surface, however unreachable at request time.
var (
	// ErrRecipientRequired reports a preference write whose recipient is
	// missing. A preference row is meaningless without the user it applies
	// to, and an empty user id would store a row nothing can ever look up.
	ErrRecipientRequired = apperr.Invalid("notification.recipient_required")

	// ErrTypeNotFound reports a preference read or write that names a
	// notification type nobody registered. The type taxonomy is whatever
	// business modules declared on the host's registry (see types.go); a
	// key outside it is a stale client or a wiring bug, and the code
	// deliberately does not distinguish "declared under another name" from
	// "declared nowhere at all".
	ErrTypeNotFound = apperr.NotFound("notification.type_not_found")

	// ErrPreferenceInvalidChannels reports a channel selection the named
	// notification type can never honor: a channel outside the platform
	// vocabulary (in_app, email, sms -- see types.go), the same channel
	// listed twice, or a channel the type does not use. Refusing the whole
	// write on the first such channel is what keeps a preference row's
	// channels a subset of the type's -- the invariant ResolveChannels
	// reasons from.
	ErrPreferenceInvalidChannels = apperr.Invalid("notification.preference_invalid_channels")

	// ErrPreferenceOptoutNotAllowed reports an empty channel selection on
	// a notification type whose declaration does not permit opting out
	// (Unsubscribable false -- transactional notifications such as
	// verification codes above all). The recipient may narrow such a type
	// to one channel but never to none: at least one must keep arriving.
	// The UI equivalent is a disabled "turn all off" switch, and the M2
	// flow's refusal behaviour is pinned by this code's tests.
	ErrPreferenceOptoutNotAllowed = apperr.Invalid("notification.preference_optout_not_allowed")

	// ErrInternal reports a failure inside the module itself: a store the
	// module depends on misbehaving, a catalog lookup failing, a corrupt
	// stored row. It always travels WithCause, so the underlying error is
	// on the record even though the caller sees only the code.
	ErrInternal = apperr.Internal("notification.internal_error")
)

// The consent-ledger group: every error ContactService (contact.go) can
// return, one per reason a contact operation can fail. The statuses are
// the caller's (a host's HTTP handler, or the delivery job's mapper) whole
// answer on how to treat the failure -- see each error's own comment.
var (
	// ErrContactNotFound reports a contact operation naming an id no row
	// of the caller's tenant holds -- never a row of another tenant's,
	// which is indistinguishable from a row that does not exist (see
	// VerifiedContactRepository.ByChannelAndAddressIndex).
	ErrContactNotFound = apperr.NotFound("notification.contact_not_found")

	// ErrContactInvalidChannel reports a contact create naming a channel
	// outside the closed vocabulary (email, sms -- see types.go). There is
	// no in-app contact: the in-app channel of the inbox belongs to users,
	// never to external addresses.
	ErrContactInvalidChannel = apperr.Invalid("notification.contact_invalid_channel")

	// ErrContactInvalidAddress reports an address with no canonical form:
	// a phone number that is not valid E.164, an email that normalizes to
	// nothing. The address is never stored, and never blind-indexed -- an
	// input with no canonical form must not produce an index column at
	// all.
	ErrContactInvalidAddress = apperr.Invalid("notification.contact_invalid_address")

	// ErrContactCodeInvalid reports a verification attempt whose code is
	// wrong, expired, replayed, or directed at a contact that has nothing
	// to verify any more. The four cases are deliberately one code (see
	// VerifyCode): telling the caller which part failed hands an attacker
	// a free oracle on whether a code is still live.
	ErrContactCodeInvalid = apperr.Invalid("notification.contact_code_invalid")

	// ErrContactCodeDeliveryFailed reports a verification-code message the
	// transport refused after the code was stamped. For a create the
	// pending row is revoked before this is returned; for a resend the
	// fresh hash stays on the row (see ResendCode), harmless because the
	// previous code was already dead.
	ErrContactCodeDeliveryFailed = apperr.Internal("notification.contact_code_delivery_failed")

	// ErrContactUnsubscribed reports an operation on a contact that has
	// permanently unsubscribed: verification and resend refuse it, and
	// the deliverability gate refuses every delivery to it. The status is
	// terminal for the contact (see Unsubscribe); nothing in this API
	// revives it.
	ErrContactUnsubscribed = apperr.Conflict("notification.contact_unsubscribed")

	// ErrContactBounced reports an operation on a contact whose address
	// has proven unable to receive messages (a hard delivery failure).
	// Terminal in this round: re-proving a bounced address is a later
	// round's remediation, and AGENTS.md records the deferral.
	ErrContactBounced = apperr.Conflict("notification.contact_bounced")

	// ErrContactNotVerified reports a delivery attempt (EnsureDeliverable)
	// against a contact whose consent was never proved -- the contact is
	// still pending. Distinct from the terminal statuses because a pending
	// contact may become deliverable later: the delivery job treats this
	// as a skip, where unsubscribed and bounced are permanent refusals.
	ErrContactNotVerified = apperr.Conflict("notification.contact_not_verified")

	// ErrContactRateLimited reports a verification-code send or verify
	// attempt denied by the module's rate limits (see contactRateLimits),
	// carrying the dimension that denied it and the seconds until that
	// dimension's window resets. Every code-message path fails closed on
	// its budget; the params are what a caller renders a "try again
	// later" message from.
	ErrContactRateLimited = rateLimited("notification.contact_rate_limited")
)

// The delivery group: every error the outbound-delivery pipeline
// (DeliveryService, delivery.go) can return on its public surface. The
// group's members each carry a "field" parameter naming the offending part
// of a Dispatch.
var (
	// ErrDispatchInvalid reports a Dispatch that cannot be delivered: a
	// missing type key, a missing or unknown recipient class, a user
	// recipient without an id or a locale, an external recipient without a
	// contact id, or a payload that will not marshal. The refusal names the
	// offending field in its "field" parameter; Dispatch validates before
	// anything is enqueued, and the job handler re-validates what the queue
	// hands it, so a malformed payload dies at the API boundary or dead-
	// letters, never half-delivers.
	ErrDispatchInvalid = apperr.Invalid("notification.dispatch_invalid")
)

// The wiring group: a Module whose Register-time validation failed because
// a required seam was never supplied through NewModule. Each error names
// the missing seam; the module is unbootable until the host supplies it,
// exactly as org's Register validates its own required seams (see org's
// ErrEmailIndexerRequired). All six are Internal: a caller cannot trigger
// them once the module is registered, and a host hitting one has a
// configuration bug, not a bad request.
var (
	// ErrSMSSenderRequired reports a Register whose Module has no SMS
	// sender. The module sends verification codes by SMS as its
	// synchronous messaging exception; a module without a sender must
	// fail at boot rather than discover the gap on the first code a
	// patient needs.
	ErrSMSSenderRequired = apperr.Internal("notification.sms_sender_required")

	// ErrMailFromRequired reports a Register whose Module has no mail
	// From address. Every outbound mail this module composes (email
	// verification codes first among them) carries the address; a module
	// without one must fail at boot rather than send unaddressable mail.
	ErrMailFromRequired = apperr.Internal("notification.mail_from_required")

	// ErrContactEmailIndexerRequired reports a Register whose Module has
	// no blind indexer for email contact addresses. The module never
	// stores or queries plaintext addresses; without the indexer it
	// cannot even create a contact. org validates its own indexer the
	// same way (org.email_indexer_required).
	ErrContactEmailIndexerRequired = apperr.Internal("notification.contact_email_indexer_required")

	// ErrContactPhoneIndexerRequired reports a Register whose Module has
	// no blind indexer for phone contact addresses; the SMS twin of
	// ErrContactEmailIndexerRequired.
	ErrContactPhoneIndexerRequired = apperr.Internal("notification.contact_phone_indexer_required")

	// ErrDeliveryQueueRequired reports a Register whose Module has no
	// delivery queue. Outbound delivery is asynchronous by contract --
	// Dispatch enqueues, the worker sends -- and a module without a queue
	// has no delivery path at all; it must fail at boot rather than have
	// every Dispatch refuse at run time.
	ErrDeliveryQueueRequired = apperr.Internal("notification.delivery_queue_required")

	// ErrUserAddressResolverRequired reports a Register whose Module has
	// no user-address resolver. A user delivery's email and SMS channels
	// resolve the recipient's addresses through this seam (see
	// UserAddressResolver); without it the module cannot deliver to a
	// user on any outbound channel.
	ErrUserAddressResolverRequired = apperr.Internal("notification.user_address_resolver_required")
)

// rateLimited returns an *apperr.Error carrying HTTP 429, the status apperr
// has no constructor for.
func rateLimited(code string) *apperr.Error {
	err := apperr.Invalid(code)
	err.Status = http.StatusTooManyRequests
	return err
}
