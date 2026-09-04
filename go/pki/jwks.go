package pki

import (
	"context"
	"crypto/x509"
	"fmt"

	"github.com/go-jose/go-jose/v4"
)

// This file is round 3's JWKS-export half, docs/internal/22-pki.md's "JWKS
// export" section: two DISTINCT surfaces, deliberately not conflated --
//
//   - Service.ExportJWKS (key-lifecycle layer): the active/retiring public
//     keys of one purpose, for an EXTERNAL verifier of speed-issued
//     tokens.
//   - CAService.ExportAuthorityChainJWKS (X.509 layer): one authority's own
//     certificate chain, for the diagnosed system's own documented need --
//     pushing a jwks.json to a data-plane cluster.
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
// go-jose (github.com/go-jose/go-jose/v4) is this repository's own
// established JWK/JWKS encoder -- go/authn already depends on it
// transitively through golang-jwt (go/authn/go.mod), and it was already an
// INDIRECT dependency of this very module before this round, pulled in by
// go/pki/signer/vault's hashicorp/vault/api client (`go mod why -m
// github.com/go-jose/go-jose/v4` from this module's directory shows that
// exact chain). This round promotes it to a direct dependency of the ROOT
// package for the first time -- a consumer that imports only go/pki's root
// package (never go/pki/signer/vault) previously carried zero go-jose
// transitive dependencies, and will now carry go-jose itself. Measured with
// this repository's own required method (a throwaway module, `go mod tidy`
// under GOWORK=off): go-jose/go-jose/v4 v4.1.4 pulls in ZERO further
// indirect dependencies of its own -- the cost of this round's choice is
// exactly one direct entry, no transitive tail. Using go-jose rather than
// hand-rolling RFC 7517 JWK encoding (base64url field layout, the "kty"/
// "crv" discriminated union for OKP/Ed25519 keys, and so on) is exactly the
// case this codebase's own "check what's already available before adding a
// new dependency" rule anticipates: the library was already reachable, and
// the round's own "prove a JWKS response round-trips through a standard JWK
// parse" test requirement (jwks_test.go) is far stronger run against a
// well-established implementation's own parser than against a hand-rolled
// one asserting its own output.

// ExportJWKS exports purpose's active and retiring public keys as an RFC
// 7517 JSON Web Key Set -- the key-lifecycle layer's JWKS export. This is
// deliberately NOT what authn's own in-process token verification uses:
// docs/internal/22-pki.md's "JWKS export" section is explicit that adding a
// JWKS endpoint to authn is not what this method is for -- speed's access
// tokens are verified in-process via KeySource, and public keys travel
// through Service.VerificationKeys (service.go), never HTTP. ExportJWKS
// exists for a genuinely EXTERNAL consumer: any system, outside this
// deployment's own processes, that needs to independently verify a token
// this Service signed.
//
// Only SigningKeyStatusActive and SigningKeyStatusRetiring keys are
// included -- never SigningKeyStatusPending, unlike the internal
// verification path's ListVerifiableByPurpose. See
// SigningKeyRepository.ListByPurposeAndStatuses's own doc comment
// (repository.go) for why: a pending key is safe for THIS deployment's own
// replicas to trust ahead of its propagation window, but an external
// verifier has no relationship to that window at all and should not learn
// about a key this deployment has not started using yet.
//
// A purpose with no active or retiring key returns an empty key set
// ({"keys":[]}), never an error: a JWKS with zero keys is a legitimate, if
// unusual, answer, and returning an error here would give an external
// verifier no way to tell "not configured yet" apart from "you asked
// wrong".
func (s *Service) ExportJWKS(ctx context.Context, purpose string) (jose.JSONWebKeySet, error) {
	rows, err := s.signingKeys.ListByPurposeAndStatuses(ctx, purpose, SigningKeyStatusActive, SigningKeyStatusRetiring)
	if err != nil {
		return jose.JSONWebKeySet{}, err
	}

	keys := make([]jose.JSONWebKey, 0, len(rows))
	for _, row := range rows {
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
// authority's public key. This is the X.509 layer's JWKS export: the
// diagnosed system's own documented need, per docs/internal/22-pki.md's
// "JWKS export" section -- pushing a jwks.json to a data-plane cluster so
// it can validate JWTs (or certificates, by kid-matched public key) issued
// under this authority chain without a full X.509 path-validation library
// of its own.
//
// The chain walk is cycle-guarded the same way VerifyCertificate's is
// (revocation.go): Authority.ParentID values are application-generated and
// this round adds no database constraint preventing a corrupt cycle.
//
// ErrAuthorityNotFound if authorityID -- or any authority found while
// walking up its chain -- does not exist.
func (s *CAService) ExportAuthorityChainJWKS(ctx context.Context, authorityID string) (jose.JSONWebKeySet, error) {
	var keys []jose.JSONWebKey
	seen := make(map[string]bool)

	id := authorityID
	for id != "" {
		if seen[id] {
			return jose.JSONWebKeySet{}, fmt.Errorf("pki: authority chain cycle detected at %q", id)
		}
		seen[id] = true

		authority, err := s.authorities.FindByID(ctx, id)
		if err != nil {
			return jose.JSONWebKeySet{}, err
		}
		cert, err := parseCertificatePEM(authority.CertificatePEM)
		if err != nil {
			return jose.JSONWebKeySet{}, fmt.Errorf("pki: parse authority %q certificate: %w", id, err)
		}
		keys = append(keys, jose.JSONWebKey{
			Key:       cert.PublicKey,
			KeyID:     authority.ID,
			Algorithm: string(jose.EdDSA),
			Use:       "sig",
		})

		if authority.ParentID == nil {
			break
		}
		id = *authority.ParentID
	}
	return jose.JSONWebKeySet{Keys: keys}, nil
}
