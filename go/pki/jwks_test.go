package pki

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// --- Service.ExportJWKS -------------------------------------------------

func TestService_ExportJWKS_ContainsOnlyActiveAndRetiring(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	for _, k := range []*SigningKey{
		newTestSigningKey("kid-active", "authn.access_token", SigningKeyStatusActive),
		newTestSigningKey("kid-retiring", "authn.access_token", SigningKeyStatusRetiring),
		newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending),
		newTestSigningKey("kid-revoked", "authn.access_token", SigningKeyStatusRevoked),
	} {
		// newTestSigningKey's fixture PublicKey is not valid DER -- ExportJWKS
		// must parse it, so seed real Ed25519 public keys here instead.
		pub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("ed25519.GenerateKey: %v", err)
		}
		der, err := marshalPKIXForTest(pub)
		if err != nil {
			t.Fatalf("marshal public key: %v", err)
		}
		k.PublicKey = der
		if err := svc.signingKeys.Create(ctx, k); err != nil {
			t.Fatalf("Create(%s): %v", k.ID, err)
		}
	}

	jwks, err := svc.ExportJWKS(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ExportJWKS: %v", err)
	}
	ids := map[string]bool{}
	for _, k := range jwks.Keys {
		ids[k.KeyID] = true
	}
	if len(ids) != 2 || !ids["kid-active"] || !ids["kid-retiring"] {
		t.Errorf("ExportJWKS kids = %v, want exactly kid-active and kid-retiring", ids)
	}
	if ids["kid-pending"] || ids["kid-revoked"] {
		t.Errorf("ExportJWKS included pending or revoked, want neither: %v", ids)
	}
}

// TestService_ExportJWKS_ExcludesKeysOutsideTheirValidityWindow pins the
// JWKS-publishing half of the validity-window enforcement: the published
// set filters by status AND by validity, so an active or retiring key
// whose NotAfter has passed is never offered to an external verifier,
// however long its row stays in that status between scan runs.
func TestService_ExportJWKS_ExcludesKeysOutsideTheirValidityWindow(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }

	seed := func(id, status string, notBefore, notAfter time.Time) {
		t.Helper()
		k := newTestSigningKey(id, "authn.access_token", status)
		pub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("ed25519.GenerateKey: %v", err)
		}
		der, err := marshalPKIXForTest(pub)
		if err != nil {
			t.Fatalf("marshal public key: %v", err)
		}
		k.PublicKey = der
		k.NotBefore = notBefore
		k.NotAfter = notAfter
		if err := svc.signingKeys.Create(ctx, k); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}
	// One in-validity active key and one retiring key whose NotAfter has
	// passed -- the exact shape an expired key left in the retiring set
	// by a scan that has not run yet takes.
	seed("kid-active-valid", SigningKeyStatusActive, base.Add(-time.Hour), base.Add(365*24*time.Hour))
	seed("kid-retiring-expired", SigningKeyStatusRetiring, base.Add(-2*time.Hour), base.Add(-time.Nanosecond))

	jwks, err := svc.ExportJWKS(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ExportJWKS: %v", err)
	}
	var got []string
	for _, k := range jwks.Keys {
		got = append(got, k.KeyID)
	}
	if len(got) != 1 || got[0] != "kid-active-valid" {
		t.Fatalf("ExportJWKS kids = %v, want exactly the in-validity active key kid-active-valid (regression: an expired key must not stay published)", got)
	}
}

func TestService_ExportJWKS_EmptyForUnknownPurpose(t *testing.T) {
	svc := newTestService(t)
	jwks, err := svc.ExportJWKS(context.Background(), "no.such.purpose")
	if err != nil {
		t.Fatalf("ExportJWKS: %v", err)
	}
	if jwks.Keys == nil {
		t.Error("ExportJWKS(unknown purpose).Keys is nil, want a non-nil empty slice")
	}
	if len(jwks.Keys) != 0 {
		t.Errorf("ExportJWKS(unknown purpose) = %d keys, want 0", len(jwks.Keys))
	}
}

