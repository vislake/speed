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

// consoleMailer is the standalone deployment mode's Mailer: it prints every
// message to an io.Writer, standard output by default. A mutex guards the
// writer so that concurrent Send calls cannot interleave their output.
//
// w is an arbitrary io.Writer (os.Stdout in production), never an
// http.ResponseWriter -- see go/observability/middleware.go's statusRecorder
// (its own Write method) for the CodeQL go/reflected-xss false positive
// this field's Write call is one side of: CodeQL's points-to analysis
// conflates the two because both satisfy the identical Write([]byte) (int,
// error) signature, but they are never the same writer instance at runtime.
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
// A message carrying a ReplyTo prints one extra line, [mail] reply-to: ...,
// between the to and subject lines.
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
	if mail.ReplyTo != "" {
		fmt.Fprintf(&record, "[mail] reply-to: %s\n", mail.ReplyTo)
	}
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
