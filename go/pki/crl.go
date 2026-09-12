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
// zero AND the declared pki.crl_validity item carries no explicit row (or
// no settings reader is wired at all): the package-level constant backing
// the declared item's schema default, which settings.go's crlValidityFor
// resolves through. Seven days is a deliberately short refresh cadence for
// an internal CA that serves revocation through CRL rather than an OCSP
// responder: a week keeps a verifier that caches its last fetch from
// trusting a month-stale revocation list.
const DefaultCRLValidity = 7 * 24 * time.Hour

// maxGenerateCRLAttempts is how many read-sign-persist rounds GenerateCRL
// may make before giving up on an authority whose CRLNumber keeps moving
// under it. Sixteen is generous headroom over any plausible number of
// concurrently overlapping generators of one authority's CRL: each
// committed conditional write advances the register by exactly one, so k
// contenders need k winning rounds and the k-th winner makes at most k
// attempts -- a call can only exhaust this bound under >16-way contention,
// and exhaustion surfaces as a returned error rather than a silently
// dropped tick, which the periodic regeneration job's next run retries.
const maxGenerateCRLAttempts = 16

// encodeCRLPEM PEM-encodes a DER CRL under the standard "X509 CRL" block
// type (RFC 7468), the same way encodeCertificatePEM encodes a certificate.
func encodeCRLPEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der}))
}

// GenerateCRL (re)generates authorityID's CRL: a crypto/x509 Certificate
// Revocation List listing every certificate CertificateRevocationRepository
// records as revoked under that authority, signed by the authority's own
// key, and persists it onto the Authority row (CRLPEM/CRLNumber/
// CRLIssuedAt/CRLNextUpdate).
//
// validity controls NextUpdate - ThisUpdate; a value <=0 resolves through
// the declared pki.crl_validity item, falling back to DefaultCRLValidity
// when no explicit row applies (settings.go's crlValidityFor).
// CRLNumber (RFC 5280 §5.2.3) increases by exactly one
// on every SUCCESSFUL call, including a call that finds zero revocations --
// an empty CRL is still a valid, meaningfully-refreshed document (its
// NextUpdate tells a verifier when to check again), never skipped just
// because nothing new happened.
//
// # Concurrent calls are arbitrated on the CRL-number register
//
// The document is signed from a snapshot (the authority row's CRLNumber and
// the revocation ledger read below), then persisted through
// AuthorityRepository.UpdateCRLIfCurrent (repository.go) -- ONE guarded
// UPDATE matching only a row whose CRLNumber is still the number this call
// read, never a blind full-row save. Two overlapping GenerateCRL calls for
// one authority can both read CRLNumber N, but only the first call's
// conditional write lands; the loser re-reads and regenerates at the
// winner's number (N+1 -> N+2), so however many calls overlap, every
// successful call advances the register by exactly one and a losing call's
// revocation snapshot is folded into its retry's document rather than
// dropped. The retry loop is bounded by maxGenerateCRLAttempts: a call that
// keeps losing the arbitration that many times returns an error rather than
// overwriting, and the periodic regeneration job's next run retries.
//
// GenerateCRL runs regardless of authority.CRLDistributionPoint: that field
// only controls whether OTHER certificates this authority signs carry a
// CRLDistributionPoints extension pointing back at it (ca.go's
// CreateIntermediateCA/IssueCertificate); it is not a gate on whether a CRL
// document itself may exist. The HTTP surface that SERVES the generated
// document (the handler's own CRL fetch operation, handler.go) is a
// separate concern.
//
// ErrAuthorityNotFound if authorityID does not exist. A Signer.Sign failure
// that is not already a coded *apperr.Error is wrapped as
// ErrSignerUnavailable -- see errors.go's own doc comment for the code's
// full accounting.
func (s *CAService) GenerateCRL(ctx context.Context, authorityID string, validity time.Duration) (*Authority, error) {
	validity = s.crlValidityFor(ctx, validity)

	// The read-sign-persist cycle runs inside this loop because the persist
	// step is a guarded CAS: a call that loses it must not overwrite the
	// concurrent winner's committed document, it must re-read the fresh
	// register and regenerate at the number the winner left free.
	for attempt := 1; ; attempt++ {
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
		crlPEM := encodeCRLPEM(der)

		moved, err := s.authorities.UpdateCRLIfCurrent(ctx, authority.ID, authority.CRLNumber, nextNumber, crlPEM, now, template.NextUpdate)
		if err != nil {
			return nil, fmt.Errorf("pki: store CRL for authority %q: %w", authorityID, err)
		}
		if moved {
			// This call's conditional write landed: its own document and
			// register values are what the row now holds committed.
			authority.CRLNumber = nextNumber
			authority.CRLPEM = crlPEM
			authority.CRLIssuedAt = &now
			nextUpdate := template.NextUpdate
			authority.CRLNextUpdate = &nextUpdate

			observability.FromContext(ctx).Info("pki CRL generated",
				"authority_id", authorityID,
				"crl_number", nextNumber,
				"revoked_count", len(entries),
			)
			return authority, nil
		}
		// Lost the CAS: a concurrent GenerateCRL committed a higher-numbered
		// CRL between this call's read and its write. Regenerate at the
		// fresh number -- never write this stale snapshot over the winner.
		if attempt >= maxGenerateCRLAttempts {
			return nil, fmt.Errorf("pki: generate CRL for authority %q: concurrent generation kept advancing crl_number across %d attempts", authorityID, maxGenerateCRLAttempts)
		}
	}
}

