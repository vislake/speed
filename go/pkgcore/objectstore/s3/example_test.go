package s3_test

// Runnable documentation for the S3-backed ObjectStore, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead of
// silently rotting.

import (
	"context"
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
// declaration pkgcore.WithObjectStore takes, in one call -- and a
// configuration with a field missing or an endpoint minio-go rejects comes
// back as an error here, where NewObjectStore panics, so a host assembling
// Kernel options can still abandon them. Nothing is dialed, and there is
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

	// The pair a host passes to pkgcore.WithObjectStore.
	fmt.Println(store != nil, caps)
	// Output:
	// true MultiReplicaSafe|SurvivesRestart
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

// ExampleRegistration shows the name-registration path for this seam, where
// the typed configuration is the Config itself: the host assembles it in
// code -- here from credentials resolved by its own secret machinery -- and
// wraps it in the registration factory instead of flattening it into the
// string-keyed pkgcore.Config. The factory's result registers under a name
// of the host's own (the built-in "objectstore.s3" name is taken) and is
// named in a Preset entry. The host keeps ownership: the factory's New
// returns the bare store, so Kernel.Shutdown never touches it, and there is
// no client this package would close. Nothing here dials; the service is
// contacted on the store's first operation.
func ExampleRegistration() {
	storeCfg := s3.Config{
		Endpoint:  "s3.internal:9000",
		Bucket:    "objects",
		AccessKey: "access-key",
		SecretKey: "secret-key",
		Region:    "us-east-1",
		UseSSL:    true,
	}

	name := "objectstore.s3.prod"
	if err := pkgcore.ObjectStoreRegistry.Register(s3.Registration(name, storeCfg)); err != nil {
		fmt.Println("register:", err)
		return
	}

	preset := pkgcore.PresetStandalone.With("objectstore", pkgcore.SeamPreset{Implementation: name})
	reg, err := pkgcore.NewKernel(pkgcore.WithPreset(preset)).Bootstrap(context.Background())
	fmt.Println(err, reg.ObjectStore() != nil)

	// Output:
	// <nil> true
}
