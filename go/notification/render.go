package notification

import (
	"errors"
	"fmt"

	"github.com/vislake/speed/go/pkgcore/i18n"
)

// renderContent renders one notification type's message copy -- the title
// and the body -- in the recipient's locale, from the host's merged message
// catalog.
//
// # Template-id convention
//
// A business module that declares a notification type owns its copy: for a
// type key <module>.<entity>.<action> ("billing.invoice_paid"), the
// templates live in that module's own locale files under the ids
// <type_key>.title and <type_key>.body -- inside the declaring module's
// catalog id space, which is exactly why pkgcore/i18n's Builder.AddModule
// requires every id in a module's bundle to start with that module's name.
// notification never ships templates for types it does not declare, and its
// own bundle (locales/) carries only its error codes; delivery renders from
// the merged catalog the host assembled from every module's bundles.
//
// This function is the delivery path's render step, but the delivery
// subscriber itself is a later block of this round. renderContent is
// therefore unexported today, existing as this file's single render seam
// pinned by tests so the convention above is proven before the subscriber
// builds on it; the later block lifts it into the module's exported surface
// when a consumer outside the package first needs it.
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
func renderContent(catalog *i18n.Catalog, locale, typeKey string, params map[string]any) (title, body string, err error) {
	if catalog == nil {
		return "", "", ErrInternal.WithCause(errors.New("notification: render called with no catalog"))
	}

	title, err = catalog.Lookup(locale, typeKey+".title", params)
	if err != nil {
		return "", "", ErrInternal.WithCause(fmt.Errorf("notification: render title for %s: %w", typeKey, err))
	}
	body, err = catalog.Lookup(locale, typeKey+".body", params)
	if err != nil {
		return "", "", ErrInternal.WithCause(fmt.Errorf("notification: render body for %s: %w", typeKey, err))
	}
	return title, body, nil
}
