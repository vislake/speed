package sharing

// component.go registers the "sharing" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as the "sharing" module's single implementation. The component builds the
// same *Module every other caller builds through NewModule, so the module's
// services, HTTP surface and registration behavior are one implementation
// reachable two ways.

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/sharing/locales"
	"github.com/vislake/speed/go/sharing/migrations"
)

// sharingComponent is the component descriptor for "sharing". It declares
// MultiReplicaSafe: the module's state is its rows in the shared database,
// the expiry sweep collapses concurrent enqueues through a shared queue, and
// the access rate limiter reads the shared key-value store.
//
// Requires the database as its one mandatory product. The queue, the tenant
// configuration reader, the resource resolver and the key-value store are
// all optional, matching the module's own construction contract: an unwired
// queue only makes EnqueueExpirySweep refuse at call time, an unwired reader
// falls back to the default expiry, an unwired resolver still runs the
// access decision but can serve no bytes, and an unwired store fails the
// rate limiter closed.
//
// Init runs the module's one declaration entry point, Register, inside the
// assembly's Init stage -- the one stage whose seats accept writes -- so the
// component world declares exactly what the module's Register declares.
var sharingComponent = pkgcore.Component{
	Name:         "sharing",
	Module:       "sharing",
	Provides:     []any{(*Module)(nil)},
	Capabilities: pkgcore.MultiReplicaSafe,
	Requires: []pkgcore.Requirement{
		{Token: (*gorm.DB)(nil)},
		{Token: (*jobs.Queue)(nil), Optional: true},
		{Token: (*TenantConfigReader)(nil), Optional: true},
		{Token: (*ResourceResolver)(nil), Optional: true},
		{Token: (*pkgcore.KVStore)(nil), Optional: true},
	},
	Migrations:  migrations.FS,
	Locales:     locales.FS,
	OpenAPISpec: openAPISpecYAML,
	New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		var opts []Option
		queue, ok, err := pkgcore.GetOptional[jobs.Queue](reg)
		if err != nil {
			return nil, err
		}
		if ok {
			opts = append(opts, WithQueue(queue))
		}
		cfg, ok, err := pkgcore.GetOptional[TenantConfigReader](reg)
		if err != nil {
			return nil, err
		}
		if ok {
			opts = append(opts, WithTenantConfigReader(cfg))
		}
		resolver, ok, err := pkgcore.GetOptional[ResourceResolver](reg)
		if err != nil {
			return nil, err
		}
		if ok {
			opts = append(opts, WithResourceResolver(resolver))
		}
		return NewModule(db, opts...), nil
	},
	Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
		m, ok := instance.(*Module)
		if !ok {
			return fmt.Errorf("sharing: component init got a %T instance, want *sharing.Module", instance)
		}
		return m.Register(reg)
	},
}

func init() { pkgcore.MustRegister(sharingComponent) }
