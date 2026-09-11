package postgres_test

// Runnable documentation for this package's constructors, compiled and
// executed by `go test` like every other package's examples, so an API
// change that invalidates the documented usage fails the build instead
// of silently rotting. The package's component descriptor -- the
// component a composition configuration selects as its module's
// implementation -- is exercised by the package's own component_test.go.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
	eventbuspostgres "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
)

// ExampleNewEventBus shows the PostgreSQL-backed counterpart of
// pkgcore.NewMemoryEventBus and eventbus/redis.NewEventBus: one bus per
// replica, all sharing the deployment's PostgreSQL database, so an event
// published on any replica reaches the subscribers of every one of them,
// including a replica that restarts and reconnects under the same
// replicaID. The DSN points at a closed port so this example stays
// hermetic: pgxpool.New never dials at construction, and Subscribe's
// background listener goroutine only ever reaches for PostgreSQL after
// this example has already printed its output. A distributed-mode host
// must call postgres.EnsureSchema once against the pool before the first
// Subscribe, and must Close the bus when it shuts the replica down, or the
// listener goroutine keeps running.
func ExampleNewEventBus() {
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		fmt.Println("unexpected pool construction error:", err)
		return
	}
	defer pool.Close()

	// replicaID must be stable across THIS replica's own restarts -- a
	// pod name, a hostname, a configured identifier -- so that a process
	// restarting under the same id resumes catch-up exactly where it left
	// off instead of starting over at the live end. See EventBus's own
	// doc comment for the full guarantee.
	bus := eventbuspostgres.NewEventBus(pool, "replica-1")
	defer bus.Close() // stops the listener goroutine the first Subscribe started

	bus.Subscribe("authn.user_created", func(ctx context.Context, evt pkgcore.Event) error {
		// On a replica with a reachable PostgreSQL, this runs for every
		// event of this type any replica publishes: asynchronously, on
		// the bus's own listener goroutine, with the JSON shape of the
		// payload -- except on the very replica that called Publish,
		// which receives the original Go value synchronously instead.
		fmt.Printf("org: default workspace for %v in tenant %s\n", evt.Payload, evt.TenantID)
		return nil
	})

	fmt.Println("bus wired and listening; no event was published")
	// Output:
	// bus wired and listening; no event was published
}
