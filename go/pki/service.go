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

// DefaultCacheTTL is the key-set cache's anti-loss expiry -- the fallback
// poll docs/internal/22-pki.md's "caching" section requires behind event
// invalidation. A package-level named constant for the same reason
// rbac.DefaultCacheTTL is one (backend coding standard §10: stable domain
// defaults, not a dynamic configuration item -- reading one would make pki
// depend on config, an edge this round does not add).
const DefaultCacheTTL = 30 * time.Second

// Service is the key-lifecycle layer's public entry point --
// docs/internal/22-pki.md's "two-layer structure" upper layer, and the type
// authn's KeySource switch holds a *Service (or something structurally
// identical to it) behind.
//
// # Round 2: the real state machine
//
// Every method below matches the shape docs/internal/22-pki.md's "authn's
// integration" section requires of authn's KeySource interface: signatures
// built entirely from standard-library types, because structural interface
// satisfaction across two packages' own named types requires exact,
// literal signature equality (two packages' named structs are never the
// same type -- see keySourceShape below for the compile-time proof this
// package carries of its own conformance).
//
//   - EnsurePurpose still creates a key and marks it SigningKeyStatusActive
//     synchronously when purpose has no active key at all -- the bootstrap
//     case, where no multi-replica cache-propagation race exists yet
//     because nothing has verified anything under the purpose's kid before.
//     The pending/propagation-window dance is for ROTATION, not bootstrap:
//     see lifecycle.go's StageDueRotations, PromoteDuePending and
//     RetireDueRetiring, which the jobs-driven expiry scan (job.go) drives.
//   - ActiveSigner and VerificationKeys read through this Service's
//     process-local key-set cache (cache.go) rather than querying the
//     database on every call, invalidated by this module's own published
//     events plus a TTL fallback poll -- the caching round 1's AGENTS.md
//     flagged as its own known limitation.
//   - EnsurePurpose still does not verify that an already-active key's
//     Algorithm matches the requested one on a repeated call -- see
//     AGENTS.md's Known limitations, carried forward from round 1
//     unchanged.
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
func NewService(signer Signer, signerName string, signingKeys *SigningKeyRepository, cacheTTL, propagationWindow, renewalLeadTime time.Duration) *Service {
	return &Service{
		signer:            signer,
		signerName:        signerName,
		signingKeys:       signingKeys,
		cache:             newKeySetCache(cacheTTL),
		now:               time.Now,
		propagationWindow: propagationWindow,
		renewalLeadTime:   renewalLeadTime,
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
	// Round 3: revocation must invalidate the key-set cache through this
	// SAME event-subscription mechanism, never a second cache-clearing
	// path -- see RevokeSigningKey's own doc comment (revocation.go).
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
// doesn't (go/rbac/service.go's onRoleBindingChanged): on the in-memory bus
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
// has an active key, EnsurePurpose returns nil without creating a second
// one. It does not currently verify that the existing key's Algorithm
// matches the algorithm argument -- see AGENTS.md's Known limitations.
//
// This is the BOOTSTRAP path, not rotation: it creates a key and marks it
// SigningKeyStatusActive in the same call, synchronously, with no
// propagation window. That is safe here specifically because a purpose
// with no active key has never signed anything -- there is no multi-replica
// cache-propagation race to protect against on a kid nothing has verified
// yet. Rotating an EXISTING active key goes through the pending/
// propagation-window path instead -- see lifecycle.go.
func (s *Service) EnsurePurpose(ctx context.Context, purpose, algorithm string, maxCredentialLifetime time.Duration) error {
	if purpose == "" {
		return fmt.Errorf("pki: EnsurePurpose requires a non-empty purpose")
	}
	if maxCredentialLifetime <= 0 {
		return fmt.Errorf("pki: EnsurePurpose requires a positive maxCredentialLifetime")
	}

	_, err := s.signingKeys.FindActiveByPurpose(ctx, purpose)
	if err == nil {
		// Already has an active key -- nothing to bootstrap. A future
		// round may decide what an Algorithm mismatch here means (error?
		// rotate?); see AGENTS.md's Known limitations.
		return nil
	}
	if !isNoActiveKey(err) {
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

	now := s.now().UTC()
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

// ActiveSigner returns the kid, algorithm and a context-aware signing
// function for purpose's currently active key. ErrNoActiveKey if
// EnsurePurpose was never called for purpose (or, in a later round, if
// every key for it has since been revoked).
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
	if entry.active == nil {
		return "", "", nil, ErrNoActiveKey
	}
	keyRef := entry.active.KeyRef
	sign := func(ctx context.Context, input []byte) ([]byte, error) {
		return s.signer.Sign(ctx, keyRef, input)
	}
	return entry.active.ID, entry.active.Algorithm, sign, nil
}

// VerificationKeys returns every key for purpose that is still safe to
// verify against -- see SigningKeyRepository.ListVerifiableByPurpose for
// exactly which statuses that is. The anonymous return-slice element type
// is not a stylistic choice: a named type here would break structural
// satisfaction of authn's KeySource (docs/internal/22-pki.md's "authn's
// integration" section explains why two packages' named types can never
// satisfy one another structurally), so it is written out in full, matching
// KeySource's own declaration exactly.
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
	out := make([]struct {
		KID       string
		Algorithm string
		Public    crypto.PublicKey
	}, 0, len(entry.verifiable))
	for _, row := range entry.verifiable {
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

// isNoActiveKey reports whether err is (a decorated) ErrNoActiveKey.
func isNoActiveKey(err error) bool {
	found, ok := apperr.As(err)
	return ok && found.Code == ErrNoActiveKey.Code
}

// keySourceShape mirrors, field for field and in the same order,
// go/authn's future KeySource interface as docs/internal/22-pki.md's
// "authn's integration" section specifies it. It exists purely as a compile-time
// proof that *Service already satisfies that shape TODAY, without this
// module importing authn (which would invert the module dependency
// direction docs/internal/01-architecture.md fixes: authn depends on pki,
// never the reverse). When authn's round-2 KeySource is declared for real,
// this is the interface it must be declared identically to; a mismatch
// here is this module's bug to fix, not authn's.
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
