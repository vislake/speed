package notification

import (
	"context"
	"fmt"
	"io"
)

// SMS is one outbound text message, already rendered by the caller: the
// recipient's number and the message text. The transport carries bytes, it
// does not decide who may be written to or what they are told -- consent
// checking and rendering belong to the caller, exactly as pkgcore.Mailer's
// doc comment says of its own Mail.
//
// To is a phone number in E.164 form ("+8613800138000"), the canonical form
// dbkit.NormalizePhoneE164 produces; every number this module sends to has
// passed through that normalizer on its way into verified_contacts.
type SMS struct {
	To   string
	Text string
}

// SMSSender is the outbound-SMS contract of this module.
//
// It exists because pkgcore has no SMS seam: authn's console and HTTP senders
// live inside go/authn and cannot be imported (this module never imports
// authn). The interface below is structurally identical to authn's own
// sender, so a host's wiring can hand the same implementation to both
// modules without either importing the other -- see the R3 adjudication's
// note in the round journal: promoting SMS to a pkgcore seam alongside
// Mailer is a later-round pkgcore change, recorded as a deferral in
// AGENTS.md.
//
// Send must honour a cancelled context by returning its context's error
// instead of sending, must not retain the SMS after returning, and must be
// safe for concurrent use by multiple goroutines -- the three transport
// contracts pkgcore.Mailer pins for email, mirrored here so every
// implementation honours them identically.
type SMSSender interface {
	Send(ctx context.Context, sms SMS) error
}

// NewConsoleSMSSender returns an SMSSender that prints every message to w,
// one record per send. It is the zero-external-dependency implementation:
// the standalone deployment mode's sender, and the test double every unit
// test in this package asserts on.
//
// w is required -- there is deliberately no variant that falls back to
// os.Stdout. The writer is captured at construction, so a test can point the
// sender at a buffer and assert on the printed record; a sender that picked
// its own sink would make every test that wants to see what was "sent" reach
// for process-global output instead. The console sender validates nothing a
// real gateway would (number shape, deliverability): it is a transport for a
// composition that has no transport, and the module's own address
// normalization already ran before anything reaches Send.
func NewConsoleSMSSender(w io.Writer) SMSSender {
	return &consoleSMSSender{w: w}
}

// consoleSMSSender prints one record per Send to the writer it was built
// with.
type consoleSMSSender struct {
	w io.Writer
}

func (s *consoleSMSSender) Send(_ context.Context, sms SMS) error {
	_, err := fmt.Fprintf(s.w, "SMS to %s: %s\n", sms.To, sms.Text)
	return err
}
