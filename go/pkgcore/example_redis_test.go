package pkgcore_test

// Runnable documentation for the Redis-backed, distributed-mode
// implementations of the KVStore and EventBus seams. Like example_test.go,
// which holds the shared wiring and standalone-mode examples, every example
// here is compiled and executed by `go test`, so an API change that
// invalidates the documented usage fails the build instead of silently
// rotting. The Redis examples live in their own file because they are the
// only examples that import a third-party client; keeping them apart means
// the rest of the package's examples stay readable for what they document.

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/vislake/speed/go/pkgcore"
)

// ExampleNewRedisKVStore shows the distributed-mode counterpart of
// NewMemoryKVStore: the host hands the deployment's Redis client to the
// store, which implements the same KVStore interface and semantics the
// memory store of ExampleKVStore walks through, so code written against the
// interface runs against either. Constructing the go-redis client dials
// nothing; the store's first operation is what reaches for the server, which
// is why this example can run in any environment.
func ExampleNewRedisKVStore() {
	client := redis.NewClient(&redis.Options{Addr: "redis:6379"})
	defer client.Close()

	kv := pkgcore.NewRedisKVStore(client)
	var _ pkgcore.KVStore = kv // drop-in for the memory store of ExampleKVStore

	fmt.Println("store wired; its first operation dials the server")
	// Output:
	// store wired; its first operation dials the server
}

// ExampleNewRedisEventBus shows the distributed-mode counterpart of
// NewMemoryEventBus: one bus per replica, all sharing the deployment's Redis
// client, so an event published on any replica reaches the subscribers of
// every one of them. The address points at a closed port so that this example
// stays hermetic: Subscribe starts a reader goroutine that reaches for Redis,
// and no event is published here anyway -- Publish is what actually delivers.
// A distributed-mode host must Close the bus when it shuts the replica down,
// or the reader goroutines keep running.
func ExampleNewRedisEventBus() {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer client.Close()

	bus := pkgcore.NewRedisEventBus(client)
	defer bus.Close() // stops the reader goroutines the first Subscribe started

	bus.Subscribe("authn.user_created", func(ctx context.Context, evt pkgcore.Event) error {
		// On a replica with a real Redis this runs for every event of this
		// type any replica publishes: asynchronously, on the bus's reader
		// goroutine, with the JSON shape of the payload.
		fmt.Printf("org: default workspace for %v in tenant %s\n", evt.Payload, evt.TenantID)
		return nil
	})

	fmt.Println("bus wired and reading; no event was published")
	// Output:
	// bus wired and reading; no event was published
}
