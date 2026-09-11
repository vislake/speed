package rbac

// component.go carries rbac's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contracts it consumes, and the callbacks that
// construct and declare it. Its Init runs the module's one declaration entry
// point, Register, and then Attach -- the permission-catalog snapshot that
// publishes the runtime *Service -- inside the assembly's Init stage, the
// one stage whose seats accept writes: Attach installs the Service's own
// subscriptions and job handlers, so it can run nowhere else. The *Service
// is put into the by-type context, where a consumer requires and reads it.
// The snapshot therefore covers the declarations made before this
// component's Init turn in plan order, not the full catalog.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/rbac/locales"
	"github.com/vislake/speed/go/rbac/migrations"
)

// componentConfig is rbac's configuration schema in the assembly: the
// construction-time knobs NewModule's options carry. A cache_ttl of zero
// leaves the module default in place.
type componentConfig struct {
	CacheTTL time.Duration `json:"cache_ttl"`
}

// component returns rbac's component descriptor: the value init registers,
// so a composition configuration can select the module and the assembly can
// construct it from the database product in the by-type context. Its Init
// declares through Register and publishes the runtime *Service through
// Attach.
func component() pkgcore.Component {
	return pkgcore.Component{
		Name:   moduleName,
		Module: moduleName,
		Requires: []pkgcore.Requirement{
			// The database connection the module's tables live in, the
			// selected db component's product.
			{Token: (*gorm.DB)(nil)},
			// The queue the org-event reaps are enqueued on instead of run
			// inside the event delivery. Optional: without one the module
			// keeps the synchronous best-effort reaping, the shape WithQueue
			// documents.
			{Token: (*jobs.Queue)(nil), Optional: true},
			// The subtree resolver that turns granted node ids into
			// materialized paths, taken as rbac's own structural interface
			// -- rbac never imports the module the adapter reads. Optional:
			// node-scoped grants simply cannot materialize without one, the
			// shape WithSubtreeResolver documents.
			{Token: (*SubtreeResolver)(nil), Optional: true},
		},
		// The construction product is the *Module; the *Service Attach
		// builds from it is the runtime evaluation surface host steps take.
		Provides:     []any{(*Module)(nil), (*Service)(nil)},
		ConfigSchema: (*componentConfig)(nil),
		Migrations:   migrations.FS,
		Locales:      locales.FS,
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
			var c componentConfig
			if err := cfg.Decode(&c); err != nil {
				return nil, err
			}
			db, err := pkgcore.Get[*gorm.DB](reg)
			if err != nil {
				return nil, err
			}
			var opts []Option
			if c.CacheTTL != 0 {
				opts = append(opts, WithCacheTTL(c.CacheTTL))
			}
			queue, err := pkgcore.Get[jobs.Queue](reg)
			switch {
			case err == nil:
				opts = append(opts, WithQueue(queue))
			case errors.Is(err, pkgcore.ErrMissingRequirement):
				// The optional queue is absent: reaps run synchronously
				// inside the event delivery, the shape WithQueue documents.
			default:
				return nil, err
			}
			subtree, err := pkgcore.Get[SubtreeResolver](reg)
			switch {
			case err == nil:
				opts = append(opts, WithSubtreeResolver(subtree))
			case errors.Is(err, pkgcore.ErrMissingRequirement):
				// The optional resolver is absent: node-scoped grants
				// cannot materialize, the shape WithSubtreeResolver
				// documents.
			default:
				return nil, err
			}
			return NewModule(db, opts...), nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("rbac: component init got a %T instance, want *rbac.Module", instance)
			}
			if err := m.Register(reg); err != nil {
				return err
			}
			svc, err := m.Attach(reg)
			if err != nil {
				return err
			}
			reg.Put(svc)
			return nil
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
