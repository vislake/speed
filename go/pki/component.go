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
		Capabilities:  pkgcore.MultiReplicaSafe,
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
// register their own names from their subpackages; the seam registration of
// the same name (signer_registry.go) stays the name-based path for a
// Preset-shaped caller.
//
// Requires the database (its *gorm.DB product) and builds the signer over
// that shared connection -- not the second connection the flat seam adapter
// must open for itself, because a flat Config cannot carry a *gorm.DB.
// Provides (*Signer)(nil), the contract a consumer's token resolves through.
// Capabilities are deliberately 0, the same non-declaration the seam
// registration records: LocalSigner decrypts the private key into this
// process's memory for the duration of a signing call, so it does not
// declare KeyNeverLeavesBoundary. Like every LocalSigner caller, this
// component expects LocalKeySerializerName to be registered (once, at
// bootstrap, before any connection using the schema opens) against the
// cipher the host injected; the pki component's own host wiring is where
// that registration goes.
var signerLocalComponent = pkgcore.Component{
	Name:         "signer.local",
	Module:       "signer",
	Provides:     []any{(*Signer)(nil)},
	Capabilities: 0,
	ConfigSchema: nil, // the shared connection replaces the adapter's dialect/dsn pair
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
