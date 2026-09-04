package pki

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// newTestServiceWithClock returns a Service like newTestService, but with a
// controllable clock (svc.now) and a real in-memory EventBus wired directly
// (bypassing Module.Register/attachBus, which this file's tests do not
// need) so tests can assert on published events.
func newTestServiceWithClock(t *testing.T) (*Service, *eventRecorder) {
	t.Helper()
	svc := newTestService(t)
	rec := newEventRecorder()
	svc.bus = pkgcore.NewMemoryEventBus()
	svc.bus.Subscribe(EventSigningKeyStaged, rec.record)
	svc.bus.Subscribe(EventSigningKeyActivated, rec.record)
	svc.bus.Subscribe(EventSigningKeyRetired, rec.record)
	return svc, rec
}

// eventRecorder collects every SigningKeyLifecycleEvent a subscribed
// Service publishes, in order.
type eventRecorder struct {
	events []pkgcore.Event
}

func newEventRecorder() *eventRecorder { return &eventRecorder{} }

func (r *eventRecorder) record(_ context.Context, evt pkgcore.Event) error {
	r.events = append(r.events, evt)
	return nil
}

func (r *eventRecorder) typesOf() []string {
	types := make([]string, len(r.events))
	for i, evt := range r.events {
		types[i] = evt.Type
	}
	return types
}

// --- PromoteDuePending ------------------------------------------------------

// TestService_PromoteDuePending_WaitsForThePropagationWindow proves the
// core reason the pending state exists at all (docs/internal/22-pki.md's
// section on why pending exists: a distributed race): a pending key
// created less than propagationWindow ago is left untouched.
func TestService_PromoteDuePending_WaitsForThePropagationWindow(t *testing.T) {
	svc, rec := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	pending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
	pending.CreatedAt = now
	if err := svc.signingKeys.Create(ctx, pending); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}

	promoted, err := svc.PromoteDuePending(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PromoteDuePending: %v", err)
	}
	if len(promoted) != 0 {
		t.Errorf("PromoteDuePending promoted %v before the propagation window elapsed, want none", promoted)
	}
	if len(rec.events) != 0 {
		t.Errorf("PromoteDuePending published %d event(s) before promoting anything", len(rec.events))
	}

	got, err := svc.signingKeys.FindByID(ctx, "kid-pending")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Status != SigningKeyStatusPending {
		t.Errorf("status = %q, want still %q", got.Status, SigningKeyStatusPending)
	}
}

// TestService_PromoteDuePending_PromotesPastTheWindow_AndDemotesThePrevious
// proves both halves of the one atomic transition
// SigningKeyRepository.PromoteToActive documents: a pending key past its
// propagation window is promoted to active, and the purpose's previously
// active key is demoted to retiring in the same call -- which is also why
// there is no separate ".retiring" event (events.go).
func TestService_PromoteDuePending_PromotesPastTheWindow_AndDemotesThePrevious(t *testing.T) {
	svc, rec := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	previous := newTestSigningKey("kid-previous", "authn.access_token", SigningKeyStatusActive)
	previous.RetiringOverlap = 15 * time.Minute
	if err := svc.signingKeys.Create(ctx, previous); err != nil {
		t.Fatalf("seed previous active key: %v", err)
	}

	pending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
	pending.CreatedAt = now.Add(-2 * time.Hour)
	pending.RetiringOverlap = 15 * time.Minute
	if err := svc.signingKeys.Create(ctx, pending); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}

	promoted, err := svc.PromoteDuePending(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PromoteDuePending: %v", err)
	}
	if len(promoted) != 1 || promoted[0] != "kid-pending" {
		t.Fatalf("PromoteDuePending promoted %v, want [kid-pending]", promoted)
	}

	active, err := svc.signingKeys.FindByID(ctx, "kid-pending")
	if err != nil {
		t.Fatalf("FindByID(kid-pending): %v", err)
	}
	if active.Status != SigningKeyStatusActive || active.ActivatedAt == nil {
		t.Errorf("kid-pending = %+v, want SigningKeyStatusActive with ActivatedAt set", active)
	}

	demoted, err := svc.signingKeys.FindByID(ctx, "kid-previous")
	if err != nil {
		t.Fatalf("FindByID(kid-previous): %v", err)
	}
	if demoted.Status != SigningKeyStatusRetiring || demoted.RetiringAt == nil {
		t.Errorf("kid-previous = %+v, want SigningKeyStatusRetiring with RetiringAt set", demoted)
	}

	if got := rec.typesOf(); len(got) != 1 || got[0] != EventSigningKeyActivated {
		t.Errorf("published events = %v, want exactly one EventSigningKeyActivated", got)
	}
}

