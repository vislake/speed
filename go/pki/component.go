package pki

// component.go carries pki's descriptors for the config-driven component
// assembly: the pki module's selection key, assets, consumed contracts and
// lifecycle callbacks, and the "signer.local" implementation the signer
// module binds. The pki descriptor's Init runs the module's one declaration
// entry point, Register, inside the assembly's Init stage -- the one stage
// whose seats accept writes -- so the module's declarations reach the
// assembly's seats exactly as they reach the kernel bootstrap's registry.
//
// The pki descriptor declares no Prepare callback.
// RegisterLocalKeySerializer -- the step that registers the cipher GORM
// resolves the LocalSigner's private-key column through -- consumes the
// pki.local_key_cipher_key material and must run before the connection that
// parses the LocalSigner's model opens; the host wiring
// (pki.RegisterLocalKeySerializer over the material it resolves) is the path
// that performs it.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki/locales"
	"github.com/vislake/speed/go/pki/migrations"
)

// signerModuleName is the module name every signer implementation component
// carries (the "signer" in "signer.local", "signer.vault",
// "signer.aws-kms"): the member directory the pki module's own descriptor
// resolves its selected signer through, and the family segment a member
// name's module-facing signer name is read out of
// (signerNameFromMember).
const signerModuleName = "signer"

// signerNameFromMember derives the module-facing signer name from a selected
// signer member's component name: the name's last dot-separated segment.
// A host that clones the registered descriptor under its own name -- the
// capabilityComponent shape, "reference-app.signer.local" or
// "__APP_NAME__.signer.local" -- contributes only a prefix to the name, so
// the identity every key row records is the segment after the final dot:
// "signer.local" and every "<host>.signer.local" clone all read "local", the
// name LocalSigner's rows have always carried, while a provider member keeps
// its own identity ("signer.aws-kms" -> "aws-kms", "signer.vault-direct" ->
// "vault-direct"). A name carrying no dot reads as itself -- the degenerate
// case, since every registered signer name spells the family segment first.
func signerNameFromMember(member string) string {
	if idx := strings.LastIndex(member, "."); idx >= 0 {
		return member[idx+1:]
	}
	return member
}

// LocalKeyCipherKeyPath is the key path of the AES key sealing
// pki_local_keys' private-key column: the derive field
// "local_key_cipher_key" under the component's own "pki" namespace, so the
// key resolves at the platform key path it has always carried
// (pkgcore.BootstrapKeyPurpose embeds the path, and a rename would silently
// rotate the key). It is also the material address a host's wiring reads the
// key from.
const LocalKeyCipherKeyPath = "pki.local_key_cipher_key"

// componentConfig is pki's configuration schema in the assembly: the
// construction-time knobs NewModule's options carry, as structured
// configuration, and the process-start local-key cipher as a derive field.
// The component's namespace is "pki", so the key resolves at
// LocalKeyCipherKeyPath rather than under the default components.pki.
// prefix. Every other field is a pointer, so an omitted key leaves the
// module's own default in place while a present key is applied verbatim --
// including an explicit zero, which WithCacheTTL documents as disabling the
// key-set cache outright.
type componentConfig struct {
	// LocalKeyCipherKey seals the LocalSigner private-key column
	// (pki_local_keys, via RegisterLocalKeySerializer). It is a separate
	// secret from every other module's key material, because dbkit's
	// key-separation rule applies across modules and not only within one,
	// and the keys it seals are the ones authn's access tokens are
	// ultimately signed with, so a host that leaves it at the development
	// default ships with signing keys sealed under a key committed to this
	// repository's own source. The derive option resolves it through the
	// five-source chain (an explicit flag/environment/file value, the
	// root-key derivation, the declared defaults table).
	LocalKeyCipherKey []byte         `json:"local_key_cipher_key" config:"derive,sensitive,group=pki"`
	PropagationWindow *time.Duration `json:"propagation_window"`
	RenewalLeadTime   *time.Duration `json:"renewal_lead_time"`
	ExpiryScanWindow  *time.Duration `json:"expiry_scan_window"`
	CacheTTL          *time.Duration `json:"cache_ttl"`
}

// ConfigDocs implements pkgcore.Documented: the operator-facing contract of
// the schema's sensitive key-material field.
func (*componentConfig) ConfigDocs() map[string]pkgcore.FieldDoc {
	return map[string]pkgcore.FieldDoc{
		"local_key_cipher_key": {
			Description: "AES key sealing go/pki's LocalSigner private-key column, the key authn's access tokens are ultimately signed with; separate from every other key, since dbkit's key-separation rule spans modules, not only one.",
			Default:     "documented non-secret development default",
		},
	}
}

