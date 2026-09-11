package pki

import (
	"context"
	"crypto"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// defaultKeyValidity is how long a signing key EnsurePurpose creates stays
// valid before the expiry scan stages a replacement. It is also the
// validity duration a rotation-staged key inherits when nothing more
// specific applies (see stageRotation).
const defaultKeyValidity = 24 * time.Hour * 365

// DefaultCacheTTL is the key-set cache's anti-loss expiry -- the TTL fallback
// that expires a cached key set even when the invalidation event for a change
// never arrives. A package-level named constant for the same reason
// rbac.DefaultCacheTTL is one: a stable domain default, not a dynamic
// configuration item.
const DefaultCacheTTL = 30 * time.Second

// Service is the key-lifecycle layer's public entry point and the type
// authn's KeySource switch holds a *Service (or something structurally
// identical to it) behind.
//
// # The state machine
//
// Every method below keeps the shape authn's KeySource interface requires:
// signatures built entirely from standard-library types, because structural
// interface satisfaction across two packages' own named types requires
// exact, literal signature equality (two packages' named structs are never
// the same type -- see keySourceShape below for the compile-time proof this
// package carries of its own conformance).
//
//   - EnsurePurpose creates a key and marks it SigningKeyStatusActive
//     synchronously when purpose has no active key at all -- the bootstrap
//     case, where no multi-replica cache-propagation race exists yet
//     because nothing has verified anything under the purpose's kid before.
//     The pending/propagation-window dance is for ROTATION, not bootstrap:
//     see lifecycle.go's StageDueRotations, PromoteDuePending and
//     RetireDueRetiring, which the jobs-driven expiry scan (job.go) drives.
//     When the purpose's active key has fallen out of its validity window
//     and no pending successor is in flight, EnsurePurpose heals the
//     purpose itself -- revokes the expired key and creates an in-validity
//     replacement -- so a purpose never stays dead waiting for a scan tick
//     that may not be wired.
//   - ActiveSigner and VerificationKeys read through this Service's
//     process-local key-set cache (cache.go) rather than querying the
//     database on every call, invalidated by this module's own published
//     events plus a TTL fallback poll.
//   - EnsurePurpose does not verify that an already-active key's Algorithm
//     matches the requested one on a repeated call; an Algorithm mismatch
//     is a known limitation of the call (see EnsurePurpose's own doc
//     comment).
//
// # Validity is enforced at key-take time, never expressed through status
//
// A signing key's NotBefore/NotAfter are enforced on EVERY read path at
// the moment the key is taken -- ActiveSigner refuses an out-of-validity
// active key with ErrNoActiveKey, VerificationKeys offers only
// in-validity keys, ExportJWKS publishes only in-validity keys, and
// EnsurePurpose treats an out-of-validity active key as no key at all
// (healing as described above). Status and validity are two independent
// axes: the state machine's statuses say how far rotation has progressed,
// while the validity window says whether the key may be USED at this
// instant, and the two can disagree (an "active" key whose NotAfter
// passed because the expiry scan has not run yet).
//
// The boundary direction, chosen and written down rather than defaulted:
// both ends of the window are INCLUSIVE (a key is valid at its NotAfter
// and refused a moment later), the identical boundary crypto/x509 applies
// to a certificate's validity window. For the verify path the choice is
// strict now-filtering, NOT judgment by a token's issue time -- pki never
// parses the credentials it enables (VerificationKeys returns public keys
// and metadata only, per the KeySource shape), so the token issue time is
// not available where the key is chosen. The availability cost of strict
// now-filtering is bounded by the rotation design itself: the expiry scan
// demotes an active key to retiring (its last possible signing instant)
// when its replacement activates, which the rotation lead time schedules
// a default of thirty days before NotAfter -- so tokens signed by an
// in-validity key normally outlive their key's NotAfter only when a host
// declares a maxCredentialLifetime close to or beyond its renewal lead
// time, which such a host must size itself (WithRenewalLeadTime). The
// security failure being bought off is unbounded and scan-dependent: an
// expired key accepted on every path until the scan happens to advance
// its status -- which, in the limit, means an expired key accepted
// forever on a host that never runs the scan.
type Service struct {
	signer      Signer
	signerName  string
	signingKeys *SigningKeyRepository

	// bus is the pkgcore.EventBus this Service publishes lifecycle events
	// on and subscribes its own cache-invalidation handler to. It is nil
	// until attachBus runs (Module.Register), which is also when the
	// subscription is installed -- see attachBus's own doc comment for why
	// that is not a problem for EnsurePurpose calls that happen to race it.
	bus pkgcore.EventBus

	cache *keySetCache

	// now is the clock every lifecycle decision reads, overridable in
	// tests so propagation-window and overlap-period boundaries are
	// testable without sleeping.
	now func() time.Time

	// propagationWindow and renewalLeadTime are RotationConfig's resolved
	// defaults, applied when the caller of a lifecycle.go method passes a
	// zero RotationConfig field. See WithPropagationWindow/
	// WithRenewalLeadTime on Module for how a host overrides them.
	propagationWindow time.Duration
	renewalLeadTime   time.Duration

	// expiryScanWindow is the period one expiry-scan idempotency key covers
	// (DefaultExpiryScanWindow unless the host overrides it through
	// WithExpiryScanWindow): EnqueueExpiryScan (job.go) places each enqueue
	// in the window its clock read falls in, so same-window enqueues from a
	// multi-replica scheduler collapse into one job and later windows get
	// their own. A window at or below the host's scheduler interval voids
	// the dedup -- every tick lands in a fresh window -- so hosts whose
	// cadence approaches the default must size the window themselves.
	expiryScanWindow time.Duration

	// queue is the jobs.Queue EnqueueExpiryScan schedules the expiry-scan
	// task on (job.go). It is nil until attachQueue runs (Module.Register,
	// only when the host supplied one via WithQueue) -- a nil queue is a
	// legitimate, supported configuration: EnsurePurpose/ActiveSigner/
	// VerificationKeys need no queue at all, and a host that runs no
	// workers simply gets no automatic rotation, exactly like storage's
	// identical "a nil queue makes the schedule point fail with a plain
	// error" contract for its own optional sweep.
	queue jobs.Queue
}

// NewService returns a Service that creates keys through signer (recorded
// on every row under signerName) and persists them through signingKeys, with
// a key-set cache expiring after cacheTTL (DefaultCacheTTL if the caller has
// no reason to override it; a value <=0 disables the cache, reading through
// to the database on every call -- the setting tests that want to observe
// every write immediately use).
//
// expiryScanWindow is the period one expiry-scan idempotency key covers
// (see Service.expiryScanWindow's own doc comment); a value <=0 falls back
// to DefaultExpiryScanWindow.
func NewService(signer Signer, signerName string, signingKeys *SigningKeyRepository, cacheTTL, propagationWindow, renewalLeadTime, expiryScanWindow time.Duration) *Service {
	if expiryScanWindow <= 0 {
		expiryScanWindow = DefaultExpiryScanWindow
	}
	return &Service{
		signer:            signer,
		signerName:        signerName,
		signingKeys:       signingKeys,
		cache:             newKeySetCache(cacheTTL),
		now:               time.Now,
		propagationWindow: propagationWindow,
		renewalLeadTime:   renewalLeadTime,
		expiryScanWindow:  expiryScanWindow,
	}
}

// attachBus hands Service the registry's EventBus and subscribes its own
// cache-invalidation handler to the three lifecycle events -- the same
// "subscribe to your own events" shape go/rbac's Module.Attach documents,
// for the identical reason: it is how a replica learns about a rotation a
// different replica performed, and running the local write's invalidation
// through the same handler as the remote one means there is a single
// invalidation code path to get right rather than two that could drift.
//
// Called from Module.Register, which -- per pkgcore.Module's own contract
// -- performs no I/O: this is a plain field assignment plus a subscription
// registration, exactly like go/storage's serviceHost.attach.
func (s *Service) attachBus(reg *pkgcore.Registry) {
	s.bus = reg.Events.Bus()
	reg.Events.Subscribe(EventSigningKeyStaged, s.onSigningKeyLifecycleEvent)
	reg.Events.Subscribe(EventSigningKeyActivated, s.onSigningKeyLifecycleEvent)
	reg.Events.Subscribe(EventSigningKeyRetired, s.onSigningKeyLifecycleEvent)
	// Revocation invalidates the key-set cache through this SAME
	// event-subscription mechanism, never a second cache-clearing path --
	// see RevokeSigningKey's own doc comment (revocation.go).
	reg.Events.Subscribe(EventSigningKeyRevoked, s.onSigningKeyLifecycleEvent)
}

// attachQueue hands Service the jobs.Queue the host wired via WithQueue, so
// EnqueueExpiryScan (job.go) has somewhere to schedule onto. Called from
// Module.Register only when the host supplied one -- a plain field
// assignment, so Register's no-I/O contract stands, exactly like attachBus.
func (s *Service) attachQueue(queue jobs.Queue) {
	s.queue = queue
}

// Close releases Service's background resources: it stops the key-set
// cache's janitor goroutine and waits for it to exit. It is idempotent, and
// Service stays usable -- and correct -- afterwards: without a janitor,
// expired entries are still refused by the cache's own expiry check
// (cache.go's get), they are simply no longer proactively reclaimed. See
// rbac.Service.Close's identical doc comment for the same reasoning applied
// to that module's decision cache.
func (s *Service) Close() error {
	s.cache.close()
	return nil
}

// onSigningKeyLifecycleEvent is the subscriber attachBus installs. It drops
// the affected purpose's cached entry, wherever in the deployment the
// change happened, so the next ActiveSigner/VerificationKeys call reads
// fresh.
//
// It never returns an error, for the same reason rbac's own subscriber
// doesn't (go/rbac/authorizer_service.go's onRoleBindingChanged): on the in-memory bus
// this handler runs synchronously inside the publishing call, and the row
// is already committed by the time the event is published, so a returned
// error would make an already-successful write report failure. A payload
// of an unrecognized shape is dropped for the same reason; the cache's TTL
// fallback covers that case.
func (s *Service) onSigningKeyLifecycleEvent(_ context.Context, evt pkgcore.Event) error {
	payload, ok := signingKeyLifecycleEventFromWire(evt.Payload)
	if !ok {
		return nil
	}
	s.cache.invalidate(payload.Purpose)
	return nil
}

// publish sends evt on s.bus when one is attached. Before attachBus runs
// (which is only possible if a caller uses a Service outside the normal
// Module wiring, since Register always attaches one before returning) evt
// is silently dropped rather than panicking on a nil bus -- the cache's TTL
// poll is still correct in that case, just slower to converge, which is a
// strictly better failure mode than crashing the write that already
// committed.
func (s *Service) publish(ctx context.Context, evt pkgcore.Event) {
	if s.bus == nil {
		return
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		observability.FromContext(ctx).Error("pki: publish signing key lifecycle event failed",
			"event_type", evt.Type,
			"error", err,
		)
	}
}

// EnsurePurpose declares that purpose needs a signing key of algorithm, with
// a retiring overlap period that must eventually cover maxCredentialLifetime.
//
// Idempotent in the sense a caller needs at bootstrap: if purpose already
// has an active key WITHIN its validity window, EnsurePurpose returns nil
// without creating a second one. It does not verify that the existing
// key's Algorithm matches the algorithm argument: a repeated call with a
// different algorithm for the same purpose silently keeps the first key, a
// known limitation of this call.
//
// # Self-healing an out-of-validity active key
//
// Validity is enforced at key-take time (see this type's own doc comment),
// so an active key whose NotAfter has passed -- the expiry scan never ran,
// or ran too late -- is refused by ActiveSigner. EnsurePurpose must not
// then answer "already has an active key" about that expired row and leave
// the purpose dead until a scan tick: when the purpose's active key is out
// of validity AND no pending successor is in flight (the scan staged one
// but stopped before promoting it -- in which case the scan, or a host's
// PromoteNow, owns the rotation and bootstrap must not churn the purpose
// with a second candidate), EnsurePurpose revokes the expired key through
// the same guarded transition and event Service.RevokeSigningKey uses --
// revoking is what frees the active slot for a replacement, since the
// partial unique index allows one active row per purpose, and what tells
// every replica's cache the key is gone -- and falls through to create an
// in-validity replacement in the same call. Revoking rather than silently
// overwriting keeps the expired key's fate on the record: its row carries
// the RevocationReason and the EventSigningKeyRevoked event reaches the
// same subscribers an operator-initiated revoke reaches.
//
// This is the BOOTSTRAP path, not rotation: it creates a key and marks it
// SigningKeyStatusActive in the same call, synchronously, with no
// propagation window. That is safe here specifically because a purpose
// with no active key has never signed anything -- there is no multi-replica
// cache-propagation race to protect against on a kid nothing has verified
// yet. Rotating an EXISTING active key goes through the pending/
// propagation-window path instead -- see lifecycle.go. (The self-heal's
// revoke-then-create has the same bounded staleness profile as any
// revocation -- a replica whose cache still holds the revoked kid can
// keep using it for at most the cache TTL, exactly like RevokeSigningKey's
// own documented behavior.)
func (s *Service) EnsurePurpose(ctx context.Context, purpose, algorithm string, maxCredentialLifetime time.Duration) error {
	if purpose == "" {
		return fmt.Errorf("pki: EnsurePurpose requires a non-empty purpose")
	}
	if maxCredentialLifetime <= 0 {
		return fmt.Errorf("pki: EnsurePurpose requires a positive maxCredentialLifetime")
	}

	now := s.now()
	active, err := s.signingKeys.FindActiveByPurpose(ctx, purpose)
	if err == nil {
		if keyInValidity(*active, now) {
			// Already has an in-validity active key -- nothing to
			// bootstrap. What an Algorithm mismatch here should mean
			// (error? rotate?) is an open question; the call neither
			// detects it nor acts on it (see this method's own doc
			// comment).
			return nil
		}
		// The active key is out of validity. Heal only when no USABLE
		// pending successor is in flight -- see this method's own doc
		// comment. A pending key the scan staged is itself out of validity
		// only when the scan stopped so long that even the staged rotation
		// expired; leaving that row behind would let the next scan tick
		// promote a dead key into the very active slot this heal is about
		// to fill, so it is revoked alongside the expired active key.
		pending, listErr := s.signingKeys.ListByPurposeAndStatuses(ctx, purpose, SigningKeyStatusPending)
		if listErr != nil {
			return listErr
		}
		for i := range pending {
			if keyInValidity(pending[i], now) {
				// A usable successor is staged; the scan (or PromoteNow)
				// promotes it. Bootstrap must not create a rival.
				return nil
			}
		}
		for i := range pending {
			if _, revokeErr := s.RevokeSigningKey(ctx, pending[i].ID, expiredKeyRevokeReason); revokeErr != nil {
				return revokeErr
			}
		}
		if _, revokeErr := s.RevokeSigningKey(ctx, active.ID, expiredKeyRevokeReason); revokeErr != nil {
			return revokeErr
		}
		// Fall through: the active slot is free, create a fresh key below.
	} else if !apperr.HasCode(err, ErrNoActiveKey.Code) {
		return err
	}

	keyRef, pub, err := s.signer.GenerateKey(ctx, algorithm)
	if err != nil {
		return err
	}
	pkix, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return fmt.Errorf("pki: marshal public key for purpose %q: %w", purpose, err)
	}

	// The key's validity window is anchored to the same clock read the
	// existence check above used -- a fresh key must be in validity from
	// the instant this call decided the purpose needed one.
	now = now.UTC()
	key := &SigningKey{
		ID:              uuid.NewString(),
		Purpose:         purpose,
		Algorithm:       algorithm,
		SignerName:      s.signerName,
		KeyRef:          keyRef,
		Status:          SigningKeyStatusActive,
		PublicKey:       pkix,
		NotBefore:       now,
		NotAfter:        now.Add(defaultKeyValidity),
		ActivatedAt:     &now,
		RetiringOverlap: maxCredentialLifetime,
	}
	if err := s.signingKeys.Create(ctx, key); err != nil {
		// The write is arbitrated by uq_pki_signing_keys_active_purpose (at
		// most one active row per purpose), so this can fail one way only:
		// a concurrent replica's heal created its replacement between this
		// call's existence check and its insert. That is not a failure of
		// THIS call -- the goal state (an in-validity active key) exists --
		// so re-read and confirm rather than surfacing a unique-violation
		// error to a caller whose purpose was just healed. The re-read is
		// authoritative either way: a purpose that still lacks an
		// in-validity active key after the failed insert genuinely has a
		// store problem and must not be answered as healed.
		if healed, reErr := s.signingKeys.FindActiveByPurpose(ctx, purpose); reErr == nil && keyInValidity(*healed, s.now()) {
			return nil
		}
		return fmt.Errorf("pki: store signing key for purpose %q: %w", purpose, err)
	}
	s.cache.invalidate(purpose)

	observability.FromContext(ctx).Info("pki signing key activated",
		"kid", key.ID,
		"purpose", purpose,
		"algorithm", algorithm,
	)
	s.publish(ctx, pkgcore.Event{
		Type: EventSigningKeyActivated,
		Payload: SigningKeyLifecycleEvent{
			Purpose:    purpose,
			KID:        key.ID,
			OccurredAt: now,
		},
	})
	return nil
}

