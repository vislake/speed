package sharing

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

const testTenant = pkgcore.TenantID("tenant-a")

func testCtx() context.Context {
	return pkgcore.WithTenant(context.Background(), testTenant)
}

// newTestService returns a Service over a fresh migrated database, wired to
// a real in-memory registry (bus + declared audit action) so
// emitSensitiveAudit and event publishing both work in tests, not just fail
// closed.
func newTestService(t *testing.T, cfg TenantConfigReader) (*Service, pkgcore.EventBus) {
	t.Helper()
	svc := NewService(newTestDB(t), cfg)
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(AuditActionSensitiveShareCreate); err != nil {
		t.Fatalf("AuditActions.Add: %v", err)
	}
	svc.attach(reg)
	return svc, bus
}

// fixedClock returns a func() time.Time service.now can be pinned to, so
// tests reason about an exact instant rather than a moving time.Now().
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// fakeTenantConfigReader is a minimal, in-test TenantConfigReader double.
type fakeTenantConfigReader struct {
	d   time.Duration
	ok  bool
	err error
}

func (f fakeTenantConfigReader) ShareDefaultExpiry(context.Context, pkgcore.TenantID) (time.Duration, bool, error) {
	return f.d, f.ok, f.err
}

// --- Create --------------------------------------------------------------

func TestService_Create_ResourceRefRequired(t *testing.T) {
	svc, _ := newTestService(t, nil)
	_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "   "})
	assertCode(t, err, ErrResourceRefRequired.Code)
}

// TestService_Create_ForeverRefused pins rule 2's refusal half
// (docs/internal/07-platform-services.md's "never-expiring links are not
// allowed" rule): an explicit request for a never-expiring share must be
// REFUSED, not silently allowed.
func TestService_Create_ForeverRefused(t *testing.T) {
	svc, _ := newTestService(t, nil)
	_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", Forever: true})
	assertCode(t, err, ErrExpiryRequired.Code)
}

// TestService_Create_NoExpiryFallsBackToDefault pins rule 2's forcing half:
// a request with no ExpiresAt and no TenantConfigReader wired gets
// defaultShareExpiry, never a nil/never-expiring row.
func TestService_Create_NoExpiryFallsBackToDefault(t *testing.T) {
	svc, _ := newTestService(t, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	result, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Share.ExpiresAt == nil {
		t.Fatalf("ExpiresAt is nil, want a forced default -- a share must never be persisted with no expiry")
	}
	want := now.Add(defaultShareExpiry)
	if !result.Share.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v (now + defaultShareExpiry)", result.Share.ExpiresAt, want)
	}
}

// TestService_Create_NoExpiryUsesTenantConfiguredDefault proves a wired
// TenantConfigReader's answer is honored over defaultShareExpiry when the
// tenant has configured one.
func TestService_Create_NoExpiryUsesTenantConfiguredDefault(t *testing.T) {
	cfg := fakeTenantConfigReader{d: 7 * 24 * time.Hour, ok: true}
	svc, _ := newTestService(t, cfg)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	result, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := now.Add(7 * 24 * time.Hour)
	if !result.Share.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v (the tenant-configured 7 days)", result.Share.ExpiresAt, want)
	}
}

// TestService_Create_TenantConfigReaderReportingUnconfigured_FallsBackToDefault
// proves ok == false (the tenant configured nothing) behaves exactly as if
// no TenantConfigReader were wired at all.
func TestService_Create_TenantConfigReaderReportingUnconfigured_FallsBackToDefault(t *testing.T) {
	cfg := fakeTenantConfigReader{ok: false}
	svc, _ := newTestService(t, cfg)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	result, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := now.Add(defaultShareExpiry)
	if !result.Share.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v (defaultShareExpiry)", result.Share.ExpiresAt, want)
	}
}

