//go:build integration

// Package s3_test holds go/pkgcore/objectstore/s3's integration tier: tests
// that exercise ObjectStore against a real RustFS server. It is physically
// separate from the package's unit tests (all of which live in package s3
// itself, one file per source file, per the backend coding standard's
// testing layout rule) and carries the "integration" build tag: a plain
// "go test ./..." never compiles or runs anything in this directory; it is
// invoked explicitly with "go test -tags=integration ./...". This mirrors
// go/pkgcore's own (pre-split) integration tier, which this directory's
// tests are moved out of, and go/jobs/integration_test's identical
// convention.
//
// Every test here spins up its own disposable RustFS container through
// go/pkgcore/rustfstest and requires a working Docker (or
// Docker-API-compatible) daemon; there is no fallback or
// skip-on-missing-Docker path. RustFS (https://github.com/rustfs/rustfs)
// is the S3-API-compatible server this repository's own tests and demos
// run against; rustfstest's package doc records how the container is
// started for an image with no dedicated testcontainers-go module.
package s3_test

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/objectstore/s3"
	"github.com/vislake/speed/go/pkgcore/rustfstest"
)

// bucket is the bucket every fixture in this tier provisions: a fixture
// choice, not a store requirement -- s3.NewObjectStore addresses whatever
// bucket its Config names.
const bucket = "objects"

// startRustfsObjectStore starts a disposable RustFS container, creates a
// fresh bucket on it, and returns an S3-backed ObjectStore pointed at that
// bucket, addressing it in the lookup style the caller names, the
// container already cleaned up via t.Cleanup on test completion (pass or
// fail), so nothing leaks past its owning test. Every integration test
// calls this for its own container, keeping tests isolated from one
// another at the cost of a few seconds of startup each. The bucket is
// created here rather than by the store: provisioning a bucket is a
// hosting operation, and s3.NewObjectStore deliberately never provisions
// its own.
func startRustfsObjectStore(t *testing.T, ctx context.Context, lookup s3.BucketLookupType) pkgcore.ObjectStore {
	t.Helper()

	endpoint := rustfstest.Start(t, ctx)
	rustfstest.CreateBucket(t, ctx, rustfstest.NewClient(t, endpoint), bucket)
	return s3.NewObjectStore(s3.Config{
		Endpoint:     endpoint,
		Bucket:       bucket,
		AccessKey:    rustfstest.AccessKey,
		SecretKey:    rustfstest.SecretKey,
		BucketLookup: lookup,
	})
}

// startRustfsPersistentStore is startRustfsObjectStore's counterpart for
// the one test that stops and restarts its own container mid-test
// (TestObjectStore_DeclaredSurvivesRestart_ProvenAgainstContainerRestart):
// it returns the container itself, the fixed endpoint it advertises, and a
// makeStore factory -- every call returns a fresh S3-backed store over a
// fresh client, all pointed at the one bucket the fixture provisioned.
// rustfstest.StartPersistent supplies the fixture's two properties the
// restart proof rests on: the S3 port is published to a fixed, rather than
// random, host port, and RustFS stores the bucket and every object under
// the /data directory it mounts, inside the container's own filesystem --
// which survives a stop/start of the same container the way a real S3
// service's durable storage survives a service restart.
func startRustfsPersistentStore(t *testing.T, ctx context.Context) (testcontainers.Container, string, func() pkgcore.ObjectStore) {
	t.Helper()

	container, endpoint := rustfstest.StartPersistent(t, ctx)
	rustfstest.CreateBucket(t, ctx, rustfstest.NewClient(t, endpoint), bucket)

	makeStore := func() pkgcore.ObjectStore {
		return s3.NewObjectStore(s3.Config{
			Endpoint:  endpoint,
			Bucket:    bucket,
			AccessKey: rustfstest.AccessKey,
			SecretKey: rustfstest.SecretKey,
		})
	}
	return container, endpoint, makeStore
}
