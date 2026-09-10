package org

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// testMailData is the fixture every render test starts from.
func testMailData(locale string) invitationMailData {
	return invitationMailData{
		to:        "ada@example.test",
		nodeName:  "Downtown Clinic",
		acceptURL: testLinkBase + "a-token",
		locale:    locale,
	}
}

func TestRenderInvitationMail_RendersEachSupportedLocale(t *testing.T) {
	catalog := newTestHost(t).catalog

	rendered := map[string]pkgcore.Mail{}
	for _, locale := range []string{i18n.LocaleZHCN, i18n.LocaleENUS} {
		mail, err := renderInvitationMail(catalog, testMailFrom, testMailData(locale))
		if err != nil {
			t.Fatalf("renderInvitationMail(%s): %v", locale, err)
		}
		if mail.Subject == "" || mail.Text == "" || mail.HTML == "" {
			t.Fatalf("renderInvitationMail(%s) left a part empty: %+v", locale, mail)
		}
		if mail.From != testMailFrom || len(mail.To) != 1 || mail.To[0] != "ada@example.test" {
			t.Errorf("headers = From %q To %v, want %q and the invitee", mail.From, mail.To, testMailFrom)
		}
		for _, body := range []string{mail.Text, mail.HTML} {
			if !strings.Contains(body, testLinkBase+"a-token") {
				t.Errorf("a %s body does not carry the accept link", locale)
			}
			if !strings.Contains(body, "Downtown Clinic") {
				t.Errorf("a %s body does not name the node", locale)
			}
		}
		// The message satisfies the rules every pkgcore.Mailer enforces
		// before anything touches the wire (see pkgcore.ErrInvalidMail): a
		// non-empty From, a real recipient, and no line break smuggled into
		// a header field. A rendering mistake that broke one of them would
		// otherwise surface as a send-time failure in production.
		for name, field := range map[string]string{"From": mail.From, "Subject": mail.Subject, "To": mail.To[0]} {
			if strings.ContainsAny(field, "\r\n") {
				t.Errorf("the %s header of the %s message carries a line break: %q", name, locale, field)
			}
		}
		rendered[locale] = mail
	}

	// The two languages genuinely differ; a bundle that accidentally copied
	// one into the other would pass every other assertion here.
	if rendered[i18n.LocaleZHCN].Subject == rendered[i18n.LocaleENUS].Subject {
		t.Error("the zh-CN and en-US subjects are identical; one language is not translated")
	}
}

// TestRenderInvitationMail_UnknownLocale_IsLoud pins the catalog's
// no-fallback contract at org's own boundary: an unsupported locale is an
// error, never silently the other language's text.
func TestRenderInvitationMail_UnknownLocale_IsLoud(t *testing.T) {
	catalog := newTestHost(t).catalog

	if _, err := renderInvitationMail(catalog, testMailFrom, testMailData("fr-FR")); err == nil {
		t.Fatal("renderInvitationMail(fr-FR) succeeded; an unsupported locale must be an error")
	}
}

// TestRenderInvitationMail_MissingKey_IsAnErrorNotABlankBody is the failure
// the i18n design exists to make visible. A catalog without org's invitation
// ids must not produce an empty subject or an empty body.
func TestRenderInvitationMail_MissingKey_IsAnErrorNotABlankBody(t *testing.T) {
	incomplete := fstest.MapFS{
		"zh-CN.toml": &fstest.MapFile{Data: []byte(`"org.unrelated" = "unrelated"` + "\n")},
		"en-US.toml": &fstest.MapFile{Data: []byte(`"org.unrelated" = "unrelated"` + "\n")},
	}
	builder := i18n.NewBuilder()
	if err := builder.AddModule(moduleName, incomplete); err != nil {
		t.Fatalf("AddModule: %v", err)
	}

	mail, err := renderInvitationMail(builder.Build(), testMailFrom, testMailData(i18n.LocaleENUS))
	if err == nil {
		t.Fatalf("renderInvitationMail with an incomplete catalog succeeded, producing %+v", mail)
	}
	if !apperr.HasCode(err, ErrInternal.Code) {
		t.Errorf("error = %v, want org.internal_error", err)
	}
}

func TestRenderInvitationMail_NoCatalog(t *testing.T) {
	_, err := renderInvitationMail(nil, testMailFrom, testMailData(i18n.LocaleENUS))
	if !apperr.HasCode(err, ErrInternal.Code) {
		t.Errorf("error = %v, want org.internal_error", err)
	}
}

