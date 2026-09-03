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
	if !hasCode(err, ErrInternal.Code) {
		t.Errorf("error = %v, want org.internal_error", err)
	}
}

func TestRenderInvitationMail_NoCatalog(t *testing.T) {
	_, err := renderInvitationMail(nil, testMailFrom, testMailData(i18n.LocaleENUS))
	if !hasCode(err, ErrInternal.Code) {
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

func TestNegotiateLocale(t *testing.T) {
	catalog := newTestHost(t).catalog

	tests := []struct {
		name      string
		catalog   *i18n.Catalog
		requested string
		want      string
	}{
		{"a served locale is honored", catalog, i18n.LocaleENUS, i18n.LocaleENUS},
		{"the default is honored", catalog, i18n.LocaleZHCN, i18n.LocaleZHCN},
		{"an unserved locale falls back", catalog, "fr-FR", i18n.LocaleZHCN},
		{"an empty preference falls back", catalog, "", i18n.LocaleZHCN},
		{"no catalog falls back", nil, i18n.LocaleENUS, i18n.LocaleZHCN},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := negotiateLocale(tc.catalog, tc.requested); got != tc.want {
				t.Errorf("negotiateLocale(%q) = %q, want %q", tc.requested, got, tc.want)
			}
		})
	}
}

func TestSendMail_WithoutATransport(t *testing.T) {
	mail := pkgcore.Mail{From: testMailFrom, To: []string{"ada@example.test"}, Text: "hello"}

	if err := sendMail(context.Background(), nil, mail); !hasCode(err, ErrInternal.Code) {
		t.Errorf("sendMail with no host error = %v, want org.internal_error", err)
	}
	host := newTestHost(t)
	host.mailer = nil
	if err := sendMail(context.Background(), host, mail); !hasCode(err, ErrInternal.Code) {
		t.Errorf("sendMail with no mailer error = %v, want org.internal_error", err)
	}
}

// TestMail_RendersRecipientLocale_NotOperatorLocale pins the SERVICE-layer
// half of the coding standard's rule -- backend-generated content renders in
// the RECIPIENT's locale -- by driving InviteService.Invite directly: two
// invitations, each with a different InviteRequest.Locale, must render two
// different subjects, and Invite must never look anywhere else (an
// operator-scoped field, say) for the language to use.
//
// It deliberately does NOT exercise where InviteRequest.Locale itself comes
// from at the HTTP boundary -- that is
// TestHandler_OrgCreateInvitation_LocaleIsFromRequestBody_NeverAcceptLanguage
// (handler_test.go), which is the test that actually proves the operator's
// own Accept-Language header is never consulted. The two tests are
// deliberately split at the same seam the code is: this one to the request
// struct, that one to the header the request struct must never come from.
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
// Accept-Language org does not serve is a preference, not a command: the
// invitation is still sent, in the platform default.
func TestMail_UnsupportedRequestedLocale_FallsBackWithoutFailing(t *testing.T) {
	f := newInviteFixture(t)

	result, err := f.m.Invitations().Invite(f.ctx, InviteRequest{
		Email: "ada@example.test", NodeID: f.left.ID, InviterUserID: "u-inviter", Locale: "fr-FR",
	})
	if err != nil {
		t.Fatalf("Invite(fr-FR): %v", err)
	}
	if result.Invitation.Locale != i18n.LocaleZHCN {
		t.Errorf("captured locale = %q, want the platform default %q", result.Invitation.Locale, i18n.LocaleZHCN)
	}
	if len(f.host.mailer.messages()) != 1 {
		t.Error("the invitation was not sent")
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
