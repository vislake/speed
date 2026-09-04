package vault

import (
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki"
)

// TestInit_RegistersBothNamesOnTheSharedRegistry proves this package's
// init() lands both "signer.vault" (Capabilities: none) and
// "signer.vault-direct" (Capabilities: KeyNeverLeavesBoundary) on
// pki.SignerRegistry -- mirroring
// go/pkgcore/objectstore/s3/register_test.go's identical assertion for its
// own built-in.
func TestInit_RegistersBothNamesOnTheSharedRegistry(t *testing.T) {
	cfg := pkgcore.Config{"address": "https://vault.example.com:8200", "token": "t", "wrapping_key_name": "wrap"}

	impl, caps, err := pki.SignerRegistry.Build("signer.vault", cfg)
	if err != nil {
		t.Fatalf(`Build("signer.vault") error = %v, want nil`, err)
	}
	if impl == nil {
		t.Error(`Build("signer.vault") returned a nil Signer`)
	}
	if caps != 0 {
		t.Errorf(`Build("signer.vault") capabilities = %v, want none`, caps)
	}

	impl, caps, err = pki.SignerRegistry.Build("signer.vault-direct", cfg)
	if err != nil {
		t.Fatalf(`Build("signer.vault-direct") error = %v, want nil`, err)
	}
	if impl == nil {
		t.Error(`Build("signer.vault-direct") returned a nil Signer`)
	}
	if caps != pkgcore.KeyNeverLeavesBoundary {
		t.Errorf(`Build("signer.vault-direct") capabilities = %v, want KeyNeverLeavesBoundary`, caps)
	}
}

// TestInit_EmptyConfigRequiresConfig mirrors
// go/pkgcore/objectstore/s3/register_test.go's identical assertion: this
// seam has no safe default address or token, so an empty Config fails with
// pkgcore.ErrMissingSeamConfig rather than silently building an unusable
// signer.
func TestInit_EmptyConfigRequiresConfig(t *testing.T) {
	if _, _, err := pki.SignerRegistry.Build("signer.vault", pkgcore.Config{}); err == nil {
		t.Error("Build() with an empty Config succeeded, want ErrMissingSeamConfig")
	}
}

func TestConfigFromFlat_MissingFieldReturnsErrMissingSeamConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  pkgcore.Config
	}{
		{name: "missing everything", cfg: pkgcore.Config{}},
		{name: "missing address", cfg: pkgcore.Config{"token": "t"}},
		{name: "missing token", cfg: pkgcore.Config{"address": "https://vault.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := configFromFlat(tt.cfg)
			if !errors.Is(err, pkgcore.ErrMissingSeamConfig) {
				t.Fatalf("configFromFlat(%v) error = %v, want it to wrap ErrMissingSeamConfig", tt.cfg, err)
			}
		})
	}
}

func TestDirectSignerFromConfig_MissingWrappingKeyIsFine(t *testing.T) {
	// signer.vault-direct never needs wrapping_key_name -- only the
	// envelope name does.
	_, err := directSignerFromConfig(pkgcore.Config{"address": "https://vault.example.com", "token": "t"})
	if err != nil {
		t.Fatalf("directSignerFromConfig() error = %v, want nil", err)
	}
}

func TestEnvelopeSignerFromConfig_MissingWrappingKeyFails(t *testing.T) {
	_, err := envelopeSignerFromConfig(pkgcore.Config{"address": "https://vault.example.com", "token": "t"})
	if err == nil {
		t.Fatal("envelopeSignerFromConfig() without wrapping_key_name succeeded, want an error")
	}
}
