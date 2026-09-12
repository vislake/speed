package kmsaws

// component.go registers the "signer.aws-kms" and "signer.aws-kms-direct"
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

// signerComponentConfig is the configuration schema both kmsaws signer
// components share: one field per key the seam registration documents, so a
// composition block spells the same settings a flat pkgcore.Config carries
// and neither face grows a setting the other lacks.
type signerComponentConfig struct {
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	WrappingKeyID   string `json:"wrapping_key_id"`
}

// signerAWSKMSComponent is the component descriptor for "signer.aws-kms":
// the envelope-mode AWS KMS signer over the configuration the component's
// own block spells out, declaring the same 0 capabilities the seam
// registration declares -- envelope mode decrypts the real key into this
// process's memory to sign, so it does not satisfy
// pkgcore.KeyNeverLeavesBoundary. Its New funnels through
// envelopeSignerFromConfig -- the package's one construction path for this
// name -- so the component face and the seam face cannot diverge on the
// forced mode, the required-field checks or the coded config refusal. The
// component requires no database: NewSigner builds a KMS client that issues
// no request until its first operation, and the block carries the region
// and credentials itself.
var signerAWSKMSComponent = pkgcore.Component{
	Name:         "signer.aws-kms",
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
			"region":            c.Region,
			"access_key_id":     c.AccessKeyID,
			"secret_access_key": c.SecretAccessKey,
			"session_token":     c.SessionToken,
			"wrapping_key_id":   c.WrappingKeyID,
		})
	},
}

// signerAWSKMSDirectComponent is the component descriptor for
// "signer.aws-kms-direct": the direct-sign-mode AWS KMS signer, mirroring
// signerAWSKMSComponent's shape with the other forced mode and the matching
// capability declaration -- the key is generated inside, and never leaves,
// the AWS KMS service, so this component satisfies
// pkgcore.KeyNeverLeavesBoundary exactly as its registration declares.
var signerAWSKMSDirectComponent = pkgcore.Component{
	Name:         "signer.aws-kms-direct",
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
			"region":            c.Region,
			"access_key_id":     c.AccessKeyID,
			"secret_access_key": c.SecretAccessKey,
			"session_token":     c.SessionToken,
			"wrapping_key_id":   c.WrappingKeyID,
		})
	},
}

func init() {
	pkgcore.MustRegister(signerAWSKMSComponent)
	pkgcore.MustRegister(signerAWSKMSDirectComponent)
}
