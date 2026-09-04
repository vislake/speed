package kmsaws

// Self-registration for this package's two built-in implementations,
// mirroring go/pki/signer/vault's own register.go (itself mirroring
// go/pkgcore's split subpackages' database/sql-style driver-registration
// pattern): importing this package -- for side effect alone -- registers
// "signer.aws-kms" and "signer.aws-kms-direct" on pki.SignerRegistry.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki"
)

func init() {
	mustRegister(pkgcore.Registration[pki.Signer]{
		Name:         "signer.aws-kms",
		Capabilities: 0,
		New:          envelopeSignerFromConfig,
	})
	mustRegister(pkgcore.Registration[pki.Signer]{
		Name:         "signer.aws-kms-direct",
		Capabilities: pkgcore.KeyNeverLeavesBoundary,
		New:          directSignerFromConfig,
	})
}

// mustRegister adds r to pki.SignerRegistry and panics if that fails. See
// go/pki/signer/vault/register.go's identical helper for why this package
// carries its own copy rather than sharing one.
func mustRegister(r pkgcore.Registration[pki.Signer]) {
	if err := pki.SignerRegistry.Register(r); err != nil {
		panic(fmt.Sprintf("pki/signer/kmsaws: builtin implementation registration failed: %v", err))
	}
}

// envelopeSignerFromConfig adapts pkgcore.Config onto NewSigner with Mode
// forced to ModeEnvelope -- see go/pki/signer/vault's identical function
// for why mode is fixed by the registered name, not read from cfg.
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
			"pki/signer/kmsaws: builtin signer.aws-kms seam: %w: requires \"region\", \"access_key_id\" and \"secret_access_key\"",
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