// TestRenderInvitationMail_EscapesTheNodeNameInHTML pins the one escaping
// decision in this file: go-i18n renders with text/template, which does not
// escape, and a node name is tenant-supplied text.
func TestRenderInvitationMail_EscapesTheNodeNameInHTML(t *testing.T) {
	catalog := newTestHost(t).catalog
	data := testMailData(i18n.LocaleENUS)
	data.nodeName = `Clinic <script>alert("x")</script>`

	mail, err := renderInvitationMail(catalog, testMailFrom, data)
	if err != nil {
		t.Fatalf("renderInvitationMail: %v", err)
	}
	if strings.Contains(mail.HTML, "<script>") {
		t.Errorf("the HTML body carries an unescaped tag:\n%s", mail.HTML)
	}
	if !strings.Contains(mail.HTML, "&lt;script&gt;") {
		t.Errorf("the HTML body does not carry the escaped name:\n%s", mail.HTML)
	}
	// The plain-text body takes it verbatim: there is nothing to escape into.
	if !strings.Contains(mail.Text, "<script>") {
		t.Errorf("the plain-text body was escaped as if it were HTML:\n%s", mail.Text)
	}
}

// TestInvitationLocale_Chain drives the invitation chain's tier order on
// the service rule itself: the declared locale wins when the catalog serves
// it, an unusable or absent declared value falls to the requester's
// Accept-Language (the frontend chain's transported value), and the
// platform default en-US is the terminal tier -- for a request whose
// header matches nothing too, never a silent substitution.
func TestInvitationLocale_Chain(t *testing.T) {
	f := newInviteFixture(t)

	tests := []struct {
		name           string
		declared       string
		acceptLanguage string
		want           string
	}{
		{"a served declared locale wins", i18n.LocaleZHCN, i18n.LocaleENUS, i18n.LocaleZHCN},
		{"an unserved declared value falls to the requester language", "fr-FR", i18n.LocaleZHCN, i18n.LocaleZHCN},
		{"an absent declared value falls to the requester language", "", i18n.LocaleENUS, i18n.LocaleENUS},
		{"a header prefix matches", "", "zh", i18n.LocaleZHCN},
		{"no declared value and no usable header lands on the default", "", "fr-FR", i18n.LocaleENUS},
		{"nothing at all lands on the default", "", "", i18n.LocaleENUS},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.m.Invitations().invitationLocale(tc.declared, tc.acceptLanguage); got != tc.want {
				t.Errorf("invitationLocale(%q, %q) = %q, want %q", tc.declared, tc.acceptLanguage, got, tc.want)
			}
		})
	}
}

func TestSendMail_WithoutATransport(t *testing.T) {
	mail := pkgcore.Mail{From: testMailFrom, To: []string{"ada@example.test"}, Text: "hello"}

	if err := sendMail(context.Background(), nil, mail); !apperr.HasCode(err, ErrInternal.Code) {
		t.Errorf("sendMail with no host error = %v, want org.internal_error", err)
	}
	host := newTestHost(t)
	host.mailer = nil
	if err := sendMail(context.Background(), host, mail); !apperr.HasCode(err, ErrInternal.Code) {
		t.Errorf("sendMail with no mailer error = %v, want org.internal_error", err)
	}
}

