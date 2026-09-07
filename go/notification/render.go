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
	ChannelInApp: {"title", "body"},
	ChannelEmail: {"subject", "body_text"},
	ChannelSMS:   {"text"},
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
// The type directory's copy -- the description each type serves alongside
// its key -- is a second id space sharing this convention's shape; see
// renderTypeDescription below.
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

// copyParamsForChannel returns the subset of params the copy of typeKey on
// channel in locale actually renders, and whether that copy could be
// established at all. ok is false when no copy could be rendered (a nil
// catalog, a missing template part, a locale the catalog does not know) --
// callers must keep params untouched on !ok, since nothing is known about
// which parameters the copy depends on; the delivery's own render failure
// is the honest referee for an unrenderable copy, never this probe.
//
// A parameter is kept exactly when the copy depends on it: rendering the
// channel's parts with the parameter removed produces output identical to
// the full-parameter render only when no part's output ever reflected the
// parameter, so a removed-parameter render that errors or differs proves
// the copy needs the parameter, and one that matches byte for byte proves
// it does not. The probe never drops a parameter whose removal would
// change the copy, by construction: what survives is precisely the set
// whose removal leaves every rendered part untouched, which is also
// exactly the set the copy was rendered from.
//
// The kept set is a pure function of (catalog, locale, typeKey, channel,
// params) -- rendering is deterministic and the probe has no side effects
// -- so every replica and every retry narrows the same payload to the
// same result.
func copyParamsForChannel(catalog *i18n.Catalog, locale, typeKey, channel string, params map[string]any) (map[string]any, bool) {
	if catalog == nil {
		return nil, false
	}
	full, err := renderContent(catalog, locale, typeKey, channel, params)
	if err != nil {
		return nil, false
	}
	if len(params) == 0 {
		return nil, true
	}
	kept := make(map[string]any, len(params))
	for name, value := range params {
		probe := make(map[string]any, len(params)-1)
		for k, v := range params {
			if k != name {
				probe[k] = v
			}
		}
		if len(probe) == 0 {
			probe = nil
		}
		parts, err := renderContent(catalog, locale, typeKey, channel, probe)
		if err != nil {
			// Removing the parameter broke the copy: the templates need it
			// in a way the rendering depends on. Keep it.
			kept[name] = value
			continue
		}
		if !sameRenderedParts(parts, full) {
			kept[name] = value
		}
	}
	return kept, true
}

// sameRenderedParts reports whether two part maps are identical, part for
// part -- the byte-for-byte comparison copyParamsForChannel's removal
// probe judges a parameter's copy dependence by.
func sameRenderedParts(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for part, text := range a {
		if b[part] != text {
			return false
		}
	}
	return true
}

// renderTypeDescription renders one notification type's directory copy --
// the Description the type directory (handler.go's NotificationListTypes)
// serves alongside each declared type -- in the locale the request
// negotiated, from the host's merged message catalog.
//
// # Description-id convention
//
// The directory copy lives in the same id space as the channel templates
// renderContent resolves, under the id the type key and ".description"
// spell: a business module that declares "billing.invoice_paid" ships
// "billing.invoice_paid.description" in both languages of its own bundle,
// next to the type's channel templates, under the same AddModule id
// constraint. The copy renders with no parameters -- the directory serves
// the type itself, never an instance of it, so no per-message params exist
// to interpolate -- and a description template that nevertheless references
// one is a loud render failure like any other, never an empty slot: the
// producer forgot that directory copy must stand alone, and the coded
// failure names the type so the gap is traceable.
//
// # Failure mode
//
// The strictness is renderContent's own: a nil catalog or a missing
// description id is ErrInternal.WithCause -- never a fallback to another
// language and never a hole in the directory listing. A type whose
// declaring module shipped no directory copy fails the whole listing
// (handler.go's NotificationListTypes), so the gap cannot hide as a
// missing row a client renders around.
func renderTypeDescription(catalog *i18n.Catalog, locale, typeKey string) (string, error) {
	if catalog == nil {
		return "", ErrInternal.WithCause(errors.New("notification: render called with no catalog"))
	}
	text, err := catalog.Lookup(locale, typeKey+".description", map[string]any{})
	if err != nil {
		return "", ErrInternal.WithCause(fmt.Errorf("notification: render %s.description: %w", typeKey, err))
	}
	return text, nil
}
