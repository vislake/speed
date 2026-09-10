package pkgcore

import (
	"context"
	"errors"
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

func TestBootstrapRegistrar_Add_RejectsContradictoryDeclarations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  BootstrapKey
	}{
		{
			name: "empty key path",
			key:  BootstrapKey{Format: "string"},
		},
		{
			name: "unknown format",
			key:  BootstrapKey{Key: "authn.pii_cipher_key", Format: "bytes"},
		},
		{
			name: "sensitive key with no description",
			key: BootstrapKey{
				Key:       "authn.pii_cipher_key",
				Format:    "hexkey",
				Sensitive: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
			if err := reg.Bootstrap.Add(tt.key); !errors.Is(err, ErrInvalidBootstrapKey) {
				t.Fatalf("Add() error = %v, want it to wrap ErrInvalidBootstrapKey", err)
			}
			if keys := reg.Bootstrap.Keys(); len(keys) != 0 {
				t.Errorf("Keys() = %v, want nothing registered when Add fails", keys)
			}
		})
	}
}

func TestBootstrapRegistrar_Add_RejectsRepeatedKeys(t *testing.T) {
	t.Parallel()

	first := bootstrapDecl()
	second := bootstrapDecl()
	second.Description = "the same key with different text"

	tests := []struct {
		name string
		add  func(reg BootstrapRegistrar) error
	}{
		{
			name: "twice within one call",
			add: func(reg BootstrapRegistrar) error {
				return reg.Add(first, second)
			},
		},
		{
			name: "once in each of two calls",
			add: func(reg BootstrapRegistrar) error {
				if err := reg.Add(first); err != nil {
					return err
				}
				return reg.Add(second)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
			err := tt.add(r.Bootstrap)
			if !errors.Is(err, ErrDuplicateBootstrapKey) {
				t.Fatalf("Add() error = %v, want it to wrap ErrDuplicateBootstrapKey", err)
			}
			if !strings.Contains(err.Error(), first.Key) {
				t.Errorf("error = %q, want it to name the duplicated key %q", err, first.Key)
			}
		})
	}
}

// TestBootstrapRegistrar_Add_RegistersNothingWhenOneItemIsInvalid pins the
// all-or-nothing rule: a call carrying one good and one bad declaration must
// not leave the good one behind.
func TestBootstrapRegistrar_Add_RegistersNothingWhenOneItemIsInvalid(t *testing.T) {
	t.Parallel()

	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	err := reg.Bootstrap.Add(bootstrapDecl(), BootstrapKey{Format: "string"})
	if !errors.Is(err, ErrInvalidBootstrapKey) {
		t.Fatalf("Add() error = %v, want it to wrap ErrInvalidBootstrapKey", err)
	}
	if keys := reg.Bootstrap.Keys(); len(keys) != 0 {
		t.Errorf("Keys() = %v, want nothing registered", keys)
	}
}

func TestBootstrapRegistrar_Keys_ReturnsRegistrationOrderCopy(t *testing.T) {
	t.Parallel()

	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	first := bootstrapDecl()
	second := BootstrapKey{
		Key:         "config.master_key",
		Format:      "hexkey",
		Sensitive:   true,
		Description: "Master key the config module seals Sensitive values with.",
		Group:       "config",
	}
	if err := reg.Bootstrap.Add(first, second); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	keys := reg.Bootstrap.Keys()
	if len(keys) != 2 || keys[0].Key != first.Key || keys[1].Key != second.Key {
		t.Fatalf("Keys() = %v, want the two declarations in registration order", keys)
	}
	keys[0].Key = "mutated"
	if again := reg.Bootstrap.Keys(); again[0].Key != first.Key {
		t.Errorf("Keys() = %v, want each call to return its own copy", again)
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
