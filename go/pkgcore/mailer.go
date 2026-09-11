package pkgcore

import (
	"context"
	"errors"
)

// ErrInvalidMail is returned by Mailer.Send when the message fails the rules
// every implementation enforces: an empty From, no recipients, an empty
// recipient, a control character inside an address field (From, a To entry or
// ReplyTo), a newline inside the Subject, or neither body set. A Send that
// returns it has not touched the wire. The offending value is deliberately
// kept out of the error text, because a message may carry sensitive data.
var ErrInvalidMail = errors.New("pkgcore: invalid mail")

// Mail is one outbound email message, already rendered by the caller. The
// address fields hold plain addresses (name-and-address display forms are the
// caller's job, and must not smuggle in control characters), no header field
// may carry a line break, and the bodies are plain text and HTML alternatives
// of the same content, at least one of which must be non-empty.
type Mail struct {
	From    string
	To      []string
	Subject string

	// ReplyTo is the optional address replies should go to instead of From,
	// for a sender that is not a monitored inbox (a no-reply address, say).
	// Empty means the transport's own default applies: the SMTP mailer falls
	// back to SMTPConfig.ReplyTo when it is set, and a message whose mail and
	// config both leave it empty carries no Reply-To header at all. The
	// field's own value always wins over an implementation default.
	ReplyTo string

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
// Send must honour a cancelled context by returning its context's error
// instead of sending: a Send that begins on an already-cancelled context
// sends nothing, and a cancellation that lands mid-delivery interrupts the
// send as soon as the transport can notice it. Send must not retain the mail
// after returning, and implementations must be safe for concurrent use by
// multiple goroutines. A message that fails the shared rules above is
// rejected with ErrInvalidMail before anything is sent.
type Mailer interface {
	// Send delivers mail to every recipient in mail.To.
	Send(ctx context.Context, mail Mail) error
}
