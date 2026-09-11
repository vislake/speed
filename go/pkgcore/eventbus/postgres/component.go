package postgres

// component.go registers the "eventbus.postgres" component with pkgcore's
// global component registration: the descriptor a composition configuration
// selects as the "eventbus" module's implementation. It lives beside the
// implementation it adapts, the same file-locality the package's own init
// component registration (component.go) keeps, and carries the package's own outbox
// migration set so a database component applies it from the component
// itself (pkgcore's Assets) instead of a host calling a schema helper by
// hand.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
	postgresmigrations "github.com/vislake/speed/go/pkgcore/eventbus/postgres/migrations"
)

// eventBusPostgresConfig is the "eventbus.postgres" component's
// configuration schema, one field per key a composition block may carry,
// the same two required settings the flat seam Config adapter reads.
type eventBusPostgresConfig struct {
	DSN       string `json:"dsn"`
	ReplicaID string `json:"replica_id"`
}

// eventBusPostgresComponent is the component descriptor for
// "eventbus.postgres": the outbox-backed bus over a pool built from the
// component's own configuration, declaring the same exported Capabilities
// the seam registration declares. Its New funnels through newPoolAndReplica,
// the shared construction its New also uses -- passing
// the component's own context down to pgxpool.New instead of the adapter's
// background one -- so both channels resolve the same pool-and-replicaID
// pair from their two spellings of the same settings. The pool is built
// here, so the component owns it: Close releases it through the closable
// value's own Close, which stops the bus first. Its Migrations carry the
// package's postgres-only outbox DDL for the assembly's database component
// to apply in the Verify stage.
var eventBusPostgresComponent = pkgcore.Component{
	Name:         "eventbus.postgres",
	Module:       "eventbus",
	Provides:     []any{(*pkgcore.EventBus)(nil)},
	Capabilities: Capabilities,
	ConfigSchema: (*eventBusPostgresConfig)(nil),
	Migrations:   postgresmigrations.FS,
	New: func(ctx context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c eventBusPostgresConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		pool, replicaID, err := newPoolAndReplica(ctx, c.DSN, c.ReplicaID)
		if err != nil {
			return nil, err
		}
		return &closableEventBus{EventBus: NewEventBus(pool, replicaID), closePool: pool.Close}, nil
	},
	Close: func(_ context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
		if closable, ok := instance.(interface{ Close() error }); ok {
			return closable.Close()
		}
		return nil
	},
}

func init() { pkgcore.MustRegister(eventBusPostgresComponent) }
