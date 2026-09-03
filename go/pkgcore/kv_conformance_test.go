// This file lives in package pkgcore_test — the external test package,
// distinct from kv_test.go's internal package pkgcore — because it must
// import go/pkgcore/kvstoretest, which itself imports go/pkgcore: an
// internal test file (package pkgcore) importing a package that imports
// pkgcore back is an import cycle Go's toolchain refuses ("import cycle not
// allowed in test"), while an external test file compiles as a separate
// package and carries no such restriction. This is the mechanical
// exception the backend coding standard's testing-layout rule names for
// exactly this situation (package x vs. package x_test cases cannot share
// a file), not a new test-organization convention — see
// eventbus_conformance_test.go's identical note.
package pkgcore_test

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
func TestMemoryKVStore_ConformsToKVStoreContract(t *testing.T) {
	t.Parallel()
	kvstoretest.AssertConforms(t, func() pkgcore.KVStore {
		return pkgcore.NewMemoryKVStore()
	})
}
