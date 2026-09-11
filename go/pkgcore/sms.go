package pkgcore

import (
	"context"
)

// SMS is one message to deliver to a phone number, already rendered by the
// caller. Rendering (the recipient's locale, the message template), consent
// checks and number normalization are the caller's business, never the
// transport's -- the same division pkgcore.Mailer's doc comment draws for
// email -- and To travels in whatever form the caller built it in: every
// implementation passes it through to its transport unchanged, and a caller
// that needs a canonical form (the E.164 shape dbkit.NormalizePhoneE164
// produces, say) normalizes before the message reaches this seam.
//
// A message reaches a transport in two renderings of the same fact, and
// which one a transport uses depends on what the carrier supports. Text is
// the fully rendered message, the whole of what a free-text transport (the
// console sender, the operator HTTP gateway, the twilio adapter) delivers.
// MessageID, Locale and Params are the message's identity and its
// interpolation values, the whole of what a carrier with no free-text send
// (the aliyun and tencent adapters, which instantiate an approved account
// template) needs: such an adapter selects the account template its config
// maps from "<locale>/<message_id>" and fills the template's variables from
// Params, never from Text. The two renderings are filled by the same caller
// from the same render, so a template-typed adapter ignores Text and a
// free-text adapter ignores the identity fields.
type SMS struct {
	To   string
	Text string

	// MessageID is the stable id of the locale message Text was rendered
	// from -- go/notification's "<type_key>.sms.text" ids and go/authn's
	// "authn.sms.verification_code" are the two in-repo producers. It is
	// message identity, not a purpose: the same id names the same message
	// in every locale, so a template-typed adapter can key templates by
	// (Locale, MessageID) without knowing which module sent the message.
	// It is empty for a caller that has only free text to deliver.
	MessageID string

	// Locale is the locale Text was actually rendered in -- the value the
	// render used, after any fallback the rendering call applied, never the
	// recipient's stored preference: the two can differ (a stored locale
	// the render does not ship falls back), and a template-typed adapter
	// must select the template in the language the message was really
	// rendered in. It is empty for a caller that has only free text.
	Locale string

	// Params carries the interpolation values the rendered message used,
	// keyed by the names the message template's own placeholders spell out
	// ({{.code}}, {{.minutes}}), already stringified. A template-typed
	// adapter forwards exactly the variables its selected account template
	// declares and no others. The map may be empty or nil; a free-text
	// transport ignores it.
	//
	// A Params value can be a credential (a verification code). An
	// implementation must not log, echo or otherwise surface values, and
	// the errors it returns must name a missing variable, never its value.
	Params map[string]string
}

// SMSSender is the outbound-SMS contract shared by every module that
// delivers a short message to a phone number. It is the contract go/authn's
// phone-login verification codes and go/notification's sms-channel
// deliveries both send through -- those two modules used to declare
// identical SMSSender interfaces of their own, which forced a host wiring
// both to hand one implementation to each, and forced a notification host
// that wanted carrier delivery to reimplement the adapters go/authn's
// subpackages already were; the seam lives here, on the dependency floor
// both modules stand on, so one contract and one set of implementations
// serve every consumer.
//
// The interface is designed against the weakest implementation it must
// support, exactly as Mailer is: Send takes one already-rendered SMS and
// reports only success or failure. The message shape is one struct, the
// call is one round trip, and nothing else is promised -- no delivery
// receipt, no retry policy, no capability probe, no number validation. What
// an implementation owes the template-identity fields is its own
// documented contract: a free-text transport ignores them, while a
// template-typed carrier adapter must refuse a message it has no mapped
// template for before contacting its gateway, never fall back to another
// template or to sending free text.
//
// Send must honour a cancelled context by returning its context's error
// instead of sending, must not retain the SMS after returning, and must be
// safe for concurrent use by multiple goroutines. An error means the message
// was not delivered; the caller decides what a delivery failure means for
// its flow (go/authn answers a failed code delivery indistinguishably from a
// request for an unregistered number, and go/notification settles the
// attempt as failed and marks the contact bounced on a permanent transport
// error). A failure that is the destination's own verdict on the number --
// the carrier refusing this number permanently (an invalid number, a
// blacklisted one) -- must be wrapped with ErrTransportPermanent; every
// other failure travels unwrapped, per that sentinel's own boundary.
//
// Unlike the four assembly-resolved seams (EventBus, KVStore, Mailer and
// ObjectStore), SMSSender deliberately has no component descriptor and no
// capability declaration, and the assembly never resolves one: no consumer
// takes its SMS transport from the assembly -- go/authn and go/notification both receive
// the sender through their own module-wiring options, and each enforces its
// own wiring-time requirement on it (a distributed-mode authn refuses to
// boot without an explicitly wired sender rather than defaulting to one that
// prints to a writer nobody reads). The promotion makes the seam a shared
// contract and shared implementations; it does not move SMS onto the assembly.
// The console implementation (sms_console.go) is the
// zero-external-dependency one; NewHTTPSMSSender (sms_http.go) is the
// operator-gateway
// transport, and the sms/aliyun, sms/tencent and sms/twilio subpackages
// carry the three real carrier adapters.
type SMSSender interface {
	// Send delivers sms. An error means the message was not delivered.
	Send(ctx context.Context, sms SMS) error
}
