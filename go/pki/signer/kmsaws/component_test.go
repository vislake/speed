package kmsaws

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/pki"
)

// kmsawsComponentSettings returns the settings both resolution paths are
// fed from: the component's composition block and the seam registration's
// flat pkgcore.Config carry the identical key set and values.
func kmsawsComponentSettings() pkgcore.Config {
	return pkgcore.Config{
		"region":            "eu-west-1",
		"access_key_id":     "AKIAEXAMPLE",
		"secret_access_key": "secret-example",
		"wrapping_key_id":   "11111111-2222-3333-4444-555555555555",
	}
}

// componentBlock converts a flat Config into the map shape a composition
// block carries: the same keys, the same values.
func componentBlock(cfg pkgcore.Config) map[string]any {
	block := make(map[string]any, len(cfg))
	for key, value := range cfg {
		block[key] = value
	}
	return block
}

// TestComponentsWellFormed runs the component descriptor contract every
// component package's suite asserts for both signer descriptors.
func TestComponentsWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, signerAWSKMSComponent)
	componenttest.AssertWellFormed(t, signerAWSKMSDirectComponent)
}

// TestComponentsAssembleThroughRegistry drives each descriptor through the
// assembly's stages the way a host would: selection from a composition
// configuration, construction, and the product read back at the contract
// type consumers use.
func TestComponentsAssembleThroughRegistry(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"signer.aws-kms", "signer.aws-kms-direct"} {
		t.Run(name, func(t *testing.T) {
			reg := pkgcore.NewComponentRegistry()
			reg.Put(pkgcore.NewComponentConfig(map[string]any{
				"components": map[string]any{name: componentBlock(kmsawsComponentSettings())},
			}))
			if err := reg.Prepare(ctx); err != nil {
				t.Fatalf("Prepare() error = %v, want %s selected and validated", err, name)
			}
			if err := reg.Construct(ctx); err != nil {
				t.Fatalf("Construct() error = %v, want %s constructed", err, name)
			}
			t.Cleanup(func() { _ = reg.Close(context.Background()) })

			built, err := pkgcore.Get[pki.Signer](reg)
			if err != nil {
				t.Fatalf("Get[pki.Signer] error = %v, want the component's product", err)
			}
			if _, ok := built.(*signer); !ok {
				t.Errorf("Get[pki.Signer] = %T, want *kmsaws.signer", built)
			}
		})
	}
}

// TestComponentAndSeamRegistrationAgree pins the coexistence of the two
// resolution paths under one name: both build the same implementation from
// the same settings, and the component-face capability read reports exactly
// what the registry-face requirement check enforces -- the two comparisons
// a capability-requiring caller can perform must never disagree.
func TestComponentAndSeamRegistrationAgree(t *testing.T) {
	ctx := context.Background()
	settings := kmsawsComponentSettings()

	for _, name := range []string{"signer.aws-kms", "signer.aws-kms-direct"} {
		t.Run(name, func(t *testing.T) {
			seamSigner, seamCaps, err := pki.SignerRegistry.Build(name, settings)
			if err != nil {
				t.Fatalf("SignerRegistry.Build(%s) error = %v", name, err)
			}

			reg := pkgcore.NewComponentRegistry()
			reg.Put(pkgcore.NewComponentConfig(map[string]any{
				"components": map[string]any{name: componentBlock(settings)},
			}))
			if prepareErr := reg.Prepare(ctx); prepareErr != nil {
				t.Fatalf("Prepare() error = %v", prepareErr)
			}
			if constructErr := reg.Construct(ctx); constructErr != nil {
				t.Fatalf("Construct() error = %v", constructErr)
			}
			t.Cleanup(func() { _ = reg.Close(context.Background()) })

			componentSigner, err := pkgcore.Get[pki.Signer](reg)
			if err != nil {
				t.Fatalf("Get[pki.Signer] error = %v", err)
			}
			if reflect.TypeOf(componentSigner) != reflect.TypeOf(seamSigner) {
				t.Errorf("component built %T, seam registration built %T; the two faces must resolve the same implementation", componentSigner, seamSigner)
			}

			caps, err := pkgcore.ComponentCapabilities(reg, name)
			if err != nil || caps != seamCaps {
				t.Errorf("ComponentCapabilities(%s) = (%v, %v), want the seam registration's %v", name, caps, err, seamCaps)
			}

			required := pkgcore.KeyNeverLeavesBoundary
			_, seamErr := pki.BuildSignerRequiring(name, settings, required)
			if (seamErr == nil) != caps.Has(required) {
				t.Errorf("BuildSignerRequiring(%s) error = %v while the component-face read says Has(%v) = %v; the two capability comparisons must agree", name, seamErr, required, caps.Has(required))
			}
		})
	}
}

// TestComponentConfigRefusalMatchesTheRegistry pins the refusal parity for
// missing configuration: both faces fail with pkgcore.ErrMissingSeamConfig
// for an empty block, and both name the missing wrapping key for an
// envelope-mode config that carries no wrapping_key_id.
func TestComponentConfigRefusalMatchesTheRegistry(t *testing.T) {
	ctx := context.Background()

	if _, _, err := pki.SignerRegistry.Build("signer.aws-kms", pkgcore.Config{}); !errors.Is(err, pkgcore.ErrMissingSeamConfig) {
		t.Errorf("SignerRegistry.Build with an empty Config = %v, want it to wrap ErrMissingSeamConfig", err)
	}
	if _, _, err := pki.SignerRegistry.Build("signer.aws-kms", pkgcore.Config{"region": "eu-west-1", "access_key_id": "a", "secret_access_key": "s"}); err == nil || !strings.Contains(err.Error(), "WrappingKeyID") {
		t.Errorf("SignerRegistry.Build without a wrapping key = %v, want NewSigner's envelope-mode error", err)
	}

	for _, tt := range []struct {
		name  string
		block map[string]any
		want  string
	}{
		{name: "signer.aws-kms", block: nil, want: "ErrMissingSeamConfig"},
		{name: "signer.aws-kms", block: map[string]any{"region": "eu-west-1", "access_key_id": "a", "secret_access_key": "s"}, want: "WrappingKeyID"},
	} {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(pkgcore.NewComponentConfig(map[string]any{
			"components": map[string]any{tt.name: tt.block},
		}))
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		err := reg.Construct(ctx)
		if err == nil {
			t.Errorf("Construct with block %v succeeded, want a refusal", tt.block)
		} else if tt.want == "ErrMissingSeamConfig" && !errors.Is(err, pkgcore.ErrMissingSeamConfig) {
			t.Errorf("Construct with an empty block = %v, want it to wrap ErrMissingSeamConfig", err)
		} else if tt.want != "ErrMissingSeamConfig" && !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Construct without a wrapping key = %v, want it to name %s", err, tt.want)
		}
		_ = reg.Close(context.Background())
	}
}
