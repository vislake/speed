package pki

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// DefaultPropagationWindow is how long a newly staged SigningKeyStatusPending
// key waits before ScanExpiry promotes it to SigningKeyStatusActive.
//
// docs/internal/22-pki.md's section on why the pending state exists (a
// distributed race) explains why the wait exists at all: a multi-replica
// deployment's
// process-local caches must all have had a chance to observe the new
// public key (via event or the cache's own fallback poll) before any
// replica starts signing with it, or a token signed by the replica that
// saw the rotation first can arrive at a replica whose cache has not
// caught up, and fail to verify. The default is a small multiple of
// DefaultCacheTTL -- generous enough that the cache's fallback poll alone
// (with no event delivered at all) still catches up well within the
// window.
const DefaultPropagationWindow = 5 * DefaultCacheTTL

// DefaultRenewalLeadTime is how far ahead of a signing key's NotAfter
// ScanExpiry stages its replacement, so a purpose is never carried past a
// hard cutoff with no pending successor in flight. Thirty days against
// defaultKeyValidity's one-year default leaves a wide, deliberately
// generous margin for the propagation window plus the retiring overlap to
// both complete before the key that triggered the rotation actually
// expires.
const DefaultRenewalLeadTime = 30 * 24 * time.Hour

// RotationConfig configures one ScanExpiry call. A zero value for either
// field falls back to the Service's own configured default (the
// propagationWindow/renewalLeadTime NewService, or Module's
// WithPropagationWindow/WithRenewalLeadTime options, were built with).
type RotationConfig struct {
	// PropagationWindow overrides DefaultPropagationWindow for this call.
	PropagationWindow time.Duration
	// RenewalLeadTime overrides DefaultRenewalLeadTime for this call.
	RenewalLeadTime time.Duration
}

// ScanReport summarizes what one ScanExpiry call did: the kid of every key
// that made each transition, in the order they were processed. Every slice
// is nil (never a non-nil empty slice) when a scan made no transition of
// that kind, so a caller can check len() without also checking for nil.
type ScanReport struct {
	// Staged holds the kid of every new SigningKeyStatusPending key
	// StageDueRotations generated.
	Staged []string
	// Activated holds the kid of every key PromoteDuePending promoted to
	// SigningKeyStatusActive.
	Activated []string
	// Retired holds the kid of every key RetireDueRetiring retired.
	Retired []string
}

// ScanExpiry runs the jobs-driven expiry scan's three steps in order --
// promote, retire, stage -- and is what job.go's Handler calls on each
// tick. The order matters: promoting first means a purpose whose pending
// key just went active is not immediately re-staged by the staging step in
// the same pass (its fresh NotAfter is far from renewalLeadTime), and
// retiring before staging means a purpose is never carried into staging
// with a stale retiring key still occupying database rows this call could
// have cleared.
//
// docs/internal/22-pki.md's "rotation" section draws the boundary this
// method respects: it advances the state machine and publishes events, and
// NEVER pushes a key to any external system, restarts any process, or
// verifies that a host's own rollout succeeded -- that section is explicit
// that pushing to any external system is never this module's job.
func (s *Service) ScanExpiry(ctx context.Context, cfg RotationConfig) (ScanReport, error) {
	propagationWindow := cfg.PropagationWindow
	if propagationWindow <= 0 {
		propagationWindow = s.propagationWindow
	}
	if propagationWindow <= 0 {
		propagationWindow = DefaultPropagationWindow
	}
	renewalLeadTime := cfg.RenewalLeadTime
	if renewalLeadTime <= 0 {
		renewalLeadTime = s.renewalLeadTime
	}
	if renewalLeadTime <= 0 {
		renewalLeadTime = DefaultRenewalLeadTime
	}

	var report ScanReport

	activated, err := s.PromoteDuePending(ctx, propagationWindow)
	if err != nil {
		return report, fmt.Errorf("pki: promote due pending keys: %w", err)
	}
	report.Activated = activated

	retired, err := s.RetireDueRetiring(ctx)
	if err != nil {
		return report, fmt.Errorf("pki: retire due retiring keys: %w", err)
	}
	report.Retired = retired

	staged, err := s.StageDueRotations(ctx, renewalLeadTime)
	if err != nil {
		return report, fmt.Errorf("pki: stage due rotations: %w", err)
	}
	report.Staged = staged

	return report, nil
}