// TestMail_RendersRecipientLocale_NotOperatorLocale pins the SERVICE-layer
// half of the chain: two invitations, each declaring a different
// InviteRequest.Locale, must render two different subjects -- the declared
// value is the chain's highest tier and the send renders what creation
// captured, whoever later triggers the send.
//
// It deliberately does NOT exercise where InviteRequest.Locale and
// InviteRequest.AcceptLanguage themselves come from at the HTTP boundary --
// that is TestHandler_OrgCreateInvitation_LocaleChain (handler_test.go). The
// two tests are split at the same seam the code is: this one to the request
// struct and its rendering, that one to the request fields the HTTP layer
// fills.
func TestMail_RendersRecipientLocale_NotOperatorLocale(t *testing.T) {
	f := newInviteFixture(t)

	english, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "ada@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter", Locale: i18n.LocaleENUS,
	})
	if err != nil {
		t.Fatalf("Invite(en-US): %v", err)
	}
	chinese, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "grace@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter", Locale: i18n.LocaleZHCN,
	})
	if err != nil {
		t.Fatalf("Invite(zh-CN): %v", err)
	}
	if english.Invitation.Locale != i18n.LocaleENUS || chinese.Invitation.Locale != i18n.LocaleZHCN {
		t.Fatalf("captured locales = %q and %q", english.Invitation.Locale, chinese.Invitation.Locale)
	}

	sent := f.host.mailer.messages()
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}
	// One inviter, one operator language, two recipients, two languages.
	if sent[0].Subject == sent[1].Subject {
		t.Errorf("both invitations rendered the same subject %q; the recipient's locale was ignored", sent[0].Subject)
	}

	catalog := f.host.catalog
	wantEN, err := catalog.Lookup(i18n.LocaleENUS, msgInvitationSubject, map[string]any{"node_name": f.left.Name})
	if err != nil {
		t.Fatalf("Lookup(en-US): %v", err)
	}
	wantZH, err := catalog.Lookup(i18n.LocaleZHCN, msgInvitationSubject, map[string]any{"node_name": f.left.Name})
	if err != nil {
		t.Fatalf("Lookup(zh-CN): %v", err)
	}
	if sent[0].Subject != wantEN {
		t.Errorf("first subject = %q, want the en-US %q", sent[0].Subject, wantEN)
	}
	if sent[1].Subject != wantZH {
		t.Errorf("second subject = %q, want the zh-CN %q", sent[1].Subject, wantZH)
	}
}

// TestMail_UnsupportedRequestedLocale_FallsBackWithoutFailing pins that an
// A locale org does not serve is a preference, not a command: the declared
// value is skipped, the chain continues to the requester's language, and
// the invitation is still sent -- and with neither a usable declared value
// nor a usable header, it lands on the platform default.
func TestMail_UnsupportedRequestedLocale_FallsBackWithoutFailing(t *testing.T) {
	f := newInviteFixture(t)

	// The declared value is unusable; the requester's language is not, so
	// the chain's second relevant tier answers.
	viaHeader, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "ada@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter",
		Locale: "fr-FR", AcceptLanguage: i18n.LocaleZHCN,
	})
	if err != nil {
		t.Fatalf("Invite(fr-FR, zh header): %v", err)
	}
	if viaHeader.Invitation.Locale != i18n.LocaleZHCN {
		t.Errorf("captured locale = %q, want the requester language %q", viaHeader.Invitation.Locale, i18n.LocaleZHCN)
	}
	if len(f.host.mailer.messages()) != 1 {
		t.Error("the invitation was not sent")
	}

	// Nothing usable anywhere: the platform default.
	viaDefault, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "grace@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter", Locale: "fr-FR",
	})
	if err != nil {
		t.Fatalf("Invite(fr-FR, no header): %v", err)
	}
	if viaDefault.Invitation.Locale != i18n.LocaleENUS {
		t.Errorf("captured locale = %q, want the platform default %q", viaDefault.Invitation.Locale, i18n.LocaleENUS)
	}
}

// TestMail_NoPlaintextTokenOrAddressInLogs is a security assertion with real
// teeth: it captures everything org logs during a successful invitation and
// fails if the bearer token or the invitee's address appears anywhere in it.
//
// Both are exactly the values the redaction rules name -- a token is a
// credential and an address is PII -- and a log line is written to a sink
// somebody else operates.
func TestMail_NoPlaintextTokenOrAddressInLogs(t *testing.T) {
	f := newInviteFixture(t)

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := obs.WithLogger(f.ctx, logger)

	const address = "ada.lovelace@example.test"
	result, err := f.m.Invitations().Invite(ctx, InviteRequest{
		Email: address, NodeID: f.left.ID, InviterUserID: "u-inviter", Locale: i18n.LocaleENUS,
	})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if _, err := f.m.Invitations().Accept(ctx, result.Token, "u-ada"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	output := logged.String()
	if output == "" {
		t.Fatal("nothing was logged; this test would pass vacuously")
	}
	if strings.Contains(output, result.Token) {
		t.Error("the invitation token appears in a log line")
	}
	if strings.Contains(strings.ToLower(output), address) {
		t.Error("the invitee's address appears in a log line")
	}
	// The blind index is what identifies the recipient for support purposes,
	// and it is safe to log precisely because it is not the address.
	if !strings.Contains(output, result.Invitation.EmailIndex) {
		t.Error("no log line carries the blind index; the send is untraceable for support")
	}
}
