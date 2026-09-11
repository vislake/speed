package audit

// component.go carries the audit module's descriptor for the config-driven
// component assembly: the selection key a composition configuration names,
// the assets the module brings, the contract it consumes, and the callback
// that constructs it. The descriptor is additive: pkgcore.Module.Register,
// driven by the host's bootstrap, remains the module's declaration path --
// its three subscriptions and its two declarations -- and the descriptor
// states the same surface in the assembly's terms.

import (
	"context"
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/dbkit/audit/migrations"
)

// component returns the audit module's component descriptor: the value init
// registers, so a composition configuration can select the module and the
// assembly can construct it from the database product in the by-type
// context.
func component() pkgcore.Component {
	return pkgcore.Component{
		Name:   moduleName,
		Module: moduleName,
		Requires: []pkgcore.Requirement{
			// The database connection the audit_events table lives in, the
			// selected db component's product.
			{Token: (*gorm.DB)(nil)},
		},
		// The construction product is the *Module, whose subscriptions
		// normalize every captured write and recorded action into the
		// audit trail.
		Provides: []any{(*Module)(nil)},
		// The module takes no configuration: its whole construction input
		// is the database, and a composition block for it accepts no keys.
		// It ships no user-facing messages (no query or report surface
		// exists yet), hence the empty locale set.
		Locales:    embed.FS{},
		Migrations: migrations.FS,
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			db, err := pkgcore.Get[*gorm.DB](reg)
			if err != nil {
				return nil, err
			}
			return New(db), nil
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
