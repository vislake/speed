package org

// component.go carries org's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contracts it consumes, and the callback that
// constructs it. The descriptor is additive: pkgcore.Module.Register, driven
// by the host's bootstrap, remains org's declaration path, and the descriptor
// states the same surface in the assembly's terms.
//
// The descriptor declares no Prepare callback. RegisterEmailSerializer and
// NewEmailIndexer -- the pre-open registrations GORM resolves the invitation
// address column and its blind index through -- consume the
// org.invitation_email_index_key material and the host's configuration
// cipher, and the by-purpose material source that hands a component its own
// declared material is not part of the assembly yet; registering either
// value from anything else here would state a different contract than the
// declaration does. The host wiring (org.RegisterEmailSerializer plus the
// host-built indexer) is the path that performs both today.

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/org/locales"
	"github.com/vislake/speed/go/org/migrations"
)

// componentConfig is org's configuration schema in the assembly: the
// construction-time knobs NewModule's options carry, as structured
// configuration. The invitation-address blind-index key is process-start key
// material and stays on the bootstrap seat (bootstrapKeyDecl), never in
// configuration.
type componentConfig struct {
	MailFrom      string        `json:"mail_from"`
	ReplyTo       string        `json:"reply_to"`
	InvitationTTL time.Duration `json:"invitation_ttl"`
	MaxDepth      int           `json:"max_depth"`
}

// component returns org's component descriptor: the value init registers, so
// a composition configuration can select the module and the assembly can
// construct it from the database product in the by-type context.
func component() pkgcore.Component {
	return pkgcore.Component{
		Name:   moduleName,
		Module: moduleName,
		Requires: []pkgcore.Requirement{
			// The database connection the module's tables live in, the
			// selected db component's product.
			{Token: (*gorm.DB)(nil)},
			// The feature-flag reader org asks about its own flags, taken as
			// org's own structural interface so the dependency never
			// becomes an import edge to the module that serves it.
			// Optional: without one, the flags' declared defaults apply.
			{Token: (*FeatureGate)(nil), Optional: true},
			// The caller-identity resolver org's two caller-scoped endpoints
			// use. Optional: without one those endpoints fail closed with
			// ErrSubjectUnresolved rather than refusing the boot.
			{Token: (*SubjectResolver)(nil), Optional: true},
		},
		// The construction product is the *Module; the Scope it exposes is
		// what authorization consumers are adapted to.
		Provides:      []any{(*Module)(nil)},
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
			if c.MailFrom != "" {
				opts = append(opts, WithMailFrom(c.MailFrom))
			}
			if c.ReplyTo != "" {
				opts = append(opts, WithReplyTo(c.ReplyTo))
			}
			if c.InvitationTTL > 0 {
				opts = append(opts, WithInvitationTTL(c.InvitationTTL))
			}
			if c.MaxDepth != 0 {
				opts = append(opts, WithMaxDepth(c.MaxDepth))
			}
			gate, err := pkgcore.Get[FeatureGate](reg)
			switch {
			case err == nil:
				opts = append(opts, WithFeatureGate(gate))
			case errors.Is(err, pkgcore.ErrMissingRequirement):
				// The optional gate is absent: the flags' declared defaults
				// apply, the shape WithFeatureGate documents.
			default:
				return nil, err
			}
			resolver, err := pkgcore.Get[SubjectResolver](reg)
			switch {
			case err == nil:
				opts = append(opts, WithSubjectResolver(resolver))
			case errors.Is(err, pkgcore.ErrMissingRequirement):
				// The optional resolver is absent: the two caller-scoped
				// endpoints fail closed, the shape WithSubjectResolver
				// documents.
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