// expiredKeyRevokeReason is the RevocationReason EnsurePurpose records
// when its self-heal revokes a key that fell out of its validity window
// (see EnsurePurpose's own doc comment). It deliberately reads as a
// machine cause, distinct from an operator's incident reason, so the row
// (and the audit view that surfaces RevocationReason) can tell the two
// apart.
const expiredKeyRevokeReason = "signing key expired past its NotAfter before rotation completed; EnsurePurpose created a replacement"

// validityWindowCovers reports whether the [notBefore, notAfter] validity
// window covers now: usable from NotBefore through NotAfter, both ends
// inclusive -- the identical boundary crypto/x509 applies to a
// certificate's validity window (valid AT NotAfter, invalid a moment
// later). This is the single time-window predicate behind every read
// path's validity enforcement -- keyInValidity (below) wraps it for
// SigningKey rows (ActiveSigner, VerificationKeys, ExportJWKS and
// EnsurePurpose's existence check), and ExportAuthorityChainJWKS (jwks.go)
// applies it to each authority's parsed certificate -- so the boundary is
// defined in exactly one place.
func validityWindowCovers(notBefore, notAfter, now time.Time) bool {
	return !now.Before(notBefore) && !now.After(notAfter)
}

// keyInValidity reports whether key's validity window covers now -- the
// SigningKey-row shape of the single validityWindowCovers predicate above.
func keyInValidity(key SigningKey, now time.Time) bool {
	return validityWindowCovers(key.NotBefore, key.NotAfter, now)
}