// RegenerateAllCRLs runs GenerateCRL with a zero validity argument for
// every authority this deployment has -- so each document resolves its
// window through the declared pki.crl_validity item (DefaultCRLValidity
// when no explicit row applies) -- the batch operation the registered
// regeneration handler drives on a schedule (below) and a host may also
// call directly for an on-demand, deployment-wide refresh. It is best-effort: one authority's
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
// schedules and the module's registered regeneration handler claims. One run regenerates every
// authority's CRL (RegenerateAllCRLs) -- pki_authorities is platform data,
// so there is no per-tenant shape to this task, the identical reasoning
// job.go's taskTypeExpiryScan doc comment gives for that task.
const taskTypeCRLRegenerate = "pki.crl_regenerate"

// DefaultCRLRegenerateWindow is the period one CRL-regeneration idempotency
// key covers: a regeneration is enqueued under the key of the
// DefaultCRLRegenerateWindow window (jobs.ScheduleWindowStart) its enqueue
// falls in, so the same-window duplicates a multi-replica scheduler
// produces collapse into one job, while an enqueue in a later window is a
// NEW job and regenerates again -- the identical window semantics
// DefaultExpiryScanWindow documents for the expiry-scan task, with the
// identical reasoning: without a key, N replicas at a 1-minute cadence
// fire N regenerations a minute, each GenerateCRL run producing its own
// full document and advancing the authority's crl_number register (wasted
// work and write churn -- the generated documents are identical in
// substance, only the register numbers differ); with a key but no window,
// jobs' unconditional idempotency would give exactly one regeneration per
// database file, and a default-validity CRL (DefaultCRLValidity, seven
// days) would outlive its NextUpdate with no scheduled refresh at all.
//
// The window must be significantly larger than the host's scheduler
// interval, or every tick lands in a fresh window and the dedup is void
// (the expiry-scan window's own doc comment makes the same point).
const DefaultCRLRegenerateWindow = time.Hour

// crlRegenerateKeyPrefix is the prefix of every CRL-regeneration
// idempotency key. It is a named constant because two derivations must
// agree on it byte for byte: the manual EnqueueCRLRegenerate path and the
// declaration (CAService.crlRegenerateSchedule) a jobs.Scheduler expands
// -- one window must resolve one key through both paths, and both derive
// it through jobs.SchedulePlatformIdempotencyKey with this prefix.
const crlRegenerateKeyPrefix = "pki.crl_regenerate:"

