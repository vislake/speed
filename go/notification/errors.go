package notification

import (
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
