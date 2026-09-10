package redis

// The bare-injection path's one-step constructor, beside NewKVStore (a
// host-built client the host keeps owning) and Registration (a host-built
// client carried through the name-registration channel): FromAddr is for
// the host that has an address and nothing else to wire.

import (
	"errors"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// OwnedKVStore is what FromAddr returns: the distributed KVStore itself,
// whose promoted methods satisfy pkgcore.KVStore, plus the Close() error
// method that releases the Redis client FromAddr built for it. The host
// that injected the value into its Kernel calls Close once at shutdown,
// because an injected seam is the host's to close: Kernel.Shutdown never
// touches it. Closing the client is also what refuses later operations --
// go-redis returns ErrClosed from every command against a closed client --
// so Close is the end of the store's usable life, matching the
// "eventbus.redis" bus's own Close contract.
//
// The type exists so the ownership is visible in the static type FromAddr
// returns. The bare pkgcore.KVStore NewKVStore returns carries no Close at
// all, and the client it was built on is the host's to close; this value
// built its own client and must release it, the same way the "kv.redis"
// registration's own closeable value releases the client the registration
// built (register.go).
type OwnedKVStore struct {
	pkgcore.KVStore
	closeClient func() error
}

// Close releases the Redis client FromAddr built for the store.
func (s *OwnedKVStore) Close() error {
	if s.closeClient != nil {
		return s.closeClient()
	}
	return nil
}

// FromAddr returns the "kv.redis" store over a client it builds for addr,
// together with this package's declared Capabilities -- the one-step form of
// the bare-injection path, replacing the trio a host with only an address
// would otherwise hand-assemble before wiring it imperatively:
//
//	client := redis.NewClient(&redis.Options{Addr: addr})
//	store := kvredis.NewKVStore(client)
//	pkgcore.WithKVStore(store, kvredis.Capabilities)
//
// The returned capability is the exported Capabilities constant the built-in
// registration also declares, so a host never spells the bits out and the
// declaration assembly validates cannot drift from the one the constructor
// reports.
//
// addr is an address in go-redis's own Options.Addr grammar ("host:port",
// with go-redis supplying the default port for a bare host) -- not a
// redis:// URL. An empty addr is refused with an error: go-redis treats an
// empty Addr as "localhost:6379", a fallback that is right for the
// configuration channel, where a zero-configuration Preset must still be
// able to build something (clientFromConfig's own default), and a silent
// wiring mistake in an explicit constructor. Every other address is
// accepted as-is: nothing is dialed here, per NewKVStore's construction
// contract, so an unreachable or malformed address surfaces on the first
// operation instead of at construction.
//
// The returned value owns the client FromAddr built (see OwnedKVStore). A
// host that needs credentials, a non-zero database index, TLS material, or
// one client shared across seams builds the client itself and calls
// NewKVStore, or wraps it in this package's Registration factory.
func FromAddr(addr string) (*OwnedKVStore, pkgcore.Capability, error) {
	if addr == "" {
		return nil, 0, errors.New("pkgcore/kv/redis: FromAddr requires a non-empty address")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	return &OwnedKVStore{
		KVStore:     NewKVStore(client),
		closeClient: client.Close,
	}, Capabilities, nil
}