func TestService_Create_ExplicitExpiryHonored(t *testing.T) {
	svc, _ := newTestService(t, nil)
	explicit := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	result, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", ExpiresAt: &explicit})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !result.Share.ExpiresAt.Equal(explicit) {
		t.Errorf("ExpiresAt = %v, want the caller's explicit %v", result.Share.ExpiresAt, explicit)
	}
}

func TestService_Create_InvalidMaxViews(t *testing.T) {
	svc, _ := newTestService(t, nil)
	zero := 0
	_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", MaxViews: &zero})
	assertCode(t, err, ErrInvalidMaxViews.Code)

	negative := -1
	_, err = svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", MaxViews: &negative})
	assertCode(t, err, ErrInvalidMaxViews.Code)
}

// TestService_Create_TokenIsNeverPersisted pins the Share.TokenHash doc
// comment's rule: the row stores only the hash, and the hash matches the
// once-returned raw token.
func TestService_Create_TokenIsNeverPersisted(t *testing.T) {
	svc, _ := newTestService(t, nil)
	result, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Token == "" {
		t.Fatalf("CreateResult.Token is empty")
	}
	if result.Share.TokenHash != hashShareToken(result.Token) {
		t.Errorf("Share.TokenHash does not match the hash of the returned token")
	}
	if result.Share.TokenHash == result.Token {
		t.Errorf("Share.TokenHash equals the raw token -- the raw token must never be what is stored")
	}
}

func TestService_Create_PasswordIsHashedNeverPlaintext(t *testing.T) {
	svc, _ := newTestService(t, nil)
	password := "let-me-in"
	result, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", Password: &password})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Share.PasswordHash == nil {
		t.Fatalf("PasswordHash is nil")
	}
	if *result.Share.PasswordHash == password {
		t.Fatalf("PasswordHash equals the plaintext password")
	}
	ok, err := verifySharePassword(*result.Share.PasswordHash, password)
	if err != nil {
		t.Fatalf("verifySharePassword: %v", err)
	}
	if !ok {
		t.Errorf("verifySharePassword(stored hash, original password) = false, want true")
	}
}

// TestService_Create_SensitiveEmitsAuditEvent pins rule 4
// (docs/internal/07-platform-services.md's "sensitive resource sharing
// needs confirmation" rule): Sensitive: true fires the sensitive-share
// audit action through the declarative audit.Emit path.
func TestService_Create_SensitiveEmitsAuditEvent(t *testing.T) {
	svc, bus := newTestService(t, nil)

	var captured []audit.RecordedEvent
	bus.Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		rec, ok := evt.Payload.(audit.RecordedEvent)
		if !ok {
			t.Fatalf("payload is %T, want audit.RecordedEvent", evt.Payload)
		}
		captured = append(captured, rec)
		return nil
	})

	result, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", Sensitive: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(captured) != 1 {
		t.Fatalf("captured %d audit.EventRecorded events, want 1", len(captured))
	}
	if captured[0].Action != AuditActionSensitiveShareCreate {
		t.Errorf("Action = %q, want %q", captured[0].Action, AuditActionSensitiveShareCreate)
	}
	if captured[0].Resource.ID != result.Share.ID {
		t.Errorf("Resource.ID = %q, want %q", captured[0].Resource.ID, result.Share.ID)
	}
}

func TestService_Create_NotSensitiveEmitsNoAuditEvent(t *testing.T) {
	svc, bus := newTestService(t, nil)

	var count int
	bus.Subscribe(audit.EventRecorded, func(context.Context, pkgcore.Event) error {
		count++
		return nil
	})

	if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", Sensitive: false}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if count != 0 {
		t.Errorf("captured %d audit.EventRecorded events for a non-sensitive create, want 0", count)
	}
}

func TestService_Create_PublishesShareCreated(t *testing.T) {
	svc, bus := newTestService(t, nil)
	var got pkgcore.Event
	bus.Subscribe(EventShareCreated, func(_ context.Context, evt pkgcore.Event) error {
		got = evt
		return nil
	})
	result, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	payload, ok := got.Payload.(ShareCreatedPayload)
	if !ok {
		t.Fatalf("payload is %T, want ShareCreatedPayload", got.Payload)
	}
	if payload.ShareID != result.Share.ID {
		t.Errorf("payload.ShareID = %q, want %q", payload.ShareID, result.Share.ID)
	}
}

