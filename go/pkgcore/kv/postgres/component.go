package postgres

// component.go registers the "kv.postgres" component with pkgcore's global
// component registration: the descriptor a composition configuration
// selects as the "kv" module's implementation. It lives beside the
// implementation it adapts, the same file-locality the package's own init
// registration (register.go) keeps, and carries the package's own entry-table
// migration set so a database component applies it from the component
// itself (pkgcore's Assets) instead of a host calling EnsureSchema by hand.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
	postgresmigrations "github.com/vislake/speed/go/pkgcore/kv/postgres/migrations"
)

// kvPostgresConfig is the "kv.postgres" component's configuration schema:
// DSN, the one required setting the flat seam Config adapter also reads.
type kvPostgresConfig struct {
	DSN string `json:"dsn"`
}

// kvPostgresComponent is the component descriptor for "kv.postgres": the
// PostgreSQL store over a pool built from the component's own configuration,
// declaring the same exported Capabilities the seam registration declares.
// Its New funnels through newPool, the shared construction register.go's
// flat adapter also uses -- passing the component's own context down to
// pgxpool.New instead of the adapter's background one -- so both channels
// resolve the same pool from their two spellings of the same setting. The
// pool is built here, so the component owns it: Close releases it through
// the closable value's own Close. Its Migrations carry the package's
// postgres-only entry-table DDL for the assembly's database component to
// apply in the Verify stage.
var kvPostgresComponent = pkgcore.Component{
	Name:         "kv.postgres",
	Module:       "kv",
	Provides:     []any{(*pkgcore.KVStore)(nil)},
	Capabilities: Capabilities,
	ConfigSchema: (*kvPostgresConfig)(nil),
	Migrations:   postgresmigrations.FS,
	New: func(ctx context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c kvPostgresConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		pool, err := newPool(ctx, c.DSN)
		if err != nil {
			return nil, err
		}
		return &closableKVStore{KVStore: NewKVStore(pool), closePool: pool.Close}, nil
	},
	Close: func(_ context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
		if closable, ok := instance.(interface{ Close() error }); ok {
			return closable.Close()
		}
		return nil
	},
}

func init() { pkgcore.MustRegister(kvPostgresComponent) }
