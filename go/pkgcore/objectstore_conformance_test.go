// This file lives in package pkgcore_test — the external test package,
// distinct from objectstore_test.go's internal package pkgcore — because it
// must import go/pkgcore/objectstoretest, which itself imports go/pkgcore:
// an internal test file (package pkgcore) importing a package that imports
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
	"github.com/vislake/speed/go/pkgcore/objectstoretest"
)

// TestLocalObjectStore_ConformsToObjectStoreContract proves the local
// ObjectStore this package builds still satisfies the shared contract
// objectstoretest.AssertConforms checks, after the deployment-composition
// retrofit (Phase 1) generalized how a Kernel resolves and validates its
// ObjectStore seam — this is what proves the retrofit did not silently
// change NewLocalObjectStore's own behavior for its existing callers.
func TestLocalObjectStore_ConformsToObjectStoreContract(t *testing.T) {
	t.Parallel()
	objectstoretest.AssertConforms(t, func() pkgcore.ObjectStore {
		return pkgcore.NewLocalObjectStore(t.TempDir())
	})
}
