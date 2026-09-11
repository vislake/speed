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
// property this implementation exists to provide. The built-in registration
// below and the Registration factory a host wraps a self-built pool in both
// declare this one exported value, so the declaration a host reads off this
// package and the one assembly validates cannot drift apart. A host
// injecting a hand-built bus through the by-type context passes it as the
// injection's capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableEventBus is the value "eventbus.postgres"'s registration returns:
// the bus itself (whose promoted methods satisfy pkgcore.EventBus) plus the
// Close() error method that releases the pool the registration built, per
// the Registration-level resource-ownership contract. A host that calls
// NewEventBus itself gets the bare *EventBus and keeps owning its pool,
// exactly as that constructor's own doc comment promises; only the
// preset-built value carries the registration's closer.
type closableEventBus struct {
	*EventBus
	closePool func()
}

// Close stops the bus and then releases the pool the registration built.
func (b *closableEventBus) Close() error {
	b.EventBus.Close()
	if b.closePool != nil {
		b.closePool()
	}
	return nil
}

// newPoolAndReplica validates and builds the pool-replicaID pair for the
// "eventbus.postgres" settings both configuration channels resolve to: the
// flat pkgcore.Config adapter above (which carries no context of its own --
// Registration.New has none -- hence its background context) and the
// "eventbus.postgres" component (component.go), whose typed configuration
// carries the same two fields and whose New passes its own context down.
// Both required keys and the pool construction live here, so the two
// channels cannot drift on them; nothing is dialed here, per pgxpool.New's
// own laziness.
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
