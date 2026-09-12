package vault

// component.go registers the "signer.vault" and "signer.vault-direct"
// components with pkgcore's global component registration: the descriptors
// a composition configuration selects as the "signer" module's members.
// They live beside the implementation they adapt, the same file-locality
// the package's own seam registration (register.go) keeps. Each component
// carries the identical name as its seam registration -- the two faces are
// two resolution paths to the same implementation, and the registration
// stays the name-based path a Preset-shaped caller resolves through.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki"
)

// signerComponentConfig is the configuration schema both vault signer
// components share: one field per key the seam registration documents, so a
// composition block spells the same settings a flat pkgcore.Config carries
// and neither face grows a setting the other lacks.
type signerComponentConfig struct {
	Address         string `json:"address"`
	Token           string `json:"token"`
	Namespace       string `json:"namespace"`
	MountPath       string `json:"mount_path"`
	WrappingKeyName string `json:"wrapping_key_name"`
}

// signerVaultComponent is the component descriptor for "signer.vault": the
// envelope-mode Vault Transit signer over the configuration the component's
// own block spells out, declaring the same 0 capabilities the seam
// registration declares -- envelope mode decrypts the real key into this
// process's memory to sign, so it does not satisfy
// pkgcore.KeyNeverLeavesBoundary. Its New funnels through
// envelopeSignerFromConfig -- the package's one construction path for this
// name -- so the component face and the seam face cannot diverge on the
// forced mode, the required-field checks or the coded config refusal. The
// component requires no database: NewSigner builds a Vault client that
// dials lazily, and the block carries the endpoint and token itself.
var signerVaultComponent = pkgcore.Component{
	Name:         "signer.vault",
	Module:       "signer",
	Provides:     []any{(*pki.Signer)(nil)},
	Capabilities: 0,
	ConfigSchema: (*signerComponentConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c signerComponentConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		return envelopeSignerFromConfig(pkgcore.Config{
			"address":           c.Address,
			"token":             c.Token,
			"namespace":         c.Namespace,
			"mount_path":        c.MountPath,
			"wrapping_key_name": c.WrappingKeyName,
		})
	},
}

// signerVaultDirectComponent is the component descriptor for
// "signer.vault-direct": the direct-sign-mode Vault signer, mirroring
// signerVaultComponent's shape with the other forced mode and the matching
// capability declaration -- the key is generated inside, and never leaves,
// the Vault service, so this component satisfies
// pkgcore.KeyNeverLeavesBoundary exactly as its registration declares.
var signerVaultDirectComponent = pkgcore.Component{
	Name:         "signer.vault-direct",
	Module:       "signer",
	Provides:     []any{(*pki.Signer)(nil)},
	Capabilities: pkgcore.KeyNeverLeavesBoundary,
	ConfigSchema: (*signerComponentConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c signerComponentConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		return directSignerFromConfig(pkgcore.Config{
			"address":           c.Address,
			"token":             c.Token,
			"namespace":         c.Namespace,
			"mount_path":        c.MountPath,
			"wrapping_key_name": c.WrappingKeyName,
		})
	},
}

func init() {
	pkgcore.MustRegister(signerVaultComponent)
	pkgcore.MustRegister(signerVaultDirectComponent)
}
