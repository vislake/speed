//go:build integration

package s3_test

// The SurvivesRestart half of this implementation's declaration, verified:
// the registration in register.go declares MultiReplicaSafe | SurvivesRestart
// (register_test.go pins the registry to return exactly those bits) and this
// file runs the shared objectstoretest.AssertSurvivesRestart protocol — the
// ObjectStore twin of kvstoretest.AssertSurvivesRestart — against a genuine
// restart of the real state-holding service, the RustFS container every
// test in this directory runs against. An object stored before the restart
// must be readable afterwards through a fresh store over a fresh client,
// and the restarted service must take new writes. See register.go's own doc
// comment for what the declaration promises: the objects this store reads
// and writes are the S3 service's own durable records, which outlive the
// service's restart by design.

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/objectstoretest"
)

// TestObjectStore_DeclaredSurvivesRestart_ProvenAgainstContainerRestart is
// the verification the registration's SurvivesRestart declaration rests on,
// driven through the shared protocol (objectstoretest.AssertSurvivesRestart,
// whose caps argument carries the declared bits and refuses a call that
// does not declare SurvivesRestart): store a payload through the S3-backed
// store, stop and start the RustFS container — the state-holding service
// restart pkgcore.Capability's doc comment names — and read the payload
// back through a fresh store over a fresh client, then store and read a
// second payload through the restarted service to prove it fully
// operational. The restart closure returns only once the restarted service
// answers reads authoritatively as well (rustfs_container_test.go's
// waitForRustfsReadsReady), so the post-restart assertions below judge the
// stored bytes, never the service's own startup window. The proof's teeth:
// a store whose bytes lived only in the client process (the default
// objectstore.local's throwaway temp directory is the unit-scale model)
// would fail the post-restart read here, exactly as objectstoretest's own
// unit suite pins.
func TestObjectStore_DeclaredSurvivesRestart_ProvenAgainstContainerRestart(t *testing.T) {
	ctx := context.Background()
	container, endpoint, makeStore := startRustfsPersistentStore(t, ctx)

	objectstoretest.AssertSurvivesRestart(t, pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart,
		func() pkgcore.ObjectStore {
			return makeStore()
		},
		func() {
			if err := container.Stop(ctx, nil); err != nil {
				t.Fatalf("stop rustfs container: %v", err)
			}
			if err := container.Start(ctx); err != nil {
				t.Fatalf("restart rustfs container: %v", err)
			}
			waitForRustfsReady(t, ctx, endpoint)
			waitForRustfsReadsReady(t, ctx, makeStore())
		})
}
