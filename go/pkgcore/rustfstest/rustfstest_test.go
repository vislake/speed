// Tests for the container constructors themselves. Every test starts a
// real container, so each probe-skips when no Docker daemon is reachable
// (testutil.RequireDocker) and runs the constructor end to end otherwise
// -- this is what proves the package's own plumbing (image pin, health
// check, credential wiring, the pinned-port restart fixture) rather than
// the server's behavior, which the consuming tiers' suites own.
package rustfstest_test

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore/internal/testutil"
	"github.com/vislake/speed/go/pkgcore/rustfstest"
)

// TestStart_CreateBucketRoundTrip proves the common constructor end to
// end: the container starts, a client built for the returned endpoint
// works, and a bucket created through it is visible to the same server.
func TestStart_CreateBucketRoundTrip(t *testing.T) {
	testutil.RequireDocker(t)

	ctx := context.Background()
	endpoint := rustfstest.Start(t, ctx)
	client := rustfstest.NewClient(t, endpoint)
	rustfstest.CreateBucket(t, ctx, client, "objects")

	exists, err := client.BucketExists(ctx, "objects")
	if err != nil {
		t.Fatalf("BucketExists(objects): %v", err)
	}
	if !exists {
		t.Fatal("the bucket created through the fixture's client does not exist on the server")
	}
}

// TestStartPersistent_KeepsBucketAcrossRestart proves the pinned-port
// fixture's core contract: the endpoint it advertises stays the same
// across a container stop and start, and the server comes back with its
// /data directory intact -- the two properties the survives-restart
// proofs build on.
func TestStartPersistent_KeepsBucketAcrossRestart(t *testing.T) {
	testutil.RequireDocker(t)

	ctx := context.Background()
	container, endpoint := rustfstest.StartPersistent(t, ctx)
	client := rustfstest.NewClient(t, endpoint)
	rustfstest.CreateBucket(t, ctx, client, "objects")

	if err := container.Stop(ctx, nil); err != nil {
		t.Fatalf("stop rustfs container: %v", err)
	}
	if err := container.Start(ctx); err != nil {
		t.Fatalf("restart rustfs container: %v", err)
	}
	rustfstest.WaitReady(t, ctx, endpoint)

	exists, err := client.BucketExists(ctx, "objects")
	if err != nil {
		t.Fatalf("BucketExists(objects) after restart: %v", err)
	}
	if !exists {
		t.Fatal("the bucket did not survive the container restart, so the advertised endpoint or the /data persistence is broken")
	}
}
