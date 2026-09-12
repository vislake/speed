package cases

// component.go registers the "cases" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as this app-local module's implementation. Its contribution is the case
// domain's versioned migration set -- the tables and indexes its repository
// reads and writes.

import (
	"context"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/cases/migrations"
)

// casesSchemaCarrier is the component's product. A component must construct
// a non-nil product, and this one's product is only its own presence: what
// the component contributes is its declared migration set, which the
// database component applies during the Verify stage, before any
// post-assembly step -- the repository's construction included -- can run.
type casesSchemaCarrier struct{}

// casesComponent is the component descriptor for "cases". It declares
// MultiReplicaSafe -- the domain's state is its rows in the shared database
// and nothing it holds lives in one process alone.
//
// Requires the database as its one mandatory product: the migration set it
// carries is schema for that database, so a composition selecting "cases"
// without a database component fails the assembly by name instead of
// silently applying nothing.
//
// Migrations is the domain's schema of record: internal/cases/migrations,
// one subdirectory per dialect, the CREATE ... IF NOT EXISTS statements the
// repository's tables and indexes are made of. The ledger keys the set by
// the module name ("cases"), not by the component name, so a host renaming
// or overriding the component does not fork the log.
var casesComponent = pkgcore.Component{
	Name:         "cases",
	Module:       "cases",
	Capabilities: pkgcore.MultiReplicaSafe,
	Requires:     []pkgcore.Requirement{{Token: (*gorm.DB)(nil)}},
	Migrations:   migrations.FS,
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		return &casesSchemaCarrier{}, nil
	},
}

func init() { pkgcore.MustRegister(casesComponent) }
