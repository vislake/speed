package pki

import (
	"context"
	"crypto/ed25519"
	"sync"
	"testing"
	"time"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	db := newTestDB(t)
	signer := NewLocalSigner(db)
	svc := NewService(signer, "local", NewSigningKeyRepository(db), DefaultCacheTTL, DefaultPropagationWindow, DefaultRenewalLeadTime, DefaultExpiryScanWindow)
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func TestService_EnsurePurpose_RejectsEmptyPurpose(t *testing.T) {
	svc := newTestService(t)
	if err := svc.EnsurePurpose(context.Background(), "", AlgorithmEd25519, time.Minute); err == nil {
		t.Fatalf("EnsurePurpose(empty purpose) succeeded, want an error")
	}
}

func TestService_EnsurePurpose_RejectsNonPositiveMaxCredentialLifetime(t *testing.T) {
	svc := newTestService(t)
	if err := svc.EnsurePurpose(context.Background(), "authn.access_token", AlgorithmEd25519, 0); err == nil {
		t.Fatalf("EnsurePurpose(zero maxCredentialLifetime) succeeded, want an error")
	}
}

// TestService_EnsurePurpose_CreatesAnActiveKeySynchronously pins the
// bootstrap path: EnsurePurpose does not stage a pending key and wait for
// a propagation window -- it creates a key and marks it active in the same
// call. See Service's own doc comment.
func TestService_EnsurePurpose_CreatesAnActiveKeySynchronously(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}

	kid, algorithm, sign, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner: %v", err)
	}
	if kid == "" {
		t.Fatalf("ActiveSigner returned an empty kid")
	}
	if algorithm != AlgorithmEd25519 {
		t.Errorf("ActiveSigner algorithm = %q, want %q", algorithm, AlgorithmEd25519)
	}
	if _, err := sign(ctx, []byte("hello")); err != nil {
		t.Errorf("the returned sign function failed: %v", err)
	}
}

// TestService_EnsurePurpose_Idempotent proves a second call for a purpose
// that already has an active key is a no-op: it must not create a second
// row (which the migration's partial unique index would refuse anyway) or
// return an error.
func TestService_EnsurePurpose_Idempotent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose (first call): %v", err)
	}
	kid1, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner (first): %v", err)
	}

	if err = svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose (second call): %v", err)
	}
	kid2, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner (second): %v", err)
	}
	if kid1 != kid2 {
		t.Errorf("EnsurePurpose called twice produced two different active keys: %q then %q", kid1, kid2)
	}
}

func TestService_ActiveSigner_NoActiveKey(t *testing.T) {
	svc := newTestService(t)
	if _, _, _, err := svc.ActiveSigner(context.Background(), "authn.access_token"); !apperrIs(err, ErrNoActiveKey) {
		t.Errorf("ActiveSigner(no key ever created) error = %v, want ErrNoActiveKey", err)
	}
}

// TestService_VerificationKeys_ReturnsNonRevokedKeys proves VerificationKeys
// returns every key for the purpose that is not revoked -- the
// "all still-verifiable keys" answer, per Service's own doc comment.
func TestService_VerificationKeys_ReturnsNonRevokedKeys(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}
	// A revoked key for the same purpose must be excluded.
	if err := svc.signingKeys.Create(ctx, newTestSigningKey("kid-revoked", "authn.access_token", SigningKeyStatusRevoked)); err != nil {
		t.Fatalf("seed revoked key: %v", err)
	}

	keys, err := svc.VerificationKeys(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("VerificationKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("VerificationKeys returned %d keys, want 1 (the active one, revoked excluded)", len(keys))
	}
	if keys[0].Algorithm != AlgorithmEd25519 {
		t.Errorf("VerificationKeys[0].Algorithm = %q, want %q", keys[0].Algorithm, AlgorithmEd25519)
	}
	if keys[0].Public == nil {
		t.Errorf("VerificationKeys[0].Public is nil, want a parsed public key")
	}
}

// --- Validity-window enforcement (NotBefore/NotAfter at key-take time) ---
//
// The tests below pin the time x state cross product the signing-key state
// machine alone cannot express: a key in SigningKeyStatusActive whose
// NotAfter has passed is still "active" as far as the status vocabulary
// goes, yet must be refused on every read path AT THE MOMENT THE KEY IS
// TAKEN -- whether or not the expiry scan has run.
//
// Every test pins the Service's clock (svc.now) rather than sleeping, so
// the boundary assertions are exact: a key's validity window is
// [NotBefore, NotAfter], both ends inclusive -- the identical boundary
// crypto/x509 applies to a certificate's validity window -- and the key is
// refused strictly before NotBefore and strictly after NotAfter.