// TestService_PromoteDuePending_FirstActivationForAPurpose_HasNoPrevious
// covers the case a purpose's very first pending key is promoted with no
// prior active key to demote.
func TestService_PromoteDuePending_FirstActivationForAPurpose_HasNoPrevious(t *testing.T) {
	svc, _ := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	pending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
	pending.CreatedAt = now.Add(-2 * time.Hour)
	if err := svc.signingKeys.Create(ctx, pending); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}

	promoted, err := svc.PromoteDuePending(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PromoteDuePending: %v", err)
	}
	if len(promoted) != 1 {
		t.Fatalf("PromoteDuePending promoted %v, want exactly one", promoted)
	}
}

// --- RetireDueRetiring -------------------------------------------------------

// TestService_RetireDueRetiring_WaitsForTheOverlapPeriod proves a retiring
// key whose overlap period has not yet elapsed is left untouched --
// retiring it early would fail verification for credentials still in
// flight (docs/internal/22-pki.md's section on why the retiring overlap
// period's length is declared by the consumer, not pki itself).
func TestService_RetireDueRetiring_WaitsForTheOverlapPeriod(t *testing.T) {
	svc, rec := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	retiringAt := now
	key := newTestSigningKey("kid-retiring", "authn.access_token", SigningKeyStatusRetiring)
	key.RetiringAt = &retiringAt
	key.RetiringOverlap = time.Hour
	if err := svc.signingKeys.Create(ctx, key); err != nil {
		t.Fatalf("seed retiring key: %v", err)
	}

	retired, err := svc.RetireDueRetiring(ctx)
	if err != nil {
		t.Fatalf("RetireDueRetiring: %v", err)
	}
	if len(retired) != 0 {
		t.Errorf("RetireDueRetiring retired %v before the overlap period elapsed, want none", retired)
	}
	if len(rec.events) != 0 {
		t.Errorf("RetireDueRetiring published %d event(s) before retiring anything", len(rec.events))
	}
}

// TestService_RetireDueRetiring_RetiresPastTheOverlapPeriod is the positive
// case of the above: once now is past RetiringAt+RetiringOverlap, the key
// is retired and EventSigningKeyRetired is published.
func TestService_RetireDueRetiring_RetiresPastTheOverlapPeriod(t *testing.T) {
	svc, rec := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()

	retiringAt := now.Add(-2 * time.Hour)
	key := newTestSigningKey("kid-retiring", "authn.access_token", SigningKeyStatusRetiring)
	key.RetiringAt = &retiringAt
	key.RetiringOverlap = time.Hour
	if err := svc.signingKeys.Create(ctx, key); err != nil {
		t.Fatalf("seed retiring key: %v", err)
	}
	svc.now = func() time.Time { return now }

	retired, err := svc.RetireDueRetiring(ctx)
	if err != nil {
		t.Fatalf("RetireDueRetiring: %v", err)
	}
	if len(retired) != 1 || retired[0] != "kid-retiring" {
		t.Fatalf("RetireDueRetiring retired %v, want [kid-retiring]", retired)
	}

	got, err := svc.signingKeys.FindByID(ctx, "kid-retiring")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Status != SigningKeyStatusRetired || got.RetiredAt == nil {
		t.Errorf("kid-retiring = %+v, want SigningKeyStatusRetired with RetiredAt set", got)
	}
	if got := rec.typesOf(); len(got) != 1 || got[0] != EventSigningKeyRetired {
		t.Errorf("published events = %v, want exactly one EventSigningKeyRetired", got)
	}
}

// TestService_RetireDueRetiring_SkipsARowWithNoRetiringAt covers the
// defensive branch RetireDueRetiring's own doc comment names as
// unreachable through the normal write path (PromoteToActive always sets
// RetiringAt in the same transaction that sets the retiring status) --
// exercised here only by direct repository seeding, the same technique
// TestService_VerificationKeys_ReturnsNonRevokedKeys already uses for an
// unreachable-through-Service row shape.
func TestService_RetireDueRetiring_SkipsARowWithNoRetiringAt(t *testing.T) {
	svc, _ := newTestServiceWithClock(t)
	ctx := context.Background()

	key := newTestSigningKey("kid-retiring", "authn.access_token", SigningKeyStatusRetiring)
	key.RetiringAt = nil
	if err := svc.signingKeys.Create(ctx, key); err != nil {
		t.Fatalf("seed retiring key: %v", err)
	}

	retired, err := svc.RetireDueRetiring(ctx)
	if err != nil {
		t.Fatalf("RetireDueRetiring: %v", err)
	}
	if len(retired) != 0 {
		t.Errorf("RetireDueRetiring retired %v for a row with no RetiringAt, want none", retired)
	}
}

