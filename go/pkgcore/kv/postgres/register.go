package postgres

// Self-registration for the built-in "kv.postgres" implementation,
// mirroring kv/redis/register.go's own pattern exactly: importing this
// package -- for side effect alone, if the host calls nothing else in it --
// registers "kv.postgres" on pkgcore's shared KVStoreRegistry.
//
// Unlike "kv.redis", this name is not what pkgcore.PresetDistributed points
// the "kv" seam at today (see kv.go's package doc comment); a host wanting
// this implementation instead builds its own Preset naming "kv.postgres",
// or bypasses the preset layer entirely with NewKVStore and
// pkgcore.WithKVStore.
//
// The trade this package accepts, same as kv/redis, eventbus/redis and
// eventbus/postgres: a host that forgets to import it turns "missing
// kv.postgres" from a compile-time failure into a Bootstrap-time
// pkgcore.ErrUnknownImplementation.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
)

func init() {
	mustRegister(pkgcore.KVStoreRegistry, pkgcore.Registration[pkgcore.KVStore]{
		Name:         "kv.postgres",
		Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart,
		New: func(cfg pkgcore.Config) (pkgcore.KVStore, error) {
			pool, err := poolFromConfig(cfg)
			if err != nil {
				return nil, fmt.Errorf("pkgcore/kv/postgres: builtin kv.postgres seam: %w", err)
			}
			// The pool was built here, from cfg -- the registration owns it,
			// not a host that never saw it -- so the returned value's
			// Close() error releases it. Kernel.Bootstrap records that
			// Close and runs it on Shutdown (or on its own failure path);
			// see pkgcore.Registration's resource-ownership contract. Note
			// that a store reached through the registry therefore closes
			// with its pool at Shutdown; a host that wants Sweep on a
			// store whose pool it owns calls NewKVStore directly instead.
			return &closableKVStore{KVStore: NewKVStore(pool), closePool: pool.Close}, nil
		},
	})
}

// closableKVStore is the value "kv.postgres"'s registration returns: the
// store itself (whose promoted methods satisfy pkgcore.KVStore) plus the
// Close() error method that releases the pool the registration built, per
// the Registration-level resource-ownership contract. A host that calls
// NewKVStore itself gets the bare *Store and keeps owning its pool, exactly
// as that constructor's own doc comment promises; only the preset-built
// value carries the registration's closer.
type closableKVStore struct {
	pkgcore.KVStore
	closePool func()
}

// Close releases the pool the registration built.
func (s *closableKVStore) Close() error {
	if s.closePool != nil {
		s.closePool()
	}
	return nil
}

// mustRegister adds r to registry and panics if that fails, mirroring
// kv/redis/register.go's and eventbus/postgres/register.go's identical
// unexported helper -- this package cannot call either of theirs (both are
// unexported outside their own package), so it carries its own copy, per
// go/pkgcore/AGENTS.md's packaging rule the same duplication those two
// files' own doc comments already cite.
func mustRegister[T any](registry *pkgcore.SeamRegistry[T], r pkgcore.Registration[T]) {
	if err := registry.Register(r); err != nil {
		panic(fmt.Sprintf("pkgcore/kv/postgres: builtin implementation registration failed: %v", err))
	}
}

// poolFromConfig builds the *pgxpool.Pool "kv.postgres" adapts onto
// NewKVStore, from the flat string config a Preset hands a seam
// constructor. Unlike kv/redis's clientFromConfig, there is no safe
// zero-configuration default: a bare "localhost:5432" DSN is not the
// sensible PostgreSQL default kv/redis's "localhost:6379" is for Redis (no
// database name or credentials are ever universal) -- so it is required,
// and its absence is reported the same way eventbus/postgres's
// poolAndReplicaFromConfig already reports a missing "dsn", rather than the
// connection being dialed here only to fail unhelpfully later. This table
// (pkgcore_kv_entries) must already exist -- call EnsureSchema against the
// returned pool, or apply the migration another way, before the store's
// first operation; a Preset-built store cannot call EnsureSchema itself,
// since a Preset's own contract is "resolve an implementation", never "also
// migrate a database".
func poolFromConfig(cfg pkgcore.Config) (*pgxpool.Pool, error) {
	dsn := cfg["dsn"]
	if dsn == "" {
		return nil, fmt.Errorf(`missing required config key "dsn"`)
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return nil, fmt.Errorf("build connection pool: %w", err)
	}
	return pool, nil
}
