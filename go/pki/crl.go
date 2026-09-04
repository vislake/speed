package pki

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// DefaultCRLValidity is how long a generated CRL claims to be current
// (NextUpdate - ThisUpdate) when GenerateCRL's own validity argument is
// zero. A named package-level constant, not a config item read at call
// time -- matching DefaultPropagationWindow/DefaultRenewalLeadTime's
// identical "declared as a config item for host visibility, resolved as a
// Go constant this round" split (module.go's ConfigCRLValidity item
// documents why). Seven days is a deliberately short refresh cadence for an
// internal CA -- docs/internal/22-pki.md's "revocation" section frames
// short validity plus CRL as the whole point of skipping an OCSP
// responder, and a week keeps a verifier that caches its last fetch from
// trusting a month-stale revocation list.
const DefaultCRLValidity = 7 * 24 * time.Hour

// encodeCRLPEM PEM-encodes a DER CRL under the standard "X509 CRL" block
// type (RFC 7468), the same way encodeCertificatePEM encodes a certificate.
func encodeCRLPEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der}))
}

// GenerateCRL (re)generates authorityID's CRL: a crypto/x509 Certificate
// Revocation List listing every certificate CertificateRevocationRepository
// records as revoked under that authority, signed by the authority's own
// key, and persists it onto the Authority row (CRLPEM/CRLNumber/
// CRLIssuedAt/CRLNextUpdate) -- round 3's addition, docs/internal/22-pki.md's
// "revocation" section: "generate a CRL, not an OCSP responder".
//
// validity controls NextUpdate - ThisUpdate; a value <=0 falls back to
// DefaultCRLValidity. CRLNumber (RFC 5280 §5.2.3) increases by exactly one
// on every call, including a call that finds zero revocations -- an empty
// CRL is still a valid, meaningfully-refreshed document (its NextUpdate
// tells a verifier when to check again), never skipped just because
// nothing new happened.
//
// GenerateCRL runs regardless of authority.CRLDistributionPoint: that field
// only controls whether OTHER certificates this authority signs carry a
// CRLDistributionPoints extension pointing back at it (ca.go's
// CreateIntermediateCA/IssueCertificate); it is not a gate on whether a CRL
// document itself may exist. The HTTP surface that SERVES the generated
// document (crat's own fetch operation, handler.go) is a separate concern.
//
// ErrAuthorityNotFound if authorityID does not exist. A Signer.Sign failure
// that is not already a coded *apperr.Error is wrapped as
// ErrSignerUnavailable -- see errors.go's own doc comment for why this is
// this error code's first real trigger.
func (s *CAService) GenerateCRL(ctx context.Context, authorityID string, validity time.Duration) (*Authority, error) {
	if validity <= 0 {
		validity = DefaultCRLValidity
	}

	authority, err := s.authorities.FindByID(ctx, authorityID)
	if err != nil {
		return nil, err
	}
	issuerCert, err := parseCertificatePEM(authority.CertificatePEM)
	if err != nil {
		return nil, fmt.Errorf("pki: parse authority %q certificate: %w", authorityID, err)
	}

	revocations, err := s.revocations.ListByAuthority(ctx, authorityID)
	if err != nil {
		return nil, err
	}
	entries := make([]x509.RevocationListEntry, 0, len(revocations))
	for _, rev := range revocations {
		serial, ok := new(big.Int).SetString(rev.Serial, 16)
		if !ok {
			return nil, fmt.Errorf("pki: revocation ledger entry %q has an unparseable serial %q", rev.ID, rev.Serial)
		}
		entries = append(entries, x509.RevocationListEntry{
			SerialNumber:   serial,
			RevocationTime: rev.RevokedAt,
		})
	}

	now := time.Now().UTC()
	nextNumber := authority.CRLNumber + 1
	template := &x509.RevocationList{
		Number:                    big.NewInt(nextNumber),
		ThisUpdate:                now,
		NextUpdate:                now.Add(validity),
		RevokedCertificateEntries: entries,
	}

	signer := signerAdapter{ctx: ctx, signer: s.signer, keyRef: authority.KeyRef, public: issuerCert.PublicKey}
	der, err := x509.CreateRevocationList(rand.Reader, template, issuerCert, signer)
	if err != nil {
		if _, ok := apperr.As(err); ok {
			return nil, err
		}
		return nil, ErrSignerUnavailable.WithCause(err)
	}

	authority.CRLNumber = nextNumber
	authority.CRLPEM = encodeCRLPEM(der)
	authority.CRLIssuedAt = &now
	nextUpdate := template.NextUpdate
	authority.CRLNextUpdate = &nextUpdate
	if err := s.authorities.Update(ctx, authority); err != nil {
		return nil, fmt.Errorf("pki: store CRL for authority %q: %w", authorityID, err)
	}

	observability.FromContext(ctx).Info("pki CRL generated",
		"authority_id", authorityID,
		"crl_number", nextNumber,
		"revoked_count", len(entries),
	)
	return authority, nil
}

