package pkgcore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// smsConsoleComponent is the component descriptor for "sms.console", the
// print-to-stdout sender a composition configuration selects as the "sms"
// module's implementation; it registers itself from this file's init.
// Stateless is declared for the same reason mailer.console declares it:
// each Send writes the message to the writer and returns, so a restart
// drops nothing. The component prints to standard output -- the component
// is the caller that declares the writer choice NewConsoleSMSSender's own
// contract requires, and a test or host that wants to capture the record
// builds its sender itself. It takes no configuration -- ConfigSchema stays
// nil, so a composition block carrying any key for it is refused as
// unknown.
var smsConsoleComponent = Component{
	Name:         "sms.console",
	Module:       "sms",
	Provides:     []any{(*SMSSender)(nil)},
	Capabilities: Stateless,
	New: func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
		return NewConsoleSMSSender(os.Stdout), nil
	},
}

func init() { MustRegister(smsConsoleComponent) }

// consoleSMSSender is the zero-external-dependency SMSSender: it prints
// every message to an io.Writer instead of sending anything -- the
// console-form degradation for SMS ("printed to stdout"), the same role
// consoleMailer plays for email. A mutex guards the writer so that
// concurrent Send calls cannot interleave their output. It is a free-text
// transport: it prints To and Text and ignores the template-identity fields
// (MessageID, Locale, Params), whose only consumer is a template-typed
// carrier adapter.
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
