package pkgcore

import (
	"context"
	"strings"
	"testing"
)

// bootstrapDecl is a valid declaration the tests vary one field at a time.
func bootstrapDecl() BootstrapKey {
	return BootstrapKey{
		Key:         "authn.pii_cipher_key",
		Format:      "hexkey",
		Default:     "documented non-secret development default",
		Sensitive:   true,
		Description: "AES key sealing authn's encrypted PII columns.",
		Group:       "authn",
	}
}

// TestNewRegistry_WiresBootstrapSeat pins the seat's presence on a registry
// built through the constructor every host and test uses.
func TestNewRegistry_WiresBootstrapSeat(t *testing.T) {
	t.Parallel()

	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	if reg.Bootstrap == nil {
		t.Fatal("NewRegistry() left Registry.Bootstrap nil, want the in-memory registrar")
	}
	if keys := reg.Bootstrap.Keys(); len(keys) != 0 {
		t.Errorf("Keys() = %v, want an empty registrar on a fresh registry", keys)
	}
}

// TestBootstrap_KeyOnBothSeats_Fails pins the layer boundary's machine
// defence: the two seats cannot see each other while modules register, so the
// overlap is refused where both are complete.
func TestBootstrap_KeyOnBothSeats_Fails(t *testing.T) {
	t.Parallel()

	declaring := regTestModule{
		name: "authn",
		register: func(reg *Registry) error {
			return reg.Bootstrap.Add(bootstrapDecl())
		},
	}
	conflicting := regTestModule{
		name: "config",
		deps: []string{"authn"},
		register: func(reg *Registry) error {
			return reg.Config.Add(ConfigItem{
				Key:         "authn.pii_cipher_key",
				Type:        "string",
				Description: "a runtime item claiming the bootstrap key's name",
			})
		},
	}

	reg, err := NewKernel().Bootstrap(context.Background(), conflicting, declaring)
	if err == nil {
		t.Fatalf("Bootstrap() error = nil, want a failure naming the doubly declared key")
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	for _, want := range []string{bootstrapDecl().Key, "reg.Config", "reg.Bootstrap"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

// TestBootstrap_SamePrefixOnBothSeats_Allowed pins the other side of the rule:
// the two layers are told apart by the whole dotted key, never by shape, so a
// runtime item and a bootstrap key sharing a module prefix register cleanly.
func TestBootstrap_SamePrefixOnBothSeats_Allowed(t *testing.T) {
	t.Parallel()

	declaring := regTestModule{
		name: "authn",
		register: func(reg *Registry) error {
			return reg.Bootstrap.Add(bootstrapDecl())
		},
	}
	sibling := regTestModule{
		name: "config",
		deps: []string{"authn"},
		register: func(reg *Registry) error {
			return reg.Config.Add(ConfigItem{
				Key:         "authn.password_min_length",
				Type:        "int",
				Default:     12,
				Description: "a runtime item under the same module prefix",
			})
		},
	}

	reg, err := NewKernel().Bootstrap(context.Background(), sibling, declaring)
	if err != nil {
		t.Fatalf("Bootstrap() error = %v, want a clean boot", err)
	}
	if keys := reg.Bootstrap.Keys(); len(keys) != 1 {
		t.Errorf("Keys() = %v, want the one declared bootstrap key", keys)
	}
}
