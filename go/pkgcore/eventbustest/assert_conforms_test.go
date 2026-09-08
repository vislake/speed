package eventbustest

import (
	"os"
	"os/exec"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestAssertConforms_MemoryEventBus proves AssertConforms passes end to end
// against pkgcore.NewMemoryEventBus, the built-in implementation every
// AssertConforms check was written against first. This test exists so the suite itself — subscript, waitFor,
// assertPayloadSequence and all — is exercised inside this package's own
// unit test run, not only via a caller two modules away.
//
// The in-memory bus is single-process: the factory returns the same
// instance twice, the faithful two-instance model of a one-replica
// deployment (see the package doc comment). The caps argument carries the
// in-memory bus's honest declaration — pkgcore registers "eventbus.memory"
// with no capability bits (its own builtin_implementations.go), so the
// suite runs the single-instance checks only.
func TestAssertConforms_MemoryEventBus(t *testing.T) {
	t.Parallel()
	AssertConforms(t, 0, func() (pkgcore.EventBus, pkgcore.EventBus) {
		bus := pkgcore.NewMemoryEventBus()
		return bus, bus
	})
}

// TestAssertConformsGated_CapsWithoutMultiReplicaSafe_SkipCrossInstanceChecks
// pins the gate's skip direction through the exact entry a leg calls: two
// independent localOnlyBus instances share no state, so every
// cross-instance check would reject the pair — and under caps declaring no
// MultiReplicaSafe the suite must not run them, and passes on the
// single-instance checks alone. This is the declaration-honest shape of an
// implementation that claims no bit: nothing cross-instance is claimed, so
// nothing cross-instance is verified, and a pair that could not satisfy a
// claim it never made is not a failure.
func TestAssertConformsGated_CapsWithoutMultiReplicaSafe_SkipCrossInstanceChecks(t *testing.T) {
	assertConforms(t, 0, func() (pkgcore.EventBus, pkgcore.EventBus) {
		return newLocalOnlyBus(), newLocalOnlyBus()
	}, teethBudget)
}

// TestAssertConformsGated_MultiReplicaSafeDeclared_RunsCrossInstanceChecks
// pins the gate's run direction: the same pair of independent localOnlyBus
// instances, under caps declaring MultiReplicaSafe, must fail the suite —
// a declaration of the bit without a pair that can deliver across
// instances is exactly the "declaration turns CI red" shape. Failure is
// asserted by re-executing this test binary with an environment guard,
// since a deliberate suite failure cannot be observed on the *testing.T
// that drives it; the child runs assertConforms with the shorted teeth
// budget, so the run direction is pinned without paying the real
// cross-instance wait.
func TestAssertConformsGated_MultiReplicaSafeDeclared_RunsCrossInstanceChecks(t *testing.T) {
	if os.Getenv("EVENTBUSTEST_GATE_NEGATIVE") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAssertConformsGated_MultiReplicaSafeDeclared_RunsCrossInstanceChecks$")
		cmd.Env = append(os.Environ(), "EVENTBUSTEST_GATE_NEGATIVE=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("AssertConforms with MultiReplicaSafe declared accepted a pair that never delivers across instances: the cross-instance assertions did not run. Child output:\n%s", out)
		}
		return
	}
	assertConforms(t, pkgcore.MultiReplicaSafe, func() (pkgcore.EventBus, pkgcore.EventBus) {
		return newLocalOnlyBus(), newLocalOnlyBus()
	}, teethBudget)
}
