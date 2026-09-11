package redis

// component.go registers the "eventbus.redis" component with pkgcore's
// global component registration: the descriptor a composition configuration
// selects as the "eventbus" module's implementation. It lives beside the
// implementation it adapts, the same file-locality the package's own init
// component registration keeps.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// eventBusRedisConfig is the "eventbus.redis" component's configuration
// schema, one field per key a composition block may carry: Addr, Password
// and DB (the numeric database index).
type eventBusRedisConfig struct {
	Addr     string `json:"addr"`
	Password string `json:"password"`
	DB       int    `json:"db"`
}

// eventBusRedisComponent is the component descriptor for "eventbus.redis":
// the Redis Streams-backed bus over a client built from the component's own
// configuration, declaring the package's exported Capabilities. Its New
// funnels through newClient, the package's shared construction path, so the
// same "localhost:6379" fallback applies. The client is built here, so the
// component owns it: Close releases it through the closable value's own
// Close, which stops the bus first.
var eventBusRedisComponent = pkgcore.Component{
	Name:         "eventbus.redis",
	Module:       "eventbus",
	Provides:     []any{(*pkgcore.EventBus)(nil)},
	Capabilities: Capabilities,
	ConfigSchema: (*eventBusRedisConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c eventBusRedisConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		client := newClient(c.Addr, c.Password, c.DB)
		return &closableEventBus{EventBus: NewEventBus(client), closeClient: client.Close}, nil
	},
	Close: func(_ context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
		if closable, ok := instance.(interface{ Close() error }); ok {
			return closable.Close()
		}
		return nil
	},
}

func init() { pkgcore.MustRegister(eventBusRedisComponent) }
