package pkgcore

// bootstrap_key_purpose.go carries the derivation convention of the bootstrap
// seat: the one supported purpose spelling a declared key path derives under,
// composed by the host with dbkit.DeriveKey. The seat itself
// (bootstrap_key.go) declares keys; this file fixes what each declared path
// derives to, and the stability contract a rename must respect.

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidBootstrapKeyPath reports a declared bootstrap key path that is
// empty or carries an empty segment (a leading, trailing or doubled dot),
// passed to BootstrapKeyPurpose. The error names the offending path -- a
// declaration, never secret material -- and matches with errors.Is. It is a
// plain sentinel rather than an apperr-coded error, like its siblings the
// declaration sentinels: derivation runs at boot, before any HTTP surface
// exists, so there is no response envelope for a code to travel in.
var ErrInvalidBootstrapKeyPath = errors.New("pkgcore: invalid bootstrap key path")

// BootstrapKeyPurpose returns the derivation purpose string of one declared
// bootstrap key path: the literal concatenation "speed." + keyPath + ".v1",
// never a string-surgery transform of the path.
//
// It is the one supported purpose spelling for the keys modules declare on
// this seat, and the input that turns a root secret into one declared key's
// material: a host derives the material at keyPath by composing the two
// stable contracts -- dbkit.DeriveKey(rootKey, purpose) with the purpose
// this function returns. A pure function of the path, rather than a signer
// that takes the booted registry's declared keys, is what the call sites
// require: a host derives these materials before the declaration turn can run
// (dbkit serializer registration precedes dbkit.Open, which precedes
// Bootstrap), so the registry is not yet in hand where the derivation
// happens. Whether a path is a declared key at all is the host's binding
// check (config.Verify against the registry's declarations), and resolving
// the precedence between a derived material and an explicitly configured one
// is likewise the host's policy, never this function's.
//
// keyPath is a dotted bootstrap key path as BootstrapKey.Key carries it, for
// example "config.cipher_key". The path must be non-empty and carry no empty
// segment (a leading, trailing or doubled dot); a path that is not fails with
// ErrInvalidBootstrapKeyPath.
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
//	config.cipher_key                    -> speed.config.cipher_key.v1
//	notification.contact_index_key       -> speed.notification.contact_index_key.v1
//	org.invitation_email_index_key       -> speed.org.invitation_email_index_key.v1
//	pki.local_key_cipher_key             -> speed.pki.local_key_cipher_key.v1
func BootstrapKeyPurpose(keyPath string) (string, error) {
	if err := validateBootstrapKeyPath(keyPath); err != nil {
		return "", err
	}
	return "speed." + keyPath + ".v1", nil
}

// validateBootstrapKeyPath reports the shape contradiction in a declared key
// path: empty, or carrying an empty segment.
func validateBootstrapKeyPath(keyPath string) error {
	if keyPath == "" {
		return fmt.Errorf("%w: key path %q is empty", ErrInvalidBootstrapKeyPath, keyPath)
	}
	for _, segment := range strings.Split(keyPath, ".") {
		if segment == "" {
			return fmt.Errorf("%w: key path %q carries an empty segment", ErrInvalidBootstrapKeyPath, keyPath)
		}
	}
	return nil
}
