package pki

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TestCAService_SignCertificate_SignsAndSignatureVerifies proves the
// signing side of the issue -> sign -> verify loop SignCertificate exists
// for: a signature over a message, made with an issued certificate's own
// key through the Signer seam, verifies against the certificate's public
// key exactly as an independent verifier -- the standard library's
// ed25519.Verify over the leaf certificate's public key -- would check it.
func TestCAService_SignCertificate_SignsAndSignatureVerifies(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)

	message := []byte(`{"object_id":"obj-1","content_sha256":"abc"}`)
	signature, err := ca.SignCertificate(ctx, cert.ID, message)
	if err != nil {
		t.Fatalf("SignCertificate: %v", err)
	}
	if len(signature) == 0 {
		t.Fatal("SignCertificate returned an empty signature")
	}

	leaf, err := parseCertificatePEM(cert.CertificatePEM)
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("leaf public key type = %T, want ed25519.PublicKey", leaf.PublicKey)
	}
	if !ed25519.Verify(pub, message, signature) {
		t.Errorf("signature does not verify against the certificate's own public key")
	}

	// A different message must not verify under the same signature -- the
	// signature is over the exact message bytes, never a hash or prefix a
	// second document could share.
	if ed25519.Verify(pub, append(message, '!'), signature) {
		t.Errorf("signature verifies against a DIFFERENT message -- the signer bound nothing to the message")
	}
}

// TestCAService_SignCertificate_RequiresTenant pins the tenant contract:
// SignCertificate reads a pki_certificates row, which is tenant data, so a
// context with no tenant fails closed before anything is signed -- the
// identical rule IssueCertificate and RevokeCertificate enforce.
func TestCAService_SignCertificate_RequiresTenant(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)

	if _, err := ca.SignCertificate(context.Background(), cert.ID, []byte("message")); err == nil {
		t.Fatalf("SignCertificate(no tenant in ctx) succeeded, want pkgcore.ErrNoTenant")
	}
}

// TestCAService_SignCertificate_CertificateNotFound pins the not-found
// answer: a certificate id that names no certificate of ctx's tenant is an
// error, never a signature.
func TestCAService_SignCertificate_CertificateNotFound(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	if _, err := ca.SignCertificate(ctx, "does-not-exist", []byte("message")); err == nil {
		t.Fatalf("SignCertificate(missing certificate) succeeded, want an error")
	}
}

// TestCAService_SignCertificate_RevokedCertificate_Refused pins the first
// refusal VerifyCertificate applies, mirrored on the signing side: a
// revoked certificate's key signs nothing, answered with the coded
// ErrCertificateRevoked before the Signer is ever touched.
func TestCAService_SignCertificate_RevokedCertificate_Refused(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)
	if _, err := ca.RevokeCertificate(ctx, cert.ID, "compromised"); err != nil {
		t.Fatalf("RevokeCertificate: %v", err)
	}

	if _, err := ca.SignCertificate(ctx, cert.ID, []byte("message")); !apperr.HasCode(err, ErrCertificateRevoked.Code) {
		t.Errorf("SignCertificate(revoked certificate) error = %v, want ErrCertificateRevoked", err)
	}
}

// TestCAService_SignCertificate_RevokedAuthorityInChain_Refused pins the
// second refusal: a revoked authority anywhere in the certificate's issuing
// chain -- here the direct issuer, seeded through the repository exactly as
// VerifyCertificate's own chain-refusal test seeds it -- stops signing with
// the coded ErrCertificateRevoked, the same chain-wide answer
// verification gives. Without this check a revoked authority's keys could
// keep producing signatures every verifier refuses.
func TestCAService_SignCertificate_RevokedAuthorityInChain_Refused(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	authority, cert := issueTestCertificate(t, ca, ctx)

	authority.Status = AuthorityStatusRevoked
	if err := ca.authorities.Update(ctx, authority); err != nil {
		t.Fatalf("seed revoked authority: %v", err)
	}

	if _, err := ca.SignCertificate(ctx, cert.ID, []byte("message")); !apperr.HasCode(err, ErrCertificateRevoked.Code) {
		t.Errorf("SignCertificate(revoked issuing authority) error = %v, want ErrCertificateRevoked", err)
	}
}

