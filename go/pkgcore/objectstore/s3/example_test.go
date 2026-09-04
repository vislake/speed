package s3_test

// Runnable documentation for the S3-backed ObjectStore, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead of
// silently rotting.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/objectstore/s3"
)

// ExampleNewObjectStore shows the distributed deployment mode's object
// store: an S3-compatible service (MinIO, Aliyun OSS or AWS S3) reached
// through the bucket and credentials in s3.Config, the counterpart of
// pkgcore.NewLocalObjectStore. Nothing is dialed at construction -- the
// service is contacted on the first operation -- so a host can wire the
// store at startup whether or not the service is reachable, and an unusable
// configuration (an empty endpoint, bucket or credential) panics there
// instead, where the wiring error is visible.
func ExampleNewObjectStore() {
	store := s3.NewObjectStore(s3.Config{
		Endpoint:  "s3.example.com:9000",
		Bucket:    "objects",
		AccessKey: "access-key",
		SecretKey: "secret-key",
		Region:    "us-east-1",
		UseSSL:    true, // HTTPS: the setting for anything beyond a local MinIO
	})
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// this constructor satisfies the ObjectStore interface -- the local-store
	// counterpart of pkgcore.NewLocalObjectStore -- so it is kept rather than
	// inlined, which would leave the value unused.
	var _ pkgcore.ObjectStore = store

	fmt.Println("store wired; the first operation contacts the service")
	// Output:
	// store wired; the first operation contacts the service
}

// Example demonstrates the package's self-registration: importing it for
// side effect -- as a distributed-mode host does with a blank import when it
// wants pkgcore.WithPreset(pkgcore.PresetDistributed) to resolve the
// "objectstore" seam -- makes "objectstore.s3" build through pkgcore's
// shared ObjectStoreRegistry, the database/sql-style driver pattern this
// package follows.
func Example() {
	cfg := pkgcore.Config{"endpoint": "s3.example.com", "bucket": "objects", "access_key": "ak", "secret_key": "sk"}
	store, caps, err := pkgcore.ObjectStoreRegistry.Build("objectstore.s3", cfg)
	fmt.Println(err, store != nil, caps)

	// Output:
	// <nil> true MultiReplicaSafe|SurvivesRestart
}