// --- Access ----------------------------------------------------------------

func TestService_Access_GrantedOnValidToken(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	share, err := svc.Access(testCtx(), created.Token, AccessParams{IP: "203.0.113.1"})
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if share.ID != created.Share.ID {
		t.Errorf("Access returned share %q, want %q", share.ID, created.Share.ID)
	}
	if share.ViewCount != 1 {
		t.Errorf("ViewCount = %d, want 1", share.ViewCount)
	}
}

// TestService_Access_RevokedShare_ImmediatelyDenied is the explicit test
// rule 3 (docs/internal/07-platform-services.md's "revocation takes effect
// immediately" rule) calls
// for: create, access succeeds, revoke, access immediately fails, with no
// caching involved anywhere on this module's own side.
func TestService_Access_RevokedShare_ImmediatelyDenied(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, accessErr := svc.Access(testCtx(), created.Token, AccessParams{}); accessErr != nil {
		t.Fatalf("Access (before revoke): %v", accessErr)
	}

	if revokeErr := svc.Revoke(testCtx(), created.Share.ID); revokeErr != nil {
		t.Fatalf("Revoke: %v", revokeErr)
	}

	_, err = svc.Access(testCtx(), created.Token, AccessParams{})
	assertCode(t, err, ErrNotAccessible.Code)
}

func TestService_Access_ExpiredShare_Denied(t *testing.T) {
	svc, _ := newTestService(t, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)
	past := now.Add(-time.Hour)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", ExpiresAt: &past})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = svc.Access(testCtx(), created.Token, AccessParams{})
	assertCode(t, err, ErrNotAccessible.Code)
}

func TestService_Access_ViewExhausted_Denied(t *testing.T) {
	svc, _ := newTestService(t, nil)
	one := 1
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", MaxViews: &one})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, accessErr := svc.Access(testCtx(), created.Token, AccessParams{}); accessErr != nil {
		t.Fatalf("Access (first, should be granted): %v", accessErr)
	}
	_, err = svc.Access(testCtx(), created.Token, AccessParams{})
	assertCode(t, err, ErrNotAccessible.Code)
}

func TestService_Access_UnknownToken_Denied(t *testing.T) {
	svc, _ := newTestService(t, nil)
	_, err := svc.Access(testCtx(), "this-token-was-never-issued", AccessParams{})
	assertCode(t, err, ErrNotAccessible.Code)
}

// --- AccessPublic ----------------------------------------------------

