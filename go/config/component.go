package config

// component.go carries config's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contracts it consumes, and the callbacks that
// construct, declare and serve it. Its Init runs the module's one
// declaration entry point, Register, inside the assembly's Init stage -- the
// one stage whose seats accept writes. Its Start takes the schema snapshot
// (docs/internal/29 §5.2): by then every component's Init turn has run, so
// the declaration set the schema folds together is structurally complete,
// and the runtime *Service is built (or completed) and published from
// there. A host that needs the service during Init -- before every
// component has declared -- attaches earlier itself and publishes; the
// Start step then completes that snapshot rather than building a second
// service.
//
// The module's own system purpose moves with it: the descriptor's
// SystemPurposes carries the system-write purpose Register used to register
// itself, and the assembly registers it at the Init stage's entry.

import (
	"context"
	"embed"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/go/config/migrations"
)

// CipherKeyPath is the key path of the AES key config's cipher is built
// from: the derive field "cipher_key" under the component's own "config"
// namespace, so the key resolves at the platform key path it has always
// carried (Pkgcore.BootstrapKeyPurpose embeds the path, and a rename would
// silently rotate every Sensitive value's key). It is also the material
// address a host's wiring reads the key from.
const CipherKeyPath = "config.cipher_key"

// componentConfig is config's configuration schema in the assembly: the
// construction-time knobs NewModule's options carry, and the process-start
// cipher key as a derive field. The component's namespace is "config", so the
// key resolves at CipherKeyPath rather than at the field's bare local path.
// PollInterval is a pointer because an explicit
// zero is meaningful -- it disables the anti-loss poller outright
// (WithPollInterval's documented contract) -- and the omitted key must leave
// the module's own default in place.
type componentConfig struct {
	// CipherKey seals every Sensitive dynamic-configuration value (the
	// configs table stores base64 ciphertext). It is a process-start key
	// rather than a configuration item for the reason the table states
	// structurally: the key that encrypts the configs table cannot live in
	// the configs table. The derive option resolves it through the
	// five-source chain (an explicit flag/environment/file value, the
	// root-key derivation, the declared defaults table).
	CipherKey    []byte         `json:"cipher_key" config:"derive,sensitive,group=config"`
	PollInterval *time.Duration `json:"poll_interval"`
}

// ConfigDocs implements pkgcore.Documented: the operator-facing contract of
// the schema's sensitive key-material field.
func (*componentConfig) ConfigDocs() map[string]pkgcore.FieldDoc {
	return map[string]pkgcore.FieldDoc{
		"cipher_key": {
			Description: "The AES cipher key the config module seals every Sensitive dynamic-configuration value with (the configs table stores base64 ciphertext); the key that encrypts the table cannot live in the table, so it comes from the host's process-start input.",
			Default:     "documented non-secret development default",
		},
	}
}

// component returns config's component descriptor: the value init registers,
// so a composition configuration can select the module and the assembly can
// construct it from the database product in the by-type context. Its Init
// declares through Register and publishes the runtime *Service through
// Attach.
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
		// The construction product is the *Module. The *Service Attach
		// builds from it is the runtime configuration and feature-flag
		// reader consumers take; it is a runtime service published from the
		// Start callback (a host may publish it earlier), so it stays out
		// of Provides -- a service never resolves a token requirement.
		Provides: []any{(*Module)(nil)},
		// config's state is its rows in the deployment's shared database
		// and the shared event bus, so several replicas may run it at once.
		Capabilities: pkgcore.MultiReplicaSafe,
		ConfigSchema: (*componentConfig)(nil),
		// The one system context this module takes -- the system-scope
		// configuration write -- is descriptor data the assembly registers
		// at the Init stage's entry, before any Init callback runs.
		SystemPurposes: []pkgcore.SystemPurpose{SystemPurposeSystemWrite},
		// The "config" namespace keeps the schema's cipher_key field at the
		// platform key path the key has always carried (CipherKeyPath)
		// instead of the field's bare local path.
		ConfigNamespace: "config",
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
			// The cipher is optional: absent, a schema with no Sensitive
			// item is served fine, and one with a Sensitive item is refused
			// at attachment.
			cipher, ok, err := pkgcore.GetOptional[*dbkit.Cipher](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				opts = append(opts, WithCipher(cipher))
			}
			// The resolver is optional: absent, requests read platform
			// defaults, the shape WithResolver documents.
			resolver, ok, err := pkgcore.GetOptional[tenancy.Resolver](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				opts = append(opts, WithResolver(resolver))
			}
			return NewModule(db, opts...), nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("config: component init got a %T instance, want *config.Module", instance)
			}
			return m.Register(reg)
		},
		// Start takes the full-catalog snapshot: every Init callback has
		// run, so the seats hold the complete declaration set. A host that
		// attached earlier (its own Init-stage consumer needed the
		// service) already published the Service and this step completes
		// its schema to the same complete set.
		Start: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("config: component start got a %T instance, want *config.Module", instance)
			}
			svc, attached, err := m.CompleteSnapshot(reg)
			if err != nil {
				return err
			}
			if attached {
				reg.Put(svc)
			}
			return nil
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
