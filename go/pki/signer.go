package pki

import (
	"context"
	"crypto"
)

// Signer performs signing operations against a key this module manages. It
// deliberately does NOT expose a way to read a private key back out --
// see the package doc comment, and docs/internal/22-pki.md's "Signer seam"
// section, for why a Protect(key)/Unprotect(ref) shape was considered and
// rejected: that shape means the private key must exist in plaintext inside
// this process's memory, which defeats the entire point of a KMS-backed
// implementation.
//
// keyRef is an opaque handle, never key material. Business tables
// (pki_signing_keys, pki_authorities, pki_certificates) store only keyRef
// and the owning Signer's name -- never a private key, in any form.
//
// This interface deliberately does not reuse the standard library's
// crypto.Signer: that type's Sign method takes no context.Context. For
// LocalSigner that is irrelevant, but a KMS-backed implementation's Sign is
// a network call on a path a login walks through -- without a context there
// is no way to carry a timeout, a cancellation, or a trace span, none of
// which are optional on that path.
//
// The shape accommodates two different implementation strategies without
// choosing between them: an envelope mode (the real private key lives
// encrypted somewhere else, decrypted into memory only for the duration of
// one Sign call) and a direct-sign mode (the private key is generated
// inside, and never leaves, an external service; every Sign call is an API
// round trip). LocalSigner is neither -- its "envelope" is dbkit's own
// field-level encryption and the decrypted key lives only inside one Sign
// call's stack, but the boundary it protects is a database column, not a
// separate service. The vault and kmsaws implementations (round 4) are true
// envelope and direct-sign implementations respectively.
type Signer interface {
	// GenerateKey creates a new key for algorithm and returns an opaque
	// keyRef plus its public key. algorithm is one of the Algorithm
	// constants; an implementation that cannot produce it returns
	// ErrAlgorithmUnsupportedBySigner.
	GenerateKey(ctx context.Context, algorithm string) (keyRef string, public crypto.PublicKey, err error)

	// Sign signs input with the key named by keyRef and returns the raw
	// signature bytes.
	//
	// input's meaning is algorithm-dependent and this interface does not
	// normalize it to "always a digest": for AlgorithmEd25519, input is the
	// COMPLETE message -- PureEdDSA hashes internally, and both this
	// package's own crypto/ed25519 usage and every KMS/HSM's Ed25519 mode
	// (Vault Transit's "ed25519"; AWS KMS's ED25519_SHA_512 with
	// MessageType RAW) share that same convention, which is also exactly
	// RFC 8037's JWT "EdDSA". A future non-EdDSA algorithm (ecdsa-p256, if
	// one is ever added) would instead take the message's SHA-256 digest --
	// see docs/internal/22-pki.md's Signer section for the full table.
	// Getting this wrong for AWS KMS is not cosmetic: MessageType RAW vs.
	// DIGEST select genuinely different signature algorithms
	// (ED25519_SHA_512 vs. ED25519_PH_SHA_512), and a signature made under
	// the wrong one does not verify.
	//
	// ErrKeyNotFound if keyRef is not recognized.
	Sign(ctx context.Context, keyRef string, input []byte) ([]byte, error)

	// Public returns the public key for keyRef. ErrKeyNotFound if keyRef is
	// not recognized.
	Public(ctx context.Context, keyRef string) (crypto.PublicKey, error)

	// Destroy permanently removes the key named by keyRef.
	// ErrKeyNotFound if keyRef is not recognized.
	Destroy(ctx context.Context, keyRef string) error
}
