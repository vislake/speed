package s3_test

// Runnable documentation for this package's constructors, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead
// of silently rotting. The package's component descriptor -- the
// component a composition configuration selects as its module's
// implementation -- is exercised by the package's own component_test.go.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/objectstore/s3"
)

// ExampleNewObjectStore shows the distributed deployment mode's object
// store: an S3-compatible service (MinIO, Aliyun OSS or AWS S3) reached
// through the bucket and credentials in s3.Config, the counterpart of
// pkgcore.NewLocalObjectStore. BucketLookup pins how the store addresses
// the bucket; here the service's wildcard bucket hosts do not resolve
// under its own name, so the path style is named explicitly --
// s3.BucketLookupAuto, the zero value, derives the style from the endpoint
// instead. Nothing is dialed at construction -- the service is contacted on
// the first operation -- so a host can wire the store at startup whether or
// not the service is reachable, and an unusable configuration (an empty
// endpoint, bucket or credential, an unknown BucketLookup) panics there
// instead, where the wiring error is visible.
func ExampleNewObjectStore() {
	store := s3.NewObjectStore(s3.Config{
		Endpoint:     "s3.example.com:9000",
		Bucket:       "objects",
		AccessKey:    "access-key",
		SecretKey:    "secret-key",
		Region:       "us-east-1",
		UseSSL:       true, // HTTPS: the setting for anything beyond a local MinIO
		BucketLookup: s3.BucketLookupPath,
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

// ExampleFromConfig shows the bare-injection path's one-step constructor:
// the same store ExampleNewObjectStore builds, plus the capability
// declaration the objectstore value's configuration block takes, in one call -- and a
// configuration with a field missing or an endpoint minio-go rejects comes
// back as an error here, where NewObjectStore panics, so a host composing
// the objectstore value gets the error instead of a panic. Nothing is dialed, and there is
// nothing to release afterwards: the minio-go client exposes no close.
func ExampleFromConfig() {
	store, caps, err := s3.FromConfig(s3.Config{
		Endpoint:  "s3.internal:9000",
		Bucket:    "objects",
		AccessKey: "access-key",
		SecretKey: "secret-key",
		Region:    "us-east-1",
		UseSSL:    true,
	})
	if err != nil {
		fmt.Println("from config:", err)
		return
	}

	// The pair a host passes to the objectstore value's configuration block.
	fmt.Println(store != nil, caps)
	// Output:
	// true MultiReplicaSafe|SurvivesRestart
}
