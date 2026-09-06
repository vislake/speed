package pki

import (
	"context"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// ReclaimReport summarizes what one Service.ReclaimRetired call did: the
// kid of every retired key that reached each outcome, in the order they
// were processed. Every slice is nil (never a non-nil empty slice) when a
// call produced no key of that outcome, so a caller can check len()
// without also checking for nil -- the same convention ScanReport follows.
type ReclaimReport struct {
	// Destroyed holds the kid of every retired key whose private-key
	// material is gone by the time the call returns: either this call's
	// Signer.Destroy returned nil, or it returned ErrKeyNotFound, which
	// means a previous reclaim (or a manual Destroy) already removed the
	// material -- the same end state, reached one call earlier. The two
	// are deliberately not separated into distinct lists: both mean
	// "reclaimed", and only the log distinguishes which run did it.
	Destroyed []string
	// NotOwned holds the kid of every retired row whose SignerName is not
	// this Service's own signerName. Such a row's material belongs to a
	// different Signer (a second Service instance sharing this table), and
	// this Service never touches it -- see ReclaimRetired's own doc
	// comment for why a guard, rather than blind trust in the interface,
	// is the load-bearing part of that promise.
	NotOwned []string
	// Failed holds the kid of every retired key whose Signer.Destroy
	// returned a genuine error (anything other than ErrKeyNotFound). Each
	// failure is logged with the kid and the error; the key stays retired
	// with its material in place, so a later call re-attempts it.
	Failed []string
}

// ReclaimRetired is the key-lifecycle layer's retirement-and-reclaim point:
// for every SigningKeyStatusRetired key this Service's own signer owns, it
// calls the signer's Destroy on the key's material, then reports what
// happened.
//
// # Why this method exists, and why it is host-invoked
//
// The expiry scan's last transition leaves keys at retired with their
// private-key material still in place: LocalSigner's encrypted
// pki_local_keys rows accumulate forever, and the vault/kmsaws direct-sign
// providers' keys stay live in the provider. That is deliberate -- this
// module's AGENTS.md records the census finding P2-2 as assessed and
// deferred precisely because destruction is a lifecycle-policy decision,
// not a wiring detail, for three reasons this method turns into its
// contract:
//
//  1. Destroy under the direct-sign modes is REAL provider-side deletion
//     (Vault deletes the Transit key outright; AWS KMS schedules deletion
//     behind its enforced minimum 7-day pending window). The expiry scan
//     must never do that by itself: ScanExpiry's own doc comment draws the
//     module's boundary as "advances the state machine and publishes
//     events, and NEVER pushes a key to any external system" -- the same
//     "the module manages the state machine, the host owns every push"
//     division docs/internal/22-pki.md's rotation section draws. Destroy
//     on a direct-sign key IS a push to an external system, so it lives
//     here, behind an explicit host call, not inside the jobs-driven scan.
//  2. Whether, and how eagerly, retired material may be destroyed is a
//     per-deployment policy. ReclaimRetired leaves that policy to the
//     host: a deployment "enables reclamation" by calling this method on
//     its own schedule (after each expiry-scan drain for a LocalSigner
//     deployment; rarely and deliberately for a direct-sign deployment;
//     see below), and its schedule IS the retention declaration -- the
//     module adds no configuration schema, the same reason
//     DefaultCacheTTL is a named constant rather than a dynamic
//     configuration item.
//  3. The state machine's vocabulary already carries the consultable
//     boundary this method acts on: retired means "the retiring overlap
//     period has elapsed", which the design defines as "nothing signed
//     under this key is still offered for verification" -- by the time a
//     key is retired, the module has no remaining use for its private
//     material. No new post-retired state is needed for a host that
//     reclaims deliberately; whether automatic destruction needs one is a
//     question this round records rather than answers (see AGENTS.md's
//     round entry).
//
// # What Destroy means per implementation
//
// This method cannot know what an implementation's Destroy does -- that is
// the Signer seam's own business, and each implementation's Destroy doc
// comment says it plainly: LocalSigner physically deletes the encrypted
// pki_local_keys row (real reclamation of the accumulating rows P2-2
// measured); vault/kmsaws in ModeDirectSign ask the provider to remove the
// key; vault/kmsaws in ModeEnvelope validate that the keyRef (which IS the
// ciphertext, held in this table's own key_ref column) still decrypts and
// otherwise no-op, because dropping the caller-held row is what actually
// destroys an envelope-mode key. Reclaiming an envelope-mode deployment
// therefore reclaims nothing -- by the module's own recorded boundary, the
// ciphertext row's retention is a row-history decision for the host, not a
// destroy this module can perform for it. The report's Destroyed list must
// be read through that lens: it reports "Destroy answered nil", and what
// nil means is the implementation's documented Destroy semantics.
//
// # What this method guarantees
//
//   - It touches only SigningKeyStatusRetired rows. Keys in every other
//     status -- pending, active, retiring, revoked -- are still doing
//     lifecycle work (a retiring key is still verifiable; the module's
//     guarantee that a retiring key stays valid until its overlap truly
//     elapsed is exactly why destruction waits for retired) and are never
//     offered to Destroy.
//   - It touches only rows whose SignerName equals this Service's own
//     signerName. A row owned by a different Signer implementation (a
//     second Service instance sharing the pki_signing_keys table) is
//     reported in NotOwned and never handed to this Service's signer: one
//     signer's Destroy on another signer's keyRef is at best a spurious
//     provider call and at worst -- when keyRef namespaces collide, as a
//     LocalSigner's uuid keyRef and a Vault Transit key name both can -- a
//     cross-owner delete of material this Service does not own. The
//     ErrKeyNotFound-as-already-reclaimed rule below makes the guard
//     load-bearing rather than cosmetic: without it, a signer that cannot
//     find a foreign keyRef would report "converged" and a foreign row's
//     live material would be skipped forever while its owner believed
//     reclamation had run.
//   - ErrKeyNotFound from Destroy means the material is already gone --
//     a previous call destroyed it, or the keyRef no longer resolves -- so
//     the key is counted as Destroyed rather than Failed. This is what
//     makes repeated calls converge quietly for implementations that
//     answer that way (LocalSigner does; a vault/kmsaws direct-sign call
//     against an already-destroyed provider key may answer with the
//     provider's own error instead -- that lands in Failed, and the log
//     names the kid so an operator can reconcile it against the provider's
//     console. Providers that want the silent-convergence answer may adopt
//     ErrKeyNotFound in a future edit, exactly as errors.go already notes
//     for other codes).
//   - A failed Destroy never aborts the walk: the failure is logged and
//     reported in Failed, and the remaining keys are still processed. The
//     failed key stays retired with its material in place, so the next
//     call re-attempts it -- an interruption at any point converges on the
//     next run rather than being lost.
//   - Revoked keys are deliberately NOT reclaimed, even though revocation
//     also ends a key's verification life. Revocation is the emergency
//     path: incident response may need the material of the very key that
//     was just stopped, and an emergency action should never silently
//     bundle deletion of anything. A host that wants a revoked key's
//     material gone calls its signer's Destroy directly.
//
// # No event, no cache invalidation
//
// ReclaimRetired changes no row and publishes no event: retired keys are
// already outside every replica's verifiable key set and the active
// pointer (ListVerifiableByPurpose excludes them), so no other replica's
// cache needs to learn anything, and a Destroy answer is not a state
// transition the event vocabulary names. Hosts observe reclamation through
// the returned report or their own scheduling logs.
//
// Concurrent calls are safe in the sense that this method writes no
// database state: two overlapping calls may ask Destroy twice for one key,
// and each implementation answers as its own contract says (LocalSigner's
// second answer is ErrKeyNotFound, counted as Destroyed). Like
// ScanExpiry's own overlapping-tick tolerance, the harm of a duplicate is
// bounded by the guarded update it never performs.
//
// The only error this method itself returns is a database failure loading
// the retired set, before any Destroy is attempted.
func (s *Service) ReclaimRetired(ctx context.Context) (ReclaimReport, error) {
	retired, err := s.signingKeys.ListByStatus(ctx, SigningKeyStatusRetired)
	if err != nil {
		return ReclaimReport{}, err
	}

	var report ReclaimReport
	for _, key := range retired {
		if key.SignerName != s.signerName {
			report.NotOwned = append(report.NotOwned, key.ID)
			continue
		}

		if err := s.signer.Destroy(ctx, key.KeyRef); err != nil {
			if isKeyNotFound(err) {
				observability.FromContext(ctx).Info("pki retired signing key already reclaimed",
					"kid", key.ID,
					"purpose", key.Purpose,
					"signer_name", key.SignerName,
				)
				report.Destroyed = append(report.Destroyed, key.ID)
				continue
			}
			observability.FromContext(ctx).Error("pki retired signing key reclamation failed",
				"kid", key.ID,
				"purpose", key.Purpose,
				"signer_name", key.SignerName,
				"error", err,
			)
			report.Failed = append(report.Failed, key.ID)
			continue
		}

		observability.FromContext(ctx).Info("pki retired signing key material destroyed",
			"kid", key.ID,
			"purpose", key.Purpose,
			"signer_name", key.SignerName,
		)
		report.Destroyed = append(report.Destroyed, key.ID)
	}
	return report, nil
}

// isKeyNotFound reports whether err is (a decorated) ErrKeyNotFound.
func isKeyNotFound(err error) bool {
	found, ok := apperr.As(err)
	return ok && found.Code == ErrKeyNotFound.Code
}
