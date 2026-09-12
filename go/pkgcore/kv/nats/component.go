package nats

// component.go registers the "kv.nats" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as the "kv" module's implementation. It lives beside the implementation it
// adapts, the same file-locality the package's own init registration keeps.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// kvNATSConfig is the "kv.nats" component's configuration schema, one field
// per key a composition block may carry.
type kvNATSConfig struct {
	URL      string `json:"url"`
	Bucket   string `json:"bucket"`
	User     string `json:"user"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

// kvNATSComponent is the component descriptor for "kv.nats": the JetStream
// KV store over a connection dialed from the component's own configuration,
// declaring the package's exported Capabilities. Its New funnels through
// newConn and natsBucketOrDefault, so the package's URL fallback, client
// name and bucket fallback apply to the connection and bucket it builds, and
// it passes its own context through to the bucket provisioning. The
// connection is dialed in New, so the component owns it: the value New
// returns carries the closer, which the assembly's close stage runs in
// place of a declared Close callback.
var kvNATSComponent = pkgcore.Component{
	Name:         "kv.nats",
	Module:       "kv",
	Provides:     []any{(*pkgcore.KVStore)(nil)},
	Capabilities: Capabilities,
	ConfigSchema: (*kvNATSConfig)(nil),
	New: func(ctx context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c kvNATSConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		conn, err := newConn(c.URL, c.User, c.Password, c.Token)
		if err != nil {
			return nil, err
		}
		store, err := NewKVStore(ctx, conn, natsBucketOrDefault(c.Bucket))
		if err != nil {
			// Best-effort close of the connection this New dialed and owns:
			// the store's construction is the failure that matters.
			conn.Close()
			return nil, err
		}
		return &closableKVStore{KVStore: store, closeConn: conn.Close}, nil
	},
}

func init() { pkgcore.MustRegister(kvNATSComponent) }
