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
//     signer" (round 4's vault/kmsaws), but neither of those packages
//     declares or returns it -- their Sign/Public/Destroy failures are
//     unwrapped fmt.Errorf, verified by grep against
//     go/pki/signer/{vault,kmsaws}/signer.go before writing this comment.
//     This round gives it a real, first trigger instead: GenerateCRL
//     (crl.go) wraps a Signer.Sign failure that is not already an
//     *apperr.Error (LocalSigner's ErrKeyNotFound passes through
//     unwrapped) as ErrSignerUnavailable, since CRL signing is exactly the
//     revocation-adjacent path where a KMS-backed signer's network failure
//     would first surface. Round 4's providers may adopt this code
//     directly in a future edit; nothing here requires them to.
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

	// ErrCertificateRevoked reports that CAService.VerifyCertificate refused
	// a certificate because it -- or an authority in its chain up to the
	// root -- is CertificateStatusRevoked / AuthorityStatusRevoked.
	// apperr.Conflict, not apperr.Invalid: the certificate's shape is fine,
	// its current state is what conflicts with the request to trust it,
	// the identical reasoning org.ErrInvitationRevoked already applies to
	// an accept against a revoked invitation.
	ErrCertificateRevoked = apperr.Conflict("pki.certificate_revoked")

	// ErrSignerUnavailable reports that a Signer call this module made on
	// the caller's behalf failed for a reason that is not itself a coded
	// *apperr.Error -- today, only GenerateCRL's CRL-signing call (crl.go).
	// apperr.Internal, not apperr.NotFound or apperr.Invalid: the caller
	// did nothing wrong, the signing backend did not answer, matching
	// storage.ErrStoreUnavailable's identical "the infrastructure seam
	// failed" shape.
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
	ErrSignerUnavailable.Code,
	ErrPropagationWindowNotElapsed.Code,
	ErrCRLNotGenerated.Code,
	ErrInternal.Code,
	ErrInvalidRequestBody.Code,
	ErrRevocationReasonRequired.Code,
}