// --- StageDueRotations -------------------------------------------------------

// TestService_StageDueRotations_StagesAheadOfExpiry proves rotation is
// generated ahead of a hard cutoff (docs/internal/22-pki.md's "rotation"
// section), copying the active key's purpose, algorithm and
// RetiringOverlap onto the new pending row.
func TestService_StageDueRotations_StagesAheadOfExpiry(t *testing.T) {
	svc, rec := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	active := newTestSigningKey("kid-active", "authn.access_token", SigningKeyStatusActive)
	active.NotBefore = now.Add(-350 * 24 * time.Hour)
	active.NotAfter = now.Add(10 * 24 * time.Hour)
	active.RetiringOverlap = 15 * time.Minute
	if err := svc.signingKeys.Create(ctx, active); err != nil {
		t.Fatalf("seed active key: %v", err)
	}

	staged, err := svc.StageDueRotations(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("StageDueRotations: %v", err)
	}
	if len(staged) != 1 {
		t.Fatalf("StageDueRotations staged %v, want exactly one", staged)
	}

	newKey, err := svc.signingKeys.FindByID(ctx, staged[0])
	if err != nil {
		t.Fatalf("FindByID(%q): %v", staged[0], err)
	}
	if newKey.Status != SigningKeyStatusPending {
		t.Errorf("staged key status = %q, want %q", newKey.Status, SigningKeyStatusPending)
	}
	if newKey.Purpose != active.Purpose || newKey.Algorithm != active.Algorithm {
		t.Errorf("staged key = %+v, want purpose/algorithm to match the active key %+v", newKey, active)
	}
	if newKey.RetiringOverlap != active.RetiringOverlap {
		t.Errorf("staged key RetiringOverlap = %v, want %v (carried forward from the active key)", newKey.RetiringOverlap, active.RetiringOverlap)
	}
	if got := rec.typesOf(); len(got) != 1 || got[0] != EventSigningKeyStaged {
		t.Errorf("published events = %v, want exactly one EventSigningKeyStaged", got)
	}
}

// TestService_StageDueRotations_SkipsAPurposeAlreadyStaged proves a second
// tick does not double-stage a purpose that already has a pending
// replacement in flight.
func TestService_StageDueRotations_SkipsAPurposeAlreadyStaged(t *testing.T) {
	svc, rec := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	active := newTestSigningKey("kid-active", "authn.access_token", SigningKeyStatusActive)
	active.NotAfter = now.Add(10 * 24 * time.Hour)
	if err := svc.signingKeys.Create(ctx, active); err != nil {
		t.Fatalf("seed active key: %v", err)
	}
	alreadyPending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
	if err := svc.signingKeys.Create(ctx, alreadyPending); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}

	staged, err := svc.StageDueRotations(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("StageDueRotations: %v", err)
	}
	if len(staged) != 0 {
		t.Errorf("StageDueRotations staged %v for a purpose already carrying a pending key, want none", staged)
	}
	if len(rec.events) != 0 {
		t.Errorf("StageDueRotations published %d event(s), want none", len(rec.events))
	}
}

// TestService_StageDueRotations_SkipsAPurposeNotNearingExpiry proves an
// active key whose expiry is far beyond renewalLeadTime is left alone.
func TestService_StageDueRotations_SkipsAPurposeNotNearingExpiry(t *testing.T) {
	svc, _ := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	active := newTestSigningKey("kid-active", "authn.access_token", SigningKeyStatusActive)
	active.NotAfter = now.Add(365 * 24 * time.Hour)
	if err := svc.signingKeys.Create(ctx, active); err != nil {
		t.Fatalf("seed active key: %v", err)
	}

	staged, err := svc.StageDueRotations(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("StageDueRotations: %v", err)
	}
	if len(staged) != 0 {
		t.Errorf("StageDueRotations staged %v for a key not nearing expiry, want none", staged)
	}
}

// --- ScanExpiry ---------------------------------------------------------------