// PromoteDuePending promotes every SigningKeyStatusPending key created at
// least propagationWindow ago to SigningKeyStatusActive, demoting each
// purpose's previously active key (if any) to SigningKeyStatusRetiring in
// the same transaction (SigningKeyRepository.PromoteToActive) and
// publishing EventSigningKeyActivated once per promotion.
func (s *Service) PromoteDuePending(ctx context.Context, propagationWindow time.Duration) ([]string, error) {
	pending, err := s.signingKeys.ListByStatus(ctx, SigningKeyStatusPending)
	if err != nil {
		return nil, err
	}

	now := s.now()
	var promoted []string
	for _, key := range pending {
		if now.Sub(key.CreatedAt) < propagationWindow {
			continue
		}

		previousActiveID := ""
		if active, err := s.signingKeys.FindActiveByPurpose(ctx, key.Purpose); err == nil {
			previousActiveID = active.ID
		} else if !isNoActiveKey(err) {
			return promoted, err
		}

		if err := s.signingKeys.PromoteToActive(ctx, key.ID, previousActiveID, now); err != nil {
			return promoted, fmt.Errorf("pki: promote pending key %q for purpose %q: %w", key.ID, key.Purpose, err)
		}
		s.cache.invalidate(key.Purpose)

		observability.FromContext(ctx).Info("pki signing key activated",
			"kid", key.ID,
			"purpose", key.Purpose,
			"previous_kid", previousActiveID,
		)
		s.publish(ctx, pkgcore.Event{
			Type: EventSigningKeyActivated,
			Payload: SigningKeyLifecycleEvent{
				Purpose:     key.Purpose,
				KID:         key.ID,
				PreviousKID: previousActiveID,
				OccurredAt:  now,
			},
		})
		promoted = append(promoted, key.ID)
	}
	return promoted, nil
}

// PromoteNow attempts to promote purpose's pending key to active right now,
// rather than waiting for the next ScanExpiry tick, honoring the SAME
// propagationWindow gate PromoteDuePending applies above -- round 3's
// addition.
//
// # Why this lives next to revocation, without being part of it
//
// PromoteNow exists as a companion to Service.RevokeSigningKey
// (revocation.go): revoking a purpose's active key in an emergency leaves
// that purpose with no active signer at all until a replacement is
// promoted, and docs/internal/22-pki.md's own design deliberately never has
// revocation auto-promote a waiting successor -- getting a working signer
// back online is a host's explicit decision, never a side effect the
// module makes for it. A host that already has a pending key staged for
// the purpose (StageDueRotations having run ahead of the incident) calls
// PromoteNow to skip waiting for the next scheduled ScanExpiry tick, while
// still going through the exact same safety window ScanExpiry itself
// honors -- PromoteNow is deliberately NOT a way to bypass the window: the
// incident that makes an operator want to skip it is exactly the scenario
// the window exists to protect against being made worse by (see
// PromoteDuePending's own doc comment for the distributed-replica race the
// window prevents). This is also this round's home for
// ErrPropagationWindowNotElapsed -- round 1/2's AGENTS.md reserved that
// code for "the propagation window" without a caller that used it yet; see
// errors.go's own doc comment for the full accounting of where each of the
// three originally-reserved codes actually landed.
//
// ErrKeyNotFound if purpose has no pending key at all.
// ErrPropagationWindowNotElapsed if one exists but was staged less than
// propagationWindow ago. A zero propagationWindow argument falls back to
// s.propagationWindow (the Service's own configured default), the same
// per-call-override-with-fallback shape ScanExpiry itself uses.
func (s *Service) PromoteNow(ctx context.Context, purpose string, propagationWindow time.Duration) (string, error) {
	if propagationWindow <= 0 {
		propagationWindow = s.propagationWindow
	}
	if propagationWindow <= 0 {
		propagationWindow = DefaultPropagationWindow
	}

	pendingRows, err := s.signingKeys.ListByPurposeAndStatuses(ctx, purpose, SigningKeyStatusPending)
	if err != nil {
		return "", err
	}
	if len(pendingRows) == 0 {
		return "", ErrKeyNotFound.WithParam("purpose", purpose)
	}
	// At most one pending key per purpose in the common case --
	// StageDueRotations refuses to stage a second one while one is already
	// pending (ExistsByPurposeAndStatus's own doc comment) -- so the first
	// row is the pending key.
	pending := pendingRows[0]

	now := s.now()
	if now.Sub(pending.CreatedAt) < propagationWindow {
		return "", ErrPropagationWindowNotElapsed.
			WithParam("kid", pending.ID).
			WithParam("purpose", purpose)
	}

	previousActiveID := ""
	if active, err := s.signingKeys.FindActiveByPurpose(ctx, purpose); err == nil {
		previousActiveID = active.ID
	} else if !isNoActiveKey(err) {
		return "", err
	}

	if err := s.signingKeys.PromoteToActive(ctx, pending.ID, previousActiveID, now); err != nil {
		return "", fmt.Errorf("pki: promote pending key %q for purpose %q: %w", pending.ID, purpose, err)
	}
	s.cache.invalidate(purpose)

	observability.FromContext(ctx).Info("pki signing key activated",
		"kid", pending.ID,
		"purpose", purpose,
		"previous_kid", previousActiveID,
	)
	s.publish(ctx, pkgcore.Event{
		Type: EventSigningKeyActivated,
		Payload: SigningKeyLifecycleEvent{
			Purpose:     purpose,
			KID:         pending.ID,
			PreviousKID: previousActiveID,
			OccurredAt:  now,
		},
	})
	return pending.ID, nil
}