// RegenerateAllCRLs runs GenerateCRL, with DefaultCRLValidity, for every
// authority this deployment has -- the batch operation crlRegenerateHandler
// drives on a schedule (below) and a host may also call directly for an
// on-demand, deployment-wide refresh. It is best-effort: one authority's
// failure does not stop the others from being attempted, and every failure
// is collected into a single joined error via errors.Join, so a caller can
// tell "regenerated 3 of 4, authority X failed" apart from "the whole batch
// never ran" (a non-nil err from ListAll itself).
//
// regenerated names every authority id GenerateCRL actually succeeded for,
// in AuthorityRepository.ListAll's own (unspecified) order.
func (s *CAService) RegenerateAllCRLs(ctx context.Context) ([]string, error) {
	authorities, err := s.authorities.ListAll(ctx)
	if err != nil {
		return nil, err
	}

	var regenerated []string
	var failures []error
	for _, authority := range authorities {
		if _, err := s.GenerateCRL(ctx, authority.ID, 0); err != nil {
			failures = append(failures, fmt.Errorf("authority %q: %w", authority.ID, err))
			continue
		}
		regenerated = append(regenerated, authority.ID)
	}
	if len(failures) > 0 {
		return regenerated, errors.Join(failures...)
	}
	return regenerated, nil
}

// taskTypeCRLRegenerate names the jobs queue task EnqueueCRLRegenerate
// schedules and crlRegenerateHandler claims. One run regenerates every
// authority's CRL (RegenerateAllCRLs) -- pki_authorities is platform data,
// so there is no per-tenant shape to this task, the identical reasoning
// job.go's taskTypeExpiryScan doc comment gives for that task.
const taskTypeCRLRegenerate = "pki.crl_regenerate"

// platformCRLRegenerateTenantID is the fixed jobs.Task.TenantID every
// CRL-regeneration task is enqueued under -- job.go's platformScanTenantID
// exists for the expiry-scan task specifically (its own doc comment already
// explains the jobs.Task.Validate accommodation this mirrors); a second,
// separate sentinel here keeps the two platform-wide task types' Job rows
// distinguishable in whatever admin view lists them by tenant, rather than
// silently collapsing two different "queues" onto one label.
const platformCRLRegenerateTenantID = pkgcore.TenantID("_pki_platform_crl")

// EnqueueCRLRegenerate schedules one run of RegenerateAllCRLs onto the
// queue Module was wired with (WithQueue) -- round 3's on-demand/periodic
// trigger for CRL generation, the HTTP-independent counterpart of the
// module's `crl:current` fetch operation (handler.go serves whatever
// GenerateCRL last wrote; it never generates on the read path itself).
// Carries no payload, mirroring Service.EnqueueExpiryScan's identical "read
// everything at run time" shape. No idempotency key: like the expiry scan,
// each regeneration tick is its own independent occurrence, and
// GenerateCRL's per-authority read-then-write is safe to run concurrently
// with itself (a second overlapping tick simply produces a higher-numbered
// CRL, never a corrupt one).
//
// A nil queue (Module constructed without WithQueue) reports a plain error,
// the identical "no queue wired" answer Service.EnqueueExpiryScan gives.
func (s *CAService) EnqueueCRLRegenerate(ctx context.Context) error {
	if s.queue == nil {
		return errors.New("pki: no queue wired")
	}
	_, err := s.queue.Enqueue(ctx, jobs.Task{
		Type:     taskTypeCRLRegenerate,
		TenantID: platformCRLRegenerateTenantID,
	})
	return err
}

// crlRegenerateHandler is the jobs.Handler claiming taskTypeCRLRegenerate,
// the task EnqueueCRLRegenerate schedules.
type crlRegenerateHandler struct {
	ca *CAService
}

// Type implements jobs.Handler.
func (h crlRegenerateHandler) Type() string { return taskTypeCRLRegenerate }

// Handle implements jobs.Handler. The task's payload must be empty, the
// identical task-shape check job.go's expiryScanHandler.Handle applies to
// its own task.
func (h crlRegenerateHandler) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	if len(job.Payload) != 0 {
		return jobs.Result{}, errors.New("pki: CRL-regenerate task carries an unexpected payload")
	}
	if _, err := h.ca.RegenerateAllCRLs(ctx); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{}, nil
}

// compile-time check that crlRegenerateHandler satisfies jobs.Handler.
var _ jobs.Handler = crlRegenerateHandler{}
