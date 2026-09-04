package pki

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

func newTestCAService(t *testing.T) *CAService {
	t.Helper()
	db := newTestDB(t)
	signer := NewLocalSigner(db)
	return NewCAService(signer, "local", NewAuthorityRepository(db), NewCertificateRepository(db))
}

func TestNewSerialNumber_Is16BytesAndUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		serial, err := newSerialNumber()
		if err != nil {
			t.Fatalf("newSerialNumber: %v", err)
		}
		hex := serialHex(serial)
		if seen[hex] {
			t.Fatalf("newSerialNumber produced a duplicate: %s", hex)
		}
		seen[hex] = true
		// The top bit is cleared, so the encoded byte length is at most
		// serialNumberBytes -- see newSerialNumber's own doc comment.
		if len(serial.Bytes()) > serialNumberBytes {
			t.Errorf("serial %s: %d bytes, want at most %d", hex, len(serial.Bytes()), serialNumberBytes)
		}
	}
}

func TestCAService_CreateRootCA_IssuesASelfSignedCertificate(t *testing.T) {
	ca := newTestCAService(t)
	ctx := context.Background()

	authority, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}
	if authority.Type != AuthorityTypeRoot {
		t.Errorf("Type = %q, want %q", authority.Type, AuthorityTypeRoot)
	}
	if authority.ParentID != nil {
		t.Errorf("ParentID = %v, want nil for a root authority", authority.ParentID)
	}

	cert, err := parseCertificatePEM(authority.CertificatePEM)
	if err != nil {
		t.Fatalf("parseCertificatePEM: %v", err)
	}
	if !cert.IsCA {
		t.Errorf("root certificate IsCA = false, want true")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("root certificate KeyUsage lacks KeyUsageCertSign")
	}
	// Self-signed: verifying the certificate against itself as its own
	// issuer must succeed.
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Errorf("self-signed root certificate does not verify against itself: %v", err)
	}
	if _, ok := cert.PublicKey.(ed25519.PublicKey); !ok {
		t.Errorf("root certificate public key type = %T, want ed25519.PublicKey", cert.PublicKey)
	}
}

func TestCAService_CreateIntermediateCA_SignedByTheRoot(t *testing.T) {
	ca := newTestCAService(t)
	ctx := context.Background()

	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}

	intermediate, err := ca.CreateIntermediateCA(ctx, root.ID, IntermediateCAParams{
		Subject:  pkix.Name{CommonName: "speed Intermediate CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateIntermediateCA: %v", err)
	}
	if intermediate.Type != AuthorityTypeIntermediate {
		t.Errorf("Type = %q, want %q", intermediate.Type, AuthorityTypeIntermediate)
	}
	if intermediate.ParentID == nil || *intermediate.ParentID != root.ID {
		t.Errorf("ParentID = %v, want %q", intermediate.ParentID, root.ID)
	}

	rootCert, err := parseCertificatePEM(root.CertificatePEM)
	if err != nil {
		t.Fatalf("parse root cert: %v", err)
	}
	intermediateCert, err := parseCertificatePEM(intermediate.CertificatePEM)
	if err != nil {
		t.Fatalf("parse intermediate cert: %v", err)
	}
	if !intermediateCert.IsCA {
		t.Errorf("intermediate certificate IsCA = false, want true")
	}

	pool := x509.NewCertPool()
	pool.AddCert(rootCert)
	if _, err := intermediateCert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Errorf("intermediate certificate does not chain to the root: %v", err)
	}
}

func TestCAService_CreateIntermediateCA_UnknownParent(t *testing.T) {
	ca := newTestCAService(t)
	_, err := ca.CreateIntermediateCA(context.Background(), "does-not-exist", IntermediateCAParams{
		Subject:  pkix.Name{CommonName: "orphan"},
		NotAfter: time.Now().Add(time.Hour),
	})
	if !apperrIs(err, ErrAuthorityNotFound) {
		t.Errorf("CreateIntermediateCA(unknown parent) error = %v, want ErrAuthorityNotFound", err)
	}
}

func TestCAService_IssueCertificate_FullChain(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	root, err := ca.CreateRootCA(context.Background(), RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(72 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}
	intermediate, err := ca.CreateIntermediateCA(context.Background(), root.ID, IntermediateCAParams{
		Subject:  pkix.Name{CommonName: "speed Intermediate CA"},
		NotAfter: time.Now().Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateIntermediateCA: %v", err)
	}

	cert, err := ca.IssueCertificate(ctx, intermediate.ID, CertificateParams{
		Purpose:  "tenant.jwt_signing",
		Subject:  pkix.Name{CommonName: "acme.speed.internal"},
		DNSNames: []string{"acme.speed.internal"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("IssueCertificate: %v", err)
	}
	if cert.AuthorityID != intermediate.ID {
		t.Errorf("AuthorityID = %q, want %q", cert.AuthorityID, intermediate.ID)
	}
	if cert.KeyDelivered {
		t.Errorf("KeyDelivered = true, want false for a freshly issued certificate")
	}
	if cert.TenantID != "tenant-acme" {
		t.Errorf("TenantID = %q, want %q", cert.TenantID, "tenant-acme")
	}

	rootCert, err := parseCertificatePEM(root.CertificatePEM)
	if err != nil {
		t.Fatalf("parse root cert: %v", err)
	}
	intermediateCert, err := parseCertificatePEM(intermediate.CertificatePEM)
	if err != nil {
		t.Fatalf("parse intermediate cert: %v", err)
	}
	endEntityCert, err := parseCertificatePEM(cert.CertificatePEM)
	if err != nil {
		t.Fatalf("parse end-entity cert: %v", err)
	}
	if endEntityCert.IsCA {
		t.Errorf("end-entity certificate IsCA = true, want false")
	}

	roots := x509.NewCertPool()
	roots.AddCert(rootCert)
	intermediates := x509.NewCertPool()
	intermediates.AddCert(intermediateCert)
	if _, err := endEntityCert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates}); err != nil {
		t.Errorf("end-entity certificate does not chain to the root through the intermediate: %v", err)
	}
}

func TestCAService_IssueCertificate_RequiresTenant(t *testing.T) {
	ca := newTestCAService(t)

	root, err := ca.CreateRootCA(context.Background(), RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}

	_, err = ca.IssueCertificate(context.Background(), root.ID, CertificateParams{
		Purpose:  "tenant.jwt_signing",
		Subject:  pkix.Name{CommonName: "no-tenant"},
		NotAfter: time.Now().Add(time.Hour),
	})
	if err == nil {
		t.Fatalf("IssueCertificate(no tenant in ctx) succeeded, want pkgcore.ErrNoTenant")
	}
}
