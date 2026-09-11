package pkgcore

import (
	"errors"
	"strings"
	"testing"
)

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
		Key:         "config.cipher_key",
		Format:      "hexkey",
		Sensitive:   true,
		Description: "The AES cipher key the config module seals Sensitive values with.",
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