// TestService_ScanExpiry_RunsPromoteRetireStageInOrder drives a single
// ScanExpiry call over a purpose in every transitional state at once --
// a pending key past its propagation window, the active key it replaces
// (which must end up retiring, not yet retired, since ScanExpiry runs in
// one pass), and a second, unrelated purpose's retiring key past its
// overlap period -- and checks ScanReport reflects all three transitions.
func TestService_ScanExpiry_RunsPromoteRetireStageInOrder(t *testing.T) {
	svc, _ := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	// Purpose "authn.access_token": an active key about to be replaced by a
	// pending key whose propagation window has already elapsed.
	active := newTestSigningKey("kid-active", "authn.access_token", SigningKeyStatusActive)
	active.NotAfter = now.Add(400 * 24 * time.Hour)
	active.RetiringOverlap = 15 * time.Minute
	if err := svc.signingKeys.Create(ctx, active); err != nil {
		t.Fatalf("seed active key: %v", err)
	}
	pending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
	pending.CreatedAt = now.Add(-2 * time.Hour)
	if err := svc.signingKeys.Create(ctx, pending); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}

	// Purpose "tenant.jwt_signing": a retiring key past its overlap period.
	retiringAt := now.Add(-2 * time.Hour)
	retiring := newTestSigningKey("kid-retiring", "tenant.jwt_signing", SigningKeyStatusRetiring)
	retiring.RetiringAt = &retiringAt
	retiring.RetiringOverlap = time.Hour
	if err := svc.signingKeys.Create(ctx, retiring); err != nil {
		t.Fatalf("seed retiring key: %v", err)
	}
	// tenant.jwt_signing needs an active key too, or StageDueRotations has
	// nothing to consider for it -- give it one far from expiry so no
	// staging fires for this purpose.
	otherActive := newTestSigningKey("kid-other-active", "tenant.jwt_signing", SigningKeyStatusActive)
	otherActive.NotAfter = now.Add(365 * 24 * time.Hour)
	if err := svc.signingKeys.Create(ctx, otherActive); err != nil {
		t.Fatalf("seed other active key: %v", err)
	}

	report, err := svc.ScanExpiry(ctx, RotationConfig{PropagationWindow: time.Hour, RenewalLeadTime: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("ScanExpiry: %v", err)
	}

	if len(report.Activated) != 1 || report.Activated[0] != "kid-pending" {
		t.Errorf("report.Activated = %v, want [kid-pending]", report.Activated)
	}
	if len(report.Retired) != 1 || report.Retired[0] != "kid-retiring" {
		t.Errorf("report.Retired = %v, want [kid-retiring]", report.Retired)
	}
	// kid-active was just demoted to retiring in this same pass, with a
	// fresh RetiringAt -- it must NOT also appear staged for replacement
	// (its own NotAfter is still 400 days out) and must not be retired
	// (its overlap period has not started, let alone elapsed).
	demoted, err := svc.signingKeys.FindByID(ctx, "kid-active")
	if err != nil {
		t.Fatalf("FindByID(kid-active): %v", err)
	}
	if demoted.Status != SigningKeyStatusRetiring {
		t.Errorf("kid-active status = %q after ScanExpiry, want %q", demoted.Status, SigningKeyStatusRetiring)
	}
}

// TestService_ScanExpiry_FallsBackToServiceDefaults proves a zero
// RotationConfig resolves to the Service's own configured
// propagationWindow/renewalLeadTime (NewService's constructor arguments),
// not DefaultPropagationWindow/DefaultRenewalLeadTime unconditionally.
func TestService_ScanExpiry_FallsBackToServiceDefaults(t *testing.T) {
	db := newTestDB(t)
	signer := NewLocalSigner(db)
	svc := NewService(signer, "local", NewSigningKeyRepository(db), DefaultCacheTTL, time.Hour, time.Hour)
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	pending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
	pending.CreatedAt = now.Add(-2 * time.Hour)
	if err := svc.signingKeys.Create(ctx, pending); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}

	report, err := svc.ScanExpiry(ctx, RotationConfig{})
	if err != nil {
		t.Fatalf("ScanExpiry: %v", err)
	}
	if len(report.Activated) != 1 {
		t.Errorf("report.Activated = %v, want the pending key promoted using the Service's own 1-hour propagation window default", report.Activated)
	}
}

// --- PromoteNow (round 3) ----------------------------------------------

