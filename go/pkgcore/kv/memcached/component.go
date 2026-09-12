package memcached

// component.go registers the "kv.memcached" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as the "kv" module's implementation. It lives beside the implementation it
// adapts, the same file-locality the package's own init registration keeps.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// kvMemcachedConfig is the "kv.memcached" component's configuration schema:
// Addrs, the comma-separated server list.
type kvMemcachedConfig struct {
	Addrs string `json:"addrs"`
}

// kvMemcachedComponent is the component descriptor for "kv.memcached": the
// Memcached store over a client built from the component's own
// configuration, declaring the package's exported Capabilities (honestly
// MultiReplicaSafe alone: Memcached has no persistence mechanism of any
// kind, so no SurvivesRestart -- see the package doc comment). Its New
// funnels through newClient, the package's shared construction path, so the
// same "localhost:11211" fallback and comma-splitting apply. The client is
// built here, so the component owns it: the value New returns carries the
// closer, which the assembly's close stage runs in place of a declared
// Close callback.
var kvMemcachedComponent = pkgcore.Component{
	Name:         "kv.memcached",
	Module:       "kv",
	Provides:     []any{(*pkgcore.KVStore)(nil)},
	Capabilities: Capabilities,
	ConfigSchema: (*kvMemcachedConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c kvMemcachedConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		client := newClient(c.Addrs)
		return &closableKVStore{KVStore: NewKVStore(client), closeClient: client.Close}, nil
	},
}

func init() { pkgcore.MustRegister(kvMemcachedComponent) }