// ActiveSigner returns the kid, algorithm and a context-aware signing
// function for purpose's currently active key. ErrNoActiveKey if
// EnsurePurpose was never called for purpose, if every key for it has
// since been revoked, or if the purpose's active key is outside its
// validity window -- the last checked AT THE MOMENT THE KEY IS TAKEN,
// against the service clock, so a key whose NotAfter passes between cache
// loads is refused on the very next call, never served for the remainder
// of a cache TTL (see this type's own doc comment for the boundary
// choice).
//
// It reads through the key-set cache (cache.go): a cache hit costs no
// database round trip, and a miss loads and populates every status the
// cache holds for purpose in one query (loadKeySet) rather than one query
// per method.
//
// It returns a signing function rather than a crypto.Signer for the exact
// reason Signer.Sign itself takes a context.Context -- see that interface's
// doc comment.
func (s *Service) ActiveSigner(ctx context.Context, purpose string) (string, string, func(context.Context, []byte) ([]byte, error), error) {
	entry, err := s.keySet(ctx, purpose)
	if err != nil {
		return "", "", nil, err
	}
	now := s.now()
	if entry.active == nil || !keyInValidity(*entry.active, now) {
		// No active key, or the active key is not usable at this instant
		// (before its NotBefore, past its NotAfter). Both answer the same:
		// this purpose has no usable signer right now -- the answer that
		// fails closed, and the answer the state machine's rotation (or
		// EnsurePurpose's self-heal) eventually repairs.
		return "", "", nil, ErrNoActiveKey
	}
	keyRef := entry.active.KeyRef
	sign := func(ctx context.Context, input []byte) ([]byte, error) {
		return s.signer.Sign(ctx, keyRef, input)
	}
	return entry.active.ID, entry.active.Algorithm, sign, nil
}

