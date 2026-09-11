package authn

// component.go carries authn's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contracts it consumes, and the callbacks that
// prepare, construct and declare it.
//
// Both process-start keys the module declares (bootstrapKeyDecls) come from
// the assembly's own material source. Prepare builds the PII cipher from the
// authn.pii_cipher_key material and registers the serializer there -- the
// registration must precede the connection that parses the module's models,
// which is why it is a Prepare callback and not part of New. New reads the
// authn.blind_index_key material to build the blind indexers its service
// refuses to run without. The module's one declaration entry point, Register,
// runs as the descriptor's Init callback: inside the assembly's Init stage,
// the one stage whose seats accept writes.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/authn/locales"
	"github.com/vislake/speed/go/authn/migrations"
)

// componentConfig is authn's configuration schema in the assembly: the
// construction-time knobs NewModule's options carry that are not key
// material. The PII cipher key and the blind-index key are process-start key
// material and stay in the descriptor's BootstrapKeys (bootstrapKeyDecls),
// never in configuration. The argon2id cost parameters are bootstrap configuration
// rather than runtime items -- a deployment's fixed hashing budget -- which
// is why they live here and not beside authn.password_min_length on the
// runtime configuration seat.
type componentConfig struct {
	RevocationMode  string                `json:"revocation_mode"`
	TrustedProxies  []string              `json:"trusted_proxies"`
	SMSCodeTTL      time.Duration         `json:"sms_code_ttl"`
	Issuer          string                `json:"issuer"`
	AccessTokenTTL  time.Duration         `json:"access_token_ttl"`
	RefreshTokenTTL time.Duration         `json:"refresh_token_ttl"`
	SessionTTL      time.Duration         `json:"session_ttl"`
	Password        *passwordParamsConfig `json:"password"`
}

// passwordParamsConfig is the argon2id cost parameter block: the same five
// values PasswordParams carries, under the assembly's structured
// configuration. A block that leaves any parameter at zero is refused at
// construction rather than accepted into hashing.
type passwordParamsConfig struct {
	Memory      uint32 `json:"memory"`
	Iterations  uint32 `json:"iterations"`
	Parallelism uint8  `json:"parallelism"`
	SaltLength  uint32 `json:"salt_length"`
	KeyLength   uint32 `json:"key_length"`
}

