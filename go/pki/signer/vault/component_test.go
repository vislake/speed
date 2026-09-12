package vault

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/pki"
)

// vaultComponentSettings returns the settings these tests feed the
// component's composition block: componentBlock wraps them into the block's
// map shape below.
func vaultComponentSettings() pkgcore.Config {
	return pkgcore.Config{
		"address":           "https://vault.example.test:8200",
		"token":             "hvs.component-token",
		"wrapping_key_name": "wrapping-key",
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
	componenttest.AssertWellFormed(t, signerVaultComponent)
	componenttest.AssertWellFormed(t, signerVaultDirectComponent)
}

// TestComponentsAssembleThroughRegistry drives each descriptor through the
// assembly's stages the way a host would: selection from a composition
// configuration, construction, and the product read back at the contract
// type consumers use.
func TestComponentsAssembleThroughRegistry(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"signer.vault", "signer.vault-direct"} {
		t.Run(name, func(t *testing.T) {
			reg := pkgcore.NewComponentRegistry()
			reg.Put(pkgcore.NewComponentConfig(map[string]any{
				"components": map[string]any{name: componentBlock(vaultComponentSettings())},
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
				t.Errorf("Get[pki.Signer] = %T, want *vault.signer", built)
			}
		})
	}
}

// TestComponentCapabilityDeclarations pins the component-face capability
// read per name: the envelope mode declares 0 and the direct-sign mode the
// boundary bit -- the declaration a capability-requiring caller compares
// against.
func TestComponentCapabilityDeclarations(t *testing.T) {
	ctx := context.Background()
	settings := vaultComponentSettings()

	want := map[string]pkgcore.Capability{
		"signer.vault":        0,
		"signer.vault-direct": pkgcore.KeyNeverLeavesBoundary,
	}
	for _, name := range []string{"signer.vault", "signer.vault-direct"} {
		t.Run(name, func(t *testing.T) {
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

			caps, err := pkgcore.ComponentCapabilities(reg, name)
			if err != nil || caps != want[name] {
				t.Errorf("ComponentCapabilities(%s) = (%v, %v), want %v", name, caps, err, want[name])
			}
		})
	}
}

// TestComponentConfigRefusal pins the refusal for missing configuration:
// an empty block fails with pkgcore.ErrMissingSeamConfig, and an
// envelope-mode config that carries no wrapping key names the missing
// WrappingKeyName.
func TestComponentConfigRefusal(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		name  string
		block map[string]any
		want  string
	}{
		{name: "signer.vault", block: nil, want: "ErrMissingSeamConfig"},
		{name: "signer.vault", block: map[string]any{"address": "https://vault.example.test:8200", "token": "t"}, want: "WrappingKeyName"},
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
