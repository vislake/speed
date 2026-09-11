package pkgcore

import (
	"fmt"
	"net/mail"
	"strings"
	"unicode"
)

// validateMail enforces the rules ErrInvalidMail describes. It is shared by
// every implementation so that a message accepted by one backend is accepted
// by all of them, and the checks run before a message touches the wire.
//
// This is the injection barrier for the raw interpolation in mailer_smtp.go:
// every Mailer.Send calls validateMail first and returns before reaching
// buildMessage, so neither a CR nor an LF in any header field -- From, To,
// Subject or the optional ReplyTo -- ever survives to end a header line, and
// the address fields (From, every To element, the optional ReplyTo) carry no
// control character at all. The address rule is deliberately wider than the
// header rule: the addresses also travel unescaped into the SMTP commands
// themselves (MAIL FROM, RCPT TO), where net/smtp's own command-line guard
// rejects CR and LF only, so anything else in that class would reach the
// relay verbatim. The Subject keeps the CR/LF-only rule: a tab is legal
// header whitespace, non-ASCII travels through RFC 2047 encoding in
// buildMessage, and a subject without a CR or LF cannot end its header line.
//
// The address fields carry a second, structural rule: each must parse as one
// address (isValidAddress, net/mail.ParseAddress), because a value that is
// not even address-shaped -- a bare string, a missing "@", two addresses in
// one field -- would reach the relay verbatim in exactly the same two places
// the control rule guards. The structural rule is exactly the parser, so the
// display form and every other shape the parser accepts stay accepted.
//
// CodeQL's go/email-injection alert on mailer_smtp.go does not recognize this
// validate-then-return-early pattern as a sanitizing barrier across the two
// separate call sites (Send validates, then later in the same function calls
// buildMessage), and the query models no sanitizer, so no code change clears
// it -- reviewed and confirmed a false positive; do not remove this check
// without re-auditing that alert. See mailer_console_test.go's
// TestConsoleMailer_RejectsInvalidMail, mailer_smtp_test.go's
// TestSMTPMailer_Send_RejectsInvalidMailWithoutTouchingTheWire and
// TestSMTPMailer_Send_ControlCharacterAddressesNeverReachTheWire and
// TestSMTPMailer_Send_AddressesThatDoNotParseNeverReachTheWire, and
// mailertest/assert_conforms.go for the tests pinning this guarantee.
func validateMail(mail Mail) error {
	if mail.From == "" {
		return fmt.Errorf("%w: From is empty", ErrInvalidMail)
	}
	if len(mail.To) == 0 {
		return fmt.Errorf("%w: To has no recipients", ErrInvalidMail)
	}
	for _, recipient := range mail.To {
		if recipient == "" {
			return fmt.Errorf("%w: To contains an empty recipient", ErrInvalidMail)
		}
	}
	// The line-break check comes first in each loop, so a CR or LF -- the
	// characters that would inject further header lines -- gets its own, more
	// precise message.
	for _, field := range []struct{ name, value string }{
		{"From", mail.From},
		{"ReplyTo", mail.ReplyTo},
	} {
		if strings.ContainsAny(field.value, "\r\n") {
			return fmt.Errorf("%w: %s must not contain a line break", ErrInvalidMail, field.name)
		}
		if hasControlCharacter(field.value) {
			return fmt.Errorf("%w: %s must not contain a control character", ErrInvalidMail, field.name)
		}
	}
	for _, recipient := range mail.To {
		if strings.ContainsAny(recipient, "\r\n") {
			return fmt.Errorf("%w: To must not contain a line break", ErrInvalidMail)
		}
		if hasControlCharacter(recipient) {
			return fmt.Errorf("%w: To must not contain a control character", ErrInvalidMail)
		}
	}
	// The structural rule runs after the character rules, so a control
	// character keeps its more precise message. Every address field must
	// parse as one address: a value that is not even address-shaped -- a
	// bare string, a missing "@", two addresses in one field -- would
	// travel verbatim into the MAIL FROM/RCPT TO commands and the raw DATA
	// headers, where no relay can make it deliverable. ReplyTo is optional,
	// so only a non-empty one is required to parse.
	if !isValidAddress(mail.From) {
		return fmt.Errorf("%w: From is not a valid address", ErrInvalidMail)
	}
	if mail.ReplyTo != "" && !isValidAddress(mail.ReplyTo) {
		return fmt.Errorf("%w: ReplyTo is not a valid address", ErrInvalidMail)
	}
	for _, recipient := range mail.To {
		if !isValidAddress(recipient) {
			return fmt.Errorf("%w: To contains an invalid address", ErrInvalidMail)
		}
	}
	if strings.ContainsAny(mail.Subject, "\r\n") {
		return fmt.Errorf("%w: Subject must not contain a line break", ErrInvalidMail)
	}
	if mail.Text == "" && mail.HTML == "" {
		return fmt.Errorf("%w: neither Text nor HTML body is set", ErrInvalidMail)
	}
	return nil
}

// hasControlCharacter reports whether s carries any control character: the C0
// range, DEL, or the C1 range (U+0085 among them). An address field is
// interpolated into an SMTP command line and a raw header line, and a control
// character has no legitimate place in either.
func hasControlCharacter(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// isValidAddress reports whether s parses as one address through
// net/mail.ParseAddress, the structural arbiter the address rule names: the
// plain addr-spec form ("ada@example.com") and the name-and-address display
// form ("Ada <ada@example.com>") both pass, and so does everything else the
// parser accepts -- a plus-tagged or otherwise non-ASCII local part, an IDN
// domain, a domain without a dot. The rule is exactly that parser, never a
// second opinion on top of it. A value it rejects (a bare string, a missing
// "@", an empty side, whitespace, adjacent dots, two addresses in one string)
// would travel verbatim into the MAIL FROM/RCPT TO commands and the raw
// headers, where nothing downstream can repair it. The parse failure itself
// is deliberately not returned: net/mail's errors quote input fragments
// back, and an address is sensitive, so callers get the field name and the
// sentinel, never the value.
func isValidAddress(s string) bool {
	_, err := mail.ParseAddress(s)
	return err == nil
}
