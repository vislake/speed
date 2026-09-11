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

// Capabilities is what "eventbus.postgres" declares about itself: many
// replicas may share one PostgreSQL database, and the outbox rows and
// per-replica cursors the bus persists outlive any one process -- the
// property this implementation exists to provide. The built-in registration
// below and the Registration factory a host wraps a self-built pool in both
// declare this one exported value, so the declaration a host reads off this
// package and the one assembly validates cannot drift apart. A host
// injecting a hand-built bus with pkgcore.WithEventBus passes it as the
// injection's capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

func init() {
	mustRegister(pkgcore.EventBusRegistry, pkgcore.Registration[pkgcore.EventBus]{
		Name:         "eventbus.postgres",
		Capabilities: Capabilities,
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

// Registration wraps a host-built *pgxpool.Pool into a pkgcore.Registration
// a host registers on EventBusRegistry under a name of its own and
// references from a Preset's "eventbus" entry -- the name-registration path
// for a configuration the flat pkgcore.Config cannot express: a TLS
// configuration inside the pool's connection string, a pool shared with
// another seam or with the application's own database access. The host
// keeps ownership of what it built, exactly as with pkgcore.WithEventBus:
// New returns the bare *EventBus over that pool, which carries no Close()
// error method, so Kernel.Bootstrap records no closer for it and
// Kernel.Shutdown never touches the pool or the bus -- the host closes both
// when it shuts down.
//
// replicaID carries the same cross-restart-stability requirement NewEventBus
// documents: it must identify the replica persistently, or catch-up after a
// reconnect reads the wrong watermark.
//
// name must not be "eventbus.postgres" itself: that name is already
// registered by this package's init(), and SeamRegistry.Register refuses a
// duplicate with pkgcore.ErrDuplicateImplementation. New ignores the Config
// a Preset entry carries -- everything the bus needs was decided when the
// host built the pool -- so a preset entry naming this Registration carries
// nothing but Implementation.
func Registration(name string, pool *pgxpool.Pool, replicaID string) pkgcore.Registration[pkgcore.EventBus] {
	return pkgcore.Registration[pkgcore.EventBus]{
		Name:         name,
		Capabilities: Capabilities,
		New: func(pkgcore.Config) (pkgcore.EventBus, error) {
			return NewEventBus(pool, replicaID), nil
		},
	}
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
	return newPoolAndReplica(context.Background(), cfg["dsn"], cfg["replica_id"])
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
