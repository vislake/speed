package config

import (
	"strings"

	"github.com/vislake/speed/go/dbkit"
)

// BootstrapKeyPurpose returns the derivation purpose string of one declared
// bootstrap key path: the literal concatenation "speed." + keyPath + ".v1",
// the input dbkit.DeriveKey hashes into that key's material.
//
// keyPath is a dotted bootstrap key path as pkgcore.BootstrapKey.Key carries
// it, for example "config.master_key". The path must be non-empty and carry
// no empty segment (a leading, trailing or doubled dot); a path that is not
// fails with ErrInvalidBootstrapKeyPath.
//
// # Stability contract: a declared key path is part of the key material
//
// The purpose string embeds keyPath verbatim, and dbkit.DeriveKey is
// deterministic -- the same (rootKey, purpose) pair always yields the same
// bytes -- so renaming a declared key path is a key rotation in effect even
// though no key is edited: everything derived under the old path stops
// being reproducible and everything derived under the new one is different
// secret material. A rename must therefore ship as a rotation -- keep the
// old material readable, or deliberately re-derive and re-seal -- never as
// a plain edit.
//
// These are the six declared key paths this repository's modules register
// today, with the purpose each freezes:
//
//	authn.blind_index_key                -> speed.authn.blind_index_key.v1
//	authn.pii_cipher_key                 -> speed.authn.pii_cipher_key.v1
//	config.master_key                    -> speed.config.master_key.v1
//	notification.contact_index_key       -> speed.notification.contact_index_key.v1
//	org.invitation_email_index_key       -> speed.org.invitation_email_index_key.v1
//	pki.local_key_cipher_key             -> speed.pki.local_key_cipher_key.v1
func BootstrapKeyPurpose(keyPath string) (string, error) {
	if err := validateBootstrapKeyPath(keyPath); err != nil {
		return "", err
	}
	return "speed." + keyPath + ".v1", nil
}

// DeriveBootstrapKeyMaterial derives one declared bootstrap key's 32-byte
// material from rootKey: the material at keyPath is
// dbkit.DeriveKey(rootKey, BootstrapKeyPurpose(keyPath)).
//
// This is the supported derivation entry for key material named by a
// declared bootstrap key path: both inputs are validated here, the purpose
// string's shape is fixed by this module, and BootstrapKeyPurpose's
// stability contract covers everything derived through this function.
// Calling dbkit.DeriveKey directly with a hand-spelled purpose string
// produces the same bytes for the same inputs but takes on none of that
// contract -- keeping the string stable becomes the caller's own
// responsibility, and the frozen declared-path shapes stop being what the
// call site observes.
//
// The signature takes a root key and one path rather than a registry and a
// batch of its declared keys, deliberately: a host derives the six
// materials before Kernel.Bootstrap can run (dbkit serializer registration
// precedes dbkit.Open, which precedes Bootstrap), so a signer that required
// the booted registry would not be callable where the derivation happens.
// Whether a path is a declared key at all is the host's binding check
// (config.Verify against the registry's declarations), not a question this
// function can answer.
//
// rootKey must be exactly 32 bytes, the length dbkit.DeriveKey requires; a
// different length fails with ErrInvalidRootKey, whose cause is
// dbkit.ErrInvalidKeySize. keyPath follows BootstrapKeyPurpose's shape
// rule. Resolving the precedence between derived material and an
// explicitly configured value is the host's policy, never this function's:
// it only produces the derived candidate.
func DeriveBootstrapKeyMaterial(rootKey []byte, keyPath string) ([]byte, error) {
	purpose, err := BootstrapKeyPurpose(keyPath)
	if err != nil {
		return nil, err
	}
	derived, err := dbkit.DeriveKey(rootKey, purpose)
	if err != nil {
		return nil, ErrInvalidRootKey.WithCause(err)
	}
	return derived, nil
}

// validateBootstrapKeyPath reports the shape contradiction in a declared
// key path: empty, or carrying an empty segment.
func validateBootstrapKeyPath(keyPath string) error {
	if keyPath == "" {
		return ErrInvalidBootstrapKeyPath.WithParam("key", keyPath)
	}
	for _, segment := range strings.Split(keyPath, ".") {
		if segment == "" {
			return ErrInvalidBootstrapKeyPath.WithParam("key", keyPath)
		}
	}
	return nil
}
