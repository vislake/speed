package memcached

// Self-registration for the built-in "kv.memcached" implementation,
// mirroring kv/redis's own database/sql-style driver-registration pattern:
// importing this package -- for side effect alone, if the host calls
// nothing else in it -- registers "kv.memcached" on pkgcore's shared
// KVStoreRegistry. Unlike "kv.redis", it is not named by pkgcore.
// PresetDistributed or any other built-in Preset: it is honestly NOT
// SurvivesRestart, so it is not a sane zero-configuration default for a
// distributed deployment mode's preset the way kv.redis is. A host that
// wants it reaches it explicitly: through KVStoreRegistry.Build or a
// Preset entry naming "kv.memcached" with its cfg,
// through the Registration factory below wrapping a client the host built,
// or by calling NewKVStore directly and injecting it with
// pkgcore.WithKVStore.
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
			// The client was built here, from cfg -- the registration owns
			// it, not a host that never saw it -- so the returned value's
			// Close() error releases it. Kernel.Bootstrap records that
			// Close and runs it on Shutdown (or on its own failure path);
			// see pkgcore.Registration's resource-ownership contract.
			return &closableKVStore{KVStore: NewKVStore(client), closeClient: client.Close}, nil
		},
	})
}

// Registration wraps a host-built *memcache.Client into a
// pkgcore.Registration a host registers on KVStoreRegistry under a name of
// its own and references from a Preset's "kv" entry -- the name-registration
// path for a configuration the flat pkgcore.Config cannot express: a
// per-server topology assembled in code, a client shared with another
// consumer. The host keeps ownership of what it built, exactly as with
// pkgcore.WithKVStore: New returns the bare store over that client, which
// carries no Close() error method, so Kernel.Bootstrap records no closer for
// it and Kernel.Shutdown never touches the client or the store -- the host
// closes both when it shuts down.
//
// name must not be "kv.memcached" itself: that name is already registered by
// this package's init(), and SeamRegistry.Register refuses a duplicate with
// pkgcore.ErrDuplicateImplementation. New ignores the Config a Preset entry
// carries -- everything the store needs was decided when the host built the
// client -- so a preset entry naming this Registration carries nothing but
// Implementation.
func Registration(name string, client *memcache.Client) pkgcore.Registration[pkgcore.KVStore] {
	return pkgcore.Registration[pkgcore.KVStore]{
		Name:         name,
		Capabilities: Capabilities,
		New: func(pkgcore.Config) (pkgcore.KVStore, error) {
			return NewKVStore(client), nil
		},
	}
}

// closableKVStore is the value "kv.memcached"'s registration returns: the
// store itself (whose promoted methods satisfy pkgcore.KVStore) plus the
// Close() error method that releases the client the registration built, per
// the Registration-level resource-ownership contract. A host that calls
// NewKVStore itself gets the bare store and keeps owning its client, exactly
// as that constructor's own doc comment promises; only the preset-built
// value carries the registration's closer. Close releases the client's idle
// pooled connections -- gomemcache dials per operation, so there is no other
// long-lived state to stop.
type closableKVStore struct {
	pkgcore.KVStore
	closeClient func() error
}

// Close releases the client the registration built.
func (s *closableKVStore) Close() error {
	if s.closeClient != nil {
		return s.closeClient()
	}
	return nil
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
	return newClient(cfg["addrs"]), nil
}

// newClient builds the *memcache.Client for the "kv.memcached" settings both
// configuration channels resolve to: the flat pkgcore.Config adapter above
// and the "kv.memcached" component (component.go), whose typed
// configuration carries the same comma-separated addrs spelling. The
// fallback address and the sharding list live here, so the two channels
// cannot drift on them; nothing is dialed, per gomemcache's own per-operation
// laziness (see clientFromConfig's doc comment above).
func newClient(addrs string) *memcache.Client {
	if addrs == "" {
		addrs = "localhost:11211"
	}
	servers := strings.Split(addrs, ",")
	for i, s := range servers {
		servers[i] = strings.TrimSpace(s)
	}
	return memcache.New(servers...)
}
