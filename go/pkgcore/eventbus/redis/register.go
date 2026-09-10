package redis

// Self-registration for the built-in "eventbus.redis" implementation,
// mirroring the database/sql driver-registration pattern: importing this
// package -- for side effect alone, if the host calls nothing else in it --
// registers "eventbus.redis" on pkgcore's shared EventBusRegistry, the name
// pkgcore.PresetDistributed already names for the "eventbus" seam. The
// registration lives here, beside the implementation it adapts, rather than
// in pkgcore's own eventbus_builtins.go: if the implementation sat in
// its own package while the registration stayed behind, PresetDistributed
// would point at a name nothing could resolve.
//
// The trade this package accepts, same as any database/sql driver: a
// distributed-mode host that forgets to import it turns "missing
// eventbus.redis" from a compile-time failure into a Bootstrap-time
// pkgcore.ErrUnknownImplementation -- the accepted database/sql trade, an
// error that names the import which fixes it.

import (
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "eventbus.redis" declares about itself: many replicas
// may share one Redis deployment, and the Redis Streams state the bus writes
// outlives any one process, exactly what the two bits mean. The built-in
// registration below and the Registration factory a host wraps a self-built
// client in both declare this one exported value, so the declaration a host
// reads off this package and the one assembly validates cannot drift apart.
// A host injecting a hand-built bus with pkgcore.WithEventBus passes it as
// the injection's capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

func init() {
	mustRegister(pkgcore.EventBusRegistry, pkgcore.Registration[pkgcore.EventBus]{
		Name:         "eventbus.redis",
		Capabilities: Capabilities,
		New: func(cfg pkgcore.Config) (pkgcore.EventBus, error) {
			client, err := clientFromConfig(cfg)
			if err != nil {
				return nil, fmt.Errorf("pkgcore/eventbus/redis: builtin eventbus.redis seam: %w", err)
			}
			// The client was built here, from cfg -- the registration owns
			// it, not a host that never saw it -- so the returned value
			// closes both halves: EventBus.Close stops the readers, and
			// client.Close releases the dialed connections, which
			// EventBus.Close deliberately leaves open because a host-built
			// client is the host's to close. Kernel.Bootstrap records this
			// Close and runs it on Shutdown (or on its own failure path);
			// see pkgcore.Registration's resource-ownership contract.
			bus := NewEventBus(client)
			return &closableEventBus{EventBus: bus, closeClient: client.Close}, nil
		},
	})
}

// Registration wraps a host-built *redis.Client into a pkgcore.Registration
// a host registers on EventBusRegistry under a name of its own and
// references from a Preset's "eventbus" entry -- the name-registration path
// for a configuration the flat pkgcore.Config cannot express: TLS material,
// a Sentinel/Cluster topology, a client shared with another seam such as
// kv/redis's. The host keeps ownership of what it built, exactly as with
// pkgcore.WithEventBus: New returns the bare *EventBus over that client,
// which carries no Close() error method, so Kernel.Bootstrap records no
// closer for it and Kernel.Shutdown never touches the client or the bus --
// the host closes both when it shuts down.
//
// name must not be "eventbus.redis" itself: that name is already registered
// by this package's init(), and SeamRegistry.Register refuses a duplicate
// with pkgcore.ErrDuplicateImplementation. New ignores the Config a Preset
// entry carries -- everything the bus needs was decided when the host built
// the client -- so a preset entry naming this Registration carries nothing
// but Implementation.
func Registration(name string, client *redis.Client) pkgcore.Registration[pkgcore.EventBus] {
	return pkgcore.Registration[pkgcore.EventBus]{
		Name:         name,
		Capabilities: Capabilities,
		New: func(pkgcore.Config) (pkgcore.EventBus, error) {
			return NewEventBus(client), nil
		},
	}
}

// closableEventBus is the value "eventbus.redis"'s registration returns: the
// bus itself (whose promoted methods satisfy pkgcore.EventBus) plus the
// Close() error method that releases the client the registration built, per
// the Registration-level resource-ownership contract. A host that calls
// NewEventBus itself gets the bare *EventBus and keeps owning its client,
// exactly as that constructor's own doc comment promises; only the
// preset-built value carries the registration's closer.
type closableEventBus struct {
	*EventBus
	closeClient func() error
}

// Close stops the bus and then releases the client the registration built.
func (b *closableEventBus) Close() error {
	b.EventBus.Close()
	if b.closeClient != nil {
		return b.closeClient()
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
		panic(fmt.Sprintf("pkgcore/eventbus/redis: builtin implementation registration failed: %v", err))
	}
}

// clientFromConfig builds the *redis.Client "eventbus.redis" adapts onto
// NewEventBus. Nothing is dialed here, mirroring NewEventBus's own "nothing
// is dialed at construction" contract. addr falls back to "localhost:6379",
// go-redis's own default and the only sensible default for a seam a
// zero-configuration Preset must still be able to build something for; a
// host that needs a real address, credentials or a non-zero database index
// sets them in cfg, or bypasses the preset layer entirely with
// pkgcore.WithEventBus(redis.NewEventBus(client), ...).
//
// kv/redis carries an identical copy of this helper rather than sharing one:
// the two packages are independent implementations of different seams, and
// neither owns the other, so duplicating a dozen lines is cheaper than
// inventing a third package for both to depend on.
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