// component returns pki's component descriptor: the value init registers, so
// a composition configuration can select the module, the assembly can
// construct it from the database component's product, and Close releases the
// key-set cache's janitor. Its Init runs the module's one declaration entry
// point, Register, inside the assembly's Init stage -- the one stage whose
// seats accept writes.
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
			// The configuration module, whose Handle this descriptor wires
			// as the module's SettingsReader: the seam that makes pki's
			// declared dynamic config items (the CA/certificate validity
			// bounds, the CRL distribution point default, the CRL validity
			// period and the two lifecycle rotation settings) effective at
			// runtime. Optional -- a composition without a config module
			// keeps every construction-time or package value, which is why
			// the read sites all carry documented fallbacks (settings.go).
			{Token: (*config.Module)(nil), Optional: true},
			// The signer member the composition selected -- the binding
			// module's one-member selection, resolved through the
			// component-name path: when exactly one "signer.*" component is
			// in the selected set, that member is this module's signer and
			// the requirement puts its construction ahead of this one. An
			// optional requirement with no selected member is satisfied by
			// nothing, and the module keeps its own LocalSigner default;
			// two selected members fail the plan as an ambiguous provider
			// before construction, the binding contract enforced by
			// selection rather than by luck of ordering.
			{Token: (*Signer)(nil), Optional: true},
		},
		// The construction deliveries are the *Module plus the *Service
		// baked into it -- the signing-key lifecycle authn's own
		// structurally declared KeySource token resolves against; both are
		// constructed in NewModule, so New puts the Service alongside its
		// own returned product and the declaration delivers in full.
		Provides: []any{(*Module)(nil), (*Service)(nil)},
		// pki's state is its platform rows in the deployment's shared
		// database and the shared event bus, so several replicas may run it
		// at once.
		Capabilities: pkgcore.MultiReplicaSafe,
		ConfigSchema: (*componentConfig)(nil),
		// The "pki" namespace keeps the schema's local_key_cipher_key field
		// at the platform key path the key has always carried
		// (LocalKeyCipherKeyPath) instead of the default components.pki.
		// prefix.
		ConfigNamespace: "pki",
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
			// The signer member selected alongside this module is the
			// module's signer: the composition's component-name selection
			// decides which implementation signs, and the member name's
			// last segment (signerNameFromMember) is the signer name every
			// key row records, so "signer.local" -- and a host's renamed
			// clone of it, "reference-app.signer.local" -- reads "local",
			// the name LocalSigner's rows have always carried, while a
			// provider member keeps its own identity ("vault", "aws-kms",
			// "vault-direct"). The Requires declaration above orders the
			// member's construction ahead of this one, so its product is
			// already in the by-type context here. A composition that
			// selects no signer member keeps the module's own LocalSigner
			// default over the shared connection, constructed below.
			if members := pkgcore.MemberNames(reg, signerModuleName); len(members) == 1 {
				signer, signerErr := pkgcore.Get[Signer](reg)
				if signerErr != nil {
					return nil, signerErr
				}
				opts = append(opts, WithSigner(signerNameFromMember(members[0]), signer))
			}
			// The queue is optional: absent, the module runs without
			// automatic rotation, and register claims neither task
			// handler nor schedule.
			queue, ok, err := pkgcore.GetOptional[jobs.Queue](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				opts = append(opts, WithQueue(queue))
			}
			// The dynamic-configuration reader: the config module's lazy
			// handle satisfies pki.SettingsReader structurally (the
			// compile-time assertion in settings.go), and the handle
			// exists from the config module's own construction, so wiring
			// it here -- long before either module's reads ever run -- is
			// a plain value capture, not a resolution.
			cfgModule, ok, err := pkgcore.GetOptional[*config.Module](reg)
			if err != nil {
				return nil, err
			}
			if ok {
				opts = append(opts, WithSettingsReader(cfgModule.Handle()))
			}
			m := NewModule(db, opts...)
			// The Service is a construction value (NewModule builds it
			// eagerly), so it is one of this component's construction-time
			// deliveries: put it so the declared token resolves for every
			// consumer, exactly as the Module product does.
			reg.Put(m.Service())
			return m, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("pki: component init got a %T instance, want *pki.Module", instance)
			}
			return m.Register(reg)
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

// signerLocalComponent is the component descriptor for "signer.local": the
// ed25519 LocalSigner the standalone deployment mode runs and every test
// can use as a double. signer is a binding module -- exactly one
// implementation is selected -- and this component is the
// zero-external-dependency one, beside the vault and kmsaws providers that
// register their own component names from their subpackages.
//
// Requires the database (its *gorm.DB product) and builds the signer over
// that shared connection, so the signer's key rows live in the same
// database as the module's every other table.
// Provides (*Signer)(nil), the contract a consumer's token resolves through.
// Capabilities are deliberately 0: LocalSigner decrypts the private key
// into this process's memory for the duration of a signing call, so it does
// not declare KeyNeverLeavesBoundary. Like every LocalSigner caller, this
// component expects LocalKeySerializerName to be registered (once, at
// bootstrap, before any connection using the schema opens) against the
// cipher the host injected; the pki component's own host wiring is where
// that registration goes.
var signerLocalComponent = pkgcore.Component{
	Name:         "signer.local",
	Module:       signerModuleName,
	Provides:     []any{(*Signer)(nil)},
	Capabilities: 0,
	ConfigSchema: nil, // no per-component settings: the shared connection is the construction input
	Requires:     []pkgcore.Requirement{{Token: (*gorm.DB)(nil)}},
	New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		return NewLocalSigner(db), nil
	},
}

func init() {
	pkgcore.MustRegister(component())
	pkgcore.MustRegister(signerLocalComponent)
}
