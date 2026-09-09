package dbkit

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

// rootKeySize is the required length in bytes of DeriveKey's rootKey
// argument: 32 bytes (256 bits), the identical high-entropy floor NewCipher
// and NewBlindIndexer already enforce for the keys they consume directly.
// There is no shorter or longer accepted length, mirroring
// encryptionKeySize's own no-lenient-fallback policy: a root secret with
// less entropy than this undermines every key DeriveKey produces from it.
const rootKeySize = 32

// derivedKeySize is DeriveKey's fixed output length: 32 bytes, exactly
// encryptionKeySize, so a derived key drops straight into NewCipher or
// NewBlindIndexer with no reshaping or truncation at the call site.
const derivedKeySize = 32

// DeriveKey deterministically derives one 32-byte, purpose-specific secret
// from rootKey using HKDF (RFC 5869) with SHA-256, so a deployer can manage
// a single high-entropy root secret instead of a separate one for every
// dbkit.NewCipher / dbkit.NewBlindIndexer call site.
//
// rootKey must be exactly 32 bytes (rootKeySize); DeriveKey returns an error
// wrapping ErrInvalidKeySize otherwise, the same sentinel NewCipher and
// NewBlindIndexer already return for their own keys, kept as one recognizable
// error identity across the whole package. purpose must be non-empty: it is
// HKDF's "info" context string, and it is what makes every key this function
// can ever produce from the same rootKey independent of every other. Callers
// MUST give every call site its own distinct, stable, versioned purpose
// string -- e.g. "speed.dbkit.config.cipher.v1", never a bare
// "config.cipher" that could silently collide with an unrelated caller's
// choice, and never reused for a second, semantically different key.
// "Stable" matters as much as "distinct": DeriveKey is deterministic --
// the same (rootKey, purpose) pair always yields the same 32 bytes, on any
// machine, at any time -- so changing a purpose string already used in
// production silently re-derives a DIFFERENT key for whatever it named,
// with the exact same blast radius as losing that key outright (every row
// it protected becomes unreadable, or unfindable through its blind index).
// The trailing ".v1" exists precisely so a deliberate re-derivation --
// "derive this key differently from now on" -- has a place to go: bump the
// version suffix, never edit the string in place.
//
// The output is always exactly derivedKeySize (32) bytes, produced by
// stdlib crypto/hkdf.Key's full RFC 5869 Extract-then-Expand pipeline
// (Extract over a nil salt, then Expand): rootKey itself supplies all the
// entropy this construction needs, so a nil salt -- HKDF's spec-defined
// default of an all-zero string as long as the hash's own output size
// (HashLen; 32 bytes for SHA-256) -- loses nothing here. This is exactly
// what makes DeriveKey suitable as a drop-in root key for NewCipher's
// activeKey/retiredKeys and NewBlindIndexer's key: both require precisely
// 32 bytes.
//
// # Why this does not violate the "never reuse key material" rule
//
// NewCipher's and BlindIndex's own doc comments warn that an encryption key
// and a blind-index HMAC key must never be the same secret: mixing them
// couples the security analysis of two constructions -- AES-GCM
// confidentiality and a deterministic HMAC index -- that were never
// designed to share key material. That rule is about reusing the literal
// same key bytes across two differently-designed constructions, not about
// where a key's bytes originally came from. Two keys DeriveKey produces
// under two distinct purpose strings are, for every practical
// cryptographic purpose, independent secrets: HKDF-Expand's output for one
// info string is computationally indistinguishable from random and reveals
// nothing about its output for a different info string, even to an
// attacker who has fully compromised one derived key (but not the root).
// Deriving dbkit's config cipher key and its blind-index key from the same
// rootKey via two distinct purposes therefore satisfies the never-reuse
// rule exactly as well as generating the two keys independently at
// random and storing them as two unrelated secrets would -- it is how
// that rule is upheld with fewer secrets to manage in a real deployment,
// not an exception to it.
//
// # The real trade-off, stated plainly
//
// Using one root secret to derive many purpose-specific keys is a
// deliberate, honest trade against the fully-independent-secrets model
// dbkit shipped before this function existed, and both costs are real:
//
//   - A leaked root key compromises every key ever derived from it at
//     once -- a strictly larger blast radius than today's
//     fully-independent-secrets model, where compromising one secret
//     compromises only what it alone protects.
//   - Rotating the root key rotates every derived key simultaneously --
//     a coarser rotation granularity than managing keys independently,
//     where one secret can be rotated without touching any other.
//
// A deployment that needs fine-grained, independent rotation for one
// specific key keeps supplying that key directly (its own environment
// variable, its own secret-manager entry) instead of deriving it, and
// mixes the two freely: derive everything from one root secret by
// default, override any single purpose's key independently when that
// purpose's own rotation cadence demands it. See
// examples/reference-app/internal/app/server.go's APP_ROOT_KEY wiring for
// exactly this precedence in practice: an explicitly-set individual key's
// own environment variable always wins over a value DeriveKey would have
// produced for it.
func DeriveKey(rootKey []byte, purpose string) ([]byte, error) {
	if len(rootKey) != rootKeySize {
		return nil, fmt.Errorf("dbkit: derive key: root key: %w: got %d bytes", ErrInvalidKeySize, len(rootKey))
	}
	if purpose == "" {
		return nil, errors.New("dbkit: derive key: purpose must not be empty")
	}

	key, err := hkdf.Key(sha256.New, rootKey, nil, purpose, derivedKeySize)
	if err != nil {
		return nil, fmt.Errorf("dbkit: derive key: %w", err)
	}
	return key, nil
}
