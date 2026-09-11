package pkgcore

import (
	"fmt"
	"strings"
)

// validateMail enforces the rules ErrInvalidMail describes. It is shared by
// every implementation so that a message accepted by one backend is accepted
// by all of them, and the checks run before a message touches the wire.
//
// This is the header-injection barrier for buildMessage's raw From/To/Subject
// interpolation in smtp_mailer.go: every Mailer.Send calls validateMail first
// and returns before reaching buildMessage, so a \r or \n in any of those
// fields -- or in the optional ReplyTo, which buildMessage interpolates the
// same way -- never survives to be written as a header line. CodeQL's
// go/email-injection alert on smtp_mailer.go does not recognize this
// validate-then-return-early pattern as a sanitizing barrier across the two
// separate call sites (Send validates, then later in the same function calls
// buildMessage) -- reviewed and confirmed a false positive; do not remove
// this check without re-auditing that alert. See console_mailer_test.go's
// TestConsoleMailer_RejectsInvalidMail, smtp_mailer_test.go's
// TestSMTPMailer_Send_RejectsInvalidMailWithoutTouchingTheWire, and
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
	// Header fields end at the first CR or LF. Accepting one inside From, To,
	// Subject or the optional ReplyTo would let a message smuggle extra
	// headers into the SMTP conversation, so every implementation rejects them
	// up front.
	for _, field := range []struct{ name, value string }{
		{"From", mail.From},
		{"ReplyTo", mail.ReplyTo},
		{"Subject", mail.Subject},
	} {
		if strings.ContainsAny(field.value, "\r\n") {
			return fmt.Errorf("%w: %s must not contain a line break", ErrInvalidMail, field.name)
		}
	}
	for _, recipient := range mail.To {
		if strings.ContainsAny(recipient, "\r\n") {
			return fmt.Errorf("%w: To must not contain a line break", ErrInvalidMail)
		}
	}
	if mail.Text == "" && mail.HTML == "" {
		return fmt.Errorf("%w: neither Text nor HTML body is set", ErrInvalidMail)
	}
	return nil
}
