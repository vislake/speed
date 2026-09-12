package nats

// component.go registers the "eventbus.nats" component with pkgcore's
// global component registration: the descriptor a composition configuration
// selects as the "eventbus" module's implementation. It lives beside the
// implementation it adapts, the same file-locality the package's own init
// component registration keeps.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// eventBusNATSConfig is the "eventbus.nats" component's configuration
// schema, one field per key a composition block may carry.
type eventBusNATSConfig struct {
	URL      string `json:"url"`
	Token    string `json:"token"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// eventBusNATSComponent is the component descriptor for "eventbus.nats":
// the JetStream-backed bus over a connection dialed from the component's own
// configuration, declaring the package's exported Capabilities. Its New
// funnels through newConn, which applies the package's URL fallback and
// retry-on-failed-connect posture to the connection it dials. The connection
// is dialed here, so the component owns it: the value New returns carries
// the closer (it stops the bus first), which the assembly's close stage
// runs in place of a declared Close callback.
var eventBusNATSComponent = pkgcore.Component{
	Name:         "eventbus.nats",
	Module:       "eventbus",
	Provides:     []any{(*pkgcore.EventBus)(nil)},
	Capabilities: Capabilities,
	ConfigSchema: (*eventBusNATSConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c eventBusNATSConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		conn, err := newConn(c.URL, c.Token, c.User, c.Password)
		if err != nil {
			return nil, err
		}
		return &closableEventBus{EventBus: NewEventBus(conn), closeConn: conn.Close}, nil
	},
}

func init() { pkgcore.MustRegister(eventBusNATSComponent) }
