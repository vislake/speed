package smilesim

// component.go registers the "smilesim" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as this app-local module's implementation. Its contribution is the
// domain's versioned migration set -- the two tables (the per-photo result
// index and the credit-reservation ledger) and the lookup index its stores
// read and write.

import (
	"context"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/smilesim/migrations"
)

// smilesimSchemaCarrier is the component's product. A component must
// construct a non-nil product, and this one's product is only its own
// presence: what the component contributes is its declared migration set,
// which the database component applies during the Verify stage, before any
// post-assembly step -- the stores' construction included -- can run.
type smilesimSchemaCarrier struct{}

// smilesimComponent is the component descriptor for "smilesim". It declares
// MultiReplicaSafe -- the domain's state is its rows in the shared database
// and nothing it holds lives in one process alone.
//
// Requires the database as its one mandatory product: the migration set it
// carries is schema for that database, so a composition selecting
// "smilesim" without a database component fails the assembly by name
// instead of silently applying nothing.
//
// Migrations is the domain's schema of record: internal/smilesim/migrations,
// one subdirectory per dialect, the CREATE ... IF NOT EXISTS statements the
// stores' tables and index are made of. The ledger keys the set by the
// module name ("smilesim"), not by the component name, so a host renaming
// or overriding the component does not fork the log.
var smilesimComponent = pkgcore.Component{
	Name:         "smilesim",
	Module:       "smilesim",
	Capabilities: pkgcore.MultiReplicaSafe,
	Requires:     []pkgcore.Requirement{{Token: (*gorm.DB)(nil)}},
	Migrations:   migrations.FS,
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		return &smilesimSchemaCarrier{}, nil
	},
}

func init() { pkgcore.MustRegister(smilesimComponent) }
