// Package mailertest verifies that a pkgcore.Mailer implementation upholds
// the contract Mailer's own doc comment describes, independent of which
// backend implements it. It plays the same role for Mailer that
// go/tenancy/tenancytest.AssertIsolated plays for dbkit.Repository[T] and
// go/pkgcore/eventbustest.AssertConforms plays for EventBus: one suite every
// implementation — built-in (pkgcore.NewConsoleMailer, pkgcore.NewSMTPMailer)
// or host-supplied through pkgcore.WithMailer — must pass, so drift between
// implementations is caught here once instead of pairwise (see
// docs/internal/03-deployment-modes.md).
package mailertest

import (
	"context"
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// AssertConforms verifies that the Mailer factory returns satisfies the
// contract documented on pkgcore.Mailer. Each subtest calls factory to get
// its own Mailer, so a factory whose backend is single-use (a fake relay
// wired to expect exactly one connection, say) is safe to hand in.
//
// What AssertConforms checks, in order: every rule pkgcore.ErrInvalidMail
// documents (empty From, no recipients, an empty recipient, a header field
// carrying a line break, and neither body set) is rejected by Send with an
// error satisfying errors.Is(err, pkgcore.ErrInvalidMail), before anything
// implementation-specific about the message runs; a Send that begins on an
// already-cancelled context fails with that context's error instead of the
// message going out; and a valid message reports success.
//
// factory's Mailer must be able to accept a real, deliverable-shaped
// message (a valid From, one recipient, a body) and report success for it —
// for a network-backed implementation this means factory must point it at a
// backend that will actually accept the message, e.g. a fake relay
// configured to accept everything, not one scripted to reject.
func AssertConforms(t *testing.T, factory func() pkgcore.Mailer) {
	t.Helper()

	validMail := pkgcore.Mail{
		From:    "ops@example.com",
		To:      []string{"ada@example.com"},
		Subject: "mailertest conformance",
		Text:    "body",
	}

	invalidCases := []struct {
		name string
		mail pkgcore.Mail
	}{
		{
			name: "empty_from",
			mail: pkgcore.Mail{To: []string{"ada@example.com"}, Text: "body"},
		},
		{
			name: "no_recipients",
			mail: pkgcore.Mail{From: "ops@example.com", Text: "body"},
		},
		{
			name: "empty_recipient",
			mail: pkgcore.Mail{From: "ops@example.com", To: []string{""}, Text: "body"},
		},
		{
			name: "header_field_with_a_line_break",
			mail: pkgcore.Mail{From: "ops@example.com", To: []string{"ada@example.com"}, Subject: "bad\r\nInjected: header", Text: "body"},
		},
		{
			name: "neither_body_set",
			mail: pkgcore.Mail{From: "ops@example.com", To: []string{"ada@example.com"}},
		},
	}

	for _, tc := range invalidCases {
		t.Run("rejects_"+tc.name, func(t *testing.T) {
			t.Helper()
			mailer := factory()
			if err := mailer.Send(context.Background(), tc.mail); !errors.Is(err, pkgcore.ErrInvalidMail) {
				t.Errorf("Send() error = %v, want errors.Is(err, pkgcore.ErrInvalidMail)", err)
			}
		})
	}

	t.Run("a_cancelled_context_fails_instead_of_sending", func(t *testing.T) {
		t.Helper()
		mailer := factory()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := mailer.Send(ctx, validMail); !errors.Is(err, context.Canceled) {
			t.Errorf("Send() with a cancelled context error = %v, want context.Canceled", err)
		}
	})

	t.Run("a_valid_message_reports_success", func(t *testing.T) {
		t.Helper()
		mailer := factory()
		if err := mailer.Send(context.Background(), validMail); err != nil {
			t.Errorf("Send() error = %v, want nil for a valid message", err)
		}
	})
}