// VerificationKeys returns every key for purpose that is still safe to
// verify against: the keys SigningKeyRepository.ListVerifiableByPurpose
// returns by status -- pending, active or retiring -- that are ALSO within
// their validity window at this instant. The status filter alone is not
// the whole answer: a key the expiry scan has not retired yet but whose
// NotAfter has passed is still "retiring" as far as the status vocabulary
// goes, and offering it would let a verifier accept tokens signed by an
// expired key -- which is exactly why the validity check runs here, at the
// moment the keys are taken, against the service clock, never against the
// state machine's statuses. (The strict now-filtering choice and its
// bounded availability cost are this type's own doc comment's subject.)
// The anonymous return-slice element type is not a stylistic choice: a
// named type here would break structural satisfaction of authn's KeySource
// -- two packages' named types can never satisfy one another structurally
// -- so it is written out in full, matching KeySource's own declaration
// exactly.
func (s *Service) VerificationKeys(ctx context.Context, purpose string) ([]struct {
	KID       string
	Algorithm string
	Public    crypto.PublicKey
}, error,
) {
	entry, err := s.keySet(ctx, purpose)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]struct {
		KID       string
		Algorithm string
		Public    crypto.PublicKey
	}, 0, len(entry.verifiable))
	for _, row := range entry.verifiable {
		if !keyInValidity(row, now) {
			continue
		}
		pub, err := x509.ParsePKIXPublicKey(row.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("pki: parse public key for kid %q: %w", row.ID, err)
		}
		out = append(out, struct {
			KID       string
			Algorithm string
			Public    crypto.PublicKey
		}{KID: row.ID, Algorithm: row.Algorithm, Public: pub})
	}
	return out, nil
}

