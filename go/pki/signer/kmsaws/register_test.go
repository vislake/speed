package kmsaws

import (
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki"
)

// TestInit_RegistersBothNamesOnTheSharedRegistry proves this package's
// init() lands both "signer.aws-kms" (Capabilities: none) and
// "signer.aws-kms-direct" (Capabilities: KeyNeverLeavesBoundary) on
// pki.SignerRegistry.
func TestInit_RegistersBothNamesOnTheSharedRegistry(t *testing.T) {
	cfg := pkgcore.Config{
		"region":            "us-east-1",
		"access_key_id":     "ak",
		"secret_access_key": "sk",
		"wrapping_key_id":   "wrap-key",
	}

	impl, caps, err := pki.SignerRegistry.Build("signer.aws-kms", cfg)
	if err != nil {
		t.Fatalf(`Build("signer.aws-kms") error = %v, want nil`, err)
	}
	if impl == nil {
		t.Error(`Build("signer.aws-kms") returned a nil Signer`)
	}
	if caps != 0 {
		t.Errorf(`Build("signer.aws-kms") capabilities = %v, want none`, caps)
	}

	impl, caps, err = pki.SignerRegistry.Build("signer.aws-kms-direct", cfg)
	if err != nil {
		t.Fatalf(`Build("signer.aws-kms-direct") error = %v, want nil`, err)
	}
	if impl == nil {
		t.Error(`Build("signer.aws-kms-direct") returned a nil Signer`)
	}
	if caps != pkgcore.KeyNeverLeavesBoundary {
		t.Errorf(`Build("signer.aws-kms-direct") capabilities = %v, want KeyNeverLeavesBoundary`, caps)
	}
}

func TestInit_EmptyConfigRequiresConfig(t *testing.T) {
	if _, _, err := pki.SignerRegistry.Build("signer.aws-kms", pkgcore.Config{}); err == nil {
		t.Error("Build() with an empty Config succeeded, want ErrMissingSeamConfig")
	}
}

func TestConfigFromFlat_MissingFieldReturnsErrMissingSeamConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  pkgcore.Config
	}{
		{name: "missing everything", cfg: pkgcore.Config{}},
		{name: "missing region", cfg: pkgcore.Config{"access_key_id": "ak", "secret_access_key": "sk"}},
		{name: "missing access key", cfg: pkgcore.Config{"region": "us-east-1", "secret_access_key": "sk"}},
		{name: "missing secret key", cfg: pkgcore.Config{"region": "us-east-1", "access_key_id": "ak"}},
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
	_, err := directSignerFromConfig(pkgcore.Config{"region": "us-east-1", "access_key_id": "ak", "secret_access_key": "sk"})
	if err != nil {
		t.Fatalf("directSignerFromConfig() error = %v, want nil", err)
	}
}

func TestEnvelopeSignerFromConfig_MissingWrappingKeyFails(t *testing.T) {
	_, err := envelopeSignerFromConfig(pkgcore.Config{"region": "us-east-1", "access_key_id": "ak", "secret_access_key": "sk"})
	if err == nil {
		t.Fatal("envelopeSignerFromConfig() without wrapping_key_id succeeded, want an error")
	}
}
