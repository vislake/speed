package pki

// component.go carries pki's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contracts it consumes, and the callbacks that
// construct and close it. The descriptor is additive: pkgcore.Module.Register,
// driven by the host's bootstrap, remains pki's declaration path, and the
// descriptor states the same surface in the assembly's terms.
//
// The descriptor declares no Prepare callback. RegisterLocalKeySerializer --
// the pre-open step that registers the cipher GORM resolves the LocalSigner's
// private-key column through -- consumes the pki.local_key_cipher_key
// material, and the by-purpose material source that hands a component its own
// declared material is not part of the assembly yet; building the cipher from
// anything else here would state a different contract than the declaration
// does. The host wiring (pki.RegisterLocalKeySerializer over the material it
// resolves) is the path that performs it today.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki/locales"
	"github.com/vislake/speed/go/pki/migrations"
)

// componentConfig is pki's configuration schema in the assembly: the
// construction-time knobs NewModule's options carry, as structured
// configuration. Every field is a pointer, so an omitted key leaves the
// module's own default in place while a present key is applied verbatim --
// including an explicit zero, which WithCacheTTL documents as disabling the
// key-set cache outright.
type componentConfig struct {
	PropagationWindow *time.Duration `json:"propagation_window"`
	RenewalLeadTime   *time.Duration `json:"renewal_lead_time"`
	ExpiryScanWindow  *time.Duration `json:"expiry_scan_window"`
	CacheTTL          *time.Duration `json:"cache_ttl"`
}

// component returns pki's component descriptor: the value init registers, so
// a composition configuration can select the module, the assembly can
// construct it from the database component's product, and Close releases the
// key-set cache's janitor.
func component() pkgcore.Component {
	return pkgcore.Component{
		Name:   moduleName,
		Module: moduleName,
		Requires: []pkgcore.Requirement{
			// The database connection the module's tables live in, the
			// selected db component's product.
			{Token: (*gorm.DB)(nil)},
			// The queue the expiry scan and the CRL regeneration are
			// scheduled on. Optional: without one the module keeps its whole
			// synchronous surface and only automatic rotation is
			// unavailable, the shape WithQueue documents.
			{Token: (*jobs.Queue)(nil), Optional: true},
		},
		// The construction product is the *Module; the *Service baked into
		// it is the signing-key lifecycle authn's own structurally declared
		// KeySource token resolves against.
		Provides:      []any{(*Module)(nil), (*Service)(nil)},
		ConfigSchema:  (*componentConfig)(nil),
		BootstrapKeys: []pkgcore.BootstrapKey{bootstrapKeyDecl},
		Migrations:    migrations.FS,
		Locales:       locales.FS,
		OpenAPISpec:   openAPISpecYAML,
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
			if c.PropagationWindow != nil {
				opts = append(opts, WithPropagationWindow(*c.PropagationWindow))
			}
			if c.RenewalLeadTime != nil {
				opts = append(opts, WithRenewalLeadTime(*c.RenewalLeadTime))
			}
			if c.ExpiryScanWindow != nil {
				opts = append(opts, WithExpiryScanWindow(*c.ExpiryScanWindow))
			}
			if c.CacheTTL != nil {
				opts = append(opts, WithCacheTTL(*c.CacheTTL))
			}
			queue, err := pkgcore.Get[jobs.Queue](reg)
			switch {
			case err == nil:
				opts = append(opts, WithQueue(queue))
			case errors.Is(err, pkgcore.ErrMissingRequirement):
				// The optional queue is absent: the module runs without
				// automatic rotation, and register claims neither task
				// handler nor schedule.
			default:
				return nil, err
			}
			return NewModule(db, opts...), nil
		},
		Close: func(_ context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("pki: component close got a %T instance, want *pki.Module", instance)
			}
			return m.Close()
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