// crlRegenerateSchedule is the module's declaration of CRL regeneration on
// the pkgcore.Registry.Schedules seat: one platform-wide task per window,
// under the CRL task's own sentinel tenant, at the service's configured
// regeneration window and keyed with the same prefix and the same window
// derivation the manual EnqueueCRLRegenerate path uses
// (jobs.ScheduleWindowStart / jobs.SchedulePlatformIdempotencyKey) -- so a
// scheduler tick and a manual enqueue landing in one window resolve one
// key and dedupe onto one job.
func (s *CAService) crlRegenerateSchedule() pkgcore.PeriodicTask {
	return pkgcore.PeriodicTask{
		Type:           taskTypeCRLRegenerate,
		Every:          s.crlRegenerateWindow,
		Scope:          pkgcore.PeriodicScopePlatform,
		KeyPrefix:      crlRegenerateKeyPrefix,
		PlatformTenant: platformCRLRegenerateTenantID,
	}
}

// platformCRLRegenerateTenantID is the fixed jobs.Task.TenantID every
// CRL-regeneration task is enqueued under -- job.go's platformScanTenantID
// exists for the expiry-scan task specifically (its own doc comment already
// explains the jobs.Task.Validate accommodation this mirrors); a second,
// separate sentinel here keeps the two platform-wide task types' Job rows
// distinguishable in whatever admin view lists them by tenant, rather than
// silently collapsing two different "queues" onto one label.
const platformCRLRegenerateTenantID = pkgcore.TenantID("_pki_platform_crl")

// EnqueueCRLRegenerate schedules one run of RegenerateAllCRLs onto the
// queue Module was wired with (WithQueue) -- the on-demand/periodic trigger
// for CRL generation, the HTTP-independent counterpart of the module's
// `crl:current` fetch operation (handler.go serves whatever GenerateCRL
// last wrote; it never generates on the read path itself). Carries no
// payload, mirroring Service.EnqueueExpiryScan's identical "read everything
// at run time" shape.
//
// The task carries a window-scoped idempotency key
// (jobs.SchedulePlatformIdempotencyKey, DefaultCRLRegenerateWindow): the enqueues
// of one window -- a multi-replica scheduler whose replicas each tick the
// same schedule instant, a manual re-run -- collapse into one job, so
// regeneration runs at most once per window however many replicas tick,
// while an enqueue in a later window is a new job and regenerates again --
// the exact window semantics job.go's EnqueueExpiryScan documents (and the
// same reason that task's doc comment gives for why a windowed key beats
// both a keyless task and a window-less key). A regeneration job that
// dead-letters poisons only its own window.
//
// GenerateCRL remains safe to run concurrently with itself -- its persist
// step is a guarded CAS on the authority's crl_number register
// (repository.go's UpdateCRLIfCurrent), so two jobs of DIFFERENT windows
// overlapping in execution converge on sequential numbers, each landing its
// own document, instead of collapsing into a last-writer-wins overwrite of
// one number -- the windowed key only removes the same-window duplicates.
//
// A nil queue (Module constructed without WithQueue) reports a plain error,
// the identical "no queue wired" answer Service.EnqueueExpiryScan gives.
func (s *CAService) EnqueueCRLRegenerate(ctx context.Context) error {
	if s.queue == nil {
		return errors.New("pki: no queue wired")
	}
	_, err := s.queue.Enqueue(ctx, jobs.Task{
		Type:           taskTypeCRLRegenerate,
		TenantID:       platformCRLRegenerateTenantID,
		IdempotencyKey: jobs.SchedulePlatformIdempotencyKey(crlRegenerateKeyPrefix, jobs.ScheduleWindowStart(s.now(), s.crlRegenerateWindow)),
	})
	return err
}

// runScheduledCRLRegenerate runs one scheduled CRL regeneration:
// RegenerateAllCRLs with its regenerated-authority list discarded -- a
// scheduled task's outcome is success or failure alone. Module.Register
// wires it through jobs.NewEmptyPayloadHandler.
func (s *CAService) runScheduledCRLRegenerate(ctx context.Context) error {
	_, err := s.RegenerateAllCRLs(ctx)
	return err
}
