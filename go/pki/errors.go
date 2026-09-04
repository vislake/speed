package pki

import "github.com/vislake/speed/go/pkgcore/apperr"

// The error index of the pki module. Every exported error is an
// *apperr.Error builder whose Code follows the <module>.<reason> convention
// the backend coding standard requires: match a decorated error with
// apperr.As(err) and compare its Code, never with == or errors.Is against
// the var below. WithParam and WithCause derive a NEW *apperr.Error rather
// than mutating the receiver, so the pointer a call returns is never the
// pointer declared here -- the same convention dbkit, tenancy and org
// already document.
//
// Every code in this file has a matching description entry in
// locales/{zh-CN,en-US}.toml, under the identical id. Only the four codes
// this round's code paths can actually return are declared here --
// pki.certificate_revoked, pki.signer_unavailable and
// pki.propagation_window_not_elapsed belong to the rounds that implement
// revocation, KMS-backed signers and the propagation window, per this
// round's own scope boundary (docs/internal/22-pki.md's "error codes" list,
// round 1 subset).
var (
	// ErrAuthorityNotFound reports that no authority with the requested id
	// exists -- CAService.CreateIntermediateCA and IssueCertificate's
	// parent lookup.
	ErrAuthorityNotFound = apperr.NotFound("pki.authority_not_found")

	// ErrKeyNotFound reports that a Signer implementation was asked to use
	// a keyRef it does not recognize (Sign, Public, Destroy).
	ErrKeyNotFound = apperr.NotFound("pki.key_not_found")

	// ErrNoActiveKey reports that Service.ActiveSigner was asked for a
	// purpose with no key currently in SigningKeyStatusActive -- either
	// EnsurePurpose was never called for it, or (in a later round) every
	// key for it has been revoked with nothing yet promoted to replace it.
	ErrNoActiveKey = apperr.NotFound("pki.no_active_key")

	// ErrAlgorithmUnsupportedBySigner reports that the requested algorithm
	// is not one a given Signer implementation can produce. LocalSigner
	// supports only AlgorithmEd25519 today, so this is the error every
	// other algorithm value gets from it; the code exists now because
	// GenerateKey's algorithm parameter already exists, even though the
	// case docs/internal/22-pki.md names it for -- AWS KMS asked for an
	// algorithm it does not support -- is round 4 territory.
	ErrAlgorithmUnsupportedBySigner = apperr.Invalid("pki.algorithm_unsupported_by_signer")
)
