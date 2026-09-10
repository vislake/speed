package redis_test

// Runnable documentation for the Redis-backed KVStore, compiled and executed
// by `go test` like every other package's examples, so an API change that
// invalidates the documented usage fails the build instead of silently
// rotting.

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
	kvredis "github.com/vislake/speed/go/pkgcore/kv/redis"
)

// ExampleNewKVStore shows the distributed-mode counterpart of
// pkgcore.NewMemoryKVStore: the host hands the deployment's Redis client to
// the store, which implements the same KVStore interface and semantics the
// memory store covers in standalone mode, so code written against the
// interface runs against either. Constructing the go-redis client dials
// nothing; the store's first operation is what reaches for the server, which
// is why this example can run in any environment.
func ExampleNewKVStore() {
	client := redis.NewClient(&redis.Options{Addr: "redis:6379"})
	defer client.Close()

	kv := kvredis.NewKVStore(client)
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// this constructor satisfies the KVStore interface -- the memory-store
	// counterpart -- so it is kept rather than inlined.
	var _ pkgcore.KVStore = kv

	fmt.Println("store wired; its first operation dials the server")
	// Output:
	// store wired; its first operation dials the server
}

// ExampleFromAddr shows the bare-injection path's one-step constructor: a
// host holding nothing but the deployment's Redis address gets the store --
// over a client FromAddr builds and owns -- and the capability declaration
// pkgcore.WithKVStore takes, in one call, instead of hand-assembling the
// go-redis client and then the store over it. Nothing dials until an
// operation runs. The returned value's Close releases that client, so the
// host calls it at shutdown -- the kernel never closes an injected seam.
func ExampleFromAddr() {
	store, caps, err := kvredis.FromAddr("127.0.0.1:1")
	if err != nil {
		fmt.Println("from addr:", err)
		return
	}
	defer func() { _ = store.Close() }()

	// The pair a host passes to pkgcore.WithKVStore.
	fmt.Println(store != nil, caps)
	// Output:
	// true MultiReplicaSafe|SurvivesRestart
}

// Example demonstrates the package's self-registration: importing it for
// side effect -- as a distributed-mode host does with a blank import when it
// wants pkgcore.WithPreset(pkgcore.PresetDistributed) to resolve the "kv"
// seam -- makes "kv.redis" build through pkgcore's shared KVStoreRegistry,
// the database/sql-style driver pattern this package follows.
func Example() {
	store, caps, err := pkgcore.KVStoreRegistry.Build("kv.redis", pkgcore.Config{})
	fmt.Println(err, store != nil, caps)

	// Output:
	// <nil> true MultiReplicaSafe|SurvivesRestart
}

// ExampleRegistration shows the name-registration path for a typed
// configuration the flat pkgcore.Config cannot express: the host builds the
// client itself -- here carrying TLS material -- wraps it in the
// registration factory, registers it under a name of its own (the built-in
// "kv.redis" name is taken), and names that registration in a Preset entry.
// One client can back several seams this way: the same object also goes to
// eventbus/redis.Registration. The host keeps ownership: the factory's New
// returns the bare store over the host's client, so Kernel.Shutdown never
// touches either, and the host closes both when it shuts down. Nothing here
// dials, because go-redis connects lazily.
func ExampleRegistration() {
	client := redis.NewClient(&redis.Options{
		Addr:      "redis.internal:6379",
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	})
	defer client.Close()

	name := "kv.redis.host"
	if err := pkgcore.KVStoreRegistry.Register(kvredis.Registration(name, client)); err != nil {
		fmt.Println("register:", err)
		return
	}

	preset := pkgcore.PresetStandalone.With("kv", pkgcore.SeamPreset{Implementation: name})
	reg, err := pkgcore.NewKernel(pkgcore.WithPreset(preset)).Bootstrap(context.Background())
	fmt.Println(err, reg.KVStore() != nil)

	// Output:
	// <nil> true
}