// keySet returns purpose's cached key set, loading and caching it from the
// database on a miss.
func (s *Service) keySet(ctx context.Context, purpose string) (keySetEntry, error) {
	now := s.now()
	if entry, ok := s.cache.get(purpose, now); ok {
		return entry, nil
	}

	rows, err := s.signingKeys.ListVerifiableByPurpose(ctx, purpose)
	if err != nil {
		return keySetEntry{}, err
	}
	var active *SigningKey
	for i := range rows {
		if rows[i].Status == SigningKeyStatusActive {
			row := rows[i]
			active = &row
			break
		}
	}
	entry := keySetEntry{active: active, verifiable: rows, loadedAt: now}
	s.cache.put(purpose, active, rows, now)
	return entry, nil
}

// keySourceShape mirrors, field for field and in the same order, go/authn's
// KeySource interface. It exists purely as a compile-time proof that
// *Service satisfies that shape without this module importing authn (which
// would invert the module dependency direction: authn depends on pki, never
// the reverse). A mismatch between this mirror and authn's own declaration
// is this module's bug to fix, not authn's.
type keySourceShape interface {
	EnsurePurpose(ctx context.Context, purpose, algorithm string, maxCredentialLifetime time.Duration) error
	ActiveSigner(ctx context.Context, purpose string) (kid string, algorithm string, sign func(context.Context, []byte) ([]byte, error), err error)
	VerificationKeys(ctx context.Context, purpose string) ([]struct {
		KID       string
		Algorithm string
		Public    crypto.PublicKey
	}, error)
}

// compile-time check that *Service satisfies keySourceShape.
var _ keySourceShape = (*Service)(nil)
