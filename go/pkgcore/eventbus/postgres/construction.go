package postgres

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "eventbus.postgres" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "eventbus.postgres" declares about itself: many
// replicas may share one PostgreSQL database, and the outbox rows and
// per-replica cursors the bus persists outlive any one process -- the
// property this implementation exists to provide. The component descriptor
// (component.go) declares this one exported value, and its component_test.go
// pins the descriptor's declaration to it, so the bits a host reads off this
// package and the assembly validates cannot drift apart.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableEventBus is the value the "eventbus.postgres" component's New
// returns: the bus itself (whose promoted methods satisfy pkgcore.EventBus)
// plus the Close() error method that releases the pool that New built, and
// the component's own Close callback releases it through this method. A host
// that calls NewEventBus itself gets the bare *EventBus and keeps owning its
// pool, exactly as that constructor's own doc comment promises; only the
// component-built value carries this closer.
type closableEventBus struct {
	*EventBus
	closePool func()
}

// Close stops the bus and then releases the pool the component's New built.
func (b *closableEventBus) Close() error {
	b.EventBus.Close()
	if b.closePool != nil {
		b.closePool()
	}
	return nil
}

// newPoolAndReplica validates and builds the pool-replicaID pair for the
// "eventbus.postgres" settings the component (component.go) resolves: its
// typed configuration carries the dsn and replica_id, and its New passes its
// own context down to pgxpool.New. Both required keys and the pool
// construction live here; nothing is dialed here, per pgxpool.New's own
// laziness.
func newPoolAndReplica(ctx context.Context, dsn, replicaID string) (*pgxpool.Pool, string, error) {
	if dsn == "" {
		return nil, "", fmt.Errorf(`missing required config key "dsn"`)
	}
	if replicaID == "" {
		return nil, "", fmt.Errorf(`missing required config key "replica_id"`)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, "", fmt.Errorf("build connection pool: %w", err)
	}
	return pool, replicaID, nil
}
