package redis_test

// Runnable documentation for the Redis-backed EventBus, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead of
// silently rotting.

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
)

// ExampleNewEventBus shows the distributed-mode counterpart of
// pkgcore.NewMemoryEventBus: one bus per replica, all sharing the
// deployment's Redis client, so an event published on any replica reaches
// the subscribers of every one of them. The address points at a closed port
// so that this example stays hermetic: Subscribe starts a reader goroutine
// that reaches for Redis, and no event is published here anyway -- Publish
// is what actually delivers. A distributed-mode host must Close the bus when
// it shuts the replica down, or the reader goroutines keep running.
func ExampleNewEventBus() {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer client.Close()

	bus := eventbusredis.NewEventBus(client)
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

// Example demonstrates the package's self-registration: importing it for
// side effect -- as a distributed-mode host does with a blank import when it
// wants pkgcore.WithPreset(pkgcore.PresetDistributed) to resolve the
// "eventbus" seam -- makes "eventbus.redis" build through pkgcore's shared
// EventBusRegistry, the database/sql-style driver pattern this package
// follows.
func Example() {
	bus, caps, err := pkgcore.EventBusRegistry.Build("eventbus.redis", pkgcore.Config{})
	fmt.Println(err, bus != nil, caps)

	// Output:
	// <nil> true MultiReplicaSafe|SurvivesRestart
}
