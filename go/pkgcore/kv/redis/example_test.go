package redis_test

// Runnable documentation for the Redis-backed KVStore, compiled and executed
// by `go test` like every other package's examples, so an API change that
// invalidates the documented usage fails the build instead of silently
// rotting.

import (
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
