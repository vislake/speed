package pkgcore

import "fmt"

// bootstrap_material.go carries the resolved bootstrap key material of one
// assembly: the by-key-path (and by-purpose) addressable source a component
// reads its own key material from during Prepare. The engine's loader builds
// one -- it resolves every registered component's BootstrapKeys declarations
// on the loader's source/derivation chain before anything is constructed --
// and publishes it into the registry with Put. Components never read the
// process environment or a file for key material themselves: they ask this
// source by the key path they declared.

// BootstrapMaterialEntry is one resolved declaration: the declared key path
// and the value it resolved to. The value carries the shape the declaring
// field holds -- the bytes of a "hexkey" declaration, the text of a "string"
// one -- exactly as the loader's chain (explicit sources, then the root-key
// derivation, then the target's default) produced it.
type BootstrapMaterialEntry struct {
	// KeyPath is the declared dotted key path, as BootstrapKey.Key carries
	// it, for example "authn.pii_cipher_key".
	KeyPath string
	// Value is what the key path resolved to. A nil value is no resolution
	// and is dropped at construction.
	Value any
}

// BootstrapMaterial is the resolved bootstrap key material of one assembly,
// addressed by key path or, equivalently, by the derivation purpose
// BootstrapKeyPurpose composes from that path. It is an immutable value: the
// loader builds it once, publishes it into the registry, and every consumer
// reads it.
type BootstrapMaterial struct {
	byPath map[string]any
	order  []string
}

// NewBootstrapMaterial returns the material source carrying entries. Entries
// with an empty key path or a nil value are dropped; a repeated key path
// keeps the first entry, because one declared path has one resolution per
// assembly. Byte values are cloned, so the source shares no backing array
// with the target struct it was read from.
func NewBootstrapMaterial(entries []BootstrapMaterialEntry) *BootstrapMaterial {
	m := &BootstrapMaterial{byPath: make(map[string]any, len(entries))}
	for _, entry := range entries {
		if entry.KeyPath == "" || entry.Value == nil {
			continue
		}
		if _, exists := m.byPath[entry.KeyPath]; exists {
			continue
		}
		value := entry.Value
		if raw, isBytes := value.([]byte); isBytes {
			value = append([]byte(nil), raw...)
		}
		m.byPath[entry.KeyPath] = value
		m.order = append(m.order, entry.KeyPath)
	}
	return m
}

// Value returns the value the declared key path resolved to, in whatever
// shape the declaring field holds it. A path this assembly never resolved --
// undeclared, or declared by a component of another assembly -- reports
// ok == false.
func (m *BootstrapMaterial) Value(keyPath string) (any, bool) {
	if m == nil {
		return nil, false
	}
	value, ok := m.byPath[keyPath]
	return value, ok
}

// Material returns the []byte material of a declared key path -- the shape a
// "hexkey" declaration resolves to. A path that resolved to another shape
// reports ok == false rather than a conversion.
func (m *BootstrapMaterial) Material(keyPath string) ([]byte, bool) {
	value, ok := m.Value(keyPath)
	if !ok {
		return nil, false
	}
	material, isBytes := value.([]byte)
	return material, isBytes
}

// ValueForPurpose returns the value addressed by a derivation purpose string
// (the BootstrapKeyPurpose of a declared key path). It is the purpose-addressed
// reading of the same source.
func (m *BootstrapMaterial) ValueForPurpose(purpose string) (any, bool) {
	if m == nil {
		return nil, false
	}
	for _, keyPath := range m.order {
		declared, err := BootstrapKeyPurpose(keyPath)
		if err != nil || declared != purpose {
			continue
		}
		return m.byPath[keyPath], true
	}
	return nil, false
}

// KeyPaths returns every declared key path this source resolved, in
// declaration order.
func (m *BootstrapMaterial) KeyPaths() []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.order...)
}

// BootstrapMaterialOf returns the bootstrap material source published into
// reg, naming the missing Put when the assembly carries none. A component's
// Prepare reads its declared key material through it.
func BootstrapMaterialOf(reg *ComponentRegistry) (*BootstrapMaterial, error) {
	m, err := Get[*BootstrapMaterial](reg)
	if err != nil {
		return nil, fmt.Errorf("pkgcore: the assembly carries no bootstrap material source: %w", err)
	}
	return m, nil
}
