package redis

// Self-registration for the built-in "kv.redis" implementation, mirroring
// the database/sql driver-registration pattern: importing this package --
// for side effect alone, if the host calls nothing else in it -- registers
// "kv.redis" on pkgcore's shared KVStoreRegistry, the name
// pkgcore.PresetDistributed already names for the "kv" seam. Before the
// split this registration lived in pkgcore's own builtin_implementations.go,
// alongside the implementation it adapts; moving the implementation out
// without moving the registration would have left PresetDistributed pointing
// at a name nothing could ever resolve.
//
// The trade this package accepts, same as any database/sql driver: a
// distributed-mode host that forgets to import it turns "missing kv.redis"
// from a compile-time failure into a Bootstrap-time
// pkgcore.ErrUnknownImplementation (docs/internal/03-deployment-modes.md's
// implementation-registry section names this cost and accepts it explicitly).

import (
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

func init() {
	mustRegister(pkgcore.KVStoreRegistry, pkgcore.Registration[pkgcore.KVStore]{
		Name:         "kv.redis",
		Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart,
		New: func(cfg pkgcore.Config) (pkgcore.KVStore, error) {
			client, err := clientFromConfig(cfg)
			if err != nil {
				return nil, fmt.Errorf("pkgcore/kv/redis: builtin kv.redis seam: %w", err)
			}
			return NewKVStore(client), nil
		},
	})
}

// mustRegister adds r to registry and panics if that fails. It is only ever
// called here, against the one name this file controls, so a failure -- a
// duplicate name -- is a programming error in this file, not a condition a
// caller could hit or would want to recover from. pkgcore's own
// builtin_implementations.go has an unexported helper of the same name and
// shape for its own five built-ins; this package cannot call that one (it is
// unexported to the root package), so it carries its own copy rather than
// inventing a different convention.
func mustRegister[T any](registry *pkgcore.SeamRegistry[T], r pkgcore.Registration[T]) {
	if err := registry.Register(r); err != nil {
		panic(fmt.Sprintf("pkgcore/kv/redis: builtin implementation registration failed: %v", err))
	}
}

// clientFromConfig builds the *redis.Client "kv.redis" adapts onto
// NewKVStore. Nothing is dialed here, mirroring NewKVStore's own "nothing is
// dialed at construction" contract. addr falls back to "localhost:6379",
// go-redis's own default and the only sensible default for a seam a
// zero-configuration Preset must still be able to build something for; a
// host that needs a real address, credentials or a non-zero database index
// sets them in cfg, or bypasses the preset layer entirely with
// pkgcore.WithKVStore(redis.NewKVStore(client), ...).
//
// eventbus/redis carries an identical copy of this helper rather than
// sharing one: the two packages are independent implementations of
// different seams, and neither owns the other, so duplicating a dozen lines
// is cheaper than inventing a third package for both to depend on
// (go/pkgcore/AGENTS.md's packaging rule).
func clientFromConfig(cfg pkgcore.Config) (*redis.Client, error) {
	addr := cfg["addr"]
	if addr == "" {
		addr = "localhost:6379"
	}

	db := 0
	if raw, ok := cfg["db"]; ok && raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid \"db\" %q: %w", raw, err)
		}
		db = parsed
	}

	return redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: cfg["password"],
		DB:       db,
	}), nil
}
