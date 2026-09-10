package redis

// The bare-injection path's one-step constructor, beside NewEventBus (a
// host-built client the host keeps owning) and Registration (a host-built
// client carried through the name-registration channel): FromAddr is for
// the host that has an address and nothing else to wire.

import (
	"errors"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// OwnedEventBus is what FromAddr returns: the distributed EventBus itself,
// whose promoted methods satisfy pkgcore.EventBus, plus the Close() error
// method that releases the Redis client FromAddr built for it. Close first
// stops the bus -- Publish refused with ErrEventBusClosed, readers cancelled
// and this instance's consumer groups destroyed, exactly the EventBus.Close
// contract -- and then closes the client. The host that injected the value
// into its Kernel calls Close once at shutdown, because an injected seam is
// the host's to close: Kernel.Shutdown never touches it.
//
// The type exists so the ownership is visible in the static type FromAddr
// returns. A bare *EventBus -- what NewEventBus returns over a client the
// host built -- deliberately does not close its client ("the client the bus
// was built on stays open, because the host owns it"); this value built its
// own client and must release it, the same way the "eventbus.redis"
// registration's own closeable value releases the client the registration
// built (register.go).
type OwnedEventBus struct {
	*EventBus
	closeClient func() error
}

// Close stops the bus and then releases the Redis client FromAddr built for
// it, in that order: the readers stop before the connections they were
// using are torn down, matching the built-in registration's own close
// sequence.
func (b *OwnedEventBus) Close() error {
	b.EventBus.Close()
	if b.closeClient != nil {
		return b.closeClient()
	}
	return nil
}

// FromAddr returns the "eventbus.redis" bus over a client it builds for
// addr, together with this package's declared Capabilities -- the one-step
// form of the bare-injection path, replacing the trio a host with only an
// address would otherwise hand-assemble before wiring it imperatively:
//
//	client := redis.NewClient(&redis.Options{Addr: addr})
//	bus := eventbusredis.NewEventBus(client)
//	pkgcore.WithEventBus(bus, eventbusredis.Capabilities)
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
// accepted as-is: nothing is dialed here, per NewEventBus's construction
// contract, so an unreachable or malformed address surfaces on the first
// operation instead of at construction.
//
// The returned value owns the client FromAddr built (see OwnedEventBus). A
// host that needs credentials, a non-zero database index, TLS material, or
// one client shared across seams builds the client itself and calls
// NewEventBus, or wraps it in this package's Registration factory.
func FromAddr(addr string) (*OwnedEventBus, pkgcore.Capability, error) {
	if addr == "" {
		return nil, 0, errors.New("pkgcore/eventbus/redis: FromAddr requires a non-empty address")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	return &OwnedEventBus{
		EventBus:    NewEventBus(client),
		closeClient: client.Close,
	}, Capabilities, nil
}
