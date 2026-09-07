package eventbustest

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestAssertConforms_MemoryEventBus proves AssertConforms passes end to end
// against pkgcore.NewMemoryEventBus, the built-in implementation every
// AssertConforms check was written against first. go/pkgcore's own
// eventbus_test.go carries the call that matters for the round's fail-fast
// property (Phase 1 didn't silently change behavior for existing callers);
// this test exists so the suite itself — subscript, waitFor,
// assertPayloadSequence and all — is exercised inside this package's own
// unit test run, not only via a caller two modules away.
//
// The in-memory bus is single-process: the factory returns the same
// instance twice, the faithful two-instance model of a one-replica
// deployment (see the package doc comment), under which every
// cross-instance assertion reduces to an ordinary local-delivery check the
// bus satisfies.
func TestAssertConforms_MemoryEventBus(t *testing.T) {
	t.Parallel()
	AssertConforms(t, func() (pkgcore.EventBus, pkgcore.EventBus) {
		bus := pkgcore.NewMemoryEventBus()
		return bus, bus
	})
}