// TestService_ActiveSigner_KeyPastNotAfterIsRefused is the sign path's
// half of the enforcement: ActiveSigner must answer ErrNoActiveKey for a
// purpose whose active key is out of validity -- the same answer it gives
// for a purpose with no active key at all -- and must keep answering with
// the key AT its NotAfter (the inclusive boundary).
func TestService_ActiveSigner_KeyPastNotAfterIsRefused(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}
	key, err := svc.signingKeys.FindActiveByPurpose(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("FindActiveByPurpose: %v", err)
	}

	// The boundary is inclusive at NotAfter: the key is still usable AT its
	// NotAfter...
	svc.now = func() time.Time { return key.NotAfter }
	if _, _, _, err := svc.ActiveSigner(ctx, "authn.access_token"); err != nil {
		t.Fatalf("ActiveSigner AT NotAfter error = %v, want success (NotAfter is inclusive)", err)
	}

	// ...and refused a moment later. This is the regression's failing leg
	// on pre-fix code: an active key past its NotAfter used to keep signing
	// because status alone decided usability, with the key's real expiry
	// depending on whether the scan job happened to have run.
	svc.now = func() time.Time { return key.NotAfter.Add(time.Nanosecond) }
	if _, _, _, err := svc.ActiveSigner(ctx, "authn.access_token"); !apperrIs(err, ErrNoActiveKey) {
		t.Fatalf("ActiveSigner just past NotAfter error = %v, want ErrNoActiveKey (regression: an expired key must not sign)", err)
	}
}

// TestService_ActiveSigner_KeyBeforeNotBeforeIsRefused pins the
// not-yet-valid half of the window. No shipped code path creates a
// future-dated key (NotBefore is always the creation instant), but the
// enforcement must cover the whole window, not just the expiry end: a
// future code path that pre-provisions keys must not be able to sign with
// them early.
func TestService_ActiveSigner_KeyBeforeNotBeforeIsRefused(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }

	future := newTestSigningKey("kid-future", "authn.access_token", SigningKeyStatusActive)
	future.NotBefore = base.Add(time.Hour)
	future.NotAfter = base.Add(2 * time.Hour)
	if err := svc.signingKeys.Create(ctx, future); err != nil {
		t.Fatalf("seed future-dated active key: %v", err)
	}

	if _, _, _, err := svc.ActiveSigner(ctx, "authn.access_token"); !apperrIs(err, ErrNoActiveKey) {
		t.Fatalf("ActiveSigner before NotBefore error = %v, want ErrNoActiveKey", err)
	}
}

// TestService_VerificationKeys_DropsKeysPastTheirNotAfter is the verify
// path's half of the enforcement: a key whose NotAfter has passed must
// stop being offered for verification the instant it passes, whatever its
// status -- the state machine's own retiring-overlap period extends a
// key's verifiability only while the key is still within its validity
// window. The availability half of the trade-off is documented on
// ActiveSigner/VerificationKeys in service.go.
func TestService_VerificationKeys_DropsKeysPastTheirNotAfter(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}
	active, err := svc.signingKeys.FindActiveByPurpose(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("FindActiveByPurpose: %v", err)
	}

	// A retiring key whose overlap period has NOT ended (status-wise it
	// stays verifiable) but whose NotAfter passes an hour after base.
	// VerificationKeys parses every returned row's PublicKey, so the
	// fixture needs a real Ed25519 DER key, not newTestSigningKey's
	// placeholder bytes.
	retiring := newTestSigningKey("kid-retiring-short-lived", "authn.access_token", SigningKeyStatusRetiring)
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	der, err := marshalPKIXForTest(pub)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	retiring.PublicKey = der
	retiring.NotBefore = active.NotBefore
	retiring.NotAfter = base.Add(time.Hour)
	if seedErr := svc.signingKeys.Create(ctx, retiring); seedErr != nil {
		t.Fatalf("seed retiring key: %v", seedErr)
	}

	// While both keys are within their windows both are offered...
	keys, err := svc.VerificationKeys(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("VerificationKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("VerificationKeys returned %d keys while both are in validity, want 2", len(keys))
	}

	// ...and once the retiring key's NotAfter passes, it must be dropped
	// even though its status is still SigningKeyStatusRetiring (this is the
	// regression's failing leg on pre-fix code, where status alone decided
	// the verifiable set and the key stayed offered until the scan ran).
	svc.now = func() time.Time { return base.Add(time.Hour).Add(time.Nanosecond) }
	keys, err = svc.VerificationKeys(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("VerificationKeys (past the retiring key's NotAfter): %v", err)
	}
	if len(keys) != 1 || keys[0].KID != active.ID {
		t.Fatalf("VerificationKeys just past the retiring key's NotAfter = %v, want exactly the in-validity active key %q (regression: an expired key must not stay verifiable)", keys, active.ID)
	}
}

