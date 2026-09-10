package dbkit

// bootstrap_key_derivation.go carries the one composition that turns a root
// secret into a declared bootstrap key's material: the bootstrap seat's
// purpose convention over the key path (pkgcore.BootstrapKeyPurpose) followed
// by this package's DeriveKey primitive. It exists so a host -- and the
// loader a host drives, go/pkgcore/config's WithKeyDerivation -- wires one
// call instead of two.

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// DeriveBootstrapKey derives the 32-byte material of one declared bootstrap
// key path from rootKey: pkgcore.BootstrapKeyPurpose fixes the purpose string
// the path derives under (the seat's one supported spelling, "speed." + keyPath
// + ".v1", stating the stability contract that makes renaming a declared path
// a rotation), and DeriveKey turns rootKey plus that purpose into the key. The
// result is exactly 32 bytes, ready for NewCipher or NewBlindIndexer unchanged,
// and deterministic: the same (rootKey, keyPath) pair always yields the same
// bytes.
//
// It is the pair the platform intends a host to wire wherever a declared
// bootstrap key's material is derived from a root key, and the function type
// fits go/pkgcore/config's WithKeyDerivation directly --
// WithKeyDerivation(dbkit.DeriveBootstrapKey) is the whole wiring, so the
// loader learns the key path of each derive-tagged field and this composition
// turns it into material. The loader passes the field's dotted key path, which
// is the same identifier syntax BootstrapKey.Key carries; whether that path is
// a key some module actually declared is the host's binding check
// (config.Verify against the registry's declarations), never this function's.
//
// Errors keep each half's own identity and add only the context that says
// which derivation failed: a path that is empty or carries an empty segment
// wraps pkgcore.ErrInvalidBootstrapKeyPath, and a rootKey that is not exactly
// 32 bytes wraps ErrInvalidKeySize -- the same sentinel NewCipher,
// NewBlindIndexer and DeriveKey return for their keys.
//
// # A declared key path is part of the key material
//
// Both halves of the composition are deterministic and the purpose embeds the
// declared path verbatim, so renaming a declared key path silently re-derives
// different material for it -- the exact blast radius of losing that key. A
// rename must therefore ship as a rotation, never as a plain edit; the seat's
// BootstrapKeyPurpose doc comment states the contract in full.
func DeriveBootstrapKey(rootKey []byte, keyPath string) ([]byte, error) {
	purpose, err := pkgcore.BootstrapKeyPurpose(keyPath)
	if err != nil {
		return nil, fmt.Errorf("dbkit: derive bootstrap key: %w", err)
	}
	key, err := DeriveKey(rootKey, purpose)
	if err != nil {
		return nil, fmt.Errorf("dbkit: derive bootstrap key %q: %w", keyPath, err)
	}
	return key, nil
}
