package org

import (
	"context"
	"errors"
	"html"
	"slices"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// The message ids org renders content from. They live in
// locales/{zh-CN,en-US}.toml with identical id sets, and never as Go string
// literals: the invitation is user-facing text, and user-facing text is not
// written in code in any language.
const (
	// msgInvitationSubject is the invitation email's subject line.
	msgInvitationSubject = "org.invitation.subject"
	// msgInvitationBodyText is the plain-text body.
	msgInvitationBodyText = "org.invitation.body_text"
	// msgInvitationBodyHTML is the HTML body.
	msgInvitationBodyHTML = "org.invitation.body_html"
	// msgDefaultWorkspaceName names the root node org creates on its own
	// when a brand-new user arrives with no tree in their tenant.
	msgDefaultWorkspaceName = "org.default_workspace_name"
)

// errNoCatalog reports that the host seam produced no message catalog. It is
// wrapped as the cause of an ErrInternal, never returned bare.
//
// In practice it means the runtime was used before Kernel.Bootstrap finished:
// Registry.Locales() is documented to be nil while modules are registering,
// which is precisely why every render reads the catalog through the host seam
// at call time instead of capturing it in Register.
var errNoCatalog = errors.New("org: the host registry carries no message catalog")

// errNoMailer reports that the host seam produced no mailer.
var errNoMailer = errors.New("org: the host registry carries no mailer")

// errNoKVStore reports that the host seam produced no key-value store, which
// leaves the invitation rate limiter with nothing to count in. It fails the
// invite rather than letting an uncounted one through: a rate limit that
// cannot be evaluated must deny, never allow.
var errNoKVStore = errors.New("org: the host registry carries no key-value store")

// InvitationLinkBuilder turns an invitation token into the URL the invitee
// clicks. The host supplies it (WithInvitationLinkBuilder) because only the
// host knows its own public address -- and, for a multi-tenant deployment,
// which host name belongs to the tenant in ctx.
//
// That per-tenant host is not decoration. InviteService.Accept resolves an
// invitation strictly inside the tenant the request context already carries
// and never reads a tenant out of the token, so the link has to arrive at
// the tenant's own entry point to be acceptable at all.
type InvitationLinkBuilder func(ctx context.Context, token string) (string, error)

// invitationMailData is everything the invitation templates interpolate.
//
// Deliberately small, and deliberately free of anything sensitive beyond the
// two values the message exists to carry: the recipient's address, which is
// the To header, and the accept URL, which embeds the token. Neither is ever
// logged.
type invitationMailData struct {
	// to is the recipient address.
	to string
	// nodeName is the display name of the node the invitee is being invited
	// into, so the message says what they are joining.
	nodeName string
	// acceptURL is the link the invitee follows, token included.
	acceptURL string
	// locale is the language to render in: the RECIPIENT's, captured on the
	// invitation row, never the operator's UI language.
	locale string
}

// renderInvitationMail builds the invitation message from the catalog.
//
// Every failure is loud. A missing message id surfaces as an error rather
// than an empty subject or a blank body, because i18n.Catalog.Lookup never
// falls back to another language -- the whole point of that design is that a
// gap is visible instead of silently rendering English at a Chinese
// recipient, and swallowing the error here would give that guarantee away.
//
// nodeName is HTML-escaped for the HTML body. It is tenant-supplied text and
// go-i18n renders with text/template, which does not escape; the plain-text
// body takes it verbatim.
func renderInvitationMail(catalog *i18n.Catalog, from string, data invitationMailData) (pkgcore.Mail, error) {
	if catalog == nil {
		return pkgcore.Mail{}, ErrInternal.WithCause(errNoCatalog)
	}

	textParams := map[string]any{
		"node_name":  data.nodeName,
		"accept_url": data.acceptURL,
	}
	htmlParams := map[string]any{
		"node_name":  html.EscapeString(data.nodeName),
		"accept_url": html.EscapeString(data.acceptURL),
	}

	subject, err := catalog.Lookup(data.locale, msgInvitationSubject, textParams)
	if err != nil {
		return pkgcore.Mail{}, ErrInternal.WithCause(err)
	}
	text, err := catalog.Lookup(data.locale, msgInvitationBodyText, textParams)
	if err != nil {
		return pkgcore.Mail{}, ErrInternal.WithCause(err)
	}
	htmlBody, err := catalog.Lookup(data.locale, msgInvitationBodyHTML, htmlParams)
	if err != nil {
		return pkgcore.Mail{}, ErrInternal.WithCause(err)
	}

	return pkgcore.Mail{
		From:    from,
		To:      []string{data.to},
		Subject: subject,
		Text:    text,
		HTML:    htmlBody,
	}, nil
}

// sendMail hands a rendered message to the host's Mailer.
//
// org owns no transport. In the standalone deployment mode the host wired
// pkgcore.NewConsoleMailer and the message is printed; in the distributed one
// it wired NewSMTPMailer. Nothing in this module knows which, and nothing in
// it branches on the deployment mode.
func sendMail(ctx context.Context, host hostSeams, mail pkgcore.Mail) error {
	if host == nil {
		return ErrInternal.WithCause(errNoMailer)
	}
	mailer := host.Mailer()
	if mailer == nil {
		return ErrInternal.WithCause(errNoMailer)
	}
	if err := mailer.Send(ctx, mail); err != nil {
		return ErrInternal.WithCause(err)
	}
	return nil
}

// negotiateLocale returns the locale an invitation should be rendered in:
// the requested one when the catalog actually serves it, and the platform
// default otherwise.
//
// It runs once, when the invitation is created, and the result is stored on
// the row -- so the message renders in the language the inviting operator
// named for the invitee at that moment (OrgCreateInvitationRequest.locale;
// see Invitation.Locale), and a later send renders the same language rather
// than whatever the sending operator happens to be using. requested is
// NEVER an Accept-Language header: the invitee has made no HTTP request of
// their own yet, so there is nothing to read one from, and the operator's
// own header would only give back the operator's language.
//
// Falling back to the default here rather than failing is right because the
// input is a preference, not a command -- an operator may name a locale the
// platform carries no catalog for, or name none at all. Falling back at
// LOOKUP time would be wrong, and the catalog refuses to: a locale it
// serves must carry every id.
func negotiateLocale(catalog *i18n.Catalog, requested string) string {
	if catalog == nil {
		return i18n.LocaleZHCN
	}
	if slices.Contains(catalog.Locales(), requested) {
		return requested
	}
	return i18n.LocaleZHCN
}