// RetireDueRetiring retires every SigningKeyStatusRetiring key whose
// overlap period (RetiringAt + RetiringOverlap) has elapsed, publishing
// EventSigningKeyRetired once per retirement.
func (s *Service) RetireDueRetiring(ctx context.Context) ([]string, error) {
	retiring, err := s.signingKeys.ListByStatus(ctx, SigningKeyStatusRetiring)
	if err != nil {
		return nil, err
	}

	now := s.now()
	var retired []string
	for _, key := range retiring {
		if key.RetiringAt == nil {
			// Defensive: PromoteToActive always sets RetiringAt in the same
			// transaction that sets the retiring status, so this should be
			// unreachable. Skip rather than retire on an unknown overlap
			// origin -- retiring a key too early is a verification outage
			// for credentials still in flight.
			continue
		}
		if now.Sub(*key.RetiringAt) < key.RetiringOverlap {
			continue
		}

		if err := s.signingKeys.RetireRetiring(ctx, key.ID, now); err != nil {
			return retired, fmt.Errorf("pki: retire key %q for purpose %q: %w", key.ID, key.Purpose, err)
		}
		s.cache.invalidate(key.Purpose)

		observability.FromContext(ctx).Info("pki signing key retired",
			"kid", key.ID,
			"purpose", key.Purpose,
		)
		s.publish(ctx, pkgcore.Event{
			Type: EventSigningKeyRetired,
			Payload: SigningKeyLifecycleEvent{
				Purpose:    key.Purpose,
				KID:        key.ID,
				OccurredAt: now,
			},
		})
		retired = append(retired, key.ID)
	}
	return retired, nil
}

// StageDueRotations generates a new SigningKeyStatusPending key for every
// purpose whose active key's NotAfter is at or before renewalLeadTime from
// now AND that does not already have a pending key staged, publishing
// EventSigningKeyStaged once per new key.
//
// Generating ahead of a hard cutoff, rather than waiting for the active key
// to actually expire, is docs/internal/22-pki.md's own "rotation" section's
// instruction: a purpose must never be carried past its active key's expiry with
// nothing already propagating to replace it.
func (s *Service) StageDueRotations(ctx context.Context, renewalLeadTime time.Duration) ([]string, error) {
	threshold := s.now().Add(renewalLeadTime)
	nearing, err := s.signingKeys.ListActiveNearingExpiry(ctx, threshold)
	if err != nil {
		return nil, err
	}

	var staged []string
	for _, active := range nearing {
		exists, err := s.signingKeys.ExistsByPurposeAndStatus(ctx, active.Purpose, SigningKeyStatusPending)
		if err != nil {
			return staged, err
		}
		if exists {
			// Already staged on a previous tick -- PromoteDuePending will
			// pick it up once its propagation window elapses.
			continue
		}

		kid, err := s.stageRotation(ctx, active)
		if err != nil {
			return staged, err
		}
		staged = append(staged, kid)
	}
	return staged, nil
}

// stageRotation generates a new pending key replacing active: same purpose
// and algorithm, the same signer this Service was built with, a validity
// window matching active's own duration, and active's own RetiringOverlap
// carried forward -- one purpose's overlap requirement does not change key
// to key (see SigningKey.RetiringOverlap's doc comment).
func (s *Service) stageRotation(ctx context.Context, active SigningKey) (string, error) {
	keyRef, pub, err := s.signer.GenerateKey(ctx, active.Algorithm)
	if err != nil {
		return "", err
	}
	pkix, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("pki: marshal public key for purpose %q: %w", active.Purpose, err)
	}

	validity := active.NotAfter.Sub(active.NotBefore)
	if validity <= 0 {
		validity = defaultKeyValidity
	}

	now := s.now().UTC()
	key := &SigningKey{
		ID:              uuid.NewString(),
		Purpose:         active.Purpose,
		Algorithm:       active.Algorithm,
		SignerName:      s.signerName,
		KeyRef:          keyRef,
		Status:          SigningKeyStatusPending,
		PublicKey:       pkix,
		NotBefore:       now,
		NotAfter:        now.Add(validity),
		RetiringOverlap: active.RetiringOverlap,
	}
	if err := s.signingKeys.Create(ctx, key); err != nil {
		return "", fmt.Errorf("pki: stage rotation key for purpose %q: %w", active.Purpose, err)
	}
	s.cache.invalidate(active.Purpose)

	observability.FromContext(ctx).Info("pki signing key staged",
		"kid", key.ID,
		"purpose", key.Purpose,
		"replaces_kid", active.ID,
	)
	s.publish(ctx, pkgcore.Event{
		Type: EventSigningKeyStaged,
		Payload: SigningKeyLifecycleEvent{
			Purpose:    active.Purpose,
			KID:        key.ID,
			OccurredAt: now,
		},
	})
	return key.ID, nil
}