// TestCAService_SignCertificate_OutsideValidityWindow_Refused pins the
// validity half of the signing-side refusal at the service clock: a
// certificate past its NotAfter -- or one whose NotBefore has not yet
// arrived, judged by the same s.now() seam the module's other
// clock-sensitive reads use -- signs nothing. The error is deliberately
// UNCODED, matching the module's "expiry is not encoded" convention on the
// verification side (VerifyCertificate hands the window to crypto/x509,
// which returns an unwrapped error for it).
func TestCAService_SignCertificate_OutsideValidityWindow_Refused(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)
	message := []byte("message")

	// Past NotAfter.
	ca.now = func() time.Time { return cert.NotAfter.Add(time.Minute) }
	if _, err := ca.SignCertificate(ctx, cert.ID, message); err == nil {
		t.Fatalf("SignCertificate(past NotAfter) succeeded, want a refusal")
	} else if coded, ok := apperr.As(err); ok {
		t.Errorf("SignCertificate(past NotAfter) error = coded %s, want an uncoded error (expiry is not encoded)", coded.Code)
	}

	// Before NotBefore.
	ca.now = func() time.Time { return cert.NotBefore.Add(-time.Minute) }
	if _, err := ca.SignCertificate(ctx, cert.ID, message); err == nil {
		t.Fatalf("SignCertificate(before NotBefore) succeeded, want a refusal")
	}

	// Back inside the window, the same certificate signs again -- the
	// refusal was the clock, never the row.
	ca.now = time.Now
	signature, err := ca.SignCertificate(ctx, cert.ID, message)
	if err != nil {
		t.Fatalf("SignCertificate(inside the window) after the clock refusals: %v", err)
	}
	leaf, err := parseCertificatePEM(cert.CertificatePEM)
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}
	if !ed25519.Verify(leaf.PublicKey.(ed25519.PublicKey), message, signature) {
		t.Errorf("signature made after the clock refusals does not verify")
	}
}

// TestCAService_SignCertificate_SignatureOverExactMessageBytes pins that
// SignCertificate signs the caller's bytes verbatim (the Signer interface's
// Ed25519 contract: the complete message, never a digest), so the message
// a signer later verifies must be byte-identical to the one signed --
// canonicalization is the caller's job, exactly as the reference app's
// attestation message canonicalization does it.
func TestCAService_SignCertificate_SignatureOverExactMessageBytes(t *testing.T) {
	ca := newTestCAService(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	_, cert := issueTestCertificate(t, ca, ctx)

	first, err := ca.SignCertificate(ctx, cert.ID, []byte("same message"))
	if err != nil {
		t.Fatalf("SignCertificate: %v", err)
	}
	second, err := ca.SignCertificate(ctx, cert.ID, []byte("same message"))
	if err != nil {
		t.Fatalf("SignCertificate again: %v", err)
	}
	other, err := ca.SignCertificate(ctx, cert.ID, []byte("same message!"))
	if err != nil {
		t.Fatalf("SignCertificate other message: %v", err)
	}

	leaf, err := parseCertificatePEM(cert.CertificatePEM)
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}
	pub := leaf.PublicKey.(ed25519.PublicKey)
	if !ed25519.Verify(pub, []byte("same message"), first) || !bytes.Equal(first, second) {
		t.Errorf("deterministic Ed25519 signing of the same message did not produce identical verifiable signatures")
	}
	if ed25519.Verify(pub, []byte("same message"), other) {
		t.Errorf("signature over a different message verifies against the original -- the message is not bound")
	}
}
