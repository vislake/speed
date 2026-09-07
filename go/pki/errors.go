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
// locales/{zh-CN,en-US}.toml, under the identical id.
//
// Round 1 declared the first four below. Round 3 (this round -- revocation,
// CRL generation, JWKS export) adds the remaining five:
//
//   - ErrCertificateRevoked and ErrPropagationWindowNotElapsed are exactly
//     the two of round 1/2's AGENTS.md's three reserved codes that this
//     round's own code paths genuinely trigger: VerifyCertificate for the
//     first (ca.go), Service.PromoteNow for the second (lifecycle.go) --
//     see PromoteNow's own doc comment for why a propagation-window guard
//     belongs to a manual promotion path rather than to revocation itself,
//     which is the deviation from round 1/2's AGENTS.md parenthetical
//     ("the propagation window") this file's own doc keeps honest about.
//   - ErrSignerUnavailable is the third reserved code, redirected: round
//     1/2's AGENTS.md parenthetically associated it with "a KMS-backed
//     signer" (round 4's vault/kmsaws). Round 3 gave it its first real
//     trigger -- GenerateCRL (crl.go) wraps a Signer.Sign failure that is
//     not already an *apperr.Error (LocalSigner's ErrKeyNotFound passes
//     through unwrapped) as ErrSignerUnavailable, since CRL signing is
//     exactly the revocation-adjacent path where a KMS-backed signer's
//     network failure would first surface. Round 3's comment predicted
//     "round 4's providers may adopt this code directly in a future edit";
//     the P2-3 closing round (2026-09-08) is that edit: kmsaws's
//     signDirect answers a KMS Sign that returns no Signature field with
//     ErrSignerUnavailable -- a signing backend that did not actually sign
//     (see its own doc comment and go/pki/signer.go's Sign contract
//     sentence). vault's own missing-signature-field answer stays an
//     unwrapped fmt.Errorf, exactly as this file's original accounting
//     recorded for both providers; what changed is that kmsaws now has a
//     coded reason to return this code, not just GenerateCRL's wrapping.
//   - ErrCRLNotGenerated is a new code this round adds outright, for the
//     CRL-fetch HTTP operation when GenerateCRL has never run for the
//     requested authority -- see crl.go.
//   - ErrInternal is the catch-all this round's HTTP Handler folds any
//     non-*apperr.Error failure into before writing a response body, the
//     same role every other module's own ErrInternal plays (see
//     storage.ErrInternal, notification.ErrInternal); no round before this
//     one needed one because no round before this one had an HTTP surface.
//   - ErrInvalidRequestBody and ErrRevocationReasonRequired are the two
//     request-validation codes this round's HTTP Handler answers with --
//     see decodeJSON and PkiRevokeSigningKey/PkiRevokeCertificate in
//     handler.go -- matching the identical storage.ErrInvalidRequestBody
//     sibling-module pattern.
//
// ErrAuthorityRevoked is the one code added after round 3 (the audit round
// that closed the issuance/verification asymmetry): round 3's verification
// path refused a revoked authority anywhere in a chain, but the issuance
// path never checked the signing authority's own status, so a revoked
// issuer could keep minting certificates every verifier rejects. Both
// CreateIntermediateCA and IssueCertificate now refuse that state with this
// code -- see its own comment in the var block below.
var (
	// ErrAuthorityNotFound reports that no authority with the requested id
	// exists -- CAService.CreateIntermediateCA and IssueCertificate's
	// parent lookup.
	ErrAuthorityNotFound = apperr.NotFound("pki.authority_not_found")

	// ErrKeyNotFound reports that a Signer implementation was asked to use
	// a keyRef it does not recognize (Sign, Public, Destroy).
	ErrKeyNotFound = apperr.NotFound("pki.key_not_found")

	// ErrNoActiveKey reports that Service.ActiveSigner was asked for a
	// purpose with no key it can currently sign with: no key is in
	// SigningKeyStatusActive (EnsurePurpose was never called for it, or
	// every key for it has been revoked with nothing yet promoted to
	// replace it), or the active key is outside its validity window --
	// before its NotBefore or past its NotAfter, the time x state cross
	// product keyInValidity (service.go) refuses at key-take time. The one
	// answer for all three leaves a caller nothing to distinguish; each is
	// repaired by the same path (EnsurePurpose's bootstrap or self-heal,
	// or the expiry scan's rotation).
	ErrNoActiveKey = apperr.NotFound("pki.no_active_key")

	// ErrAlgorithmUnsupportedBySigner reports that the requested algorithm
	// is not one a given Signer implementation can produce. LocalSigner
	// supports only AlgorithmEd25519 today, so this is the error every
	// other algorithm value gets from it; the code exists now because
	// GenerateKey's algorithm parameter already exists, even though the
	// case docs/internal/22-pki.md names it for -- AWS KMS asked for an
	// algorithm it does not support -- is round 4 territory.
	ErrAlgorithmUnsupportedBySigner = apperr.Invalid("pki.algorithm_unsupported_by_signer")

	// ErrCertificateRevoked reports that CAService.VerifyCertificate refused
	// a certificate because it -- or an authority in its chain up to the
	// root -- is CertificateStatusRevoked / AuthorityStatusRevoked, or that
	// CAService.ExportAuthorityChainJWKS refused an authority-chain export
	// because a member of the chain is AuthorityStatusRevoked. The
	// verification and export halves share the refusal through
	// walkAuthorityChain (revocation.go), the protected-contract chain walk
	// whose doc comment states which properties the two walks must keep
	// consistent; the authority_id param names the revoked member the walk
	// met, certificate_id the revoked certificate itself.
	// apperr.Conflict, not apperr.Invalid: the certificate's shape is fine,
	// its current state is what conflicts with the request to trust it,
	// the identical reasoning org.ErrInvitationRevoked already applies to
	// an accept against a revoked invitation.
	ErrCertificateRevoked = apperr.Conflict("pki.certificate_revoked")

	// ErrAuthorityRevoked reports that CAService.CreateIntermediateCA or
	// IssueCertificate was asked to sign under an authority whose Status --
	// the authority's own, or any ancestor's up to the root -- is
	// AuthorityStatusRevoked. An issuer that has been revoked (a
	// compromised key) must not keep minting certificates every downstream
	// verifier will reject, and the same holds for a chain containing a
	// revoked ancestor: issuance refuses the same chain-wide state the
	// verification path already refuses (revocation.go's VerifyCertificate).
	// apperr.Conflict, not apperr.Invalid, for the identical reason
	// ErrCertificateRevoked is: the request names a real authority, and it
	// is the authority's current state -- revoked -- that conflicts with
	// using it as an issuer. Where ErrCertificateRevoked answers "nothing
	// this authority already signed is trustable anymore", this code
	// answers "nothing NEW may be signed under it".
	ErrAuthorityRevoked = apperr.Conflict("pki.authority_revoked")

	// ErrSignerUnavailable reports that a Signer call failed because the
	// signing backend did not actually sign, with two triggers today:
	// GenerateCRL (crl.go) wraps a CRL-signing failure that is not itself a
	// coded *apperr.Error in this code, and kmsaws's signDirect
	// (go/pki/signer/kmsaws/signer.go) answers a KMS Sign whose response
	// carries no Signature field with it directly -- the provider adoption
	// this code's own round-3 history predicted. apperr.Internal, not
	// apperr.NotFound or apperr.Invalid: the caller did nothing wrong, the
	// signing backend did not answer, matching storage.ErrStoreUnavailable's
	// identical "the infrastructure seam failed" shape.
	ErrSignerUnavailable = apperr.Internal("pki.signer_unavailable")

	// ErrPropagationWindowNotElapsed reports that Service.PromoteNow was
	// asked to promote a purpose's pending key before propagationWindow has
	// elapsed since it was staged -- see PromoteNow's own doc comment.
	// apperr.Conflict: the request names a real pending key, but its
	// current age conflicts with the safety window PromoteDuePending would
	// otherwise wait out on its own schedule.
	ErrPropagationWindowNotElapsed = apperr.Conflict("pki.propagation_window_not_elapsed")

	// ErrCRLNotGenerated reports that the HTTP CRL-fetch operation was
	// asked for an authority whose CRLPEM is still empty -- GenerateCRL (or
	// the periodic pki.crl_regenerate job) has never run for it. See
	// crl.go.
	ErrCRLNotGenerated = apperr.NotFound("pki.crl_not_generated")

	// ErrInternal is the catch-all Handler folds any non-*apperr.Error
	// failure into before writing a response body -- see this var block's
	// own doc comment above.
	ErrInternal = apperr.Internal("pki.internal_error")

	// ErrInvalidRequestBody reports that decodeJSON failed to decode the
	// request body -- Handler's shared JSON-decode helper (handler.go).
	ErrInvalidRequestBody = apperr.Invalid("pki.invalid_request_body")

	// ErrRevocationReasonRequired reports that PkiRevokeSigningKey or
	// PkiRevokeCertificate was called with an empty Reason field
	// (handler.go).
	ErrRevocationReasonRequired = apperr.Invalid("pki.revocation_reason_required")
)

// errorCodes lists every code this module can return, in catalog order. It
// exists so errors_test.go can prove -- in code, not only by manual review
// of this file and the two locale files -- that every code declared here
// carries a matching locales/{zh-CN,en-US}.toml entry, and that neither
// locale file carries a message no code here returns. Keep this in step by
// hand: nothing generates it.
var errorCodes = []string{
	ErrAuthorityNotFound.Code,
	ErrKeyNotFound.Code,
	ErrNoActiveKey.Code,
	ErrAlgorithmUnsupportedBySigner.Code,
	ErrCertificateRevoked.Code,
	ErrAuthorityRevoked.Code,
	ErrSignerUnavailable.Code,
	ErrPropagationWindowNotElapsed.Code,
	ErrCRLNotGenerated.Code,
	ErrInternal.Code,
	ErrInvalidRequestBody.Code,
	ErrRevocationReasonRequired.Code,
}