// TestService_EnsurePurpose_ReplacesAnExpiredActiveKeyWhenNothingIsStaged
// pins the bootstrap path's self-healing half of the enforcement: a
// purpose whose ONLY active key has fallen out of validity -- the expiry
// scan never ran, or ran too late -- must not stay dead because
// EnsurePurpose's "already has an active key" check sees the expired row.
// EnsurePurpose revokes the expired key and creates an in-validity
// replacement in the same call, restoring the purpose to a working signer
// without waiting for a scan tick.
func TestService_EnsurePurpose_ReplacesAnExpiredActiveKeyWhenNothingIsStaged(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose (first call): %v", err)
	}
	kid1, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner (first): %v", err)
	}

	// Well past the 365-day default validity: the purpose's only key is
	// expired. ActiveSigner must refuse (this is the regression's failing
	// leg on pre-fix code, where the expired key kept signing)...
	svc.now = func() time.Time { return base.Add(2 * 365 * 24 * time.Hour) }
	if _, _, _, errAtExpiry := svc.ActiveSigner(ctx, "authn.access_token"); !apperrIs(errAtExpiry, ErrNoActiveKey) {
		t.Fatalf("ActiveSigner past the key's NotAfter error = %v, want ErrNoActiveKey (regression: an expired key must not sign)", errAtExpiry)
	}

	// ...and a re-run of the bootstrap path must heal the purpose rather
	// than answering "already has an active key" about the expired row.
	if healErr := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); healErr != nil {
		t.Fatalf("EnsurePurpose (heal call): %v", healErr)
	}
	kid2, _, sign, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner (after heal): %v", err)
	}
	if kid2 == kid1 {
		t.Fatalf("ActiveSigner after the heal returned the expired key %q, want a replacement", kid1)
	}
	if _, signErr := sign(ctx, []byte("hello")); signErr != nil {
		t.Errorf("the healed purpose's sign function failed: %v", signErr)
	}

	// The expired key is revoked, not silently left occupying the active
	// slot -- revoking is what frees the slot for the replacement (the
	// partial unique index allows one active row per purpose) and what
	// tells every replica's cache the key is gone.
	old, err := svc.signingKeys.FindByID(ctx, kid1)
	if err != nil {
		t.Fatalf("FindByID(expired key): %v", err)
	}
	if old.Status != SigningKeyStatusRevoked {
		t.Errorf("expired key status after the heal = %q, want %q", old.Status, SigningKeyStatusRevoked)
	}
	if old.RevocationReason == "" {
		t.Error("expired key RevocationReason is empty, want the reason recorded")
	}

	// The heal is one-shot: a third call sees an in-validity active key and
	// is the plain no-op it always was.
	kid3, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner (third): %v", err)
	}
	if ensureErr := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); ensureErr != nil {
		t.Fatalf("EnsurePurpose (third call): %v", ensureErr)
	}
	kid4, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner (fourth): %v", err)
	}
	if kid3 != kid4 {
		t.Errorf("EnsurePurpose after the heal created yet another key: %q then %q", kid3, kid4)
	}
}

