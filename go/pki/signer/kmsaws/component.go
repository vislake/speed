package kmsaws

// component.go registers the "signer.aws-kms" and "signer.aws-kms-direct"
// components with pkgcore's global component registration: the descriptors
// a composition configuration selects as the "signer" module's members.
// They live beside the implementation they adapt, and each descriptor's
// New funnels through the matching adapter below -- this package's one
// construction path per name -- so a composition block and a flat
// pkgcore.Config cannot diverge on validation or on the forced mode.

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki"
)

// signerComponentConfig is the configuration schema both kmsaws signer
// components share: one field per key the flat construction path reads, so a
// composition block spells the same settings a flat pkgcore.Config carries
// and neither shape grows a setting the other lacks.
type signerComponentConfig struct {
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	WrappingKeyID   string `json:"wrapping_key_id"`
}

// signerAWSKMSComponent is the component descriptor for "signer.aws-kms":
// the envelope-mode AWS KMS signer over the configuration the component's
// own block spells out, declaring 0 capabilities -- envelope mode decrypts
// the real key into this process's memory to sign, so it does not satisfy
// pkgcore.KeyNeverLeavesBoundary. Its New funnels through
// envelopeSignerFromConfig -- the package's one construction path for this
// name -- so a composition block and a flat pkgcore.Config cannot diverge
// on the forced mode, the required-field checks or the coded config
// refusal. The
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
// the AWS KMS service, so this component declares
// pkgcore.KeyNeverLeavesBoundary.
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

// envelopeSignerFromConfig adapts pkgcore.Config onto NewSigner with Mode
// forced to ModeEnvelope -- see go/pki/signer/vault's identical function
// for why mode is fixed by the component name, not read from cfg.
func envelopeSignerFromConfig(cfg pkgcore.Config) (pki.Signer, error) {
	c, err := configFromFlat(cfg)
	if err != nil {
		return nil, err
	}
	c.Mode = ModeEnvelope
	return NewSigner(c)
}

// directSignerFromConfig mirrors envelopeSignerFromConfig for
// "signer.aws-kms-direct".
func directSignerFromConfig(cfg pkgcore.Config) (pki.Signer, error) {
	c, err := configFromFlat(cfg)
	if err != nil {
		return nil, err
	}
	c.Mode = ModeDirectSign
	return NewSigner(c)
}

// configFromFlat adapts a flat pkgcore.Config onto Config. "region",
// "access_key_id" and "secret_access_key" have no safe default, so a
// Config missing any of them is rejected with pkgcore.ErrMissingSeamConfig
// before NewSigner is even called -- the same early-check convention
// go/pkgcore's own smtpMailerFromConfig and objectstore/s3's
// objectStoreFromConfig use. "wrapping_key_id" is NOT checked here even
// though ModeEnvelope requires it -- NewSigner already validates that.
func configFromFlat(cfg pkgcore.Config) (Config, error) {
	region := cfg["region"]
	accessKeyID := cfg["access_key_id"]
	secretAccessKey := cfg["secret_access_key"]
	if region == "" || accessKeyID == "" || secretAccessKey == "" {
		return Config{}, fmt.Errorf(
			"pki/signer/kmsaws: builtin signer.aws-kms component: %w: requires \"region\", \"access_key_id\" and \"secret_access_key\"",
			pkgcore.ErrMissingSeamConfig,
		)
	}
	return Config{
		Region:          region,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		SessionToken:    cfg["session_token"],
		WrappingKeyID:   cfg["wrapping_key_id"],
	}, nil
}

func init() {
	pkgcore.MustRegister(signerAWSKMSComponent)
	pkgcore.MustRegister(signerAWSKMSDirectComponent)
}