// TestService_ExportJWKS_RoundTripsThroughStandardJWKParse proves the
// export's central requirement: a JWKS response round-trips through a
// standard JWK parse (go-jose's own JSONWebKeySet unmarshal, never a
// hand-rolled decoder), the parsed public key is byte-identical to the
// original, and it genuinely verifies a signature the corresponding
// private key produced. It also asserts the marshaled JSON never carries a
// private-key field ("d"), the "public keys only" requirement made
// concrete rather than merely assumed from the type system.
func TestService_ExportJWKS_RoundTripsThroughStandardJWKParse(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}
	kid, _, sign, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner: %v", err)
	}

	jwks, err := svc.ExportJWKS(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ExportJWKS: %v", err)
	}
	if len(jwks.Keys) != 1 {
		t.Fatalf("ExportJWKS = %d keys, want 1", len(jwks.Keys))
	}

	raw, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("json.Marshal(jwks): %v", err)
	}
	if strings.Contains(string(raw), `"d"`) {
		t.Fatalf("marshaled JWKS contains a private-key \"d\" field: %s", raw)
	}

	var parsed jose.JSONWebKeySet
	if err = json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("json.Unmarshal into jose.JSONWebKeySet: %v", err)
	}
	if len(parsed.Keys) != 1 || parsed.Keys[0].KeyID != kid {
		t.Fatalf("parsed JWKS = %+v, want one key with kid %q", parsed.Keys, kid)
	}
	pub, ok := parsed.Keys[0].Key.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("parsed key type = %T, want ed25519.PublicKey", parsed.Keys[0].Key)
	}

	sig, err := sign(ctx, []byte("hello"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !ed25519.Verify(pub, []byte("hello"), sig) {
		t.Error("the JWKS-round-tripped public key does not verify a signature the corresponding private key produced")
	}
}

// --- CAService.ExportAuthorityChainJWKS ----------------------------------

func TestCAService_ExportAuthorityChainJWKS_ChainOrder(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	intermediate, _ := issueTestCertificate(t, ca, ctx)
	root := mustFindParentAuthority(t, ca, ctx, intermediate)

	jwks, err := ca.ExportAuthorityChainJWKS(ctx, intermediate.ID)
	if err != nil {
		t.Fatalf("ExportAuthorityChainJWKS: %v", err)
	}
	if len(jwks.Keys) != 2 {
		t.Fatalf("ExportAuthorityChainJWKS = %d keys, want 2 (intermediate + root)", len(jwks.Keys))
	}
	if jwks.Keys[0].KeyID != intermediate.ID {
		t.Errorf("Keys[0].KeyID = %q, want the intermediate %q first", jwks.Keys[0].KeyID, intermediate.ID)
	}
	if jwks.Keys[1].KeyID != root.ID {
		t.Errorf("Keys[1].KeyID = %q, want the root %q last", jwks.Keys[1].KeyID, root.ID)
	}
}

func TestCAService_ExportAuthorityChainJWKS_RootOnly(t *testing.T) {
	ca := newTestCAService(t)
	ctx := context.Background()
	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}

	jwks, err := ca.ExportAuthorityChainJWKS(ctx, root.ID)
	if err != nil {
		t.Fatalf("ExportAuthorityChainJWKS: %v", err)
	}
	if len(jwks.Keys) != 1 || jwks.Keys[0].KeyID != root.ID {
		t.Errorf("ExportAuthorityChainJWKS(root only) = %+v, want exactly one key for %q", jwks.Keys, root.ID)
	}
}

func TestCAService_ExportAuthorityChainJWKS_AuthorityNotFound(t *testing.T) {
	ca := newTestCAService(t)
	if _, err := ca.ExportAuthorityChainJWKS(context.Background(), "does-not-exist"); !apperrIs(err, ErrAuthorityNotFound) {
		t.Errorf("ExportAuthorityChainJWKS(missing authority) error = %v, want ErrAuthorityNotFound", err)
	}
}

