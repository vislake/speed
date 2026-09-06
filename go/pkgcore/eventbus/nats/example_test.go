package nats_test

// Runnable documentation for the NATS-backed EventBus, compiled and executed
// by `go test` like every other package's examples, so an API change that
// invalidates the documented usage fails the build instead of silently
// rotting. Mirrors eventbus/redis/example_test.go's own two examples.

import (
	"context"
	"fmt"

	natslib "github.com/nats-io/nats.go"

	"github.com/vislake/speed/go/pkgcore"
	eventbusnats "github.com/vislake/speed/go/pkgcore/eventbus/nats"
)

// ExampleNewEventBus shows the distributed-mode counterpart of
// pkgcore.NewMemoryEventBus: one bus per replica, all sharing the
// deployment's JetStream-enabled NATS connection, so an event published on
// any replica reaches the subscribers of every one of them. The URL points
// at a closed port so that this example stays hermetic:
// nats.RetryOnFailedConnect keeps Connect from failing synchronously, and no
// event is published here anyway -- Publish is what actually delivers. A
// distributed-mode host must Close the bus when it shuts the replica down,
// or the reader goroutines keep running.
func ExampleNewEventBus() {
	nc, err := natslib.Connect("127.0.0.1:1", natslib.RetryOnFailedConnect(true), natslib.MaxReconnects(-1))
	if err != nil {
		fmt.Println("connect:", err)
		return
	}
	defer nc.Close()

	bus := eventbusnats.NewEventBus(nc)
	defer bus.Close() // stops the reader goroutines the first Subscribe started

	bus.Subscribe("authn.user_created", func(ctx context.Context, evt pkgcore.Event) error {
		// On a replica with a real NATS server this runs for every event of
		// this type any replica publishes: asynchronously, on this type's own
		// JetStream consumer, with the JSON shape of the payload.
		fmt.Printf("org: default workspace for %v in tenant %s\n", evt.Payload, evt.TenantID)
		return nil
	})

	fmt.Println("bus wired and reading; no event was published")
	// Output:
	// bus wired and reading; no event was published
}

// Example demonstrates the package's self-registration: importing it for
// side effect makes "eventbus.nats" build through pkgcore's shared
// EventBusRegistry, the database/sql-style driver pattern this package
// follows -- mirroring eventbus/redis's identical Example. Unlike
// "eventbus.redis", no Preset points the "eventbus" seam at this name; a
// host wanting it calls EventBusRegistry.Build("eventbus.nats", cfg)
// explicitly, exactly as here.
func Example() {
	bus, caps, err := pkgcore.EventBusRegistry.Build("eventbus.nats", pkgcore.Config{})
	fmt.Println(err, bus != nil, caps)

	// Output:
	// <nil> true MultiReplicaSafe|SurvivesRestart
}
