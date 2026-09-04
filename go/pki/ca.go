package pki

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"

	"github.com/vislake/speed/go/observability"
)

// serialNumberBytes is the width of a certificate serial number: 16 bytes
// of crypto/rand, per docs/internal/22-pki.md's explicit diagnosis of the
// anti-pattern this replaces (System.currentTimeMillis(), which collides
// under concurrent issuance because it has millisecond resolution and no
// randomness at all). 16 bytes gives 128 bits of entropy, comfortably
// beyond RFC 5280's non-normative 20-octet ceiling once the sign bit is
// accounted for below.
const serialNumberBytes = 16

// newSerialNumber returns a new certificate serial number: 16 bytes of
// crypto/rand, interpreted as an unsigned big-endian integer. The top bit is
// cleared so the DER INTEGER encoding never needs a leading 0x00 padding
// byte for sign disambiguation -- cosmetic (crypto/x509 handles either
// encoding correctly), but it keeps the stored hex and the wire encoding's
// byte count in visible agreement.
func newSerialNumber() (*big.Int, error) {
	buf := make([]byte, serialNumberBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("pki: generate serial number: %w", err)
	}
	buf[0] &= 0x7f
	return new(big.Int).SetBytes(buf), nil
}

// serialHex renders serial the same way Authority.Serial and
// Certificate.Serial store it: lower-case hex, no separators or prefix.
func serialHex(serial *big.Int) string {
	return hex.EncodeToString(serial.Bytes())
}

// signerAdapter adapts one (Signer, keyRef) pair to the standard library's
// crypto.Signer, the shape crypto/x509.CreateCertificate requires for its
// issuer parameter. It exists only inside this file: nothing above the
// X.509 layer needs a crypto.Signer, and Signer itself deliberately does
// not implement crypto.Signer directly (see the Signer interface's own doc
// comment for why: crypto.Signer.Sign has no context.Context parameter).
//
// crypto/x509.CreateCertificate calls Sign with digest set to the FULL
// TBSCertificate bytes when the issuer key is an Ed25519 key (Go's own
// crypto/x509 special-cases Ed25519 for exactly the PureEdDSA reason
// Signer.Sign's doc comment explains), so passing digest straight through
// to signer.Sign is correct for AlgorithmEd25519 without any hashing here.
// A future non-EdDSA algorithm would need this adapter (or its caller) to
// hash first -- see Signer.Sign's doc comment.
type signerAdapter struct {
	ctx    context.Context
	signer Signer
	keyRef string
	public crypto.PublicKey
}

// Public implements crypto.Signer.
func (a signerAdapter) Public() crypto.PublicKey { return a.public }

// Sign implements crypto.Signer. rand and opts are unused: LocalSigner (and
// every Signer implementation this round ships) draws its own randomness
// internally where the algorithm needs any (Ed25519 is deterministic), and
// opts carries no information Sign needs beyond what algorithm the keyRef
// already fixed at generation time.
func (a signerAdapter) Sign(_ io.Reader, digest []byte, _ crypto.SignerOpts) ([]byte, error) {
	return a.signer.Sign(a.ctx, a.keyRef, digest)
}

// CAService issues the internal CA chain and the end-entity certificates
// authorities sign -- the X.509 layer docs/internal/22-pki.md's "two-layer
// structure" section describes.
//
// # No real consumer yet -- read this before treating this type as frozen
//
// reference-app is a dental SaaS; it does not issue certificates. This
// type ships anyway because the requirements behind it (root CA private
// keys that never rotate, no revocation, predictable serial numbers) were
// diagnosed from a real production system, not invented -- see
// docs/internal/22-pki.md's "requirement source" and "the X.509 layer has
// no real consumer yet" sections for the full argument and the three
// compensating obligations that argument imposes. AGENTS.md's Known
// limitations section carries the
// same warning: this layer's public API may be broken, without the usual
// frozen-API discipline, the moment a real consumer's first integration
// finds a parameter it cannot actually supply.
//
// # One Signer per CAService
//
// Every authority and certificate this CAService issues is signed through
// the same Signer instance, recorded on each row as SignerName/KeyRef.
// Nothing here prevents a future host from running two CAServices over two
// different Signer implementations (an offline root under "vault", online
// intermediates under "local", say) -- each authority row already carries
// its own SignerName, so a future round's lookup-by-name is a schema-
// compatible addition, not a migration.
type CAService struct {
	signer       Signer
	signerName   string
	authorities  *AuthorityRepository
	certificates *CertificateRepository
}

