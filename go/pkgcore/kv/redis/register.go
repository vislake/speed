package redis

// Self-registration for the built-in "kv.redis" implementation, mirroring
// the database/sql driver-registration pattern: importing this package --
// for side effect alone, if the host calls nothing else in it -- registers
// "kv.redis" on pkgcore's shared KVStoreRegistry, the name
// pkgcore.PresetDistributed already names for the "kv" seam. The
// registration lives here, beside the implementation it adapts, rather than
// in pkgcore's own kv_memory.go: if the implementation sat in
// its own package while the registration stayed behind, PresetDistributed
// would point at a name nothing could resolve.
//
// The trade this package accepts, same as any database/sql driver: a
// distributed-mode host that forgets to import it turns "missing kv.redis"
// from a compile-time failure into a Bootstrap-time
// pkgcore.ErrUnknownImplementation -- the accepted database/sql trade, an
// error that names the import which fixes it.

import (
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "kv.redis" declares about itself: many replicas may
// share one Redis deployment, and the key/value state the store writes
// outlives any one process (Redis's own AOF/RDB persistence is the service
// premise, the same one the distributed deployment mode's Redis-backed
// event bus relies on). The built-in registration below and the Registration
// factory a host wraps a self-built client in both declare this one exported
// value, so the declaration a host reads off this package and the one
// assembly validates cannot drift apart. A host injecting a hand-built store
// with pkgcore.WithKVStore passes it as the injection's capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

func init() {
	mustRegister(pkgcore.KVStoreRegistry, pkgcore.Registration[pkgcore.KVStore]{
		Name:         "kv.redis",
		Capabilities: Capabilities,
		New: func(cfg pkgcore.Config) (pkgcore.KVStore, error) {
			client, err := clientFromConfig(cfg)
			if err != nil {
				return nil, fmt.Errorf("pkgcore/kv/redis: builtin kv.redis seam: %w", err)
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

// Registration wraps a host-built *redis.Client into a pkgcore.Registration
// a host registers on KVStoreRegistry under a name of its own and
// references from a Preset's "kv" entry -- the name-registration path for a
// configuration the flat pkgcore.Config cannot express: TLS material, a
// Sentinel/Cluster topology, a client shared with another seam such as
// eventbus/redis's. The host keeps ownership of what it built, exactly as
// with pkgcore.WithKVStore: New returns the bare store over that client,
// which carries no Close() error method, so Kernel.Bootstrap records no
// closer for it and Kernel.Shutdown never touches the client or the store
// -- the host closes both when it shuts down.
//
// name must not be "kv.redis" itself: that name is already registered by
// this package's init(), and SeamRegistry.Register refuses a duplicate with
// pkgcore.ErrDuplicateImplementation. New ignores the Config a Preset entry
// carries -- everything the store needs was decided when the host built the
// client -- so a preset entry naming this Registration carries nothing but
// Implementation.
func Registration(name string, client *redis.Client) pkgcore.Registration[pkgcore.KVStore] {
	return pkgcore.Registration[pkgcore.KVStore]{
		Name:         name,
		Capabilities: Capabilities,
		New: func(pkgcore.Config) (pkgcore.KVStore, error) {
			return NewKVStore(client), nil
		},
	}
}

// closableKVStore is the value "kv.redis"'s registration returns: the store
// itself (whose promoted methods satisfy pkgcore.KVStore) plus the Close()
// error method that releases the client the registration built, per the
// Registration-level resource-ownership contract. A host that calls
// NewKVStore itself gets the bare store and keeps owning its client,
// exactly as that constructor's own doc comment promises; only the
// preset-built value carries the registration's closer.
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

// mustRegister adds r to registry and panics if that fails. It is only ever
// called here, against the one name this file controls, so a failure -- a
// duplicate name -- is a programming error in this file, not a condition a
// caller could hit or would want to recover from. pkgcore's own
// registries.go has an unexported helper of the same name and shape for its
// root-package built-ins; this package cannot call that one (it is
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
// is cheaper than inventing a third package for both to depend on.
func clientFromConfig(cfg pkgcore.Config) (*redis.Client, error) {
	db := 0
	if raw, ok := cfg["db"]; ok && raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid \"db\" %q: %w", raw, err)
		}
		db = parsed
	}

	return newClient(cfg["addr"], cfg["password"], db), nil
}

// newClient builds the go-redis client for the "kv.redis" settings both
// configuration channels resolve to: the flat pkgcore.Config adapter above
// and the "kv.redis" component (component.go), whose typed configuration
// carries the same three fields. The addr fallback lives here, so the two
// channels cannot drift on it; nothing is dialed, per NewKVStore's own
// construction contract.
func newClient(addr, password string, db int) *redis.Client {
	if addr == "" {
		addr = "localhost:6379"
	}
	return redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
}
