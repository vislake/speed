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
func TestAssertConforms_MemoryKVStore(t *testing.T) {
	t.Parallel()
	AssertConforms(t, func() pkgcore.KVStore {
		return pkgcore.NewMemoryKVStore()
	})
}
