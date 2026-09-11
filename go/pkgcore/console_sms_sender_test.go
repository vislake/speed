package pkgcore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// TestConsoleSMSSender_WritesToInjectedWriter proves the console transport
// writes to the writer it was given -- not to the process's real stdout,
// which is what makes it assertable at all.
func TestConsoleSMSSender_WritesToInjectedWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sender := NewConsoleSMSSender(&buf)

	if err := sender.Send(t.Context(), SMS{To: "+8613800000000", Text: "your code is 123456"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "+8613800000000") || !strings.Contains(got, "123456") {
		t.Errorf("Send() wrote %q, want it to contain the phone number and the message text", got)
	}
}

// TestConsoleSMSSender_TemplateIdentityFields_PrintsOnlyToAndText proves the
// console transport is a free-text one: a message whose template-identity
// fields (MessageID, Locale, Params) are all set prints exactly the same
// one-line record it prints for a bare To/Text message, with no identity
// field and no parameter value in the output. The Params value here is a
// verification-code stand-in: a transport that echoed it would put the
// credential on whatever the writer is pointed at, which is exactly what
// the seam's Params doc forbids.
func TestConsoleSMSSender_TemplateIdentityFields_PrintsOnlyToAndText(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sender := NewConsoleSMSSender(&buf)

	err := sender.Send(t.Context(), SMS{
		To:        "+8613800000000",
		Text:      "your code is 123456",
		MessageID: "authn.sms.verification_code",
		Locale:    "zh-CN",
		Params:    map[string]string{"code": "123456", "minutes": "5"},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	const want = "SMS to +8613800000000: your code is 123456\n"
	if got := buf.String(); got != want {
		t.Errorf("Send() wrote %q, want exactly %q -- the template-identity fields must not reach the record", got, want)
	}
}

// TestConsoleSMSSender_CancelledContext_SendsNothing proves the console
// transport honours the seam contract's first rule -- a Send that begins on
// an already-cancelled context sends nothing and returns the context's
// error -- so every consumer's test double behaves the way the interface
// doc promises.
func TestConsoleSMSSender_CancelledContext_SendsNothing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	sender := NewConsoleSMSSender(&buf)
	err := sender.Send(ctx, SMS{To: "+8613800000000", Text: "your code is 123456"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send(cancelled context) error = %v, want context.Canceled", err)
	}
	if buf.Len() != 0 {
		t.Errorf("Send(cancelled context) wrote %q, want nothing written", buf.String())
	}
}
