package memcached

// Self-registration for the built-in "kv.memcached" implementation,
// mirroring kv/redis's own database/sql-style driver-registration pattern:
// importing this package -- for side effect alone, if the host calls
// nothing else in it -- registers "kv.memcached" on pkgcore's shared
// KVStoreRegistry. Unlike "kv.redis", it is not named by pkgcore.
// PresetDistributed or any other built-in Preset: it is honestly NOT
// SurvivesRestart, so it is not a sane zero-configuration default for a
// distributed deployment mode's preset the way kv.redis is. A host that
// wants it wires it explicitly,
// either through KVStoreRegistry.Build("kv.memcached", cfg) or by calling
// NewKVStore directly and injecting it with pkgcore.WithKVStore.
//
// The trade this package accepts, same as kv/redis: a host that forgets to
// import it turns "missing kv.memcached" from a compile-time failure into a
// Bootstrap-time pkgcore.ErrUnknownImplementation -- the accepted
// database/sql trade, an error that names the import which fixes it.

import (
	"fmt"
	"strings"

	"github.com/bradfitz/gomemcache/memcache"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "kv.memcached" declares about itself: honestly
// MultiReplicaSafe alone, because many processes sharing one Memcached
// deployment see the same state, while Memcached has no persistence
// mechanism of any kind, so SurvivesRestart is deliberately absent -- see
// the package doc comment. The built-in registration below and the
// Registration factory a host wraps a self-built client in both declare
// this one exported value, so the declaration a host reads off this package
// and the one assembly validates cannot drift apart. A host injecting a
// hand-built store with pkgcore.WithKVStore passes it as the injection's
// capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe

func init() {
	mustRegister(pkgcore.KVStoreRegistry, pkgcore.Registration[pkgcore.KVStore]{
		Name:         "kv.memcached",
		Capabilities: Capabilities,
		New: func(cfg pkgcore.Config) (pkgcore.KVStore, error) {
			client, err := clientFromConfig(cfg)
			if err != nil {
				return nil, fmt.Errorf("pkgcore/kv/memcached: builtin kv.memcached seam: %w", err)
			}
			return NewKVStore(client), nil
		},
	})
}

// mustRegister adds r to registry and panics if that fails. Only ever called
// here against the one name this file controls, so a failure -- a duplicate
// name -- is a programming error in this file, not a condition a caller
// could hit or would want to recover from. kv/redis's own register.go
// carries an identical helper of the same name and shape; pkgcore's own
// unexported one in registries.go is not reachable from this
// package (root-package-private), so this is a deliberate small duplication
// rather than a shared dependency neither package would otherwise need.
func mustRegister[T any](registry *pkgcore.SeamRegistry[T], r pkgcore.Registration[T]) {
	if err := registry.Register(r); err != nil {
		panic(fmt.Sprintf("pkgcore/kv/memcached: builtin implementation registration failed: %v", err))
	}
}

// clientFromConfig builds the *memcache.Client "kv.memcached" adapts onto
// NewKVStore. gomemcache dials lazily (per operation, from its own internal
// connection pool), so nothing is dialed here either, mirroring NewKVStore's
// own "nothing is dialed at construction" contract. addrs falls back to
// "localhost:11211", the default Memcached port and the only sensible
// default for a seam a zero-configuration build must still be able to
// construct something for; a host that needs a real address or multiple
// servers (comma-separated in cfg["addrs"], sharded across by gomemcache's
// own rendezvous-hashing ServerList) sets it in cfg, or bypasses this
// registry entirely with pkgcore.WithKVStore(memcached.NewKVStore(client), ...).
func clientFromConfig(cfg pkgcore.Config) (*memcache.Client, error) {
	addrs := cfg["addrs"]
	if addrs == "" {
		addrs = "localhost:11211"
	}
	servers := strings.Split(addrs, ",")
	for i, s := range servers {
		servers[i] = strings.TrimSpace(s)
	}
	return memcache.New(servers...), nil
}
