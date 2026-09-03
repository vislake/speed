package mailertest

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestAssertConforms_ConsoleMailer proves AssertConforms passes end to end
// against pkgcore.NewConsoleMailer, the built-in implementation every check
// in this suite was written against first. go/pkgcore's own mailer_test.go
// carries the call that matters for the round's fail-fast property (Phase 1
// didn't silently change behavior for existing callers); this test exists
// so the suite itself is exercised inside this package's own unit test run.
//
// The console mailer prints every message to stdout (NewConsoleMailer's own
// doc comment), the same trade-off every other module's test suite already
// accepts when it wires one into a test pkgcore.Registry (e.g.
// go/rbac/module_test.go, go/config/module_test.go) — nothing this suite
// checks depends on what gets printed, and the printed record is harmless
// test noise, not a failure.
func TestAssertConforms_ConsoleMailer(t *testing.T) {
	t.Parallel()
	AssertConforms(t, func() pkgcore.Mailer {
		return pkgcore.NewConsoleMailer()
	})
}