// TestCAService_ExportAuthorityChainJWKS_RevokedAuthorityInChain_Refused
// proves the export refuses an AuthorityStatusRevoked member anywhere in
// the chain, exactly as VerifyCertificate refuses one at every hop of its
// chain walk: ExportAuthorityChainJWKS is the document a data-plane
// cluster with no X.509 path-validation library kid-matches against, which
// makes the export the ONLY possible enforcement point for revocation -- a
// revoked authority's public key must not stay in refreshed documents, or
// a data plane that has already pulled one keeps accepting signatures made
// with the revoked key. The revoked row is seeded directly through the
// repository, the identical precedent revocation_test.go's own
// chain-refusal test sets (no method in this module's public API writes
// AuthorityStatusRevoked).
func TestCAService_ExportAuthorityChainJWKS_RevokedAuthorityInChain_Refused(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	authority, _ := issueTestCertificate(t, ca, ctx)

	authority.Status = AuthorityStatusRevoked
	if err := ca.authorities.Update(ctx, authority); err != nil {
		t.Fatalf("seed revoked authority: %v", err)
	}

	_, err := ca.ExportAuthorityChainJWKS(ctx, authority.ID)
	ae, _ := apperr.As(err)
	if !apperr.HasCode(err, ErrCertificateRevoked.Code) {
		t.Errorf("ExportAuthorityChainJWKS(revoked authority) error = %v, want ErrCertificateRevoked (the revoked public key must never be exported)", err)
		return
	}
	if ae.Params["authority_id"] != authority.ID {
		t.Errorf("authority_id param = %v, want the revoked authority %q", ae.Params["authority_id"], authority.ID)
	}
}

// TestCAService_ExportAuthorityChainJWKS_RevokedRootAncestor_Refused is the
// same regression one hop up the chain: the walk must refuse a revoked
// ANCESTOR of the exported authority too, the identical whole-chain answer
// VerifyCertificate gives for a certificate under the same chain -- the
// root revoked while the exported intermediate stays AuthorityStatusActive,
// the same shape ca_test.go's issuance-side root-ancestor pair seeds.
func TestCAService_ExportAuthorityChainJWKS_RevokedRootAncestor_Refused(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	root, intermediate := issueRootAndIntermediate(t, ca, ctx)

	root.Status = AuthorityStatusRevoked
	if err := ca.authorities.Update(ctx, root); err != nil {
		t.Fatalf("seed revoked root ancestor: %v", err)
	}

	_, err := ca.ExportAuthorityChainJWKS(ctx, intermediate.ID)
	ae, _ := apperr.As(err)
	if !apperr.HasCode(err, ErrCertificateRevoked.Code) {
		t.Errorf("ExportAuthorityChainJWKS(active intermediate under revoked root) error = %v, want ErrCertificateRevoked naming the revoked root", err)
		return
	}
	if ae.Params["authority_id"] != root.ID {
		t.Errorf("authority_id param = %v, want the revoked root %q, not the active intermediate", ae.Params["authority_id"], root.ID)
	}
}

