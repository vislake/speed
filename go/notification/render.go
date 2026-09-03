package notification

import (
	"errors"
	"fmt"

	"github.com/vislake/speed/go/pkgcore/i18n"
)

// channelRenderParts is the per-channel template-part table, keyed by
// channel key. Each channel carries its own copy shape: an in-app message
// has a title and a body, an email a subject and a plain-text body, an SMS
// a single text -- and delivery renders exactly the parts the destination
// channel needs, never the union of all channels' copy.
var channelRenderParts = map[string][]string{
	ChannelInApp: []string{"title", "body"},
	ChannelEmail: []string{"subject", "body_text"},
	ChannelSMS:   []string{"text"},
}

// renderContent renders one notification type's message copy -- the parts
// one channel needs -- in the recipient's locale, from the host's merged
// message catalog. It returns the rendered parts keyed by part name, the
// keys of channelRenderParts[channel].
//
// # Template-id convention
//
// A business module that declares a notification type owns its copy: for a
// type key <module>.<entity>.<action> ("billing.invoice_paid") delivered
// over channel <channel> ("email"), the templates live in that module's own
// locale files under the ids
// <type_key>.<channel>.<part> -- "billing.invoice_paid.email.subject" and
// "billing.invoice_paid.email.body_text" -- inside the declaring module's
// catalog id space, which is exactly why pkgcore/i18n's Builder.AddModule
// requires every id in a module's bundle to start with that module's name.
// notification never ships templates for types it does not declare, and its
// own bundle (locales/) carries only its error codes; delivery renders from
// the merged catalog the host assembled from every module's bundles.
//
// The channel sits in the id between the type key and the part because one
// type's copy genuinely differs by channel: an SMS text is a sentence where
// an email body is paragraphs, and the i18n id space must be able to
// express both without one template stretching to serve two shapes.
//
// # Callers
//
// The delivery job renders here at send time -- never at enqueue time --
// after its send-time rechecks, so a recipient whose consent or preferences
// changed between enqueue and delivery costs nothing and a render can never
// precede the recheck that might have skipped it.
//
// # Failure mode
//
// A missing template id or a locale the catalog does not know are reported
// as ErrInternal.WithCause -- never a fallback to another language and
// never a half-rendered message. pkgcore/i18n's Catalog refuses to fall
// back by design, and delivery must not paper over a producer that forgot
// to ship its copy: the recipient gets a coded failure the operator can
// trace, not a message in the wrong language or with a hole where the text
// should be. params supplies the interpolation values the templates
// reference, keyed by the names the templates' own {{.name}} placeholders
// spell out.
func renderContent(catalog *i18n.Catalog, locale, typeKey, channel string, params map[string]any) (map[string]string, error) {
	if catalog == nil {
		return nil, ErrInternal.WithCause(errors.New("notification: render called with no catalog"))
	}

	parts, known := channelRenderParts[channel]
	if !known {
		return nil, ErrInternal.WithCause(fmt.Errorf("notification: render for unknown channel %q", channel))
	}

	out := make(map[string]string, len(parts))
	for _, part := range parts {
		id := typeKey + "." + channel + "." + part
		text, err := catalog.Lookup(locale, id, params)
		if err != nil {
			return nil, ErrInternal.WithCause(fmt.Errorf("notification: render %s for %s: %w", id, typeKey, err))
		}
		out[part] = text
	}
	return out, nil
}
