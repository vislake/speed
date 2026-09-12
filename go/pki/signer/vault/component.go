package vault

// component.go registers the "signer.vault" and "signer.vault-direct"
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

// signerComponentConfig is the configuration schema both vault signer
// components share: one field per key the flat construction path reads, so a
// composition block spells the same settings a flat pkgcore.Config carries
// and neither shape grows a setting the other lacks.
type signerComponentConfig struct {
	Address         string `json:"address"`
	Token           string `json:"token"`
	Namespace       string `json:"namespace"`
	MountPath       string `json:"mount_path"`
	WrappingKeyName string `json:"wrapping_key_name"`
}

// signerVaultComponent is the component descriptor for "signer.vault": the
// envelope-mode Vault Transit signer over the configuration the component's
// own block spells out, declaring 0 capabilities -- envelope mode decrypts
// the real key into this process's memory to sign, so it does not satisfy
// pkgcore.KeyNeverLeavesBoundary. Its New funnels through
// envelopeSignerFromConfig -- the package's one construction path for this
// name -- so a composition block and a flat pkgcore.Config cannot diverge
// on the forced mode, the required-field checks or the coded config
// refusal. The
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
// the Vault service, so this component declares
// pkgcore.KeyNeverLeavesBoundary.
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

// envelopeSignerFromConfig adapts a flat pkgcore.Config onto NewSigner with
// Mode forced to ModeEnvelope, regardless of what a caller puts in cfg --
// the Mode is what the component NAME already promised (Capabilities: 0
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
//
// A note on the Transit key's LIFECYCLE, current state of the pin doc.go's
// "in-place Transit key rotation" section describes: this name governs the
// key through the pki module's own state machine (pending -> active ->
// retiring -> retired), which rotates by creating NEW Transit key names and
// never rotates one in place. An in-place rotation of the key through
// Vault's own rotate endpoint (the one way a managed name acquires a
// version other than the one this package created it at) cannot silently
// change which version signs or which public key is served: sign requests
// pin key_version to the created version, public-key reads serve that same
// version, and a sign answer naming any other version is refused. What the
// pin does NOT do is make the in-place-rotated version usable: signatures
// and exports stay on the created version forever, and a host that wants a
// NEW key version live must rotate through the pki module's own lifecycle
// (a new name per stage). The pin's behaviour is proven against stubbed
// clients only -- no real-Vault integration leg exists (go/pki/AGENTS.md's
// Known limitations) -- so a host choosing "signer.vault-direct" should
// still read doc.go's section.
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
			"pki/signer/vault: builtin signer.vault component: %w: requires \"address\" and \"token\"",
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

func init() {
	pkgcore.MustRegister(signerVaultComponent)
	pkgcore.MustRegister(signerVaultDirectComponent)
}
