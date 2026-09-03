package notification

// This file owns the module's channel vocabulary and the conventions that
// bind the platform's notification-type taxonomy to it.
//
// # Where notification types come from
//
// notification never declares the notification types it delivers: a type is
// a declaration of the business module that emits it, registered on the
// host's registry with reg.Notifications.Add (pkgcore.NotificationType) --
// the same relationship events have with reg.Events.Publishes. The taxonomy
// a preference write or a delivery consults is therefore the LIVE registry
// list, read at call time through the registrar Register attached (see
// PreferenceService), never a snapshot: a business module may legitimately
// register its types after notification's own Register ran.
//
// # What a type declaration says
//
// Key follows <module>.<entity>.<action> ("billing.invoice_paid"), which is
// exactly why the template-id convention below stays inside every module's
// own catalog id space. Group buckets types in the preference matrix.
//
// DefaultChannels has two roles at once, because pkgcore's registry carries
// exactly one channel list per type: it is the channel set the type delivers
// on when the recipient has no preference, AND the type's supported channel
// space -- a preference write may only pick channels from it, which
// PreferenceService.Set enforces. A type that never wants to leave the in-app
// inbox, say, simply declares DefaultChannels: [in_app] alone. Unsubscribable
// reports whether an EMPTY selection is legal: transactional types
// (verification codes above all) are not, so their recipients may narrow
// them to one channel but never to none (ErrPreferenceOptoutNotAllowed).
//
// The channel vocabulary itself is closed and platform-wide; a business
// module picks from it and cannot extend it (adding a channel is a
// notification-module change, not a consumer change).
const (
	// ChannelInApp is the in-app inbox: per-tenant rows in in_app_messages
	// that a user reads inside the product. It has zero external
	// dependencies, so it works in every deployment composition, and it is
	// the channel this round builds end to end.
	ChannelInApp = "in_app"

	// ChannelEmail is delivery to the recipient's email address. The
	// transport behind it is the host's Mailer seam; for users of a tenant
	// the address itself is identity data (authn's domain), which is why
	// notification stores no address of its own for this channel.
	ChannelEmail = "email"

	// ChannelSMS is delivery to the recipient's phone number. The transport
	// is the host's SMS seam (authn ships console and HTTP senders, see
	// go/authn/sms.go); for users of a tenant the number is identity data
	// too. Messaging an external contact on either channel is governed by
	// the consent ledger, a later block of this round.
	ChannelSMS = "sms"
)

// isKnownChannel reports whether name is a member of the platform's channel
// vocabulary. PreferenceService.Set refuses a selection naming anything
// else: an unknown channel is a stale client or a typo, and storing it
// would make the row silently deliver nowhere.
func isKnownChannel(name string) bool {
	switch name {
	case ChannelInApp, ChannelEmail, ChannelSMS:
		return true
	default:
		return false
	}
}

// sortedChannels returns channels in the platform's canonical vocabulary
// order (in_app, email, sms), duplicates removed. Set stores selections in
// this order so a preference row's JSON is deterministic regardless of how
// the caller listed the channels -- the property the delivery subscriber's
// dedupe-key derivation reasons from.
func sortedChannels(channels []string) []string {
	seen := make(map[string]struct{}, len(channels))
	out := make([]string, 0, len(channels))
	for _, name := range []string{ChannelInApp, ChannelEmail, ChannelSMS} {
		if _, ok := seen[name]; ok {
			continue
		}
		for _, ch := range channels {
			if ch == name {
				seen[name] = struct{}{}
				out = append(out, name)
				break
			}
		}
	}
	return out
}