// component returns authn's component descriptor: the value init registers,
// so a composition configuration can select the module and the assembly can
// construct it from the database and signing-key products in the by-type
// context. Its Prepare registers the PII serializer over the declared cipher
// key material, its New reads the declared blind-index key, and its Init
// runs the module's one declaration entry point, Register.
func component() pkgcore.Component {
	return pkgcore.Component{
		Name:   moduleName,
		Module: moduleName,
		Requires: []pkgcore.Requirement{
			// The database connection the module's tables live in, the
			// selected db component's product.
			{Token: (*gorm.DB)(nil)},
			// The signing-key lifecycle the token issuer and verifier take
			// their keys from -- declared as authn's own structural
			// interface, so the dependency never becomes an import edge to
			// the module that implements it.
			{Token: (*KeySource)(nil)},
			// The SMS transport phone-login verification codes are delivered
			// through. Optional: the standalone deployment mode defaults to
			// the console sender, and a distributed deployment without one
			// fails construction closed (ErrMissingDistributedSMSSender)
			// rather than quietly delivering codes nobody reads.
			{Token: (*pkgcore.SMSSender)(nil), Optional: true},
			// The configuration module, whose Handle this descriptor wires
			// as the module's SettingsReader: the seam that makes authn's
			// declared dynamic config items (configItems) effective at
			// runtime. Optional -- a composition without a config module
			// keeps every construction-time value, exactly the behavior
			// before the seam existed -- which is why the read sites all
			// carry documented fallbacks (settings.go).
			{Token: (*config.Module)(nil), Optional: true},
		},
		// The construction product is the *Module. The *Service it exposes
		// is built inside Register -- it needs the event bus and key-value
		// store the registry carries -- so it is a runtime service, not a
		// construction delivery: consumers reach it through the module
		// product's own accessor, and it deliberately stays out of Provides
		// (a service never resolves a token requirement).
		Provides: []any{(*Module)(nil)},
		// authn's state is its rows in the deployment's shared database and
		// the shared event bus, so several replicas may run it at once.
		Capabilities: pkgcore.MultiReplicaSafe,
		// The one system context this module takes -- the sign-in tenant
		// enumeration -- is declared here so the assembly collects it with
		// every other selected component's purposes.
		SystemPurposes: []pkgcore.SystemPurpose{SystemPurposeSignInTenantEnumeration},
		ConfigSchema:   (*componentConfig)(nil),
		BootstrapKeys:  bootstrapKeyDecls,
		Migrations:     migrations.FS,
		Locales:        locales.FS,
		OpenAPISpec:    openAPISpecYAML,
		// Prepare builds and registers the PII serializer: GORM resolves a
		// named serializer while it parses a model's schema, so the
		// registration must land before the connection that parses this
		// module's models opens -- which is the Construct stage, after every
		// Prepare callback. The cipher is built from the material this
		// descriptor declared (bootstrapKeyDecls), never from a second
		// source.
		Prepare: func(_ context.Context, reg *pkgcore.ComponentRegistry) error {
			material, err := pkgcore.BootstrapMaterialOf(reg)
			if err != nil {
				return err
			}
			cipherKey, ok := material.Material(piiCipherKeyPath)
			if !ok {
				return fmt.Errorf("authn: the assembly resolved no material for the declared bootstrap key %q", piiCipherKeyPath)
			}
			cipher, err := dbkit.NewCipher(cipherKey)
			if err != nil {
				return fmt.Errorf("authn: build the PII cipher from %q: %w", piiCipherKeyPath, err)
			}
			return RegisterPIISerializer(cipher)
		},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
			var c componentConfig
			if err := cfg.Decode(&c); err != nil {
				return nil, err
			}
			db, err := pkgcore.Get[*gorm.DB](reg)
			if err != nil {
				return nil, err
			}
			keySource, err := pkgcore.Get[KeySource](reg)
			if err != nil {
				return nil, err
			}
			opts := []Option{WithKeySource(keySource)}
			// The blind-index key is the module's other declared key
			// material: newOptions refuses a keyless module, so the
			// descriptor reads it from the assembly's material source --
			// the same source its Prepare built the PII cipher from.
			material, err := pkgcore.BootstrapMaterialOf(reg)
			if err != nil {
				return nil, err
			}
			blindIndexKey, ok := material.Material(blindIndexKeyPath)
			if !ok {
				return nil, fmt.Errorf("authn: the assembly resolved no material for the declared bootstrap key %q", blindIndexKeyPath)
			}
			opts = append(opts, WithBlindIndexKey(blindIndexKey))
			switch c.RevocationMode {
			case "":
			case string(RevocationModeNatural):
				opts = append(opts, WithRevocationMode(RevocationModeNatural))
			case string(RevocationModeImmediate):
				opts = append(opts, WithRevocationMode(RevocationModeImmediate))
			default:
				return nil, fmt.Errorf("authn: unknown revocation_mode %q; declared modes are %q and %q", c.RevocationMode, RevocationModeNatural, RevocationModeImmediate)
			}
			if len(c.TrustedProxies) > 0 {
				opts = append(opts, WithTrustedProxies(c.TrustedProxies...))
			}
			if c.SMSCodeTTL > 0 {
				opts = append(opts, WithSMSCodeTTL(c.SMSCodeTTL))
			}
			if c.Issuer != "" {
				opts = append(opts, WithIssuer(c.Issuer))
			}
			if c.AccessTokenTTL > 0 {
				opts = append(opts, WithAccessTokenTTL(c.AccessTokenTTL))
			}
			if c.RefreshTokenTTL > 0 {
				opts = append(opts, WithRefreshTokenTTL(c.RefreshTokenTTL))
			}
			if c.SessionTTL > 0 {
				opts = append(opts, WithSessionTTL(c.SessionTTL))
			}
			if c.Password != nil {
				params := PasswordParams{
					Memory:      c.Password.Memory,
					Iterations:  c.Password.Iterations,
					Parallelism: c.Password.Parallelism,
					SaltLength:  c.Password.SaltLength,
					KeyLength:   c.Password.KeyLength,
				}
				if params.Memory == 0 || params.Iterations == 0 || params.Parallelism == 0 || params.SaltLength == 0 || params.KeyLength == 0 {
					return nil, errors.New("authn: the password block must set every argon2id cost parameter to a non-zero value")
				}
				opts = append(opts, WithPasswordParams(params))
			}
			sender, ok, err := pkgcore.GetOptional[pkgcore.SMSSender](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				opts = append(opts, WithSMSSender(sender))
			}
			// The dynamic-configuration reader: the config module's lazy
			// handle satisfies authn.SettingsReader structurally (the
			// compile-time assertion in settings.go), and the handle exists
			// from the config module's own construction, so wiring it here
			// -- long before either module's reads ever run -- is a plain
			// value capture, not a resolution.
			cfgModule, ok, err := pkgcore.GetOptional[*config.Module](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				opts = append(opts, WithSettingsReader(cfgModule.Handle()))
			}
			return NewModule(db, opts...)
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("authn: component init got a %T instance, want *authn.Module", instance)
			}
			return m.Register(reg)
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
