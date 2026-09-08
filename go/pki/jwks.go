package pki

import (
	"context"
	"crypto/x509"
	"fmt"

	"github.com/go-jose/go-jose/v4"
)

// This file carries the JWKS-export half of the module's two JWKS surfaces,
// deliberately not conflated:
//
//   - Service.ExportJWKS (key-lifecycle layer): the active/retiring public
//     keys of one purpose, for an EXTERNAL verifier of speed-issued
//     tokens.
//   - CAService.ExportAuthorityChainJWKS (X.509 layer): one authority's own
//     certificate chain, for a consumer that pushes a jwks.json to a
//     data-plane cluster.
//
// Both return ONLY public keys, never a private key or key reference, even
// for a Signer capable of direct-sign mode: nothing in either export path
// ever calls Signer.Public or reads anything beyond the PublicKey/
// CertificatePEM columns this module already stores unencrypted. This is
// not "the signer refused" -- it is structural: neither method's return
// type (jose.JSONWebKeySet built from x509.ParsePKIXPublicKey/
// x509.Certificate.PublicKey) has anywhere to put a private key even if one
// were fetched.
//
// go-jose (github.com/go-jose/go-jose/v4) is this repository's established
// JWK/JWKS encoder -- go/authn already depends on it transitively through
// golang-jwt, and go/pki/signer/vault's hashicorp/vault/api client pulls it
// in as well -- and the root package declares it as a direct dependency.
// Measured with this repository's required method (a throwaway module,
// `go mod tidy` under GOWORK=off): go-jose pulls in no further indirect
// dependencies of its own, so the cost is exactly one direct entry. Using
// go-jose rather than hand-rolling RFC 7517 JWK encoding (base64url field
// layout, the "kty"/"crv" discriminated union for OKP/Ed25519 keys, and so
// on) is exactly the case this codebase's own "check what's already
// available before adding a new dependency" rule anticipates, and proving a
// JWKS response round-trips through a well-established implementation's own
// parser (jwks_test.go) is far stronger than asserting a hand-rolled
// encoder's own output.

// ExportJWKS exports purpose's active and retiring public keys as an RFC
// 7517 JSON Web Key Set -- the key-lifecycle layer's JWKS export. This is
// deliberately NOT what authn's own in-process token verification uses:
// speed's access tokens are verified in-process via KeySource, and public
// keys travel through Service.VerificationKeys (service.go), never HTTP.
// ExportJWKS exists for a genuinely EXTERNAL consumer: any system, outside
// this deployment's own processes, that needs to independently verify a
// token this Service signed.
//
// Only SigningKeyStatusActive and SigningKeyStatusRetiring keys that are
// also WITHIN their validity window at this instant are included -- never
// SigningKeyStatusPending, unlike the internal verification path's
// ListVerifiableByPurpose, and never a key whose NotAfter has passed: an
// external verifier has no relationship to this deployment's expiry scan
// at all, so an expired key left in the active/retiring set by a scan that
// has not run yet must not stay published for verifiers to trust. See
// SigningKeyRepository.ListByPurposeAndStatuses's own doc comment
// (repository.go) for the status half, and keyInValidity (service.go) for
// the boundary.
//
// A purpose with no in-validity active or retiring key returns an empty
// key set ({"keys":[]}), never an error: a JWKS with zero keys is a
// legitimate, if unusual, answer, and returning an error here would give
// an external verifier no way to tell "not configured yet" apart from
// "you asked wrong".
func (s *Service) ExportJWKS(ctx context.Context, purpose string) (jose.JSONWebKeySet, error) {
	rows, err := s.signingKeys.ListByPurposeAndStatuses(ctx, purpose, SigningKeyStatusActive, SigningKeyStatusRetiring)
	if err != nil {
		return jose.JSONWebKeySet{}, err
	}

	now := s.now()
	keys := make([]jose.JSONWebKey, 0, len(rows))
	for _, row := range rows {
		if !keyInValidity(row, now) {
			continue
		}
		pub, err := x509.ParsePKIXPublicKey(row.PublicKey)
		if err != nil {
			return jose.JSONWebKeySet{}, fmt.Errorf("pki: parse public key for kid %q: %w", row.ID, err)
		}
		keys = append(keys, jose.JSONWebKey{
			Key:       pub,
			KeyID:     row.ID,
			Algorithm: string(jose.EdDSA),
			Use:       "sig",
		})
	}
	return jose.JSONWebKeySet{Keys: keys}, nil
}

// ExportAuthorityChainJWKS exports authorityID's own certificate chain --
// authorityID itself, then its issuer, then that issuer's own issuer, up to
// and including the root -- as an RFC 7517 JSON Web Key Set, one JWK per
// authority keyed by its Authority.ID, each entry carrying only that
// authority's public key. This is the X.509 layer's JWKS export, for a
// data-plane cluster that validates JWTs (or certificates, by kid-matched
// public key) issued under this authority chain without a full X.509
// path-validation library of its own.
//
// The chain is walked through walkAuthorityChain (revocation.go) -- the
// SAME walk VerifyCertificate uses, not a hand-copy -- whose doc comment
// carries the protected-contract statement of which properties the two
// chain walks must keep consistent. Its two load-bearing consequences for
// this export:
//
//   - A chain containing an AuthorityStatusRevoked member -- authorityID
//     itself or any ancestor up to the root -- is refused wholesale with
//     ErrCertificateRevoked naming the revoked member's authority_id, the
//     identical answer VerifyCertificate gives for a certificate under the
//     same chain: the export never serves a document for a chain the
//     module's own verifier refuses, so a data plane that refreshes from
//     this document can never be handed a revoked authority's public key.
//     (A data plane validating by kid-matched public key has no other
//     enforcement point: the revoked key must stop appearing in every
//     refreshed document the moment its compromise is believed.)
//   - A member whose own certificate is outside its validity window at
//     this instant -- judged against the parsed certificate's own
//     NotBefore/NotAfter through validityWindowCovers (service.go), the
//     same [NotBefore, NotAfter] boundary crypto/x509 applies and the
//     key-lifecycle ExportJWKS applies to signing keys -- is excluded from
//     the document rather than refused wholesale: like the signing keys
//     twin above, the export prunes a member the current time has left
//     behind and keeps the members it can still vouch for. The verifier
//     refuses such a chain wholesale (x509 validates every member's
//     window, root included), but that granularity difference never
//     extends to vouching for the out-of-validity member itself.
//
// An excluded member's key is simply absent; a chain whose members are all
// healthy exports unchanged. ErrAuthorityNotFound if authorityID -- or any
// authority found while walking up its chain -- does not exist.
func (s *CAService) ExportAuthorityChainJWKS(ctx context.Context, authorityID string) (jose.JSONWebKeySet, error) {
	keys := make([]jose.JSONWebKey, 0)
	now := s.now()
	if err := s.walkAuthorityChain(ctx, authorityID, func(authority *Authority, cert *x509.Certificate) error {
		if !validityWindowCovers(cert.NotBefore, cert.NotAfter, now) {
			// Out-of-validity member: excluded, exactly as ExportJWKS
			// excludes an out-of-validity signing key (keyInValidity,
			// service.go) -- never published for an external verifier to
			// trust. The walk continues to this member's issuer.
			return nil
		}
		keys = append(keys, jose.JSONWebKey{
			Key:       cert.PublicKey,
			KeyID:     authority.ID,
			Algorithm: string(jose.EdDSA),
			Use:       "sig",
		})
		return nil
	}); err != nil {
		return jose.JSONWebKeySet{}, err
	}
	return jose.JSONWebKeySet{Keys: keys}, nil
}
