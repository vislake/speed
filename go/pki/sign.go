package pki

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// SignCertificate signs message with the private key behind certificateID's
// certificate -- the certificate row's own KeyRef, held on this CAService's
// Signer the same way it was held at issuance (ca.go) -- for the tenant in
// ctx, and returns the raw signature bytes. It is the X.509 layer's
// signing-side counterpart of VerifyCertificate (revocation.go): the
// operation a certificate's holder (this platform, which never delivers the
// private key) performs with a certificate it has issued, exactly the shape
// the reference app's AI-output attestation consumer drives
// (examples/reference-app/internal/attestation).
//
// message's meaning is the Signer interface's own: for AlgorithmEd25519 --
// the only algorithm this layer issues (ca.go's CreateRootCA/
// CreateIntermediateCA/IssueCertificate) -- message is the COMPLETE message,
// never a digest; PureEdDSA hashes internally (Signer.Sign's own doc
// comment has the full argument). A caller that signs a structured document
// hands the document's canonical bytes over and later verifies them with
// the leaf certificate's public key through ed25519.Verify, exactly as the
// reference app's attestation gate does.
//
// # Refusals mirror VerifyCertificate's, on the signing side
//
// The method refuses before touching the Signer, in the same order
// VerifyCertificate refuses, with the same coded answers:
//
//   - a certificate row in CertificateStatusRevoked is refused with
//     ErrCertificateRevoked (param certificate_id), before anything else;
//   - an authority in the certificate's issuing chain -- its direct issuer,
//     or that issuer's own issuer, up to the root -- in
//     AuthorityStatusRevoked is refused with ErrCertificateRevoked (param
//     authority_id), through the same walkAuthorityChain (revocation.go)
//     VerifyCertificate's verification runs, so the "a revoked chain signs
//     nothing and vouches for nothing" property lives in the shared walk's
//     protected contract, never in a second hand-copy of the loop;
//   - a certificate outside its validity window at the service clock s.now()
//     (the same clock ExportAuthorityChainJWKS reads for its own validity
//     filter) is refused with an UNCODED error -- the module's standing
//     "expiry is not encoded" convention (revocation.go's VerifyCertificate
//     doc comment), which keeps this method's refusal vocabulary identical
//     to verification's.
//
// Chain-member validity is deliberately not checked here, mirroring
// issuance: CreateIntermediateCA and IssueCertificate do not consult their
// issuer's validity window before minting (ca.go), and verification -- the
// path that decides whether an already-signed message is trustworthy --
// enforces every chain member's window through crypto/x509's own path
// validation. Signing under an authority outside its window produces a
// signature every verifier will refuse by then, exactly as a certificate
// minted under one would.
//
// # Signer failures
//
// A Signer.Sign failure that is not already a coded *apperr.Error is
// wrapped as ErrSignerUnavailable, the identical treatment GenerateCRL
// applies to a CRL-signing failure (crl.go). Which coded failures can
// therefore surface depends on the Signer implementation's own answers:
// an unrecognized keyRef passes through as ErrKeyNotFound only from a
// Signer whose lookup reports it under that code -- LocalSigner's local
// keyring miss, and the empty-response shape the vault and kmsaws
// signers translate into ErrKeyNotFound when their backend reports no
// such key -- while the same keyRef reaching a vault or kmsaws backend
// that answers a raw API error is uncoded at the Signer and lands here
// as ErrSignerUnavailable. A backend that answers no signature bytes is
// the same class of infrastructure failure in both places. A KeyRef this
// CAService did not itself issue -- a row whose SignerName names a
// different Signer than s.signer's -- reaches that signer's own lookup
// and answers by those same rules, coded ErrKeyNotFound or
// ErrSignerUnavailable depending on the implementation, never a success:
// this CAService signs only what it issued (ca.go's "one Signer per
// CAService" note).
//
// ErrRecordNotFound (dbkit's own, via CertificateRepository.FindByID) when
// certificateID does not name a certificate of ctx's tenant; pkgcore's own
// fail-closed error when ctx carries no tenant at all -- the identical
// tenant contract IssueCertificate and RevokeCertificate enforce.
func (s *CAService) SignCertificate(ctx context.Context, certificateID string, message []byte) ([]byte, error) {
	cert, err := s.certificates.FindByID(ctx, certificateID)
	if err != nil {
		return nil, err
	}
	if cert.Status == CertificateStatusRevoked {
		return nil, ErrCertificateRevoked.WithParam("certificate_id", certificateID)
	}
	leaf, err := parseCertificatePEM(cert.CertificatePEM)
	if err != nil {
		return nil, fmt.Errorf("pki: parse certificate %q: %w", certificateID, err)
	}

	// The chain walk's refusal -- a revoked authority anywhere up to the
	// root -- is the signing-side half of the shared protected contract
	// walkAuthorityChain owns (revocation.go). visit is a no-op: signing
	// needs no pool-building, only the walk's refusal side effects, but the
	// walk is still the ONE place the cycle guard, the not-found answer and
	// the revoked-member refusal live.
	if walkErr := s.walkAuthorityChain(ctx, cert.AuthorityID, func(*Authority, *x509.Certificate) error { return nil }); walkErr != nil {
		return nil, walkErr
	}

	// The validity window is judged at the service clock, the same s.now()
	// seam ExportAuthorityChainJWKS reads (jwks.go) -- never a second
	// time.Now() call that tests could not pin. The refusal is deliberately
	// uncoded, per this module's expiry convention.
	if !validityWindowCovers(leaf.NotBefore, leaf.NotAfter, s.now()) {
		return nil, fmt.Errorf("pki: sign with certificate %q: certificate is outside its validity window at the service clock (not_before %s, not_after %s)",
			certificateID, leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}

	signature, err := s.signer.Sign(ctx, cert.KeyRef, message)
	if err != nil {
		if _, ok := apperr.As(err); ok {
			return nil, err
		}
		return nil, ErrSignerUnavailable.WithCause(err)
	}
	return signature, nil
}
