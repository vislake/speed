package redis

// component.go registers the "kv.redis" component with pkgcore's global
// component registration: the descriptor a composition configuration
// selects as the "kv" module's implementation. It lives beside the
// implementation it adapts, the same file-locality the package's own init
// registration (register.go) keeps.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// kvRedisConfig is the "kv.redis" component's configuration schema, one
// field per key a composition block may carry: Addr and Password as the
// flat seam Config spells them, DB the numeric database index the flat
// spelling passes as text.
type kvRedisConfig struct {
	Addr     string `json:"addr"`
	Password string `json:"password"`
	DB       int    `json:"db"`
}

// kvRedisComponent is the component descriptor for "kv.redis": the Redis
// store over a client built from the component's own configuration,
// declaring the same exported Capabilities the seam registration declares.
// Its New funnels through newClient, the shared construction register.go's
// flat adapter also uses, so both configuration channels resolve the same
// client (and the same "localhost:6379" fallback) from their two spellings
// of the same settings. The client is built here, so the component owns it:
// Close releases it through the closable value's own Close.
var kvRedisComponent = pkgcore.Component{
	Name:         "kv.redis",
	Module:       "kv",
	Provides:     []any{(*pkgcore.KVStore)(nil)},
	Capabilities: Capabilities,
	ConfigSchema: (*kvRedisConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c kvRedisConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		client := newClient(c.Addr, c.Password, c.DB)
		return &closableKVStore{KVStore: NewKVStore(client), closeClient: client.Close}, nil
	},
	Close: func(_ context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
		if closable, ok := instance.(interface{ Close() error }); ok {
			return closable.Close()
		}
		return nil
	},
}

func init() { pkgcore.MustRegister(kvRedisComponent) }
