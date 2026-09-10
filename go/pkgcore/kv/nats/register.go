package nats

// Self-registration for the built-in "kv.nats" implementation, mirroring
// kv/redis's own register.go and, before it, the database/sql driver-
// registration pattern: importing this package -- for side effect alone, if
// the host calls nothing else in it -- registers "kv.nats" on pkgcore's
// shared KVStoreRegistry. Unlike "kv.redis", no built-in Preset names
// "kv.nats" for the "kv" seam today: this registration only makes the name
// resolvable, it does not change what pkgcore.PresetDistributed picks by
// default. A distributed-mode host that wants NATS instead either builds its
// own Preset naming "kv.nats" (after blank-importing this package so the
// name resolves) or calls NewKVStore directly and wires it with
// pkgcore.WithKVStore -- exactly the two options kv/redis's own comment
// names, minus the Preset that already points at it.
//
// The trade this package accepts is also the same as kv/redis's: a host that
// does name "kv.nats" in a Preset but forgets to import this package turns
// "missing kv.nats" from a compile-time failure into a Bootstrap-time
// pkgcore.ErrUnknownImplementation -- the accepted database/sql trade, an
// error that names the import which fixes it.

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/vislake/speed/go/pkgcore"
)

// defaultBucket is the JetStream KV bucket name a zero-configuration Preset
// falls back to -- the "kv.nats" twin of kv/redis's "localhost:6379" default
// address.
const defaultBucket = "speed-kv"

// Capabilities is what "kv.nats" declares about itself: many replicas may
// share one NATS deployment, and the JetStream KV buckets the store writes
// to (file storage, the same configuration NewKVStore provisions and
// adopts) outlive any one process -- exactly what the two bits mean. The
// built-in registration below and the Registration factory a host wraps a
// self-built connection in both declare this one exported value, so the
// declaration a host reads off this package and the one assembly validates
// cannot drift apart. A host injecting a hand-built store with
// pkgcore.WithKVStore passes it as the injection's capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

func init() {
	mustRegister(pkgcore.KVStoreRegistry, pkgcore.Registration[pkgcore.KVStore]{
		Name:         "kv.nats",
		Capabilities: Capabilities,
		New: func(cfg pkgcore.Config) (pkgcore.KVStore, error) {
			conn, err := connFromConfig(cfg)
			if err != nil {
				return nil, fmt.Errorf("pkgcore/kv/nats: builtin kv.nats seam: %w", err)
			}

			// Registration.New carries no context of its own, unlike
			// NewKVStore, which needs one to provision the bucket. A host
			// that needs a bounded provisioning deadline calls NewKVStore
			// directly with its own context and wires the result with
			// pkgcore.WithKVStore instead of going through a Preset.
			store, err := NewKVStore(context.Background(), conn, natsBucketFromConfig(cfg))
			if err != nil {
				return nil, fmt.Errorf("pkgcore/kv/nats: builtin kv.nats seam: %w", err)
			}
			// connFromConfig dials a live connection (nats.Connect is
			// synchronous), built here from cfg -- so the returned value's
			// Close() error releases it. Kernel.Bootstrap records that
			// Close and runs it on Shutdown (or on its own failure path);
			// see pkgcore.Registration's resource-ownership contract.
			return &closableKVStore{KVStore: store, closeConn: conn.Close}, nil
		},
	})
}

// Registration wraps a host-built *nats.Conn and a bucket name into a
// pkgcore.Registration a host registers on KVStoreRegistry under a name of
// its own and references from a Preset's "kv" entry -- the name-registration
// path for a configuration the flat pkgcore.Config cannot express: TLS
// material, credentials minted by a callback, a connection shared with
// another seam such as eventbus/nats's. The host keeps ownership of what it
// built, exactly as with pkgcore.WithKVStore: New returns the bare store
// over that connection, which carries no Close() error method, so
// Kernel.Bootstrap records no closer for it and Kernel.Shutdown never
// touches the connection or the store -- the host closes both when it shuts
// down.
//
// New provisions the bucket through NewKVStore with a background context,
// since Registration.New carries no context of its own (the same trade the
// built-in registration above records): a host that needs the provisioning
// bounded by a deadline calls NewKVStore directly with its own context and
// wires the result with pkgcore.WithKVStore. name must not be "kv.nats"
// itself, already registered by this package's init(); SeamRegistry.Register
// refuses a duplicate with pkgcore.ErrDuplicateImplementation.
func Registration(name string, conn *nats.Conn, bucket string) pkgcore.Registration[pkgcore.KVStore] {
	return pkgcore.Registration[pkgcore.KVStore]{
		Name:         name,
		Capabilities: Capabilities,
		New: func(pkgcore.Config) (pkgcore.KVStore, error) {
			return NewKVStore(context.Background(), conn, bucket)
		},
	}
}