// TestService_PromoteNow_PropagationWindowNotElapsed proves PromoteNow
// honors the identical safety window PromoteDuePending itself waits out,
// rather than offering a bypass -- see PromoteNow's own doc comment for why
// that is deliberate.
func TestService_PromoteNow_PropagationWindowNotElapsed(t *testing.T) {
	svc, rec := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	pending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
	pending.CreatedAt = now
	if err := svc.signingKeys.Create(ctx, pending); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}

	if _, err := svc.PromoteNow(ctx, "authn.access_token", time.Hour); !apperrIs(err, ErrPropagationWindowNotElapsed) {
		t.Errorf("PromoteNow(window not elapsed) error = %v, want ErrPropagationWindowNotElapsed", err)
	}
	if len(rec.events) != 0 {
		t.Errorf("PromoteNow published %d event(s) despite refusing to promote", len(rec.events))
	}

	got, err := svc.signingKeys.FindByID(ctx, "kid-pending")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Status != SigningKeyStatusPending {
		t.Errorf("status = %q, want still %q", got.Status, SigningKeyStatusPending)
	}
}

// TestService_PromoteNow_PromotesPastTheWindow_AndDemotesThePrevious mirrors
// PromoteDuePending's identical proof, exercised through the manual trigger
// instead of ScanExpiry.
func TestService_PromoteNow_PromotesPastTheWindow_AndDemotesThePrevious(t *testing.T) {
	svc, rec := newTestServiceWithClock(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	previous := newTestSigningKey("kid-previous", "authn.access_token", SigningKeyStatusActive)
	if err := svc.signingKeys.Create(ctx, previous); err != nil {
		t.Fatalf("seed previous active key: %v", err)
	}
	pending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
	pending.CreatedAt = now.Add(-2 * time.Hour)
	if err := svc.signingKeys.Create(ctx, pending); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}

	kid, err := svc.PromoteNow(ctx, "authn.access_token", time.Hour)
	if err != nil {
		t.Fatalf("PromoteNow: %v", err)
	}
	if kid != "kid-pending" {
		t.Errorf("PromoteNow returned kid %q, want %q", kid, "kid-pending")
	}

	active, err := svc.signingKeys.FindByID(ctx, "kid-pending")
	if err != nil {
		t.Fatalf("FindByID(kid-pending): %v", err)
	}
	if active.Status != SigningKeyStatusActive {
		t.Errorf("kid-pending status = %q, want %q", active.Status, SigningKeyStatusActive)
	}
	demoted, err := svc.signingKeys.FindByID(ctx, "kid-previous")
	if err != nil {
		t.Fatalf("FindByID(kid-previous): %v", err)
	}
	if demoted.Status != SigningKeyStatusRetiring {
		t.Errorf("kid-previous status = %q, want %q", demoted.Status, SigningKeyStatusRetiring)
	}
	if got := rec.typesOf(); len(got) != 1 || got[0] != EventSigningKeyActivated {
		t.Errorf("published events = %v, want exactly one EventSigningKeyActivated", got)
	}

	// ActiveSigner must now serve kid-pending -- proving the cache was
	// invalidated by the manual promotion exactly as it is by ScanExpiry.
	gotKID, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner: %v", err)
	}
	if gotKID != "kid-pending" {
		t.Errorf("ActiveSigner = %q, want %q", gotKID, "kid-pending")
	}
}

// TestService_PromoteNow_NoPendingKey_ErrKeyNotFound proves PromoteNow
// refuses a purpose with no pending key at all, distinct from
// ErrPropagationWindowNotElapsed -- there is nothing to wait out.
func TestService_PromoteNow_NoPendingKey_ErrKeyNotFound(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.PromoteNow(context.Background(), "authn.access_token", time.Hour); !apperrIs(err, ErrKeyNotFound) {
		t.Errorf("PromoteNow(no pending key) error = %v, want ErrKeyNotFound", err)
	}
}

// TestService_PromoteNow_ZeroPropagationWindow_FallsBackToServiceDefault
// mirrors ScanExpiry's identical per-call-override-with-fallback proof.
func TestService_PromoteNow_ZeroPropagationWindow_FallsBackToServiceDefault(t *testing.T) {
	db := newTestDB(t)
	signer := NewLocalSigner(db)
	svc := NewService(signer, "local", NewSigningKeyRepository(db), DefaultCacheTTL, time.Hour, time.Hour)
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	pending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
	pending.CreatedAt = now.Add(-2 * time.Hour)
	if err := svc.signingKeys.Create(ctx, pending); err != nil {
		t.Fatalf("seed pending key: %v", err)
	}

	kid, err := svc.PromoteNow(ctx, "authn.access_token", 0)
	if err != nil {
		t.Fatalf("PromoteNow(zero propagationWindow): %v", err)
	}
	if kid != "kid-pending" {
		t.Errorf("PromoteNow returned kid %q, want %q", kid, "kid-pending")
	}
}