// TestService_AccessPublic_ResolvesTenantFromTokenAlone is the direct
// round-2 proof of AGENTS.md's former "Tenant resolution for an
// unauthenticated viewer" gap being closed: a caller supplying NO tenant
// at all (context.Background(), not testCtx()) still reaches a granted
// access, because AccessPublic resolves the tenant from the token itself
// before re-entering the ordinary Access path.
func TestService_AccessPublic_ResolvesTenantFromTokenAlone(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	share, err := svc.AccessPublic(context.Background(), created.Token, AccessParams{IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("AccessPublic(no tenant in context) error = %v, want success", err)
	}
	if share.ID != created.Share.ID {
		t.Errorf("AccessPublic returned share %q, want %q", share.ID, created.Share.ID)
	}
	if share.ViewCount != 1 {
		t.Errorf("ViewCount = %d, want 1", share.ViewCount)
	}
}

// TestService_AccessPublic_UnknownToken_Denied proves an unrecognized token
// refuses through AccessPublic's own tenant-resolution failure path
// (tenantForTokenHash), never reaching Access at all -- and still answers
// the identical ErrNotAccessible rule 5 requires.
func TestService_AccessPublic_UnknownToken_Denied(t *testing.T) {
	svc, _ := newTestService(t, nil)
	_, err := svc.AccessPublic(context.Background(), "this-token-was-never-issued", AccessParams{})
	assertCode(t, err, ErrNotAccessible.Code)
}

// TestService_AccessPublic_RevokedShare_ImmediatelyDenied re-proves rule 3
// through the genuinely-anonymous entry point, mirroring
// TestService_Access_RevokedShare_ImmediatelyDenied's authenticated-caller
// version exactly.
func TestService_AccessPublic_RevokedShare_ImmediatelyDenied(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, accessErr := svc.AccessPublic(context.Background(), created.Token, AccessParams{}); accessErr != nil {
		t.Fatalf("AccessPublic (before revoke): %v", accessErr)
	}
	if revokeErr := svc.Revoke(testCtx(), created.Share.ID); revokeErr != nil {
		t.Fatalf("Revoke: %v", revokeErr)
	}
	_, err = svc.AccessPublic(context.Background(), created.Token, AccessParams{})
	assertCode(t, err, ErrNotAccessible.Code)
}

// TestService_AccessPublic_Password mirrors TestService_Access_Password
// through the genuinely-anonymous entry point: a correct password grants,
// a wrong one refuses with the identical ErrNotAccessible.
func TestService_AccessPublic_Password(t *testing.T) {
	svc, _ := newTestService(t, nil)
	password := "s3cret"
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", Password: &password})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	wrong := "wrong"
	if _, err := svc.AccessPublic(context.Background(), created.Token, AccessParams{Password: &wrong}); !hasCode(err, ErrNotAccessible.Code) {
		t.Errorf("AccessPublic(wrong password) error = %v, want ErrNotAccessible", err)
	}

	if _, err := svc.AccessPublic(context.Background(), created.Token, AccessParams{Password: &password}); err != nil {
		t.Errorf("AccessPublic(correct password) error = %v, want success", err)
	}
}

// TestService_AccessPublic_CrossTenantTokenNeverResolvesToTheWrongTenant
// proves the narrow tenant-resolution lookup cannot be tricked into
// resolving a token minted under one tenant to a different one: it always
// resolves to the tenant that actually created the share, and a second
// tenant's own share is entirely unaffected by the first's existence.
func TestService_AccessPublic_CrossTenantTokenNeverResolvesToTheWrongTenant(t *testing.T) {
	svc, _ := newTestService(t, nil)
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	createdA, err := svc.Create(ctxA, CreateParams{ResourceRef: "a-resource"})
	if err != nil {
		t.Fatalf("Create(tenant-a): %v", err)
	}
	createdB, err := svc.Create(ctxB, CreateParams{ResourceRef: "b-resource"})
	if err != nil {
		t.Fatalf("Create(tenant-b): %v", err)
	}

	shareA, err := svc.AccessPublic(context.Background(), createdA.Token, AccessParams{})
	if err != nil {
		t.Fatalf("AccessPublic(tenant-a's token): %v", err)
	}
	if shareA.ResourceRef != "a-resource" {
		t.Errorf("AccessPublic(tenant-a's token) resolved ResourceRef %q, want %q", shareA.ResourceRef, "a-resource")
	}

	shareB, err := svc.AccessPublic(context.Background(), createdB.Token, AccessParams{})
	if err != nil {
		t.Fatalf("AccessPublic(tenant-b's token): %v", err)
	}
	if shareB.ResourceRef != "b-resource" {
		t.Errorf("AccessPublic(tenant-b's token) resolved ResourceRef %q, want %q", shareB.ResourceRef, "b-resource")
	}
}

func TestService_Access_Password(t *testing.T) {
	svc, _ := newTestService(t, nil)
	password := "s3cret"
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", Password: &password})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("missing password denied", func(t *testing.T) {
		_, err := svc.Access(testCtx(), created.Token, AccessParams{})
		assertCode(t, err, ErrNotAccessible.Code)
	})
	t.Run("wrong password denied", func(t *testing.T) {
		wrong := "not-the-password"
		_, err := svc.Access(testCtx(), created.Token, AccessParams{Password: &wrong})
		assertCode(t, err, ErrNotAccessible.Code)
	})
	t.Run("correct password granted", func(t *testing.T) {
		_, err := svc.Access(testCtx(), created.Token, AccessParams{Password: &password})
		if err != nil {
			t.Fatalf("Access(correct password): %v", err)
		}
	})
}

