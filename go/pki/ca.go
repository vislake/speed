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

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// serialNumberBytes is the width of a certificate serial number: 16 bytes
// of crypto/rand. A timestamp-based serial is the anti-pattern this width
// avoids: System.currentTimeMillis() has millisecond resolution and no
// randomness at all, so it collides under concurrent issuance. 16 bytes
// gives 128 bits of entropy, comfortably beyond RFC 5280's non-normative
// 20-octet ceiling once the sign bit is accounted for below.
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
// An added non-EdDSA algorithm would need this adapter (or its caller) to
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
// every shipped Signer implementation) draws its own randomness internally
// where the algorithm needs any (Ed25519 is deterministic), and opts
// carries no information Sign needs beyond what algorithm the keyRef
// already fixed at generation time.
func (a signerAdapter) Sign(_ io.Reader, digest []byte, _ crypto.SignerOpts) ([]byte, error) {
	return a.signer.Sign(a.ctx, a.keyRef, digest)
}

// CAService issues the internal CA chain and the end-entity certificates
// authorities sign -- the X.509 layer of the two the package doc comment
// describes.
//
// # Real consumer: the reference app's AI-output attestation
//
// The reference app's internal/attestation package is this type's real
// consumer (mandatory-first-consumer rule discharged; see AGENTS.md's
// "X.509 layer: real consumer, precise residuals" section and
// examples/reference-app/internal/attestation's package doc): at boot the
// app calls CreateRootCA + CreateIntermediateCA once per database
// (EnsureAuthorityChain), and every simulation output a tenant observes
// is attested through IssueCertificate + SignCertificate under a
// per-tenant "simulation.attestation" certificate, whose public shares
// are gated on VerifyCertificate. The integration found exactly one
// missing capability -- a way to sign with an issued certificate's key
// (SignCertificate) -- and changed nothing else: this type's public API
// is no longer under the "first consumer may break it freely" exemption,
// though it is still not held to the same frozen-API standard as the
// key-lifecycle layer (AGENTS.md's consumer record states what remains
// unconsumed and what that means for API stability).
//
// # One Signer per CAService
//
// Every authority and certificate this CAService issues is signed through
// the same Signer instance, recorded on each row as SignerName/KeyRef.
// A host running two CAServices over two different Signer implementations
// (an offline root under "vault", online intermediates under "local",
// say) is a schema-compatible composition, not a migration: each
// authority row already carries its own SignerName.
type CAService struct {
	signer       Signer
	signerName   string
	authorities  *AuthorityRepository
	certificates *CertificateRepository

	// revocations is the revocation ledger repository RevokeCertificate
	// writes to and GenerateCRL reads from (revocation.go, crl.go).
	revocations *CertificateRevocationRepository

	// bus is the pkgcore.EventBus CAService publishes pki.certificate.*
	// events on -- nil until attachBus runs (Module.Register), mirroring
	// Service.bus's identical field and the identical "silently drop before
	// attachBus runs" tolerance (revocation.go's publish).
	bus pkgcore.EventBus

	// queue is the jobs.Queue EnqueueCRLRegenerate schedules the periodic
	// CRL-regeneration task on (crl.go) -- nil until attachQueue runs
	// (Module.Register, only when the host supplied one via WithQueue),
	// mirroring Service.queue's identical field and optional-queue
	// contract.
	queue jobs.Queue

	// now is the clock EnqueueCRLRegenerate reads to place the enqueue in
	// its DefaultCRLRegenerateWindow window (crlRegenerateWindowStart) and
	// ExportAuthorityChainJWKS reads for its per-member validity filter
	// (jwks.go). It is a field, not a time.Now() call at the use site, so
	// the window an enqueue lands in and the members an export vouches for
	// are deterministic in tests -- the same clock-seam pattern
	// Service.now provides for the expiry scan -- while defaulting to the
	// real clock for every production call.
	now func() time.Time

	// crlRegenerateWindow is the period one CRL-regeneration idempotency
	// key covers (DefaultCRLRegenerateWindow; see that constant's doc
	// comment for the window semantics and the sizing obligation).
	crlRegenerateWindow time.Duration
}

// NewCAService returns a CAService that signs through signer (recorded on
// every issued row under signerName) and persists through authorities,
// certificates and the revocation ledger revocations.
func NewCAService(signer Signer, signerName string, authorities *AuthorityRepository, certificates *CertificateRepository, revocations *CertificateRevocationRepository) *CAService {
	return &CAService{
		signer:              signer,
		signerName:          signerName,
		authorities:         authorities,
		certificates:        certificates,
		revocations:         revocations,
		now:                 time.Now,
		crlRegenerateWindow: DefaultCRLRegenerateWindow,
	}
}