// TestCAService_ExportAuthorityChainJWKS_ExcludesAuthoritiesOutsideTheirValidityWindow
// pins the X.509 export's own half of the validity-window enforcement, the
// twin of TestService_ExportJWKS_ExcludesKeysOutsideTheirValidityWindow
// above: an authority whose certificate's validity window has closed at the
// export clock is excluded from the document -- never published for an
// external verifier to trust -- the exact keyInValidity boundary the
// signing-key export applies to an expired key. Nothing in the module ever
// reaps an out-of-validity authority row (authorities have no expiry-driven
// lifecycle), so this read path is the only enforcement point.
func TestCAService_ExportAuthorityChainJWKS_ExcludesAuthoritiesOutsideTheirValidityWindow(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	base := time.Now()

	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: base.Add(7 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}
	intermediate, err := ca.CreateIntermediateCA(ctx, root.ID, IntermediateCAParams{
		Subject:  pkix.Name{CommonName: "speed Intermediate CA"},
		NotAfter: base.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateIntermediateCA: %v", err)
	}

	// Advance the service clock past the intermediate's NotAfter but not the
	// root's -- the same clock-seam pattern the signing-key twin test uses.
	ca.now = func() time.Time { return base.Add(48 * time.Hour) }

	jwks, err := ca.ExportAuthorityChainJWKS(ctx, intermediate.ID)
	if err != nil {
		t.Fatalf("ExportAuthorityChainJWKS: %v", err)
	}
	var got []string
	for _, k := range jwks.Keys {
		got = append(got, k.KeyID)
	}
	if len(got) != 1 || got[0] != root.ID {
		t.Fatalf("ExportAuthorityChainJWKS kids = %v, want exactly the in-validity root %q (regression: an out-of-validity authority must not stay published)", got, root.ID)
	}
}

// TestCAService_ExportAuthorityChainJWKS_RoundTripsThroughStandardJWKParse
// mirrors the key-lifecycle layer's identical proof above, for the X.509
// layer's own export.
func TestCAService_ExportAuthorityChainJWKS_RoundTripsThroughStandardJWKParse(t *testing.T) {
	ca := newTestCAService(t)
	ctx := context.Background()
	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}

	jwks, err := ca.ExportAuthorityChainJWKS(ctx, root.ID)
	if err != nil {
		t.Fatalf("ExportAuthorityChainJWKS: %v", err)
	}

	raw, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("json.Marshal(jwks): %v", err)
	}
	if strings.Contains(string(raw), `"d"`) {
		t.Fatalf("marshaled JWKS contains a private-key \"d\" field: %s", raw)
	}

	var parsed jose.JSONWebKeySet
	if err = json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("json.Unmarshal into jose.JSONWebKeySet: %v", err)
	}
	if len(parsed.Keys) != 1 || parsed.Keys[0].KeyID != root.ID {
		t.Fatalf("parsed JWKS = %+v, want one key for %q", parsed.Keys, root.ID)
	}
	authorityCert, err := parseCertificatePEM(root.CertificatePEM)
	if err != nil {
		t.Fatalf("parse authority certificate: %v", err)
	}
	pub, ok := parsed.Keys[0].Key.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("parsed key type = %T, want ed25519.PublicKey", parsed.Keys[0].Key)
	}
	wantPub, ok := authorityCert.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("authority certificate's own PublicKey type = %T, want ed25519.PublicKey", authorityCert.PublicKey)
	}
	if !pub.Equal(wantPub) {
		t.Error("the JWKS-round-tripped public key does not match the authority certificate's own public key")
	}
}

// mustFindParentAuthority looks up child's parent Authority row directly
// through the repository -- a small test-only convenience since none of
// this file's exported methods return the parent alongside a chain walk.
func mustFindParentAuthority(t *testing.T, ca *CAService, ctx context.Context, child *Authority) *Authority {
	t.Helper()
	if child.ParentID == nil {
		t.Fatalf("mustFindParentAuthority: %q has no parent", child.ID)
	}
	parent, err := ca.authorities.FindByID(ctx, *child.ParentID)
	if err != nil {
		t.Fatalf("FindByID(parent): %v", err)
	}
	return parent
}

// marshalPKIXForTest DER-encodes pub the same way SigningKey.PublicKey
// stores it (x509.MarshalPKIXPublicKey), local to this test file since no
// production code needs to do this outside EnsurePurpose/stageRotation
// themselves.
func marshalPKIXForTest(pub ed25519.PublicKey) ([]byte, error) {
	return x509.MarshalPKIXPublicKey(pub)
}
