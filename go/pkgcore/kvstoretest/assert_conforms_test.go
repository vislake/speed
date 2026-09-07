package kvstoretest

import (
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
// deployment (see the package doc comment), under which every
// cross-instance assertion reduces to an ordinary single-store check the
// store satisfies.
func TestAssertConforms_MemoryKVStore(t *testing.T) {
	t.Parallel()
	AssertConforms(t, func() (pkgcore.KVStore, pkgcore.KVStore) {
		store := pkgcore.NewMemoryKVStore()
		return store, store
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
