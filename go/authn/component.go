package authn

// component.go carries authn's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contracts it consumes, and the callback that
// constructs it. The descriptor is additive: pkgcore.Module.Register, driven
// by the host's bootstrap, remains authn's declaration path, and the
// descriptor states the same surface in the assembly's terms.
//
// The descriptor declares no Prepare callback, and New cannot complete. Both
// are the same missing piece: the process-start key material authn declares
// as this descriptor's BootstrapKeys (bootstrapKeyDecls). Prepare would build the PII
// cipher from the authn.pii_cipher_key material and register the serializer;
// New needs the blind-index key, which newOptions refuses to construct
// without. The by-purpose material source that hands a component its own
// declared material is not part of the assembly yet, and building either
// value from anything else here would state a different contract than the
// declaration does. The host wiring (authn.RegisterPIISerializer and
// authn.WithBlindIndexKey over the material it resolves) is the path that
// supplies both today.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

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
// context.
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
		},
		// The construction product is the *Module; the *Service it exposes
		// is what host steps assemble the request chain from.
		Provides: []any{(*Module)(nil), (*Service)(nil)},
		// The one system context this module takes -- the sign-in tenant
		// enumeration -- is declared here so the assembly collects it with
		// every other selected component's purposes.
		SystemPurposes: []pkgcore.SystemPurpose{SystemPurposeSignInTenantEnumeration},
		ConfigSchema:   (*componentConfig)(nil),
		BootstrapKeys:  bootstrapKeyDecls,
		Migrations:     migrations.FS,
		Locales:        locales.FS,
		OpenAPISpec:    openAPISpecYAML,
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
			if sender, err := pkgcore.Get[pkgcore.SMSSender](reg); err == nil {
				opts = append(opts, WithSMSSender(sender))
			} else if !errors.Is(err, pkgcore.ErrMissingRequirement) {
				return nil, err
			}
			// The blind-index key is the one construction input this
			// descriptor cannot obtain: it is process-start key material
			// (bootstrapKeyDecls), and nothing in the assembly publishes key
			// material yet. The module's own construction refuses the
			// keyless wiring (newOptions), so the refusal below is the
			// deferral made loud rather than a silently keyless module; the
			// host wiring (authn.WithBlindIndexKey over its own
			// configuration) is the path that supplies the key today.
			return NewModule(db, opts...)
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
