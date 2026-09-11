package pkgcore

import (
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
// defence: the two seats cannot see each other while declarations are made,
// so the overlap is refused where both are complete -- the comparison
// Kernel.Bootstrap runs once every registration turn has finished.
func TestBootstrap_KeyOnBothSeats_Fails(t *testing.T) {
	t.Parallel()

	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	if err := reg.Bootstrap.Add(bootstrapDecl()); err != nil {
		t.Fatalf("Bootstrap.Add() = %v", err)
	}
	if err := reg.Config.Add(ConfigItem{
		Key:         "authn.pii_cipher_key",
		Type:        "string",
		Description: "a runtime item claiming the bootstrap key's name",
	}); err != nil {
		t.Fatalf("Config.Add() = %v", err)
	}

	err := validateBootstrapKeySeparation(reg)
	if err == nil {
		t.Fatal("validateBootstrapKeySeparation() = nil, want a failure naming the doubly declared key")
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

	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	if err := reg.Bootstrap.Add(bootstrapDecl()); err != nil {
		t.Fatalf("Bootstrap.Add() = %v", err)
	}
	if err := reg.Config.Add(ConfigItem{
		Key:         "authn.password_min_length",
		Type:        "int",
		Default:     12,
		Description: "a runtime item under the same module prefix",
	}); err != nil {
		t.Fatalf("Config.Add() = %v", err)
	}

	if err := validateBootstrapKeySeparation(reg); err != nil {
		t.Fatalf("validateBootstrapKeySeparation() = %v, want a clean comparison", err)
	}
	if keys := reg.Bootstrap.Keys(); len(keys) != 1 {
		t.Errorf("Keys() = %v, want the one declared bootstrap key", keys)
	}
}
