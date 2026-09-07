// This file lives in package pkgcore_test — the external test package,
// distinct from eventbus_test.go's internal package pkgcore — because it
// must import go/pkgcore/eventbustest, which itself imports go/pkgcore: an
// internal test file (package pkgcore) importing a package that imports
// pkgcore back is an import cycle Go's toolchain refuses ("import cycle not
// allowed in test"), while an external test file compiles as a separate
// package and carries no such restriction. This is the mechanical
// exception the backend coding standard's testing-layout rule names for
// exactly this situation (package x vs. package x_test cases cannot share
// a file), not a new test-organization convention.
package pkgcore_test

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
