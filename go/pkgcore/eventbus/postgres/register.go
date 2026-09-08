package postgres

// Self-registration for the built-in "eventbus.postgres" implementation,
// mirroring eventbus/redis/register.go's own pattern exactly: importing
// this package -- for side effect alone, if the host calls nothing else in
// it -- registers "eventbus.postgres" on pkgcore's shared EventBusRegistry.
//
// Unlike "eventbus.redis", this name is not what pkgcore.PresetDistributed
// points the "eventbus" seam at today (see eventbus.go's package doc
// comment); a host wanting this implementation under the Preset layer
// builds its own Preset naming "eventbus.postgres" instead, or bypasses
// the preset layer entirely with NewEventBus and pkgcore.WithEventBus.
//
// The trade this package accepts, same as eventbus/redis and any
// database/sql driver: a host that forgets to import it turns "missing
// eventbus.postgres" from a compile-time failure into a Bootstrap-time
// pkgcore.ErrUnknownImplementation.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
)

func init() {
	mustRegister(pkgcore.EventBusRegistry, pkgcore.Registration[pkgcore.EventBus]{
		Name:         "eventbus.postgres",
		Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart,
		New: func(cfg pkgcore.Config) (pkgcore.EventBus, error) {
			pool, replicaID, err := poolAndReplicaFromConfig(cfg)
			if err != nil {
				return nil, fmt.Errorf("pkgcore/eventbus/postgres: builtin eventbus.postgres seam: %w", err)
			}
			// The pool was built here, from cfg -- the registration owns it,
			// not a host that never saw it -- so the returned value closes
			// both halves: EventBus.Close stops the listener, and pool.Close
			// releases the pooled connections, which EventBus.Close
			// deliberately leaves open because a host-built pool is the
			// host's to close. Kernel.Bootstrap records this Close and runs
			// it on Shutdown (or on its own failure path); see
			// pkgcore.Registration's resource-ownership contract.
			bus := NewEventBus(pool, replicaID)
			return &closableEventBus{EventBus: bus, closePool: pool.Close}, nil
		},
	})
}

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

// mustRegister adds r to registry and panics if that fails, mirroring
// eventbus/redis/register.go's identical unexported helper -- this package
// cannot call that one (it is unexported to the redis package), so it
// carries its own copy -- the same deliberate duplication the redis
// package's own helper records.
func mustRegister[T any](registry *pkgcore.SeamRegistry[T], r pkgcore.Registration[T]) {
	if err := registry.Register(r); err != nil {
		panic(fmt.Sprintf("pkgcore/eventbus/postgres: builtin implementation registration failed: %v", err))
	}
}

// poolAndReplicaFromConfig builds the *pgxpool.Pool and reads the
// replicaID "eventbus.postgres" adapts onto NewEventBus, from the flat
// string config a Preset hands a seam constructor. Unlike
// eventbus/redis's clientFromConfig, there is no safe zero-configuration
// default for either: a bare "localhost:5432" DSN is not the sensible
// PostgreSQL default eventbus/redis's "localhost:6379" is for Redis (no
// database name or credentials are ever universal), and a random fallback
// replicaID would silently defeat the entire cross-restart durability
// guarantee this implementation exists to provide (see EventBus's own doc
// comment) -- so both are required, and their absence is reported the same
// way mailer.smtp and objectstore.s3 already report a missing required
// credential under PresetDistributed, rather than the connection being
// dialed here only to fail unhelpfully later.
func poolAndReplicaFromConfig(cfg pkgcore.Config) (*pgxpool.Pool, string, error) {
	dsn := cfg["dsn"]
	if dsn == "" {
		return nil, "", fmt.Errorf(`missing required config key "dsn"`)
	}
	replicaID := cfg["replica_id"]
	if replicaID == "" {
		return nil, "", fmt.Errorf(`missing required config key "replica_id"`)
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return nil, "", fmt.Errorf("build connection pool: %w", err)
	}
	return pool, replicaID, nil
}
