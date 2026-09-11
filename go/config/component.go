package config

// component.go carries config's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contracts it consumes, and the callback that
// constructs it. The descriptor is additive: pkgcore.Module.Register, driven
// by the host's bootstrap, remains config's declaration path, and the
// descriptor states the same surface in the assembly's terms.
//
// The descriptor declares no Init and no Start. Attach -- the schema freeze
// that also publishes the runtime *Service -- must run after every module
// has registered, and the schema freeze has no method of its own (Attach's
// buildSchema runs inside it), so the host's Attach call remains both the
// freeze and the publication until a Component's Init can reach it.

import (
	"context"
	"embed"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/go/config/migrations"
)

// componentConfig is config's configuration schema in the assembly: the
// construction-time knobs NewModule's options carry. PollInterval is a
// pointer because an explicit zero is meaningful -- it disables the
// anti-loss poller outright (WithPollInterval's documented contract) -- and
// the omitted key must leave the module's own default in place.
type componentConfig struct {
	PollInterval *time.Duration `json:"poll_interval"`
}

// component returns config's component descriptor: the value init registers,
// so a composition configuration can select the module and the assembly can
// construct it from the database product in the by-type context.
func component() pkgcore.Component {
	return pkgcore.Component{
		Name:   moduleName,
		Module: moduleName,
		Requires: []pkgcore.Requirement{
			// The database connection the configs table lives in, the
			// selected db component's product.
			{Token: (*gorm.DB)(nil)},
			// The cipher that seals Sensitive configuration values.
			// Optional: a schema with no Sensitive item needs none, and one
			// that has a Sensitive item and no cipher fails the schema
			// attachment closed with ErrCipherRequired, which is the
			// fail-closed half of the optional-dependency contract.
			{Token: (*dbkit.Cipher)(nil), Optional: true},
			// The request-to-tenant resolver the two unauthenticated
			// endpoints pick whose public configuration to serve with.
			// Optional: without one, requests read platform defaults, the
			// documented display decision for the unauthenticated case.
			{Token: (*tenancy.Resolver)(nil), Optional: true},
		},
		// The construction product is the *Module; the *Service Attach
		// builds from it is the runtime configuration and feature-flag
		// reader consumers take.
		Provides:     []any{(*Module)(nil), (*Service)(nil)},
		ConfigSchema: (*componentConfig)(nil),
		// config ships no user-facing messages: its endpoints return
		// structured codes, and the copy of any console rendering its items
		// belongs to whichever module owns that surface.
		Locales:     embed.FS{},
		Migrations:  migrations.FS,
		OpenAPISpec: openAPISpecYAML,
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
			if c.PollInterval != nil {
				opts = append(opts, WithPollInterval(*c.PollInterval))
			}
			cipher, err := pkgcore.Get[*dbkit.Cipher](reg)
			switch {
			case err == nil:
				opts = append(opts, WithCipher(cipher))
			case errors.Is(err, pkgcore.ErrMissingRequirement):
				// No cipher is available: a schema with no Sensitive item
				// is served fine, and one with a Sensitive item is refused
				// at attachment.
			default:
				return nil, err
			}
			resolver, err := pkgcore.Get[tenancy.Resolver](reg)
			switch {
			case err == nil:
				opts = append(opts, WithResolver(resolver))
			case errors.Is(err, pkgcore.ErrMissingRequirement):
				// The optional resolver is absent: requests read platform
				// defaults, the shape WithResolver documents.
			default:
				return nil, err
			}
			return NewModule(db, opts...), nil
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
