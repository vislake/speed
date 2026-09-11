package billing

// component.go carries billing's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contracts it consumes, and the callbacks that
// construct and declare it. Its Init runs the module's one declaration entry
// point, Register, inside the assembly's Init stage -- the one stage whose
// seats accept writes -- so the module's declarations reach the assembly's
// seats exactly as they reach the assembly's registry.

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing/locales"
	"github.com/vislake/speed/go/billing/migrations"
)

// component returns billing's component descriptor: the value init registers,
// so a composition configuration can select the module and the assembly can
// construct it from the database product in the by-type context. Its Init
// runs the module's one declaration entry point, Register, inside the
// assembly's Init stage -- the one stage whose seats accept writes.
func component() pkgcore.Component {
	return pkgcore.Component{
		Name:   moduleName,
		Module: moduleName,
		Requires: []pkgcore.Requirement{
			// The database connection the module's tables live in, the
			// selected db component's product.
			{Token: (*gorm.DB)(nil)},
			// The real-time usage reading the entitlements service answers
			// quota questions with, taken as billing's own structural
			// interface. Optional: without one, quota reads answer without
			// live usage, the shape UsageReader's nil contract documents.
			{Token: (*UsageReader)(nil), Optional: true},
			// The queue the active-polling fallback is scheduled on.
			// Optional: without one only the periodic re-query of stuck
			// payment events is unavailable, the shape WithQueue documents.
			{Token: (*jobs.Queue)(nil), Optional: true},
		},
		// The construction product is the *Module; the Entitlements it
		// exposes is the judgment entry point business code calls.
		Provides: []any{(*Module)(nil)},
		// billing's state is its rows in the deployment's shared database,
		// so several replicas may run it at once.
		Capabilities: pkgcore.MultiReplicaSafe,
		// The module takes no configuration: every construction input
		// besides the database is a dependency (the usage reader, the
		// queue) or host-wired implementation (the payment gateway map,
		// whose channel collection is not configuration). A composition
		// block for billing therefore accepts no keys.
		Migrations:  migrations.FS,
		Locales:     locales.FS,
		OpenAPISpec: openAPISpecYAML,
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			db, err := pkgcore.Get[*gorm.DB](reg)
			if err != nil {
				return nil, err
			}
			var usage UsageReader
			// The reader is optional: absent, quota reads answer without
			// live usage, the shape NewModule's nil usage documents.
			reader, ok, err := pkgcore.GetOptional[UsageReader](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				usage = reader
			}
			var opts []Option
			// The queue is optional: absent, the polling fallback is
			// unavailable, the shape WithQueue documents.
			queue, ok, err := pkgcore.GetOptional[jobs.Queue](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				opts = append(opts, WithQueue(queue))
			}
			return NewModule(db, usage, opts...), nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("billing: component init got a %T instance, want *billing.Module", instance)
			}
			return m.Register(reg)
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