// TestService_EnsurePurpose_ExpiredActiveKeyWithAPendingSuccessorLeavesTheRotationToTheScan
// pins the heal's boundary: when the expiry scan HAS staged a pending
// successor (the scan was running but stopped before the promotion tick),
// EnsurePurpose must not also create a replacement -- the pending key is
// the designed successor, and promoting it is the scan's job (or a
// host's PromoteNow call). Creating a fresh active key over an
// in-flight pending one would churn the purpose with two candidates for
// the same slot.
func TestService_EnsurePurpose_ExpiredActiveKeyWithAPendingSuccessorLeavesTheRotationToTheScan(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }

	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose (first call): %v", err)
	}
	active, err := svc.signingKeys.FindActiveByPurpose(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("FindActiveByPurpose: %v", err)
	}

	// Stage a pending successor the way StageDueRotations would, still
	// comfortably in validity.
	pending := newTestSigningKey("kid-pending-successor", "authn.access_token", SigningKeyStatusPending)
	pending.NotBefore = base
	pending.NotAfter = base.Add(3 * 365 * 24 * time.Hour)
	if seedErr := svc.signingKeys.Create(ctx, pending); seedErr != nil {
		t.Fatalf("seed pending successor: %v", seedErr)
	}

	svc.now = func() time.Time { return base.Add(2 * 365 * 24 * time.Hour) }
	if ensureErr := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); ensureErr != nil {
		t.Fatalf("EnsurePurpose (with a pending successor staged): %v", ensureErr)
	}

	got, err := svc.signingKeys.FindByID(ctx, active.ID)
	if err != nil {
		t.Fatalf("FindByID(expired active): %v", err)
	}
	if got.Status != SigningKeyStatusActive {
		t.Errorf("expired active key status = %q after EnsurePurpose, want still %q -- a pending successor means the scan (not bootstrap) owns the rotation", got.Status, SigningKeyStatusActive)
	}
	gotPending, err := svc.signingKeys.FindByID(ctx, pending.ID)
	if err != nil {
		t.Fatalf("FindByID(pending): %v", err)
	}
	if gotPending.Status != SigningKeyStatusPending {
		t.Errorf("pending successor status = %q after EnsurePurpose, want still %q", gotPending.Status, SigningKeyStatusPending)
	}
}

// TestService_EnsurePurpose_ConcurrentHeals_OneReplacementBothSucceed pins
// the self-heal's concurrency arbitration (service.go's EnsurePurpose doc
// comment): when two replicas heal the same expired purpose at once -- the
// expiry scan never ran, and both got a token issue at the same moment --
// the heal's replacement INSERT is arbitrated by
// uq_pki_signing_keys_active_purpose (at most one active row per purpose),
// so exactly one replacement row is ever created; the losing call's create
// fails on the index, and the convergence re-read recognizes the winner's
// in-validity replacement and reports success rather than surfacing a
// spurious unique-violation error to a caller whose purpose was just
// healed. Both calls return nil, exactly one new active key exists, and
// the expired key is revoked (the guarded revoke makes the concurrent
// second revoke an idempotent no-op).
func TestService_EnsurePurpose_ConcurrentHeals_OneReplacementBothSucceed(t *testing.T) {
	db := newTestDB(t)
	signer := NewLocalSigner(db)
	repo := NewSigningKeyRepository(db)
	first := NewService(signer, "local", repo, DefaultCacheTTL, DefaultPropagationWindow, DefaultRenewalLeadTime, DefaultExpiryScanWindow)
	t.Cleanup(func() { _ = first.Close() })
	second := NewService(signer, "local", repo, DefaultCacheTTL, DefaultPropagationWindow, DefaultRenewalLeadTime, DefaultExpiryScanWindow)
	t.Cleanup(func() { _ = second.Close() })
	ctx := context.Background()

	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	first.now = func() time.Time { return base }
	second.now = func() time.Time { return base }
	if err := first.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose (first call): %v", err)
	}
	expiredID, _, _, err := first.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner (first): %v", err)
	}

	// Both replicas' clocks jump well past the one-year validity; both
	// heal in the same instant.
	healAt := base.Add(2 * 365 * 24 * time.Hour)
	first.now = func() time.Time { return healAt }
	second.now = func() time.Time { return healAt }

	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	heal := func(svc *Service, slot int) {
		defer wg.Done()
		<-start
		errs[slot] = svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute)
	}
	wg.Add(2)
	go heal(first, 0)
	go heal(second, 1)
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent heal %d: %v -- a loser whose replacement insert lost the unique-index arbitration must converge on the winner's row, never error", i, err)
		}
	}

	// Exactly one active key exists (the winner's replacement), and the
	// expired key is revoked.
	actives, err := repo.ListByPurposeAndStatuses(ctx, "authn.access_token", SigningKeyStatusActive)
	if err != nil {
		t.Fatalf("list active keys: %v", err)
	}
	if len(actives) != 1 {
		t.Fatalf("after two concurrent heals purpose has %d active keys, want exactly 1 -- the unique index must arbitrate one replacement", len(actives))
	}
	if actives[0].ID == expiredID {
		t.Fatalf("the surviving active key is the expired one %q -- neither heal replaced it", expiredID)
	}
	expired, err := repo.FindByID(ctx, expiredID)
	if err != nil {
		t.Fatalf("FindByID(expired): %v", err)
	}
	if expired.Status != SigningKeyStatusRevoked {
		t.Errorf("expired key status after the concurrent heals = %q, want %q", expired.Status, SigningKeyStatusRevoked)
	}
}