// TestService_Access_EveryRefusalReasonIsOutwardlyIdentical pins rule 5
// (docs/internal/07-platform-services.md's "the share surface must leak
// nothing about the tenant" rule): an unknown token, a revoked share, an expired share, a
// view-exhausted share and a wrong password all answer with the exact same
// *apperr.Error -- same Code, same Status, no parameter distinguishing
// which reason applied.
func TestService_Access_EveryRefusalReasonIsOutwardlyIdentical(t *testing.T) {
	svc, _ := newTestService(t, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	unknown := "no-such-token-ever"

	past := now.Add(-time.Hour)
	expiredResult, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &past})
	if err != nil {
		t.Fatalf("Create(expired): %v", err)
	}

	revokedResult, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	if err != nil {
		t.Fatalf("Create(revoked): %v", err)
	}
	if revokeErr := svc.Revoke(testCtx(), revokedResult.Share.ID); revokeErr != nil {
		t.Fatalf("Revoke: %v", revokeErr)
	}

	one := 1
	exhaustedResult, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", MaxViews: &one})
	if err != nil {
		t.Fatalf("Create(exhausted): %v", err)
	}
	if _, accessErr := svc.Access(testCtx(), exhaustedResult.Token, AccessParams{}); accessErr != nil {
		t.Fatalf("Access(exhausting the one view): %v", accessErr)
	}

	password := "correct"
	wrong := "wrong"
	passwordResult, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", Password: &password})
	if err != nil {
		t.Fatalf("Create(password): %v", err)
	}

	cases := map[string]func() error{
		"unknown token": func() error {
			_, err := svc.Access(testCtx(), unknown, AccessParams{})
			return err
		},
		"expired": func() error {
			_, err := svc.Access(testCtx(), expiredResult.Token, AccessParams{})
			return err
		},
		"revoked": func() error {
			_, err := svc.Access(testCtx(), revokedResult.Token, AccessParams{})
			return err
		},
		"view exhausted": func() error {
			_, err := svc.Access(testCtx(), exhaustedResult.Token, AccessParams{})
			return err
		},
		"wrong password": func() error {
			_, err := svc.Access(testCtx(), passwordResult.Token, AccessParams{Password: &wrong})
			return err
		},
		"missing password": func() error {
			_, err := svc.Access(testCtx(), passwordResult.Token, AccessParams{})
			return err
		},
	}

	var reference *apperr.Error
	for name, run := range cases {
		err := run()
		got, ok := apperr.As(err)
		if !ok {
			t.Fatalf("%s: error %v does not decode as *apperr.Error", name, err)
		}
		if reference == nil {
			reference = got
			continue
		}
		if got.Code != reference.Code {
			t.Errorf("%s: Code = %q, want %q (identical to every other refusal reason)", name, got.Code, reference.Code)
		}
		if got.Status != reference.Status {
			t.Errorf("%s: Status = %d, want %d", name, got.Status, reference.Status)
		}
		if len(got.Params) != 0 {
			t.Errorf("%s: Params = %v, want none -- a parameter could itself distinguish the refusal reason", name, got.Params)
		}
	}
}

