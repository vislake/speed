package nats

// Self-registration for the "eventbus.nats" implementation, mirroring
// eventbus/redis/register.go's own init()-based database/sql driver pattern
// exactly: importing this package -- for side effect alone, if the host
// calls nothing else in it -- registers "eventbus.nats" on pkgcore's shared
// EventBusRegistry. Unlike "eventbus.redis", this name is not what
// pkgcore.PresetDistributed points the "eventbus" seam at (that Preset is
// untouched by this package); a host that wants this implementation through
// the Preset layer must build it explicitly with
// pkgcore.EventBusRegistry.Build("eventbus.nats", cfg), or call NewEventBus
// directly and wire it with pkgcore.WithEventBus.
//
// The trade this package accepts, same as any database/sql driver and the
// same one eventbus/redis's own register.go accepts: a host that forgets to
// import it turns "missing eventbus.nats" from a compile-time failure into a
// Bootstrap-time pkgcore.ErrUnknownImplementation -- the accepted
// database/sql trade, an error that names the import which fixes it.

import (
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/vislake/speed/go/pkgcore"
)

func init() {
	mustRegister(pkgcore.EventBusRegistry, pkgcore.Registration[pkgcore.EventBus]{
		Name:         "eventbus.nats",
		Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart,
		New: func(cfg pkgcore.Config) (pkgcore.EventBus, error) {
			conn, err := connFromConfig(cfg)
			if err != nil {
				return nil, fmt.Errorf("pkgcore/eventbus/nats: builtin eventbus.nats seam: %w", err)
			}
			// connFromConfig dials a live connection (nats.Connect is
			// synchronous, with RetryOnFailedConnect), and the connection
			// was built here, from cfg -- so the returned value closes both
			// halves: EventBus.Close stops the readers and deletes the
			// consumers, and conn.Close releases the dialed connection,
			// which EventBus.Close deliberately leaves open because a
			// host-built connection is the host's to close.
			// Kernel.Bootstrap records this Close and runs it on Shutdown
			// (or on its own failure path); see pkgcore.Registration's
			// resource-ownership contract.
			bus := NewEventBus(conn)
			return &closableEventBus{EventBus: bus, closeConn: conn.Close}, nil
		},
	})
}

// closableEventBus is the value "eventbus.nats"'s registration returns: the
// bus itself (whose promoted methods satisfy pkgcore.EventBus) plus the
// Close() error method that releases the connection the registration
// dialed, per the Registration-level resource-ownership contract. A host
// that calls NewEventBus itself gets the bare *EventBus and keeps owning
// its connection, exactly as that constructor's own doc comment promises;
// only the preset-built value carries the registration's closer.
type closableEventBus struct {
	*EventBus
	closeConn func()
}

// Close stops the bus and then releases the connection the registration
// dialed.
func (b *closableEventBus) Close() error {
	b.EventBus.Close()
	if b.closeConn != nil {
		b.closeConn()
	}
	return nil
}

// mustRegister adds r to registry and panics if that fails. It is only ever
// called here, against the one name this file controls, so a failure -- a
// duplicate name -- is a programming error in this file, not a condition a
// caller could hit or would want to recover from. eventbus/redis/register.go
// carries an identical copy of this helper for the identical reason: it
// cannot call pkgcore's own unexported one, and the two packages are
// independent implementations that own no shared package worth introducing
// for four lines.
func mustRegister[T any](registry *pkgcore.SeamRegistry[T], r pkgcore.Registration[T]) {
	if err := registry.Register(r); err != nil {
		panic(fmt.Sprintf("pkgcore/eventbus/nats: builtin implementation registration failed: %v", err))
	}
}

// connFromConfig builds the *nats.Conn "eventbus.nats" adapts onto
// NewEventBus. url falls back to nats.DefaultURL ("nats://127.0.0.1:4222"),
// the only sensible default for a seam a zero-configuration Preset-style
// caller must still be able to build something for; a host that needs a
// real address or credentials sets them in cfg, or bypasses this registry
// entry entirely with pkgcore.WithEventBus(nats.NewEventBus(conn), ...).
//
// Unlike eventbus/redis's own clientFromConfig, which builds a client that
// dials nothing until first use, nats.Connect always dials synchronously --
// there is no lazy *nats.Conn in nats.go's own client model. This function
// therefore always passes nats.RetryOnFailedConnect(true) (with unlimited
// reconnect attempts): if the initial dial fails, nats.Connect returns a
// live connection in its own reconnecting state instead of an error, and
// nats.go itself keeps retrying in the background. That is what keeps this
// registration's own contract identical to eventbus/redis's: building
// "eventbus.nats" through SeamRegistry.Build (and therefore
// Kernel.Bootstrap) never blocks or fails merely because NATS is not
// reachable yet at that exact moment -- the same property a host gets for
// free from go-redis's own laziness.
func connFromConfig(cfg pkgcore.Config) (*nats.Conn, error) {
	url := cfg["url"]
	if url == "" {
		url = nats.DefaultURL
	}

	opts := []nats.Option{
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1), // never give up reconnecting in the background
	}
	if token := cfg["token"]; token != "" {
		opts = append(opts, nats.Token(token))
	}
	if user := cfg["user"]; user != "" {
		opts = append(opts, nats.UserInfo(user, cfg["password"]))
	}

	return nats.Connect(url, opts...)
}
