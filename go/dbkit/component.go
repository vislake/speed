package dbkit

// component.go registers the two database components with pkgcore's global
// component registration: the descriptors a composition configuration
// selects as the "db" module's implementation, one per dialect. The dialect
// registry (open.go's RegisterDialect, the database/sql-style split the
// dialect subpackages register into) is untouched: each component pins its
// dialect by name and Open resolves it through the same registry as every
// other caller.

import (
	"context"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

// dbComponentConfig is the configuration schema both database components
// share: DSN, the one setting a connection needs beyond the dialect its
// component name pins. Decoding is strict, so a composition block naming any
// other key fails the Prepare stage by name.
type dbComponentConfig struct {
	DSN string `json:"dsn"`
}

// dbSQLiteComponent is the component descriptor for "db.sqlite": the
// SQLite connection a single-process composition selects. Capabilities are
// honestly 0 -- a SQLite file is not shared state a second replica may use,
// and it is the standalone dialect by design -- so a distributed-mode
// assembly selecting it fails the capability check rather than pretending
// the composition is sound.
var dbSQLiteComponent = dbConnectionComponent("db.sqlite", DialectSQLite, 0)

// dbPostgresComponent is the component descriptor for "db.postgres": the
// PostgreSQL connection a distributed composition selects. It declares
// MultiReplicaSafe (many replicas share one database) and SurvivesRestart
// (the rows live inside the PostgreSQL service and outlive any one process),
// the bits the distributed deployment mode requires.
var dbPostgresComponent = dbConnectionComponent("db.postgres", DialectPostgres, pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart)

func init() {
	pkgcore.MustRegister(dbSQLiteComponent)
	pkgcore.MustRegister(dbPostgresComponent)
}

// dbConnectionComponent builds one database component descriptor for dialect.
// The two components differ only in their name, dialect and declared
// capabilities, so both are built here once: New connects (only connects --
// no migration runs before every product exists), Verify applies the
// assembled migration sets, and Close releases the connection.
func dbConnectionComponent(name string, dialect Dialect, capabilities pkgcore.Capability) pkgcore.Component {
	return pkgcore.Component{
		Name:         name,
		Module:       "db",
		Provides:     []any{(*gorm.DB)(nil)},
		Capabilities: capabilities,
		ConfigSchema: (*dbComponentConfig)(nil),
		// New connects only: the dialect is pinned by the component's own
		// name, the DSN comes from the component's block, and Open installs
		// every mandatory safeguard before returning. Migration application
		// is deliberately not here -- the construction of other components
		// may yet fail, and construction failure must not touch the
		// database.
		New: func(ctx context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
			var c dbComponentConfig
			if err := cfg.Decode(&c); err != nil {
				return nil, err
			}
			return Open(ctx, Options{Dialect: dialect, DSN: c.DSN})
		},
		// Verify is where the assembled migration sets are applied: every
		// product is constructed by now, so the selected components' assets
		// are complete; the db component is dependency-free, so it runs
		// first in the stage and no other Verify sees an unmigrated schema.
		Verify: func(ctx context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			return ApplyMigrations(ctx, reg)
		},
		// Close releases the connection this component opened.
		Close: func(_ context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			if db, ok := instance.(*gorm.DB); ok {
				return Close(db)
			}
			return nil
		},
	}
}