// TestService_Access_RefusalPathsPayEqualPasswordCheckCost closes the
// timing side channel TestService_Access_EveryRefusalReasonIsOutwardlyIdentical
// cannot see: that test only asserts Code/Status/Params equality, never
// latency, so it passed even when a prior version of Access answered three
// refusal paths -- an unknown token, a share with no password configured,
// and a password-protected share accessed with no password at all -- in
// roughly the time a lookup takes, while a refusal driven by an actual
// (right-or-wrong) password guess paid argon2id's real, tens-of-milliseconds
// cost. That gap let an external prober tell "this token names a
// password-protected share" apart from every other refusal purely by
// response latency, even though every refusal already answers with the
// identical ErrNotAccessible (rule 5). This test fails on that unfixed
// code, where the three burnable paths return far faster than the real
// check, and passes once every path burns an equivalent argon2id check
// (password.go's burnSharePasswordCheck).
func TestService_Access_RefusalPathsPayEqualPasswordCheckCost(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement is slow under -short")
	}

	svc, _ := newTestService(t, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	password := "correct-horse-battery-staple"
	wrong := "an incorrect guess"
	protected, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", Password: &password})
	if err != nil {
		t.Fatalf("Create(password-protected): %v", err)
	}
	unprotected, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	if err != nil {
		t.Fatalf("Create(no password): %v", err)
	}

	const samples = 7

	// minDuration runs run samples times and keeps the fastest
	// observation -- scheduling noise (GC, OS preemption) only ever adds
	// latency on top of a call's real cost, so the minimum across several
	// runs is the most faithful estimate of that deterministic cost.
	minDuration := func(run func()) time.Duration {
		t.Helper()
		best := time.Duration(1<<63 - 1)
		for i := 0; i < samples; i++ {
			start := time.Now()
			run()
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}

	realCheck := minDuration(func() {
		_, _ = svc.Access(testCtx(), protected.Token, AccessParams{Password: &wrong})
	})

	cases := map[string]func(){
		"unknown token": func() {
			_, _ = svc.Access(testCtx(), "no-such-token-ever", AccessParams{})
		},
		"no password configured": func() {
			_, _ = svc.Access(testCtx(), unprotected.Token, AccessParams{})
		},
		"missing password on a protected share": func() {
			_, _ = svc.Access(testCtx(), protected.Token, AccessParams{})
		},
	}

	// The unfixed code answered these three paths in a small fraction of
	// the real check's time (a lookup vs. a real argon2id call).
	// Requiring at least half the real check's duration leaves ample
	// margin for scheduling noise while still failing hard against the
	// effectively-instant unfixed fast paths.
	const minFraction = 0.5
	for name, run := range cases {
		got := minDuration(run)
		if float64(got) < float64(realCheck)*minFraction {
			t.Errorf("%s: took %v, want at least %.0f%% of the real password check's %v -- a refusal path is skipping the argon2id burn and reopening the timing side channel", name, got, minFraction*100, realCheck)
		}
	}
}

// TestService_Access_LogsEveryAttempt pins rule 4's leave-a-trail half: a
// resource owner can read back who viewed a share, and how many times,
// through ListAccessLog -- including denied attempts.
func TestService_Access_LogsEveryAttempt(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, accessErr := svc.Access(testCtx(), created.Token, AccessParams{IP: "203.0.113.5", UserAgent: "test-agent", Referrer: "https://example.com"}); accessErr != nil {
		t.Fatalf("Access (granted): %v", accessErr)
	}
	if _, accessErr := svc.Access(testCtx(), "wrong-token", AccessParams{IP: "203.0.113.9"}); accessErr == nil {
		t.Fatalf("Access (unknown token) unexpectedly succeeded")
	}

	entries, err := svc.ListAccessLog(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("ListAccessLog: %v", err)
	}
	// Only the granted attempt is logged against THIS share -- the unknown
	// token matched no row in this tenant at all, so there was nothing to
	// attribute a log entry to (Service.Access's own doc comment).
	if len(entries) != 1 {
		t.Fatalf("ListAccessLog returned %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.Outcome != AccessOutcomeGranted {
		t.Errorf("Outcome = %q, want %q", got.Outcome, AccessOutcomeGranted)
	}
	if got.IP != "203.0.113.5" || got.UserAgent != "test-agent" || got.Referrer != "https://example.com" {
		t.Errorf("entry = %+v, want the request metadata recorded verbatim", got)
	}
}

func TestService_Access_LogsDeniedAttemptsAgainstAKnownShare(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if revokeErr := svc.Revoke(testCtx(), created.Share.ID); revokeErr != nil {
		t.Fatalf("Revoke: %v", revokeErr)
	}
	if _, accessErr := svc.Access(testCtx(), created.Token, AccessParams{}); accessErr == nil {
		t.Fatalf("Access(revoked share) unexpectedly succeeded")
	}

	entries, err := svc.ListAccessLog(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("ListAccessLog: %v", err)
	}
	if len(entries) != 1 || entries[0].Outcome != AccessOutcomeDenied {
		t.Fatalf("ListAccessLog = %+v, want exactly one denied entry", entries)
	}
}

// TestService_Access_ConcurrentAccessesRespectMaxViews races many
// concurrent Access calls against one MaxViews-limited share and proves
// exactly MaxViews of them are granted -- not fewer (a benign CAS race
// misreported as "not accessible") and not more (the ceiling actually
// enforced). Run with -race.
func TestService_Access_ConcurrentAccessesRespectMaxViews(t *testing.T) {
	svc, _ := newTestService(t, nil)
	const maxViews = 5
	const attempts = 30
	limit := maxViews
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", MaxViews: &limit})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var granted int32
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			if _, err := svc.Access(testCtx(), created.Token, AccessParams{}); err == nil {
				atomic.AddInt32(&granted, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&granted); got != maxViews {
		t.Errorf("granted %d accesses, want exactly %d", got, maxViews)
	}
}

// --- Revoke / Get / ListAccessLog ------------------------------------------

func TestService_Revoke_IsIdempotent(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Revoke(testCtx(), created.Share.ID); err != nil {
		t.Fatalf("Revoke (first): %v", err)
	}
	if err := svc.Revoke(testCtx(), created.Share.ID); err != nil {
		t.Fatalf("Revoke (second, already revoked): %v", err)
	}
}

func TestService_Revoke_UnknownShare(t *testing.T) {
	svc, _ := newTestService(t, nil)
	err := svc.Revoke(testCtx(), "does-not-exist")
	assertCode(t, err, ErrShareNotFound.Code)
}

func TestService_Revoke_PublishesShareRevoked(t *testing.T) {
	svc, bus := newTestService(t, nil)
	var got pkgcore.Event
	bus.Subscribe(EventShareRevoked, func(_ context.Context, evt pkgcore.Event) error {
		got = evt
		return nil
	})
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Revoke(testCtx(), created.Share.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	payload, ok := got.Payload.(ShareRevokedPayload)
	if !ok {
		t.Fatalf("payload is %T, want ShareRevokedPayload", got.Payload)
	}
	if payload.ShareID != created.Share.ID {
		t.Errorf("payload.ShareID = %q, want %q", payload.ShareID, created.Share.ID)
	}
}

func TestService_Get_UnknownShare(t *testing.T) {
	svc, _ := newTestService(t, nil)
	_, err := svc.Get(testCtx(), "does-not-exist")
	assertCode(t, err, ErrShareNotFound.Code)
}

func TestService_ListAccessLog_UnknownShare(t *testing.T) {
	svc, _ := newTestService(t, nil)
	_, err := svc.ListAccessLog(testCtx(), "does-not-exist")
	assertCode(t, err, ErrShareNotFound.Code)
}

// assertCode fails the test unless err decodes as an *apperr.Error with
// code want.
func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	got, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v does not decode as *apperr.Error (want code %q)", err, want)
	}
	if got.Code != want {
		t.Errorf("error code = %q, want %q", got.Code, want)
	}
}

// TestService_Create_NoTenantInContext proves Create fails before touching
// the database when ctx carries no tenant.
func TestService_Create_NoTenantInContext(t *testing.T) {
	svc, _ := newTestService(t, nil)
	_, err := svc.Create(context.Background(), CreateParams{ResourceRef: "storage:obj-1"})
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("error = %v, want to wrap pkgcore.ErrNoTenant", err)
	}
}
