// This file lives in package unittest — this module's dedicated unit-test
// directory for unit-tier suites with no single source file as their target
// (the backend coding standard's testing-layout rule). A seam-contract
// driver of the module root's own built-ins is such a suite, and it must be
// black-box against package pkgcore: the driver imports go/pkgcore's
// eventbustest support package, which itself imports go/pkgcore, and an
// internal test file (package pkgcore) importing a package that imports
// pkgcore back is an import cycle Go's toolchain refuses ("import cycle not
// allowed in test"). An external test package carries no such restriction.
package unittest

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/eventbustest"
)

// TestMemoryEventBus_ConformsToEventBusContract proves the in-memory
// EventBus this package builds still satisfies the shared contract
// eventbustest.AssertConforms checks, after the deployment-composition
// retrofit (Phase 1) generalized how a Kernel resolves and validates its
// EventBus seam — this is what proves the retrofit did not silently change
// NewMemoryEventBus's own behavior for its existing callers.
//
// The seam contract does not promise that Publish reports a failing
// handler's error (the shared suite therefore does not assert it; see
// eventbustest's package doc comment). The in-memory bus's own
// error-returning behavior — the "additional property" its synchronous
// same-goroutine delivery gives it, which no broker-backed bus can provide
// for a handler delivered on another replica — is pinned instead by
// memory_eventbus_test.go's TestMemoryEventBusPublish, the package-pkgcore suite
// that stays in the source package next to the code it tests.
//
// The in-memory bus is single-process, so the factory returns the same
// instance twice: the faithful two-instance model of a one-replica
// deployment (see eventbustest's package doc comment). The zero caps
// argument is the in-memory bus's own declaration — "eventbus.memory"
// registers with no capability bits — so the capability-gated suite runs
// the single-instance checks only, exactly the assertions a bus that
// claims no MultiReplicaSafe and no SurvivesRestart is entitled to run.
func TestMemoryEventBus_ConformsToEventBusContract(t *testing.T) {
	t.Parallel()
	eventbustest.AssertConforms(t, 0, func() (pkgcore.EventBus, pkgcore.EventBus) {
		bus := pkgcore.NewMemoryEventBus()
		return bus, bus
	})
}