// attachQueue hands CAService the jobs.Queue the host wired via WithQueue,
// so EnqueueCRLRegenerate (crl.go) has somewhere to schedule onto. Called
// from Module.Register only when the host supplied one -- a plain field
// assignment, exactly like Service.attachQueue.
func (s *CAService) attachQueue(queue jobs.Queue) {
	s.queue = queue
}

// attachBus hands CAService the registry's EventBus, mirroring
// Service.attachBus (service.go) but with no subscription: CAService keeps
// no cache to invalidate, so it only ever publishes, never subscribes.
// Called from Module.Register, which performs no I/O -- a plain field
// assignment, exactly like Service.attachBus.
func (s *CAService) attachBus(reg *pkgcore.ComponentRegistry) {
	s.bus = reg.EventBus()
}

// publish sends evt on s.bus when one is attached, mirroring Service.publish
// (service.go) exactly, including the "silently drop before attachBus runs"
// behavior that method's own doc comment explains.
func (s *CAService) publish(ctx context.Context, evt pkgcore.Event) {
	if s.bus == nil {
		return
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		observability.FromContext(ctx).Error("pki: publish certificate event failed",
			"event_type", evt.Type,
			"error", err,
		)
	}
}

// RootCAParams configures CreateRootCA.
type RootCAParams struct {
	// Subject is the root certificate's subject.
	Subject pkix.Name
	// NotAfter is when the root certificate stops being valid. NotBefore is
	// always time.Now() at issuance.
	NotAfter time.Time
	// CRLDistributionPoint is the URL this authority's own CRL will be
	// served at, recorded on the resulting Authority row and read at
	// issuance time by CreateIntermediateCA/IssueCertificate to populate
	// each certificate THIS authority signs with a CRLDistributionPoints
	// extension pointing back here. Empty means no extension is ever
	// written into a child certificate, never a broken placeholder URL,
	// matching every other unset-value convention this module already
	// follows (see Authority.CRLDistributionPoint's own model.go doc
	// comment for the full "child cert names ITS issuer's CRL" argument).
	// The root certificate's OWN CertificatePEM never carries this
	// extension -- nothing signs the root, so it has no meaningful "my
	// issuer's CRL" to name.
	CRLDistributionPoint string
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
		ID:                   uuid.NewString(),
		Type:                 AuthorityTypeRoot,
		ParentID:             nil,
		Subject:              params.Subject.String(),
		Serial:               serialHex(serial),
		CertificatePEM:       encodeCertificatePEM(der),
		SignerName:           s.signerName,
		KeyRef:               keyRef,
		Status:               AuthorityStatusActive,
		NotBefore:            notBefore,
		NotAfter:             params.NotAfter,
		CRLDistributionPoint: params.CRLDistributionPoint,
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

// checkNoRevokedAuthorityInChain refuses issuance when authority itself or
// any ancestor up to the root is AuthorityStatusRevoked -- the
// issuance-side mirror of the chain-verification refusal VerifyCertificate
// applies (revocation.go): a certificate minted under a chain containing a
// revoked authority is a certificate every verifier refuses, so nothing NEW
// may be signed under that chain, whether the revoked row is the direct
// issuer or a distant ancestor. The direct authority is the walk's first
// member; the walk then follows ParentID upward until a nil parent -- a
// root -- ends the chain.
//
// The returned error is ErrAuthorityRevoked, never ErrCertificateRevoked:
// nothing is being verified here, only refused an issuer -- the direct-
// authority refusal's own reasoning, applied to every ancestor -- and its
// authority_id param names the revoked row the walk actually met, which is
// the direct authority when that is the revoked one and the offending
// ancestor otherwise.
//
// The walk is cycle-guarded the same way VerifyCertificate's own walk is
// (revocation.go): Authority.ParentID values are application-generated and
// no constraint prevents a corrupt cycle, so the loop must not be able to
// spin forever on one. Each ancestor costs one FindByID, which issuance --
// never a hot path -- can afford.
func (s *CAService) checkNoRevokedAuthorityInChain(ctx context.Context, authority *Authority) error {
	seen := make(map[string]bool)
	for {
		if seen[authority.ID] {
			return fmt.Errorf("pki: authority chain cycle detected at %q", authority.ID)
		}
		seen[authority.ID] = true

		if authority.Status == AuthorityStatusRevoked {
			return ErrAuthorityRevoked.WithParam("authority_id", authority.ID)
		}
		if authority.ParentID == nil {
			return nil
		}
		parent, err := s.authorities.FindByID(ctx, *authority.ParentID)
		if err != nil {
			return err
		}
		authority = parent
	}
}

// IntermediateCAParams configures CreateIntermediateCA.
type IntermediateCAParams struct {
	// Subject is the intermediate certificate's subject.
	Subject pkix.Name
	// NotAfter is when the intermediate certificate stops being valid.
	NotAfter time.Time
	// CRLDistributionPoint is where THIS intermediate's own CRL will be
	// served -- recorded on the resulting Authority row exactly like
	// RootCAParams.CRLDistributionPoint, and read at issuance time for
	// certificates this new intermediate itself signs. It is unrelated to
	// the parent's CRLDistributionPoint, which is what gets embedded into
	// the intermediate CERTIFICATE this call issues (see CreateIntermediateCA's
	// own doc comment).
	CRLDistributionPoint string
}

// CreateIntermediateCA generates a new key pair and issues a CA certificate
// signed by parentID's authority, storing the result in pki_authorities.
// The intermediate's MaxPathLen is 0: it may sign end-entity certificates
// but never a further intermediate, keeping the chain to the three levels
// root / intermediate / end-entity.
//
// Refused with ErrAuthorityRevoked when parentID's authority -- or any of
// its ancestors up to the root -- is AuthorityStatusRevoked: a certificate
// minted under a chain containing a revoked authority is a certificate
// every verifier refuses, so a revoked authority signs nothing new (the
// issuance-side mirror of the whole-chain refusal revocation.go's
// VerifyCertificate applies), checked before any key is generated.
//
// # CRL distribution point
//
// When parent.CRLDistributionPoint is non-empty, the issued intermediate
// CERTIFICATE carries a CRLDistributionPoints extension naming it -- "if
// you want to know whether this intermediate has been revoked by its
// parent, fetch the parent's CRL", per Authority.CRLDistributionPoint's own
// model.go doc comment. An empty parent.CRLDistributionPoint omits the
// extension entirely, never a broken placeholder URL. This is unrelated to
// params.CRLDistributionPoint, which names where the NEW intermediate's own
// CRL will be served, for certificates IT goes on to sign.
func (s *CAService) CreateIntermediateCA(ctx context.Context, parentID string, params IntermediateCAParams) (*Authority, error) {
	parent, err := s.authorities.FindByID(ctx, parentID)
	if err != nil {
		return nil, err
	}
	// A chain containing an AuthorityStatusRevoked authority must not keep
	// signing -- nothing NEW may be minted under it once any of its
	// members, the parent itself or an ancestor up to the root, has been
	// revoked, the issuance-side mirror of VerifyCertificate's refusal to
	// trust anything already signed under one (revocation.go). The walk
	// runs before GenerateKey so a refused call never creates a key it
	// will not use. ErrAuthorityRevoked, never ErrCertificateRevoked:
	// nothing is being verified here, only refused an issuer.
	if err = s.checkNoRevokedAuthorityInChain(ctx, parent); err != nil {
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
	if parent.CRLDistributionPoint != "" {
		template.CRLDistributionPoints = []string{parent.CRLDistributionPoint}
	}

	parentSigner := signerAdapter{ctx: ctx, signer: s.signer, keyRef: parent.KeyRef, public: parentCert.PublicKey}
	der, err := x509.CreateCertificate(rand.Reader, template, parentCert, pub, parentSigner)
	if err != nil {
		return nil, fmt.Errorf("pki: create intermediate CA certificate: %w", err)
	}

	authority := &Authority{
		ID:                   uuid.NewString(),
		Type:                 AuthorityTypeIntermediate,
		ParentID:             &parent.ID,
		Subject:              params.Subject.String(),
		Serial:               serialHex(serial),
		CertificatePEM:       encodeCertificatePEM(der),
		SignerName:           s.signerName,
		KeyRef:               keyRef,
		Status:               AuthorityStatusActive,
		NotBefore:            notBefore,
		NotAfter:             params.NotAfter,
		CRLDistributionPoint: params.CRLDistributionPoint,
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
// Refused with ErrAuthorityRevoked when authorityID's authority -- or any
// of its ancestors up to the root -- is AuthorityStatusRevoked: a revoked
// authority signs nothing new (the issuance-side mirror of the whole-chain
// refusal revocation.go's VerifyCertificate applies), checked before any
// key is generated.
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
	// The same whole-chain revoked-issuer refusal CreateIntermediateCA
	// applies to its parent applies here to the issuing authority itself:
	// a chain containing a revoked authority -- the issuer or any ancestor
	// up to the root -- signs nothing new, checked before GenerateKey so a
	// refused call never creates a key it will not use.
	if err = s.checkNoRevokedAuthorityInChain(ctx, authority); err != nil {
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
	// See CreateIntermediateCA's identical CRL-distribution-point paragraph:
	// authority.CRLDistributionPoint names where THIS certificate's issuer's
	// CRL is served, embedded so a verifier knows where to check whether
	// this specific certificate has been revoked. Empty omits the
	// extension, never a broken placeholder URL.
	if authority.CRLDistributionPoint != "" {
		template.CRLDistributionPoints = []string{authority.CRLDistributionPoint}
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
