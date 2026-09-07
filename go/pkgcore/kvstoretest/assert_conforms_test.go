package kvstoretest

import (
	"os"
	"os/exec"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestAssertConforms_MemoryKVStore proves AssertConforms passes end to end
// against pkgcore.NewMemoryKVStore, the built-in implementation every check
// in this suite was written against first. go/pkgcore's own kv_test.go
// carries the call that matters for the round's fail-fast property (Phase 1
// didn't silently change behavior for existing callers); this test exists
// so the suite itself is exercised inside this package's own unit test run.
//
// The in-memory store is single-process: the factory returns the same
// instance twice, the faithful two-instance model of a one-replica
// deployment (see the package doc comment). The caps argument carries the
// in-memory store's honest declaration — pkgcore registers "kv.memory"
// with no capability bits (its own builtin_implementations.go), so the
// suite runs the single-instance checks only.
func TestAssertConforms_MemoryKVStore(t *testing.T) {
	t.Parallel()
	AssertConforms(t, 0, func() (pkgcore.KVStore, pkgcore.KVStore) {
		store := pkgcore.NewMemoryKVStore()
		return store, store
	})
}

// TestAssertConformsGated_CapsWithoutMultiReplicaSafe_SkipCrossInstanceChecks
// pins the gate's skip direction through the exact entry a leg calls: two
// independent memory stores share no state, so every cross-instance check
// would reject the pair — and under caps declaring no MultiReplicaSafe the
// suite must not run them, and passes on the single-instance checks alone.
// This is the declaration-honest shape of an implementation that claims no
// bit: nothing cross-instance is claimed, so nothing cross-instance is
// verified, and a pair that could not satisfy a claim it never made is not
// a failure.
func TestAssertConformsGated_CapsWithoutMultiReplicaSafe_SkipCrossInstanceChecks(t *testing.T) {
	AssertConforms(t, 0, func() (pkgcore.KVStore, pkgcore.KVStore) {
		return pkgcore.NewMemoryKVStore(), pkgcore.NewMemoryKVStore()
	})
}

// TestAssertConformsGated_MultiReplicaSafeDeclared_RunsCrossInstanceChecks
// pins the gate's run direction: the same pair of independent memory
// stores, under caps declaring MultiReplicaSafe, must fail the suite — a
// declaration of the bit without instances that can see each other's
// writes is exactly the "declaration change turns CI red" shape. Failure
// is asserted by re-executing this test binary with an environment guard,
// since a deliberate suite failure cannot be observed on the *testing.T
// that drives it; the child's cross-instance checks reject the pair
// immediately (a Set on one store is a miss on the other, no waits
// involved), so the run direction is pinned cheaply.
func TestAssertConformsGated_MultiReplicaSafeDeclared_RunsCrossInstanceChecks(t *testing.T) {
	if os.Getenv("KVSTORETEST_GATE_NEGATIVE") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAssertConformsGated_MultiReplicaSafeDeclared_RunsCrossInstanceChecks$")
		cmd.Env = append(os.Environ(), "KVSTORETEST_GATE_NEGATIVE=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("AssertConforms with MultiReplicaSafe declared accepted a pair whose instances never see each other's writes: the cross-instance assertions did not run. Child output:\n%s", out)
		}
		return
	}
	AssertConforms(t, pkgcore.MultiReplicaSafe, func() (pkgcore.KVStore, pkgcore.KVStore) {
		return pkgcore.NewMemoryKVStore(), pkgcore.NewMemoryKVStore()
	})
}

// TestCrossInstanceChecks_RejectIndependentInstances pins that the
// cross-instance checks can fail: two independent memory stores share no
// state, so a value set through the first is invisible to the second — the
// pair shape of an implementation whose replicas each hold their own
// private copy of everything, exactly what MultiReplicaSafe=false means.
// Both checks must reject the pair.
func TestCrossInstanceChecks_RejectIndependentInstances(t *testing.T) {
	checks := []struct {
		name  string
		check func(storeA, storeB pkgcore.KVStore, key string) error
	}{
		{"value set on one instance is readable on the other", checkValueVisibleAcrossInstances},
		{"delete on one instance is visible on the other", checkDeleteVisibleAcrossInstances},
	}
	for _, tt := range checks {
		storeA := pkgcore.NewMemoryKVStore()
		storeB := pkgcore.NewMemoryKVStore()
		if err := tt.check(storeA, storeB, "kvstoretest:independent-teeth:"+tt.name); err == nil {
			t.Errorf("check %q accepted two independent stores that never see each other's writes: the cross-instance assertions have no teeth", tt.name)
		}
	}
}
