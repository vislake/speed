package mailertest

import (
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestAssertConforms_ConsoleMailer proves AssertConforms passes end to end
// against pkgcore.NewConsoleMailer, the built-in implementation every check
// in this suite was written against first. This test exists so the suite
// itself is exercised inside this package's own unit test run.
//
// The console mailer prints every message to stdout (NewConsoleMailer's own
// doc comment), the same trade-off every other module's test suite already
// accepts when it wires one into a test pkgcore.ComponentRegistry (e.g.
// go/rbac/module_test.go, go/config/module_test.go) — nothing this suite
// checks depends on what gets printed, and the printed record is harmless
// test noise, not a failure.
func TestAssertConforms_ConsoleMailer(t *testing.T) {
	t.Parallel()
	AssertConforms(t, func() pkgcore.Mailer {
		return pkgcore.NewConsoleMailer()
	})
}

// TestCheckPermanentFailure pins both directions of the sentinel clause's
// verdict: a refusal a transport marked with pkgcore.ErrTransportPermanent
// (wrapped, as Send implementations wrap it) passes, and any error that does
// not carry the sentinel -- an unmarked refusal, an unwrapped transport
// failure, a nil error -- is rejected, so an implementation whose permanent
// refusals never carry the sentinel cannot slip through AssertPermanentFailure.
func TestCheckPermanentFailure(t *testing.T) {
	t.Parallel()

	marked := errors.New("smtp: 550 no such user")
	wrapped := errors.Join(errors.New("send failed"), errors.Join(pkgcore.ErrTransportPermanent, marked))
	if err := checkPermanentFailure(wrapped); err != nil {
		t.Errorf("checkPermanentFailure(wrapped sentinel) = %v, want nil", err)
	}

	for _, err := range []error{nil, marked, errors.New("send failed")} {
		if checkErr := checkPermanentFailure(err); checkErr == nil {
			t.Errorf("checkPermanentFailure(%v) = nil, want a rejection for an error that does not wrap the sentinel", err)
		}
	}
}
