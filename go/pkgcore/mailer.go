package pkgcore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// ErrInvalidMail is returned by Mailer.Send when the message fails the rules
// every implementation enforces: an empty From, no recipients, an empty
// recipient, a newline inside a header field, or neither body set. A Send that
// returns it has not touched the wire. The offending value is deliberately
// kept out of the error text, because a message may carry sensitive data.
var ErrInvalidMail = errors.New("pkgcore: invalid mail")

// Mail is one outbound email message, already rendered by the caller. The
// headers hold plain addresses (name-and-address display forms are the
// caller's job, and must not smuggle in line breaks); the bodies are plain
// text and HTML alternatives of the same content, at least one of which must
// be non-empty.
type Mail struct {
	From    string
	To      []string
	Subject string

	// Text and HTML are the two renderings of the message body. When both are
	// set, a recipient that supports HTML receives the HTML rendering and the
	// others fall back to Text. When only one is set, it is sent as-is.
	Text string
	HTML string
}

// Mailer is the outbound-email contract shared by every deployment mode: the
// console mailer printing to standard output in the standalone deployment
// mode, an SMTP client in the distributed deployment mode.
//
// The interface is deliberately designed against the weakest backend it must
// support, so Send takes one already-rendered Mail and reports only success or
// failure. Rendering (templates, the recipient's locale), consent checks and
// retry policy are the caller's business, never the transport's: a Mailer
// carries bytes, it does not decide who may be written to or what they are
// told.
//
// Send must honour a cancelled context by returning its error instead of
// sending, must not retain the mail after returning, and implementations must
// be safe for concurrent use by multiple goroutines. A message that fails the
// shared rules above is rejected with ErrInvalidMail before anything is sent.
type Mailer interface {
	// Send delivers mail to every recipient in mail.To.
	Send(ctx context.Context, mail Mail) error
}

// validateMail enforces the rules ErrInvalidMail describes. It is shared by
// every implementation so that a message accepted by one backend is accepted
// by all of them, and the checks run before a message touches the wire.
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
	// Header fields end at the first CR or LF. Accepting one inside From, To or
	// Subject would let a message smuggle extra headers into the SMTP
	// conversation, so every implementation rejects them up front.
	for _, field := range []struct{ name, value string }{
		{"From", mail.From},
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

// consoleMailer is the standalone deployment mode's Mailer: it prints every
// message to an io.Writer, standard output by default. A mutex guards the
// writer so that concurrent Send calls cannot interleave their output.
type consoleMailer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewConsoleMailer returns the standalone deployment mode's Mailer, which
// prints every message to standard output instead of sending it. The printed
// record is deliberately greppable and self-delimiting:
//
//	[mail] from: ops@example.com
//	[mail] to: ada@example.com
//	[mail] subject: Your invoice #1042 is ready
//	[mail] text/plain:
//	Hello Ada,
//	[mail] end
//
// Each line is prefixed so that a mail landing in a log stream is easy to
// filter; each body block is bracketed by markers so that consecutive
// messages cannot blur together. The mailer doubles as a test double for code
// written against Mailer: it accepts every valid message, fails on the same
// rules every implementation enforces, and shares no state between instances.
func NewConsoleMailer() Mailer {
	return &consoleMailer{w: os.Stdout}
}

// newConsoleMailer returns a console mailer writing to w. It is the
// unexported twin of NewConsoleMailer, for tests that want to assert on the
// printed record.
func newConsoleMailer(w io.Writer) Mailer {
	return &consoleMailer{w: w}
}

// Send implements Mailer.Send by printing mail to the underlying writer as one
// contiguous record, so that concurrent sends never interleave mid-message.
func (m *consoleMailer) Send(ctx context.Context, mail Mail) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateMail(mail); err != nil {
		return err
	}

	var record bytes.Buffer
	fmt.Fprintf(&record, "[mail] from: %s\n", mail.From)
	fmt.Fprintf(&record, "[mail] to: %s\n", strings.Join(mail.To, ", "))
	fmt.Fprintf(&record, "[mail] subject: %s\n", mail.Subject)
	if mail.Text != "" {
		record.WriteString("[mail] text/plain:\n")
		record.WriteString(mail.Text)
		writeTerminatingNewline(&record, mail.Text)
	}
	if mail.HTML != "" {
		record.WriteString("[mail] text/html:\n")
		record.WriteString(mail.HTML)
		writeTerminatingNewline(&record, mail.HTML)
	}
	record.WriteString("[mail] end\n")

	m.mu.Lock()
	defer m.mu.Unlock()

	// A writer may accept fewer bytes than offered without erroring; loop until
	// the whole record is out or the writer fails. A writer reporting neither
	// progress nor an error violates the io.Writer contract; without this guard
	// the loop would spin forever on it.
	remaining := record.Bytes()
	for len(remaining) > 0 {
		n, err := m.w.Write(remaining)
		if err != nil {
			return fmt.Errorf("pkgcore: console mailer write failed: %w", err)
		}
		if n == 0 {
			return errors.New("pkgcore: console mailer: writer made no progress")
		}
		remaining = remaining[n:]
	}
	return nil
}

// writeTerminatingNewline appends the newline that closes a body block, unless
// the body already ends with one. The marker that follows must start on its
// own line even when the body is not newline-terminated.
func writeTerminatingNewline(w *bytes.Buffer, body string) {
	if !strings.HasSuffix(body, "\n") {
		w.WriteByte('\n')
	}
}
