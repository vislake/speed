package org

// component.go carries org's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contracts it consumes, and the callback that
// constructs it. Its Init runs the module's one declaration entry point,
// Register, inside the assembly's Init stage -- the one stage whose seats
// accept writes -- so the module's declarations reach the assembly's seats
// exactly as they reach the assembly's registry.
//
// The descriptor declares no Prepare callback. RegisterEmailSerializer --
// the registration GORM resolves the invitation address column through --
// consumes the host's configuration cipher and must run before the
// connection that parses the module's models opens; the host wiring
// (org.RegisterEmailSerializer over the cipher it resolves) is the path that
// performs it. New builds the blind indexer from the module's own declared
// key material (the schema's invitation_email_index_key field), which is
// what Register's ErrEmailIndexerRequired precondition needs: the module
// reads the material it declared, the same reading authn's blind-index key
// takes.

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/org/locales"
	"github.com/vislake/speed/go/org/migrations"
)

// InvitationEmailIndexKeyPath is the key path of the HMAC key org's
// invitation-address blind indexer is built from: the derive field
// "invitation_email_index_key" under the component's own "org" namespace, so
// the key resolves at the platform key path it has always carried
// (pkgcore.BootstrapKeyPurpose embeds the path, and a rename would silently
// rotate the key). It is also the material address a host's wiring reads the
// key from.
const InvitationEmailIndexKeyPath = "org.invitation_email_index_key"

// componentConfig is org's configuration schema in the assembly: the
// construction-time knobs NewModule's options carry, as structured
// configuration, and the process-start blind-index key as a derive field.
// The component's namespace is "org", so the key resolves at
// InvitationEmailIndexKeyPath rather than at the field's bare local path.
type componentConfig struct {
	// InvitationEmailIndexKey indexes invitation email addresses; it is a
	// separate secret from the host's configuration cipher key on purpose.
	// A host reusing that cipher to encrypt org's Invitation.Email column is
	// the ordinary wiring, and dbkit's own rule is that an AES key must
	// never double as an HMAC key; this one additional key is what keeps
	// that rule real rather than aspirational, and an invitation whose
	// address cannot be indexed can never be found again, so the key must
	// not change between restarts. The derive option resolves it through
	// the five-source chain.
	InvitationEmailIndexKey []byte        `json:"invitation_email_index_key" config:"derive,sensitive,group=org"`
	MailFrom                string        `json:"mail_from"`
	ReplyTo                 string        `json:"reply_to"`
	InvitationTTL           time.Duration `json:"invitation_ttl"`
	MaxDepth                int           `json:"max_depth"`
}

// ConfigDocs implements pkgcore.Documented: the operator-facing contract of
// the schema's sensitive key-material field.
func (*componentConfig) ConfigDocs() map[string]pkgcore.FieldDoc {
	return map[string]pkgcore.FieldDoc{
		"invitation_email_index_key": {
			Description: "HMAC key org's blind indexer indexes invitation email addresses with; separate from every cipher key, because an AES key never doubles as an HMAC key.",
			Default:     "documented non-secret development default",
		},
	}
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
		Provides: []any{(*Module)(nil)},
		// org's state is its rows in the deployment's shared database and
		// the shared event bus, so several replicas may run it at once.
		Capabilities: pkgcore.MultiReplicaSafe,
		ConfigSchema: (*componentConfig)(nil),
		// The "org" namespace keeps the schema's invitation_email_index_key
		// field at the platform key path the key has always carried
		// (InvitationEmailIndexKeyPath) instead of the field's bare local
		// path.
		ConfigNamespace: "org",
		Migrations:      migrations.FS,
		Locales:         locales.FS,
		OpenAPISpec:     openAPISpecYAML,
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
			// publishes is where the schema's derive field resolves.
			material, err := pkgcore.BootstrapMaterialOf(reg)
			if err != nil {
				return nil, err
			}
			indexKey, ok := material.Material(InvitationEmailIndexKeyPath)
			if !ok {
				return nil, fmt.Errorf("org: the assembly resolved no material for the declared bootstrap key %q", InvitationEmailIndexKeyPath)
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
			// The gate is optional: absent, the flags' declared defaults
			// apply, the shape WithFeatureGate documents.
			gate, ok, err := pkgcore.GetOptional[FeatureGate](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				opts = append(opts, WithFeatureGate(gate))
			}
			// The resolver is optional: absent, the two caller-scoped
			// endpoints fail closed, the shape WithSubjectResolver
			// documents.
			resolver, ok, err := pkgcore.GetOptional[SubjectResolver](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				opts = append(opts, WithSubjectResolver(resolver))
			}
			// The builder is optional: absent, the module's own
			// email-invitation requirement stands, and Register refuses
			// the email-enabled default unless the composition disabled
			// it.
			builder, ok, err := pkgcore.GetOptional[InvitationLinkBuilder](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				opts = append(opts, WithInvitationLinkBuilder(builder))
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
