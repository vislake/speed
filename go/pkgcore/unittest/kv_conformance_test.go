// This file lives in package unittest — this module's dedicated unit-test
// directory for unit-tier suites with no single source file as their target
// (the backend coding standard's testing-layout rule). A seam-contract
// driver of the module root's own built-ins is such a suite, and it must be
// black-box against package pkgcore: the driver imports go/pkgcore's
// kvstoretest support package, which itself imports go/pkgcore, and an
// internal test file (package pkgcore) importing a package that imports
// pkgcore back is an import cycle Go's toolchain refuses ("import cycle not
// allowed in test"). An external test package carries no such restriction —
// see eventbus_conformance_test.go's identical note.
package unittest

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/kvstoretest"
)

// TestMemoryKVStore_ConformsToKVStoreContract proves the in-memory KVStore
// this package builds still satisfies the shared contract
// kvstoretest.AssertConforms checks, after the deployment-composition
// retrofit (Phase 1) generalized how a Kernel resolves and validates its
// KVStore seam — this is what proves the retrofit did not silently change
// NewMemoryKVStore's own behavior for its existing callers.
//
// The in-memory store is single-process, so the factory returns the same
// instance twice: the faithful two-instance model of a one-replica
// deployment (see kvstoretest's package doc comment). The zero caps
// argument is the in-memory store's own declaration — "kv.memory"
// registers with no capability bits — so the capability-gated suite runs
// the single-instance checks only, exactly the assertions a store that
// claims no MultiReplicaSafe and no SurvivesRestart is entitled to run.
func TestMemoryKVStore_ConformsToKVStoreContract(t *testing.T) {
	t.Parallel()
	kvstoretest.AssertConforms(t, 0, func() (pkgcore.KVStore, pkgcore.KVStore) {
		store := pkgcore.NewMemoryKVStore()
		return store, store
	})
}
