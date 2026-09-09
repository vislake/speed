package pkgcore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// SMS is one message to deliver to a phone number, already rendered by the
// caller. SMSSender never sees a code or a locale separately: by the time an
// SMS reaches this seam it is content, not data. Rendering (templates, the
// recipient's locale) and consent checks are the caller's business, never
// the transport's -- the same division pkgcore.Mailer's doc comment draws
// for email -- and so is number normalization: To travels in whatever form
// the caller built it in, every implementation passes it through to its
// transport unchanged, and a caller that needs a canonical form (the E.164
// shape dbkit.NormalizePhoneE164 produces, say) normalizes before the
// message reaches this seam.
type SMS struct {
	To   string
	Text string
}

// SMSSender is the outbound-SMS contract shared by every module that
// delivers a short message to a phone number. It is the contract go/authn's
// phone-login verification codes and go/notification's sms-channel
// deliveries both send through -- those two modules used to declare
// identical SMSSender interfaces of their own, which forced a host wiring
// both to hand one implementation to each, and forced a notification host
// that wanted carrier delivery to reimplement the adapters go/authn's
// subpackages already were; the seam lives here, on the dependency floor
// both modules stand on, so one contract and one set of implementations
// serve every consumer.
//
// The interface is designed against the weakest implementation it must
// support, exactly as Mailer is: Send takes one already-rendered SMS and
// reports only success or failure. The message shape is two strings, the
// call is one round trip, and nothing else is promised -- no delivery
// receipt, no retry policy, no template handling, no number validation.
//
// Send must honour a cancelled context by returning its context's error
// instead of sending, must not retain the SMS after returning, and must be
// safe for concurrent use by multiple goroutines. An error means the message
// was not delivered; the caller decides what a delivery failure means for
// its flow (go/authn answers a failed code delivery indistinguishably from a
// request for an unregistered number, and go/notification settles the
// attempt as failed and marks the contact bounced on a permanent transport
// error).
//
// Unlike the four kernel-resolved seams (EventBus, KVStore, Mailer and
// ObjectStore), SMSSender deliberately has no registry, preset or capability
// declarations, and a Kernel never resolves one: no consumer takes its SMS
// transport from the registry -- go/authn and go/notification both receive
// the sender through their own module-wiring options, and each enforces its
// own wiring-time requirement on it (a distributed-mode authn refuses to
// boot without an explicitly wired sender rather than defaulting to one that
// prints to a writer nobody reads). The promotion makes the seam a shared
// contract and shared implementations; it does not move SMS onto the kernel.
// The console implementation in this file is the zero-external-dependency
// one; NewHTTPSMSSender (http_sms_sender.go) is the operator-gateway
// transport, and the sms/aliyun, sms/tencent and sms/twilio subpackages
// carry the three real carrier adapters.
type SMSSender interface {
	// Send delivers sms. An error means the message was not delivered.
	Send(ctx context.Context, sms SMS) error
}

// consoleSMSSender is the zero-external-dependency SMSSender: it prints
// every message to an io.Writer instead of sending anything -- the
// console-form degradation for SMS ("printed to stdout"), the same role
// consoleMailer plays for email. A mutex guards the writer so that
// concurrent Send calls cannot interleave their output.
type consoleSMSSender struct {
	mu sync.Mutex
	w  io.Writer
}

// NewConsoleSMSSender returns the console SMSSender, writing every message
// to w as one record per send.
//
// w is required and never defaults to os.Stdout implicitly: an implicit
// global write target is exactly what makes a sender unassertable -- a test
// points w at a buffer and asserts on the printed record, which is how every
// consumer's suite observes what was "sent" -- and a console sender is the
// standalone deployment mode's transport and the test double of code written
// against SMSSender, so the writer it prints to must be the caller's
// declared choice.
func NewConsoleSMSSender(w io.Writer) SMSSender {
	return &consoleSMSSender{w: w}
}

// Send implements SMSSender by printing sms to the underlying writer as one
// contiguous record, so that concurrent sends never interleave mid-message.
func (s *consoleSMSSender) Send(ctx context.Context, sms SMS) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	var record bytes.Buffer
	fmt.Fprintf(&record, "SMS to %s: %s\n", sms.To, sms.Text)

	s.mu.Lock()
	defer s.mu.Unlock()

	// A writer may accept fewer bytes than offered without erroring; loop
	// until the whole record is out or the writer fails. A writer reporting
	// neither progress nor an error violates the io.Writer contract; without
	// this guard the loop would spin forever on it.
	remaining := record.Bytes()
	for len(remaining) > 0 {
		n, err := s.w.Write(remaining)
		if err != nil {
			return fmt.Errorf("pkgcore: console sms send: write failed: %w", err)
		}
		if n == 0 {
			return errors.New("pkgcore: console sms send: writer made no progress")
		}
		remaining = remaining[n:]
	}
	return nil
}