// closableKVStore is the value "kv.nats"'s registration returns: the store
// itself (whose promoted methods satisfy pkgcore.KVStore) plus the Close()
// error method that releases the connection the registration dialed, per
// the Registration-level resource-ownership contract. A host that calls
// NewKVStore itself gets the bare store and keeps owning its connection,
// exactly as that constructor's own doc comment promises; only the
// preset-built value carries the registration's closer.
type closableKVStore struct {
	pkgcore.KVStore
	closeConn func()
}

// Close releases the connection the registration dialed.
func (s *closableKVStore) Close() error {
	if s.closeConn != nil {
		s.closeConn()
	}
	return nil
}

// mustRegister adds r to registry and panics if that fails. It is only ever
// called here, against the one name this file controls, so a failure -- a
// duplicate name -- is a programming error in this file, not a condition a
// caller could hit or would want to recover from. kv/redis's own
// register.go carries an identical copy for the identical reason (see its
// doc comment).
func mustRegister[T any](registry *pkgcore.SeamRegistry[T], r pkgcore.Registration[T]) {
	if err := registry.Register(r); err != nil {
		panic(fmt.Sprintf("pkgcore/kv/nats: builtin implementation registration failed: %v", err))
	}
}

// natsURLFromConfig returns the configured NATS server URL, defaulting to
// nats.DefaultURL exactly like kv/redis's clientFromConfig defaults its
// address -- the one piece of connFromConfig's job pure enough, and worth
// enough, to unit test on its own without a live server.
func natsURLFromConfig(cfg pkgcore.Config) string {
	if url := cfg["url"]; url != "" {
		return url
	}
	return nats.DefaultURL
}

// natsBucketFromConfig returns the configured JetStream KV bucket name,
// defaulting to defaultBucket.
func natsBucketFromConfig(cfg pkgcore.Config) string {
	if bucket := cfg["bucket"]; bucket != "" {
		return bucket
	}
	return defaultBucket
}

// connFromConfig builds and connects the *nats.Conn "kv.nats" adapts onto
// NewKVStore. Unlike kv/redis's clientFromConfig, which builds a client
// without dialing (go-redis connects lazily), nats.Connect dials
// synchronously, so this call is where the builtin seam's one real network
// round trip at construction happens -- credentials, if any, come from cfg;
// a host that needs a TLS config, a custom dial timeout, or any other
// *nats.Option this narrow cfg map cannot express bypasses the preset layer
// entirely with pkgcore.WithKVStore(natskv.NewKVStore(ctx, conn, bucket)).
//
// eventbus/nats, if and when it lands, is expected to carry its own copy of
// an equivalent helper rather than share this one: the two packages are
// independent implementations of different seams, and neither owns the
// other, so duplicating a dozen lines is cheaper than inventing a third
// package for both to depend on (the same deliberate duplication
// kv/redis's own clientFromConfig comment argues for its eventbus/redis
// counterpart).
func connFromConfig(cfg pkgcore.Config) (*nats.Conn, error) {
	url := natsURLFromConfig(cfg)

	opts := []nats.Option{nats.Name("speed-pkgcore-kv")}
	if user := cfg["user"]; user != "" {
		opts = append(opts, nats.UserInfo(user, cfg["password"]))
	}
	if token := cfg["token"]; token != "" {
		opts = append(opts, nats.Token(token))
	}

	conn, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to nats at %q: %w", url, err)
	}
	return conn, nil
}
