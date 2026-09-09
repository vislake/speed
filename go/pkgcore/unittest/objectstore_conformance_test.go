// This file lives in package unittest — this module's dedicated unit-test
// directory for unit-tier suites with no single source file as their target
// (the backend coding standard's testing-layout rule). A seam-contract
// driver of the module root's own built-ins is such a suite, and it must be
// black-box against package pkgcore: the driver imports go/pkgcore's
// objectstoretest support package, which itself imports go/pkgcore, and an
// internal test file (package pkgcore) importing a package that imports
// pkgcore back is an import cycle Go's toolchain refuses ("import cycle not
// allowed in test"). An external test package carries no such restriction —
// see eventbus_conformance_test.go's identical note.
package unittest

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
