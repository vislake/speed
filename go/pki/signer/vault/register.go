package vault

// Self-registration for this package's two built-in implementations,
// mirroring the database/sql driver-registration pattern go/pkgcore's own
// split subpackages (eventbus/redis, kv/redis, objectstore/s3) already
// established: importing this package -- for side effect alone, if the
// host calls nothing else in it -- registers "signer.vault" and
// "signer.vault-direct" on pki.SignerRegistry. See config.go's doc comment
// for what the two names differ in, and doc.go for why two names rather
// than one.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki"
)

func init() {
	mustRegister(pkgcore.Registration[pki.Signer]{
		Name:         "signer.vault",
		Capabilities: 0,
		New:          envelopeSignerFromConfig,
	})
	mustRegister(pkgcore.Registration[pki.Signer]{
		Name:         "signer.vault-direct",
		Capabilities: pkgcore.KeyNeverLeavesBoundary,
		New:          directSignerFromConfig,
	})
}

// mustRegister adds r to pki.SignerRegistry and panics if that fails. It is
// only ever called here, against the two names this file controls, so a
// failure -- a duplicate name -- is a programming error in this file, not a
// condition a caller could hit or would want to recover from. This mirrors
// go/pkgcore/objectstore/s3's own unexported mustRegister of the identical
// shape; that one cannot be called from here (unexported to its own
// package), so this package carries its own copy rather than inventing a
// different convention.
func mustRegister(r pkgcore.Registration[pki.Signer]) {
	if err := pki.SignerRegistry.Register(r); err != nil {
		panic(fmt.Sprintf("pki/signer/vault: builtin implementation registration failed: %v", err))
	}
}

// envelopeSignerFromConfig adapts pkgcore.Config onto NewSigner with
// Mode forced to ModeEnvelope, regardless of what a caller puts in cfg --
// the Mode is what the REGISTERED NAME already promised (Capabilities: 0
// above), so nothing here reads a "mode" key out of cfg; see doc.go for why
// mode selection happens by name, not by configuration value.
func envelopeSignerFromConfig(cfg pkgcore.Config) (pki.Signer, error) {
	c, err := configFromFlat(cfg)
	if err != nil {
		return nil, err
	}
	c.Mode = ModeEnvelope
	return NewSigner(c)
}

// directSignerFromConfig mirrors envelopeSignerFromConfig for
// "signer.vault-direct".
func directSignerFromConfig(cfg pkgcore.Config) (pki.Signer, error) {
	c, err := configFromFlat(cfg)
	if err != nil {
		return nil, err
	}
	c.Mode = ModeDirectSign
	return NewSigner(c)
}

// configFromFlat adapts a flat pkgcore.Config onto Config. "address" and
// "token" have no safe default -- there is no such thing as a generic
// Vault server -- so a Config missing either is rejected with
// pkgcore.ErrMissingSeamConfig before NewSigner is even called, the same
// early-check convention go/pkgcore's own smtpMailerFromConfig and
// objectstore/s3's objectStoreFromConfig use for their own required
// fields. "wrapping_key_name" is NOT checked here even though ModeEnvelope
// requires it -- NewSigner already validates that, and duplicating the
// check here would just be two places that could disagree about the
// message.
func configFromFlat(cfg pkgcore.Config) (Config, error) {
	address := cfg["address"]
	token := cfg["token"]
	if address == "" || token == "" {
		return Config{}, fmt.Errorf(
			"pki/signer/vault: builtin signer.vault seam: %w: requires \"address\" and \"token\"",
			pkgcore.ErrMissingSeamConfig,
		)
	}
	return Config{
		Address:         address,
		Token:           token,
		Namespace:       cfg["namespace"],
		MountPath:       cfg["mount_path"],
		WrappingKeyName: cfg["wrapping_key_name"],
	}, nil
}