// NewCAService returns a CAService that signs through signer (recorded on
// every issued row under signerName) and persists through authorities and
// certificates.
func NewCAService(signer Signer, signerName string, authorities *AuthorityRepository, certificates *CertificateRepository) *CAService {
	return &CAService{
		signer:       signer,
		signerName:   signerName,
		authorities:  authorities,
		certificates: certificates,
	}
}

// RootCAParams configures CreateRootCA.
type RootCAParams struct {
	// Subject is the root certificate's subject.
	Subject pkix.Name
	// NotAfter is when the root certificate stops being valid. NotBefore is
	// always time.Now() at issuance.
	NotAfter time.Time
}

// CreateRootCA generates a new key pair and issues a self-signed root CA
// certificate, storing both in pki_authorities. The root's own private key
// is generated and held exactly like any other key -- through the Signer
// seam, never in the clear in this method's memory beyond what
// crypto/ed25519.GenerateKey itself produces and Signer.GenerateKey then
// takes custody of.
func (s *CAService) CreateRootCA(ctx context.Context, params RootCAParams) (*Authority, error) {
	keyRef, pub, err := s.signer.GenerateKey(ctx, AlgorithmEd25519)
	if err != nil {
		return nil, err
	}

	serial, err := newSerialNumber()
	if err != nil {
		return nil, err
	}

	notBefore := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               params.Subject,
		NotBefore:             notBefore,
		NotAfter:              params.NotAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	signer := signerAdapter{ctx: ctx, signer: s.signer, keyRef: keyRef, public: pub}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, signer)
	if err != nil {
		return nil, fmt.Errorf("pki: create root CA certificate: %w", err)
	}

	authority := &Authority{
		ID:             uuid.NewString(),
		Type:           AuthorityTypeRoot,
		ParentID:       nil,
		Subject:        params.Subject.String(),
		Serial:         serialHex(serial),
		CertificatePEM: encodeCertificatePEM(der),
		SignerName:     s.signerName,
		KeyRef:         keyRef,
		Status:         AuthorityStatusActive,
		NotBefore:      notBefore,
		NotAfter:       params.NotAfter,
	}
	if err := s.authorities.Create(ctx, authority); err != nil {
		return nil, fmt.Errorf("pki: store root CA: %w", err)
	}

	observability.FromContext(ctx).Info("pki root CA created",
		"authority_id", authority.ID,
		"subject", authority.Subject,
	)
	return authority, nil
}

// IntermediateCAParams configures CreateIntermediateCA.
type IntermediateCAParams struct {
	// Subject is the intermediate certificate's subject.
	Subject pkix.Name
	// NotAfter is when the intermediate certificate stops being valid.
	NotAfter time.Time
}

// CreateIntermediateCA generates a new key pair and issues a CA certificate
// signed by parentID's authority, storing the result in pki_authorities.
// The intermediate's MaxPathLen is 0: it may sign end-entity certificates
// but never a further intermediate, keeping the chain to the three levels
// docs/internal/22-pki.md's diagnosed system used (root / intermediate /
// end-entity).
func (s *CAService) CreateIntermediateCA(ctx context.Context, parentID string, params IntermediateCAParams) (*Authority, error) {
	parent, err := s.authorities.FindByID(ctx, parentID)
	if err != nil {
		return nil, err
	}
	parentCert, err := parseCertificatePEM(parent.CertificatePEM)
	if err != nil {
		return nil, fmt.Errorf("pki: parse parent authority %q certificate: %w", parentID, err)
	}

	keyRef, pub, err := s.signer.GenerateKey(ctx, AlgorithmEd25519)
	if err != nil {
		return nil, err
	}

	serial, err := newSerialNumber()
	if err != nil {
		return nil, err
	}

	notBefore := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               params.Subject,
		NotBefore:             notBefore,
		NotAfter:              params.NotAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	parentSigner := signerAdapter{ctx: ctx, signer: s.signer, keyRef: parent.KeyRef, public: parentCert.PublicKey}
	der, err := x509.CreateCertificate(rand.Reader, template, parentCert, pub, parentSigner)
	if err != nil {
		return nil, fmt.Errorf("pki: create intermediate CA certificate: %w", err)
	}

	authority := &Authority{
		ID:             uuid.NewString(),
		Type:           AuthorityTypeIntermediate,
		ParentID:       &parent.ID,
		Subject:        params.Subject.String(),
		Serial:         serialHex(serial),
		CertificatePEM: encodeCertificatePEM(der),
		SignerName:     s.signerName,
		KeyRef:         keyRef,
		Status:         AuthorityStatusActive,
		NotBefore:      notBefore,
		NotAfter:       params.NotAfter,
	}
	if err := s.authorities.Create(ctx, authority); err != nil {
		return nil, fmt.Errorf("pki: store intermediate CA: %w", err)
	}

	observability.FromContext(ctx).Info("pki intermediate CA created",
		"authority_id", authority.ID,
		"parent_authority_id", parent.ID,
		"subject", authority.Subject,
	)
	return authority, nil
}

