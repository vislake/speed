package org

// component.go carries org's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contracts it consumes, and the callback that
// constructs it. Its Init runs the module's one declaration entry point,
// Register, inside the assembly's Init stage -- the one stage whose seats
// accept writes -- so the module's declarations reach the assembly's seats
// exactly as they reach the kernel bootstrap's registry.
//
// The descriptor declares no Prepare callback. RegisterEmailSerializer --
// the registration GORM resolves the invitation address column through --
// consumes the host's configuration cipher and must run before the
// connection that parses the module's models opens; the host wiring
// (org.RegisterEmailSerializer over the cipher it resolves) is the path that
// performs it. New builds the blind indexer from the module's own declared
// key material (bootstrapKeyDecl), which is what Register's
// ErrEmailIndexerRequired precondition needs: the module reads the material
// it declared, the same reading authn's blind-index key takes.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/org/locales"
	"github.com/vislake/speed/go/org/migrations"
)

// componentConfig is org's configuration schema in the assembly: the
// construction-time knobs NewModule's options carry, as structured
// configuration. The invitation-address blind-index key is process-start key
// material and stays in the descriptor's BootstrapKeys (bootstrapKeyDecl),
// never in configuration.
type componentConfig struct {
	MailFrom      string        `json:"mail_from"`
	ReplyTo       string        `json:"reply_to"`
	InvitationTTL time.Duration `json:"invitation_ttl"`
	MaxDepth      int           `json:"max_depth"`
}

// component returns org's component descriptor: the value init registers, so
// a composition configuration can select the module and the assembly can
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
			// The feature-flag reader org asks about its own flags, taken as
			// org's own structural interface so the dependency never
			// becomes an import edge to the module that serves it.
			// Optional: without one, the flags' declared defaults apply.
			{Token: (*FeatureGate)(nil), Optional: true},
			// The caller-identity resolver org's two caller-scoped endpoints
			// use. Optional: without one those endpoints fail closed with
			// ErrSubjectUnresolved rather than refusing the boot.
			{Token: (*SubjectResolver)(nil), Optional: true},
			// The invitation-link builder: turning a token into the URL the
			// invitee clicks is host policy (the host's public address, per
			// tenant), so it arrives as a value in the by-type context, the
			// same shape FeatureGate and SubjectResolver take. Optional:
			// without one the module's own email-invitation requirement
			// stands (Register refuses with ErrInvitationMailRequired), and
			// a composition that delivers invitations elsewhere disables
			// the module's own leg instead.
			{Token: (*InvitationLinkBuilder)(nil), Optional: true},
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
			// The blind indexer is built from the module's own declared key
			// material: Register refuses a keyless module
			// (ErrEmailIndexerRequired), and the material source the loader
			// publishes is where the declared key path resolves.
			material, err := pkgcore.BootstrapMaterialOf(reg)
			if err != nil {
				return nil, err
			}
			indexKey, ok := material.Material(bootstrapKeyDecl.Key)
			if !ok {
				return nil, fmt.Errorf("org: the assembly resolved no material for the declared bootstrap key %q", bootstrapKeyDecl.Key)
			}
			indexer, err := NewEmailIndexer(indexKey)
			if err != nil {
				return nil, err
			}
			var opts []Option
			opts = append(opts, WithEmailIndexer(indexer))
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
			builder, err := pkgcore.Get[InvitationLinkBuilder](reg)
			switch {
			case err == nil:
				opts = append(opts, WithInvitationLinkBuilder(builder))
			case errors.Is(err, pkgcore.ErrMissingRequirement):
				// The optional builder is absent: the module's own
				// email-invitation requirement stands, and Register refuses
				// the email-enabled default unless the composition disabled
				// it.
			default:
				return nil, err
			}
			return NewModule(db, opts...), nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("org: component init got a %T instance, want *org.Module", instance)
			}
			return m.Register(reg)
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
