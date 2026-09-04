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
// round's explicit requirement: a JWKS response round-trips through a
// standard JWK parse (go-jose's own JSONWebKeySet unmarshal, never a
// hand-rolled decoder), the parsed public key is byte-identical to the
// original, and it genuinely verifies a signature the corresponding
// private key produced. It also asserts the marshaled JSON never carries a
// private-key field ("d"), the round's "public keys only" requirement made
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