// CertificateParams configures IssueCertificate.
type CertificateParams struct {
	// Purpose names what this certificate is for, e.g. "tenant.jwt_signing".
	Purpose string
	// Subject is the end-entity certificate's subject.
	Subject pkix.Name
	// DNSNames are the certificate's subject alternative names.
	DNSNames []string
	// NotAfter is when the certificate stops being valid.
	NotAfter time.Time
}

// IssueCertificate generates a new key pair and issues an end-entity
// certificate signed by authorityID's authority, for the tenant in ctx,
// storing the result in pki_certificates.
//
// ctx must carry a tenant (pkgcore.WithTenant): Certificate is tenant data,
// and CertificateRepository.Create -- reached through the embedded
// dbkit.Repository[Certificate] -- fails closed with pkgcore.ErrNoTenant
// when it does not, before anything is written.
func (s *CAService) IssueCertificate(ctx context.Context, authorityID string, params CertificateParams) (*Certificate, error) {
	authority, err := s.authorities.FindByID(ctx, authorityID)
	if err != nil {
		return nil, err
	}
	issuerCert, err := parseCertificatePEM(authority.CertificatePEM)
	if err != nil {
		return nil, fmt.Errorf("pki: parse authority %q certificate: %w", authorityID, err)
	}

	keyRef, pub, err := s.signer.GenerateKey(ctx, AlgorithmEd25519)
	if err != nil {
		return nil, err
	}

	serial, err := newSerialNumber()
	if err != nil {
		return nil, err
	}

	sans, err := marshalSANs(params.DNSNames)
	if err != nil {
		return nil, err
	}

	notBefore := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      params.Subject,
		DNSNames:     params.DNSNames,
		NotBefore:    notBefore,
		NotAfter:     params.NotAfter,
		IsCA:         false,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	issuerSigner := signerAdapter{ctx: ctx, signer: s.signer, keyRef: authority.KeyRef, public: issuerCert.PublicKey}
	der, err := x509.CreateCertificate(rand.Reader, template, issuerCert, pub, issuerSigner)
	if err != nil {
		return nil, fmt.Errorf("pki: create end-entity certificate: %w", err)
	}

	cert := &Certificate{
		ID:             uuid.NewString(),
		AuthorityID:    authority.ID,
		Purpose:        params.Purpose,
		Subject:        params.Subject.String(),
		SANs:           sans,
		Serial:         serialHex(serial),
		CertificatePEM: encodeCertificatePEM(der),
		SignerName:     s.signerName,
		KeyRef:         keyRef,
		Status:         CertificateStatusActive,
		KeyDelivered:   false,
		NotBefore:      notBefore,
		NotAfter:       params.NotAfter,
	}
	if err := s.certificates.Create(ctx, cert); err != nil {
		return nil, fmt.Errorf("pki: store certificate: %w", err)
	}

	observability.FromContext(ctx).Info("pki certificate issued",
		"certificate_id", cert.ID,
		"authority_id", authority.ID,
		"purpose", cert.Purpose,
	)
	return cert, nil
}

// encodeCertificatePEM PEM-encodes a DER certificate the way every
// CertificatePEM column in this module stores one.
func encodeCertificatePEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// parseCertificatePEM reverses encodeCertificatePEM.
func parseCertificatePEM(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("pki: no PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

// marshalSANs encodes dnsNames as the JSON array Certificate.SANs stores.
// A nil or empty slice still marshals to "[]", never SQL NULL, so a reader
// never has to distinguish "no SANs" from "column not populated yet".
func marshalSANs(dnsNames []string) (datatypes.JSON, error) {
	if dnsNames == nil {
		dnsNames = []string{}
	}
	b, err := json.Marshal(dnsNames)
	if err != nil {
		return nil, fmt.Errorf("pki: marshal SANs: %w", err)
	}
	return datatypes.JSON(b), nil
}
