package objectstoretest

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestAssertConforms_LocalObjectStore proves AssertConforms passes end to
// end against pkgcore.NewLocalObjectStore, the built-in implementation
// every check in this suite was written against first. go/pkgcore's own
// objectstore_test.go carries the call that matters for the round's
// fail-fast property (Phase 1 didn't silently change behavior for existing
// callers); this test exists so the suite itself is exercised inside this
// package's own unit test run.
func TestAssertConforms_LocalObjectStore(t *testing.T) {
	t.Parallel()
	AssertConforms(t, func() pkgcore.ObjectStore {
		return pkgcore.NewLocalObjectStore(t.TempDir())
	})
}
