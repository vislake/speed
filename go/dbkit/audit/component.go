package audit

// component.go carries the audit module's descriptor for the config-driven
// component assembly: the selection key a composition configuration names,
// the assets the module brings, the contract it consumes, and the callbacks
// that construct and declare it. Its Init runs the module's one declaration
// entry point, Register -- its three subscriptions and its two declarations
// -- inside the assembly's Init stage, the one stage whose seats accept
// writes.

import (
	"context"
	"embed"
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/dbkit/audit/migrations"
)

// component returns the audit module's component descriptor: the value init
// registers, so a composition configuration can select the module and the
// assembly can construct it from the database product in the by-type
// context. Its Init runs the module's one declaration entry point, Register,
// inside the assembly's Init stage -- the one stage whose seats accept
// writes.
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
		// The audit trail's state is its rows in the deployment's shared
		// database, so several replicas may run it at once.
		Capabilities: pkgcore.MultiReplicaSafe,
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
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("audit: component init got a %T instance, want *audit.Module", instance)
			}
			return m.Register(reg)
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
