package sharing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/componenttest"
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
	// The registry carries this test's bus as its one EventBus value --
	// the seam svc.attach and the sweep's publish read back -- and the
	// audit action declares inside the registry's one Init window, the
	// only turn in which the seats accept writes.
	reg := componenttest.NewRegistryWithBus(bus)
	if err := componenttest.Declare(reg, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add(AuditActionSensitiveShareCreate)
	}); err != nil {
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

// TestService_Create_ForeverRefused pins the never-expiring-link refusal:
// an explicit request for a never-expiring share must be REFUSED, not
// silently allowed.
func TestService_Create_ForeverRefused(t *testing.T) {
	svc, _ := newTestService(t, nil)
	_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", Forever: true})
	assertCode(t, err, ErrExpiryRequired.Code)
}

// TestService_Create_NoExpiryFallsBackToDefault pins the default-expiry
// forcing half: a request with no ExpiresAt and no TenantConfigReader wired gets
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
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)
	explicit := now.Add(7 * 24 * time.Hour)
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

// TestService_Create_SensitiveEmitsAuditEvent pins the sensitive-resource
// audit requirement: Sensitive: true fires the sensitive-share audit
// action through the declarative audit.Emit path.
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
// of the immediate-revocation rule: create, access succeeds, revoke,
// access immediately fails, with no caching involved anywhere on this
// module's own side.
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
	// Rule 2's validation refuses a share born already expired, so the
	// share is created live and the service clock moves past its expiry
	// before the access attempt.
	createAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(createAt)
	expiring := createAt.Add(time.Hour)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", ExpiresAt: &expiring})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	svc.now = fixedClock(createAt.Add(2 * time.Hour))
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
// proof of the token-based tenant resolution: a caller supplying NO tenant
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
	if _, err := svc.AccessPublic(context.Background(), created.Token, AccessParams{Password: &wrong}); !apperr.HasCode(err, ErrNotAccessible.Code) {
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

// TestService_Access_EveryRefusalReasonIsOutwardlyIdentical pins the
// outward-identical-answer rule: an unknown token, a revoked share, an
// expired share, a view-exhausted share and a wrong password all answer
// with the exact same *apperr.Error -- same Code, same Status, no
// parameter distinguishing which reason applied.
func TestService_Access_EveryRefusalReasonIsOutwardlyIdentical(t *testing.T) {
	svc, _ := newTestService(t, nil)
	// Every share below is minted at clock T0 (an already-expired share
	// cannot be created -- resolveExpiry refuses one -- so the "expired"
	// case is a share minted with a near expiry that the clock then moves
	// past before the refusal cases run; every other share's default
	// expiry is 30 days from T0, so all of them are still live at T0+2h).
	createAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(createAt)

	unknown := "no-such-token-ever"

	expiring := createAt.Add(time.Hour)
	expiredResult, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &expiring})
	if err != nil {
		t.Fatalf("Create(expiring): %v", err)
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

	// Move the clock past the expiring share's expiry: it is now the
	// "expired" refusal case, and only it.
	svc.now = fixedClock(createAt.Add(2 * time.Hour))

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
		if !apperr.HasCode(err, reference.Code) {
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
// latency. Without the burn, three refusal paths -- an unknown token, a
// share with no password configured, and a password-protected share
// accessed with no password at all -- would answer in roughly the time a
// lookup takes, while a refusal driven by an actual (right-or-wrong)
// password guess pays argon2id's real, tens-of-milliseconds cost. That gap
// would let an external prober tell "this token names a
// password-protected share" apart from every other refusal purely by
// response latency, even though every refusal already answers with the
// identical ErrNotAccessible. This test fails those three paths when they
// return far faster than the real check, requiring every path to burn an
// equivalent argon2id check (password.go's burnSharePasswordCheck).
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

	// Without the burn these three paths would answer in a small fraction
	// of the real check's time (a lookup vs. a real argon2id call).
	// Requiring at least half the real check's duration leaves ample
	// margin for scheduling noise while still failing hard against such
	// effectively-instant fast paths.
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

// TestService_Access_ConcurrentUnlimitedViews_AllSucceedAndAllCount races
// many concurrent Access calls against one UNLIMITED share (MaxViews nil --
// the common public-link shape) and proves every one of them is granted
// and every granted view is counted. An unlimited share has no ceiling
// for a compare-and-swap to arbitrate, so a bounded CAS retry loop would
// be the wrong tool there: under genuine concurrency a viewer could lose
// enough races to be refused (a false 404) and lost races would lose
// count updates. The view recording is one atomic increment
// (tryIncrementView) that cannot lose to concurrency. Run with -race.
// TestService_Access_RefusedWhileTheRouteHoldsTheReservation pins the
// in-process Access path's respect for the access route's in-flight
// reservation (the reserve/confirm/refund shape's single-flight rule):
// while the route is serving a MaxViews-limited share's view -- the
// reservation held, its bytes still streaming -- an in-process
// Service.Access for the same share is refused with the identical outward
// answer, spending nothing; the view a delivery in flight holds cannot be
// double-spent by a second path. Once the delivery fails and its refund
// releases the reservation, the same Access succeeds and spends the view.
func TestService_Access_RefusedWhileTheRouteHoldsTheReservation(t *testing.T) {
	svc, _ := newTestService(t, nil)
	one := 1
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "ref-1", MaxViews: &one})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	share, err := svc.Get(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// The route takes its reservation before any delivery begins.
	reserved, err := svc.reserveAccessView(testCtx(), share, AccessParams{})
	if err != nil {
		t.Fatalf("reserveAccessView: %v", err)
	}

	// The in-process Access is refused while the reservation stands.
	_, err = svc.Access(testCtx(), created.Token, AccessParams{})
	assertCode(t, err, ErrNotAccessible.Code)

	still, err := svc.Get(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("Get (after the refused Access): %v", err)
	}
	if still.ViewCount != 0 || still.ViewsReserved != 1 {
		t.Fatalf("row after the refused Access = ViewCount %d / ViewsReserved %d, want 0 / 1 -- the refusal must spend nothing and leave the reservation standing", still.ViewCount, still.ViewsReserved)
	}

	// The route's delivery fails and refunds the reservation: the share's
	// one view is free again, and the in-process Access now succeeds.
	if refundErr := svc.refundAccessView(testCtx(), reserved); refundErr != nil {
		t.Fatalf("refundAccessView: %v", refundErr)
	}
	after, err := svc.Access(testCtx(), created.Token, AccessParams{})
	if err != nil {
		t.Fatalf("Access (after the refund): %v", err)
	}
	if after.ViewCount != 1 {
		t.Errorf("ViewCount = %d after the post-refund Access, want 1", after.ViewCount)
	}
}

// TestService_ConfirmAccessView_StoreFailure_KeepsTheReservationHeld pins
// confirmAccessView's never-refund-after-delivery contract: when the
// confirm write itself fails after the content was fully delivered, the
// reservation is NOT released -- the view stays held in use, spent --
// exactly as go/billing's Confirm-after-success semantics never refund a
// reservation the delivered work already earned. The reservation's release
// is left to no one but a later confirm or the recorded timeout
// convergence; the failed confirm itself never returns the view to the
// share.
func TestService_ConfirmAccessView_StoreFailure_KeepsTheReservationHeld(t *testing.T) {
	svc, _ := newTestService(t, nil)
	one := 1
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "ref-1", MaxViews: &one})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	share, err := svc.Get(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reserved, err := svc.reserveAccessView(testCtx(), share, AccessParams{})
	if err != nil {
		t.Fatalf("reserveAccessView: %v", err)
	}

	// The delivery succeeded; only the confirm write fails (deterministic:
	// a SQL trigger raising on every view_count UPDATE).
	trigger := "CREATE TRIGGER sharing_test_fail_confirm BEFORE UPDATE OF view_count ON " + tableShares +
		" BEGIN SELECT RAISE(FAIL, 'injected confirm write failure'); END"
	if triggerErr := svc.shares.db.Exec(trigger).Error; triggerErr != nil {
		t.Fatalf("CREATE TRIGGER: %v", triggerErr)
	}
	if confirmErr := svc.confirmAccessView(testCtx(), reserved, AccessParams{}); confirmErr == nil {
		t.Fatalf("confirmAccessView = nil, want the internal error the failed confirm write surfaces")
	} else {
		assertCode(t, confirmErr, ErrInternal.Code)
	}

	// The reservation stands: the delivered view stays spent, never
	// refunded by the failed confirm.
	after, err := svc.Get(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("Get (after the failed confirm): %v", err)
	}
	if after.ViewsReserved != 1 || after.ViewCount != 0 {
		t.Errorf("row after the failed confirm = ViewsReserved %d / ViewCount %d, want 1 / 0 -- the delivered serve's reservation must not be refunded", after.ViewsReserved, after.ViewCount)
	}
}

// TestService_ConfirmAccessView_RevokedMidFlight_RefundsAndSettlesDenied
// pins the confirm's settle-time-liveness semantics: a share revoked while
// its delivery was in flight refuses the confirm at the database, and the
// serve is then settled as denied with its reservation released -- the
// revoked share can never serve again, so neither the view nor the
// reservation it held means anything further.
func TestService_ConfirmAccessView_RevokedMidFlight_RefundsAndSettlesDenied(t *testing.T) {
	svc, _ := newTestService(t, nil)
	one := 1
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "ref-1", MaxViews: &one})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	share, err := svc.Get(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reserved, err := svc.reserveAccessView(testCtx(), share, AccessParams{})
	if err != nil {
		t.Fatalf("reserveAccessView: %v", err)
	}

	// The owner revokes while the delivery is in flight.
	if revokeErr := svc.Revoke(testCtx(), created.Share.ID); revokeErr != nil {
		t.Fatalf("Revoke: %v", revokeErr)
	}

	// The confirm loses at the liveness guard; the serve is settled denied
	// and its reservation released -- no error: this is the honest refused
	// outcome of a delivery whose share stopped being live mid-flight.
	if confirmErr := svc.confirmAccessView(testCtx(), reserved, AccessParams{}); confirmErr != nil {
		t.Fatalf("confirmAccessView (revoked mid-flight) = %v, want nil -- the lost confirm settles as denied, not as a store error", confirmErr)
	}
	after, err := svc.Get(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("Get (after the lost confirm): %v", err)
	}
	if after.ViewsReserved != 0 || after.ViewCount != 0 {
		t.Errorf("row after the lost confirm = ViewsReserved %d / ViewCount %d, want 0 / 0", after.ViewsReserved, after.ViewCount)
	}

	entries, err := svc.ListAccessLog(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("ListAccessLog: %v", err)
	}
	if len(entries) != 1 || entries[0].Outcome != AccessOutcomeDenied {
		t.Errorf("access log after the lost confirm = %+v, want exactly one denied entry", entries)
	}
}

func TestService_Access_ConcurrentUnlimitedViews_AllSucceedAndAllCount(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const attempts = 40
	var granted int32
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			if _, accessErr := svc.Access(testCtx(), created.Token, AccessParams{}); accessErr == nil {
				atomic.AddInt32(&granted, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&granted); got != attempts {
		t.Errorf("granted %d accesses, want all %d -- a legit viewer of an unlimited share must never be refused by concurrency", got, attempts)
	}

	got, err := svc.Get(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ViewCount != attempts {
		t.Errorf("ViewCount = %d, want %d -- each granted view must be counted exactly once under concurrency", got.ViewCount, attempts)
	}

	// Second leg: the same guarantee at recordView level, without the
	// password burn that staggers the goroutines above -- a genuine
	// tournament of many goroutines hammering the view-recording guard on
	// one unlimited share. Every call must be granted (a viewer of an
	// unlimited share is refused only by revocation or expiry, never by
	// losing too many races) and every grant must land in the count. A
	// bounded-CAS retry loop would refuse a goroutine that lost enough
	// consecutive races -- the false-404 shape this tournament refuses.
	second, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-2"})
	if err != nil {
		t.Fatalf("Create (second leg): %v", err)
	}
	share, err := svc.Shares().FindByID(testCtx(), second.Share.ID)
	if err != nil {
		t.Fatalf("FindByID (second leg): %v", err)
	}

	const callsPerWorker = 100
	const workers = 30
	var won, refused, viewErrs int32
	var wg2 sync.WaitGroup
	wg2.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg2.Done()
			for c := 0; c < callsPerWorker; c++ {
				// Each winning recordView also commits a real granted log
				// row in the same transaction (the count-and-trail
				// atomicity recordView's own doc comment describes), so the
				// tournament doubles as the proof that the in-transaction
				// log insert never fails or duplicates under concurrency.
				entry := svc.accessLogEntry(testTenant, share.ID, AccessOutcomeGranted, AccessParams{})
				_, granted, viewErr := svc.recordView(testCtx(), share, svc.now(), entry)
				switch {
				case viewErr != nil:
					atomic.AddInt32(&viewErrs, 1)
				case granted:
					atomic.AddInt32(&won, 1)
				default:
					atomic.AddInt32(&refused, 1)
				}
			}
		}()
	}
	wg2.Wait()

	wantWon := int32(workers * callsPerWorker)
	if refused != 0 {
		t.Errorf("recordView refused %d of %d concurrent calls on a live unlimited share -- a viewer of an unlimited share must never be refused by concurrency", refused, wantWon)
	}
	if viewErrs != 0 {
		t.Errorf("recordView failed with %d store errors", viewErrs)
	}
	if won != wantWon {
		t.Errorf("recordView granted %d of %d concurrent calls", won, wantWon)
	}
	final, err := svc.Get(testCtx(), second.Share.ID)
	if err != nil {
		t.Fatalf("Get (second leg): %v", err)
	}
	if final.ViewCount != int(wantWon) {
		t.Errorf("ViewCount = %d after %d granted views -- each granted view must be counted exactly once under concurrency", final.ViewCount, wantWon)
	}
	logged, err := svc.ListAccessLog(testCtx(), second.Share.ID)
	if err != nil {
		t.Fatalf("ListAccessLog (second leg): %v", err)
	}
	if len(logged) != int(wantWon) {
		t.Errorf("access log rows = %d after %d granted views -- every granted view's log row must commit exactly once, in the view's own transaction", len(logged), wantWon)
	}
}

// TestService_Access_ConcurrentViewsSurviveRevoke proves Service.Revoke
// never rolls back a view recorded concurrently with it: the guarded
// revoked_at-only update (ShareRepository.markRevoked) preserves every
// view-count increment that committed before the revocation, so the count
// the owner reads back afterwards equals the number of accesses actually
// granted. A whole-row write from the revoking side's own pre-revoke read
// would erase an increment landing between that read and the write --
// exactly the shape this race refuses. Each iteration uses a fresh share
// so the race is replayed many times rather than once. Run with -race.
func TestService_Access_ConcurrentViewsSurviveRevoke(t *testing.T) {
	svc, _ := newTestService(t, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	const iterations = 10
	for iter := 0; iter < iterations; iter++ {
		revokeRaceIteration(t, svc, iter, now)
	}
}

// revokeRaceIteration runs one iteration of the race above on a fresh
// share: four recordView workers record views until the first granted view
// signals them live, Revoke races them, and the row the owner reads back
// must carry exactly the granted count and log trail. It is one function
// per iteration so the workers' stop channel has an explicit lifetime --
// closed as soon as this iteration's Revoke has landed, on every exit path
// -- instead of one deferred close per iteration accumulating on the test
// function until it returns.
//
// The workers' liveness signal is the first granted view itself -- a
// channel closed by whichever worker records it -- never a wall-clock
// poll: progress is an event, and the only timeout below is a broken-test
// detector (a worker population that never records anything), generous
// enough that a slow-but-healthy runner can never trip it.
func revokeRaceIteration(t *testing.T, svc *Service, iter int, now time.Time) {
	t.Helper()
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	share, err := svc.Shares().FindByID(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}

	var granted, workerErrors int32
	var workers sync.WaitGroup
	stop := make(chan struct{})
	firstGrant := make(chan struct{})
	for w := 0; w < 4; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// A real granted log entry rides along with each winning
				// recordView (committed in the same transaction), so
				// this race also proves the count-and-trail insert
				// survives a concurrent revocation without errors or
				// duplicates.
				entry := svc.accessLogEntry(testTenant, share.ID, AccessOutcomeGranted, AccessParams{})
				_, won, viewErr := svc.recordView(testCtx(), share, now, entry)
				if viewErr != nil {
					atomic.AddInt32(&workerErrors, 1)
					return
				}
				if !won {
					return // the share was revoked -- this worker is done
				}
				if atomic.AddInt32(&granted, 1) == 1 {
					close(firstGrant)
				}
			}
		}()
	}

	// Revoke lands the moment a view has been genuinely recorded, while the
	// workers are still recording -- the race window a whole-row
	// read-modify-write Revoke would lose increments in. The timeout is a
	// broken-test detector (a worker population that never records a view),
	// never an assertion about how fast a view must land.
	select {
	case <-firstGrant:
	case <-time.After(30 * time.Second):
		close(stop)
		workers.Wait()
		t.Fatalf("iteration %d: no view was recorded within 30s -- a worker population that never records is a broken test, not a slow runner", iter)
	}

	if revokeErr := svc.Revoke(testCtx(), created.Share.ID); revokeErr != nil {
		close(stop)
		workers.Wait()
		t.Fatalf("Revoke: %v", revokeErr)
	}
	// The workers' stop channel closes with the race itself: once Revoke
	// has landed the workers exit on their own (their next recordView is
	// refused by the revoked row), and the close is the belt-and-braces
	// release for any worker still between attempts, so Wait below can
	// never hang on a scheduling accident.
	close(stop)
	workers.Wait()

	if errs := atomic.LoadInt32(&workerErrors); errs != 0 {
		t.Fatalf("iteration %d: %d recordView store errors -- the view-recording path must not fail under this concurrency", iter, errs)
	}
	got, err := svc.Get(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ViewCount != int(atomic.LoadInt32(&granted)) {
		t.Fatalf("iteration %d: ViewCount = %d but %d views were granted -- the revoke rolled back a concurrently recorded view", iter, got.ViewCount, atomic.LoadInt32(&granted))
	}
	rows, err := svc.ListAccessLog(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("ListAccessLog: %v", err)
	}
	if len(rows) != int(atomic.LoadInt32(&granted)) {
		t.Fatalf("iteration %d: %d access log rows for %d granted views -- each granted view's log row must commit exactly once in the view's own transaction, even racing a revoke", iter, len(rows), atomic.LoadInt32(&granted))
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

// TestService_List_ReturnsTenantSharesIncludingRevoked is Service.List's own
// proof (the backing method of handler.go's SharingListShares). A revoked
// share is not filtered out -- an owner-facing listing must still be able
// to show what happened to it.
func TestService_List_ReturnsTenantSharesIncludingRevoked(t *testing.T) {
	svc, _ := newTestService(t, nil)

	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "ref-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if revokeErr := svc.Revoke(testCtx(), created.Share.ID); revokeErr != nil {
		t.Fatalf("Revoke: %v", revokeErr)
	}

	got, err := svc.List(testCtx(), 200, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d shares, want 1", len(got))
	}
	if got[0].ID != created.Share.ID {
		t.Errorf("List()[0].ID = %q, want %q", got[0].ID, created.Share.ID)
	}
	if got[0].RevokedAt == nil {
		t.Errorf("List()[0].RevokedAt is nil, want the revoked share's RevokedAt to still be visible")
	}
}

// TestService_List_ZeroLimitServesTheDefaultPage pins Service.List's page
// default: a limit of zero or less is a caller that did not ask, and the
// service answers with the same page it serves for the documented default
// (defaultListPageSize) -- the mirror of go/storage's ObjectService.List
// contract, which this method follows.
func TestService_List_ZeroLimitServesTheDefaultPage(t *testing.T) {
	svc, _ := newTestService(t, nil)
	for i := 0; i < 3; i++ {
		if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: fmt.Sprintf("ref-%d", i)}); err != nil {
			t.Fatalf("Create(%d): %v", i, err)
		}
	}

	got, err := svc.List(testCtx(), 0, "")
	if err != nil {
		t.Fatalf("List(limit 0): %v", err)
	}
	byDefault, err := svc.List(testCtx(), defaultListPageSize, "")
	if err != nil {
		t.Fatalf("List(default): %v", err)
	}
	if len(got) != len(byDefault) || len(got) != 3 {
		t.Errorf("List(limit 0) returned %d shares, List(default) %d -- both want the 3 created", len(got), len(byDefault))
	}
}

// TestService_List_UnknownCursor_AnswersShareNotFound pins Service.List's
// cursor mapping: a beforeID naming no share of the caller's tenant (one
// that never existed, or another tenant's share) is translated from
// dbkit's not-found into the module's own ErrShareNotFound with the cursor
// named in its "id" parameter -- the safe-to-disclose code every other
// owner-facing lookup answers -- never a page silently resumed from the
// wrong place.
func TestService_List_UnknownCursor_AnswersShareNotFound(t *testing.T) {
	svc, _ := newTestService(t, nil)
	if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: "ref-a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := svc.List(testCtx(), 10, "no-such-share")
	assertCode(t, err, ErrShareNotFound.Code)
	appErr, _ := apperr.As(err)
	if id, _ := appErr.Params["id"].(string); id != "no-such-share" {
		t.Errorf("refusal id param = %v, want %q", appErr.Params["id"], "no-such-share")
	}
}

// TestService_List_NeverCrossesTenants proves List reads only the caller
// tenant's own shares, mirroring every other tenant-scoped Service method's
// own isolation proof.
func TestService_List_NeverCrossesTenants(t *testing.T) {
	svc, _ := newTestService(t, nil)
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	if _, err := svc.Create(ctxA, CreateParams{ResourceRef: "ref-a"}); err != nil {
		t.Fatalf("Create(tenant-a): %v", err)
	}

	got, err := svc.List(ctxB, 200, "")
	if err != nil {
		t.Fatalf("List(tenant-b): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List(tenant-b) = %v, want none -- tenant-a's share must never appear", got)
	}
}

// assertCode fails the test unless err decodes as an *apperr.Error with
// code want.
func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	got, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v does not decode as *apperr.Error (want code %q)", err, want)
	}
	if !apperr.HasCode(err, want) {
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

// TestService_Create_ExpiryInThePast_Refused pins resolveExpiry's future
// requirement: an explicit ExpiresAt that is not strictly in the future is
// refused with sharing.expiry_out_of_range, never persisted as a share
// that is dead on arrival.
func TestService_Create_ExpiryInThePast_Refused(t *testing.T) {
	svc, _ := newTestService(t, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	exactlyNow := now
	_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &exactlyNow})
	assertCode(t, err, ErrExpiryOutOfRange.Code)

	past := now.Add(-time.Hour)
	_, err = svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &past})
	assertCode(t, err, ErrExpiryOutOfRange.Code)
}

// TestService_Create_ExpiryBeyondMaximum_Refused pins the never-expiring-
// in-disguise arm of rule 2 for a tenant with no configured default: an
// explicit ExpiresAt beyond the module's explicit-expiry ceiling --
// MaxExplicitShareLifetime, the same 30 days the unconfigured default
// names -- is refused with sharing.expiry_out_of_range, so expiresAt
// 9999-12-31 can never create the effectively-never-expiring link
// CreateParams.Forever's refusal exists to forbid. The exact ceiling
// (now + 30 days) is still honored.
func TestService_Create_ExpiryBeyondMaximum_Refused(t *testing.T) {
	svc, _ := newTestService(t, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	forever := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &forever})
	assertCode(t, err, ErrExpiryOutOfRange.Code)

	beyond := now.Add(MaxExplicitShareLifetime + time.Hour)
	_, err = svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &beyond})
	assertCode(t, err, ErrExpiryOutOfRange.Code)

	atCeiling := now.Add(MaxExplicitShareLifetime)
	result, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &atCeiling})
	if err != nil {
		t.Fatalf("Create(at the 30-day ceiling): %v", err)
	}
	if !result.Share.ExpiresAt.Equal(atCeiling) {
		t.Errorf("ExpiresAt = %v, want the ceiling request %v honored exactly", result.Share.ExpiresAt, atCeiling)
	}
}

// TestService_Create_TenantConfiguredDefaultBeyondCeiling_StillHonored pins
// the deliberate boundary of the explicit-expiry ceiling: a tenant's
// CONFIGURED default (through the TenantConfigReader seam) is the host's
// own policy, not a caller's end-run, so a configured default longer than
// the module's own 30-day MaxExplicitShareLifetime is honored unchanged --
// MaxExplicitShareLifetime's own doc comment states this boundary. The
// ceiling governs caller-supplied explicit values; it is never applied to
// the tenant-configured default itself.
func TestService_Create_TenantConfiguredDefaultBeyondCeiling_StillHonored(t *testing.T) {
	cfg := fakeTenantConfigReader{d: 60 * 24 * time.Hour, ok: true}
	svc, _ := newTestService(t, cfg)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	result, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := now.Add(60 * 24 * time.Hour)
	if !result.Share.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want the tenant-configured %v -- the configured default is host policy, not a caller request", result.Share.ExpiresAt, want)
	}
}

// TestService_Create_TenantDefaultAndExplicitExpiry_ShareOnePolicy pins
// the same-policy consistency of the two expiry paths when a tenant's
// configured default exceeds the module's own ceiling: the operative
// explicit-expiry ceiling for that tenant is RAISED to its configured
// default (explicitExpiryCeiling), so the module never refuses a
// caller-supplied explicit expiry that lies within the very envelope its
// own default path already grants -- accepting 90 days by omission while
// refusing 45 by explicitness would be the contradiction "the more
// specific you are, the more you are refused". The never-expiring-in-
// disguise arm survives the raise: an explicit request BEYOND the
// tenant's own configured default is still refused, 9999-12-31 included.
func TestService_Create_TenantDefaultAndExplicitExpiry_ShareOnePolicy(t *testing.T) {
	cfg := fakeTenantConfigReader{d: 90 * 24 * time.Hour, ok: true}
	svc, _ := newTestService(t, cfg)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = fixedClock(now)

	// An omitted expiry still resolves to the tenant's 90-day default.
	omitted, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	if err != nil {
		t.Fatalf("Create (omitted expiry): %v", err)
	}
	if want := now.Add(90 * 24 * time.Hour); !omitted.Share.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want the tenant-configured %v", omitted.Share.ExpiresAt, want)
	}

	// An explicit 45-day request -- inside the 90-day envelope the tenant's
	// own default admits -- is accepted and honored exactly, never refused
	// against the module's own 30-day ceiling.
	explicit := now.Add(45 * 24 * time.Hour)
	requested, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &explicit})
	if err != nil {
		t.Fatalf("Create (explicit 45 days, within the tenant's own 90-day envelope): %v", err)
	}
	if !requested.Share.ExpiresAt.Equal(explicit) {
		t.Errorf("ExpiresAt = %v, want the explicit request %v honored exactly", requested.Share.ExpiresAt, explicit)
	}

	// Beyond the tenant's own configured default the refusal returns:
	// nothing in this module admits a share longer than the tenant's own
	// policy, so an effectively-forever request can never slip through a
	// raised ceiling.
	beyond := now.Add(91 * 24 * time.Hour)
	_, err = svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &beyond})
	assertCode(t, err, ErrExpiryOutOfRange.Code)

	forever := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	_, err = svc.Create(testCtx(), CreateParams{ResourceRef: "r", ExpiresAt: &forever})
	assertCode(t, err, ErrExpiryOutOfRange.Code)
}

// TestService_Access_TruncatesOverlongLogMetadata pins the write-boundary
// cut of rule 4's trail: a caller-supplied User-Agent or Referer longer
// than the sharing_access_log column the migrations declare (VARCHAR(512),
// VARCHAR(64) for the IP) must be truncated to the column bound before the
// INSERT -- under PostgreSQL an over-long value would fail the write with
// error 22001 and the access would leave no trail at all. SQLite does not
// enforce VARCHAR lengths, so this test pins the Go-side cut directly:
// the stored row never carries more than the column's rune bound, the cut
// is rune-safe (a 600-rune multibyte value is cut at 512 runes, not 512
// bytes), a value at the exact bound survives verbatim, and invalid UTF-8
// bytes -- which a UTF-8-encoded PostgreSQL database refuses with error
// 22021 -- are sanitized to the replacement character instead of stored
// raw.
func TestService_Access_TruncatesOverlongLogMetadata(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	multibyteUA := strings.Repeat("€", 600) // 600 three-byte runes
	asciiReferrer := strings.Repeat("r", 900)
	overlongIP := strings.Repeat("1", 100)
	if _, accessErr := svc.Access(testCtx(), created.Token, AccessParams{
		IP: overlongIP, UserAgent: multibyteUA, Referrer: asciiReferrer,
	}); accessErr != nil {
		t.Fatalf("Access(overlong metadata): %v", err)
	}

	boundaryUA := strings.Repeat("b", accessLogUserAgentLen) // exactly at the bound
	if _, accessErr := svc.Access(testCtx(), created.Token, AccessParams{
		IP: "203.0.113.1", UserAgent: boundaryUA, Referrer: "",
	}); accessErr != nil {
		t.Fatalf("Access(boundary metadata): %v", err)
	}

	// "\xff\xfe" is one consecutive run of invalid bytes: the sanitizer
	// renders each run with a single replacement character (U+FFFD).
	replacement := string(rune(0xFFFD))
	invalidUA := "ok" + "\xff\xfe" + strings.Repeat("a", accessLogUserAgentLen)
	if _, accessErr := svc.Access(testCtx(), created.Token, AccessParams{
		IP: "203.0.113.1", UserAgent: invalidUA, Referrer: "",
	}); accessErr != nil {
		t.Fatalf("Access(invalid-UTF-8 metadata): %v", err)
	}

	entries, err := svc.ListAccessLog(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("ListAccessLog: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("ListAccessLog returned %d entries, want 3", len(entries))
	}
	storedUA := map[string]bool{}
	for _, e := range entries {
		storedUA[e.UserAgent] = true
		if len([]rune(e.IP)) > accessLogIPLen {
			t.Errorf("stored IP has %d runes, want at most %d", len([]rune(e.IP)), accessLogIPLen)
		}
		if len([]rune(e.UserAgent)) > accessLogUserAgentLen {
			t.Errorf("stored UserAgent has %d runes, want at most %d", len([]rune(e.UserAgent)), accessLogUserAgentLen)
		}
		if len([]rune(e.Referrer)) > accessLogReferrerLen {
			t.Errorf("stored Referrer has %d runes, want at most %d", len([]rune(e.Referrer)), accessLogReferrerLen)
		}
	}

	wantMultibyte := strings.Repeat("€", accessLogUserAgentLen)
	if !storedUA[wantMultibyte] {
		t.Errorf("stored UserAgent set is missing the 512-rune cut of the multibyte value -- the cut must be rune-safe, not byte-safe")
	}
	if !storedUA[boundaryUA] {
		t.Errorf("stored UserAgent set is missing the exactly-at-bound value -- a value at the bound must survive verbatim")
	}
	// The invalid-UTF-8 value: "ok" plus one replacement character (the
	// whole "\xff\xfe" run), then as many trailing runes as fit the
	// 512-rune cut (2 + 1 + 509).
	sanitized := "ok" + replacement + strings.Repeat("a", accessLogUserAgentLen-3)
	if !storedUA[sanitized] {
		t.Errorf("stored UserAgent set is missing the sanitized invalid-UTF-8 value -- invalid bytes must become the replacement character, never stored raw")
	}
}

// TestService_Access_LogWriteFailure_FailsTheAccessInsteadOfLeavingNoTrail
// pins rule 4's enforcement half: when the access-log row cannot be
// written, Access must NOT answer as if the access had been processed -- a
// granted access whose trail silently failed to commit is exactly the hole
// rule 4 exists to forbid. The failure surfaces as sharing.internal_error
// (the same shape recordView's own store failures surface as). The log
// table is dropped to force the write failure.
func TestService_Access_LogWriteFailure_FailsTheAccessInsteadOfLeavingNoTrail(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if dropErr := svc.shares.db.Exec("DROP TABLE " + tableAccessLog).Error; dropErr != nil {
		t.Fatalf("DROP TABLE sharing_access_log: %v", err)
	}

	_, err = svc.Access(testCtx(), created.Token, AccessParams{})
	assertCode(t, err, ErrInternal.Code)
}

// TestService_Access_LogWriteFailure_NeverPermanentlyConsumesTheShare
// pins the count-and-trail atomicity: a granted access whose log row
// cannot be written must not permanently exhaust the share it failed to
// log. The access-log INSERT alone is forced to fail (a trigger raising on
// the log table -- the view-count UPDATE itself stays healthy). The count
// and its granted log row commit in ONE guarded transaction (recordView's
// own doc comment), so the failed attempt rolls the count back with it:
// the first Access still fails with sharing.internal_error -- an access
// that leaves no trail must not be answered as processed -- but the share
// keeps its one view, and once the log write is healthy again a retry
// succeeds and records exactly one granted row.
func TestService_Access_LogWriteFailure_NeverPermanentlyConsumesTheShare(t *testing.T) {
	svc, _ := newTestService(t, nil)
	limit := 1
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", MaxViews: &limit})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	trigger := "CREATE TRIGGER sharing_test_fail_log_insert BEFORE INSERT ON " + tableAccessLog +
		" BEGIN SELECT RAISE(FAIL, 'injected access-log write failure'); END"
	if triggerErr := svc.shares.db.Exec(trigger).Error; triggerErr != nil {
		t.Fatalf("CREATE TRIGGER: %v", triggerErr)
	}

	// The log-failed attempt must surface as an internal error -- an
	// access that leaves no trail is never answered as if it had been
	// processed (rule 4's enforcement half).
	_, err = svc.Access(testCtx(), created.Token, AccessParams{})
	assertCode(t, err, ErrInternal.Code)

	// And it must not have consumed the share: nothing committed, so the
	// row still shows zero views.
	afterFailed, err := svc.Get(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if afterFailed.ViewCount != 0 {
		t.Fatalf("ViewCount = %d after the log-failed attempt, want 0 -- the failed attempt must not have committed its view count", afterFailed.ViewCount)
	}

	// Repair the log write and retry: the share still has its one view,
	// so the retry is granted and its row lands exactly once.
	if dropErr := svc.shares.db.Exec("DROP TRIGGER sharing_test_fail_log_insert").Error; dropErr != nil {
		t.Fatalf("DROP TRIGGER: %v", dropErr)
	}
	retried, err := svc.Access(testCtx(), created.Token, AccessParams{})
	if err != nil {
		t.Fatalf("Access (retry after repair): %v", err)
	}
	if retried.ViewCount != 1 {
		t.Errorf("ViewCount = %d after the repaired retry, want 1", retried.ViewCount)
	}

	entries, err := svc.ListAccessLog(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("ListAccessLog: %v", err)
	}
	if len(entries) != 1 || entries[0].Outcome != AccessOutcomeGranted {
		t.Errorf("ListAccessLog = %+v, want exactly one granted entry -- only the successful retry leaves its trail", entries)
	}
}

// TestService_Access_UnknownTokenStoreFailure_AnswersInternalNotNotFound
// pins the store-failure classification on Access's token lookup: a
// database failure is not a refusal reason an outside caller produced, so
// it must surface as sharing.internal_error with an Error log -- never be
// flattened into the outward 404 a genuine refusal answers with, which
// would erase the operational signal. The shares table is dropped to force
// the lookup failure.
func TestService_Access_UnknownTokenStoreFailure_AnswersInternalNotNotFound(t *testing.T) {
	svc, _ := newTestService(t, nil)
	if err := svc.shares.db.Exec("DROP TABLE " + tableShares).Error; err != nil {
		t.Fatalf("DROP TABLE sharing_shares: %v", err)
	}

	_, err := svc.Access(testCtx(), "any-token", AccessParams{})
	assertCode(t, err, ErrInternal.Code)
}

// TestService_AccessPublic_StoreFailure_AnswersInternalNotNotFound is the
// AccessPublic twin of the test above: tenantForTokenHash's own store
// failure (the token-index table dropped) must surface as
// sharing.internal_error, never be collapsed into ErrNotAccessible.
func TestService_AccessPublic_StoreFailure_AnswersInternalNotNotFound(t *testing.T) {
	svc, _ := newTestService(t, nil)
	if err := svc.shares.db.Exec("DROP TABLE " + tableTokenIndex).Error; err != nil {
		t.Fatalf("DROP TABLE sharing_token_index: %v", err)
	}

	_, err := svc.AccessPublic(context.Background(), "any-token", AccessParams{})
	assertCode(t, err, ErrInternal.Code)
}

// TestService_Access_RecordViewStoreFailure_StillLeavesLogAndEvent pins
// Access's "exactly one log row and one event per call" contract on the
// store-failure path: when the view recording itself fails (a SQL trigger
// forces every view_count update to fail), Access still records the
// attempt as a denied log entry and publishes one EventShareAccessed with
// Granted false, and THEN surfaces the internal error -- never returning
// the error with no trail at all.
func TestService_Access_RecordViewStoreFailure_StillLeavesLogAndEvent(t *testing.T) {
	svc, bus := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var got pkgcore.Event
	bus.Subscribe(EventShareAccessed, func(_ context.Context, evt pkgcore.Event) error {
		got = evt
		return nil
	})

	trigger := "CREATE TRIGGER sharing_test_fail_view_update BEFORE UPDATE OF view_count ON " + tableShares +
		" BEGIN SELECT RAISE(FAIL, 'injected view-count update failure'); END"
	if triggerErr := svc.shares.db.Exec(trigger).Error; triggerErr != nil {
		t.Fatalf("CREATE TRIGGER: %v", err)
	}

	_, err = svc.Access(testCtx(), created.Token, AccessParams{})
	assertCode(t, err, ErrInternal.Code)

	entries, listErr := svc.ListAccessLog(testCtx(), created.Share.ID)
	if listErr != nil {
		t.Fatalf("ListAccessLog: %v", listErr)
	}
	if len(entries) != 1 || entries[0].Outcome != AccessOutcomeDenied {
		t.Fatalf("ListAccessLog = %+v, want exactly one denied entry -- the store-failure attempt must still leave its trail", entries)
	}

	payload, ok := got.Payload.(ShareAccessedPayload)
	if !ok {
		t.Fatalf("event payload is %T, want ShareAccessedPayload", got.Payload)
	}
	if payload.ShareID != created.Share.ID || payload.Granted {
		t.Errorf("event payload = %+v, want share %q announced with Granted false", payload, created.Share.ID)
	}
}

// TestService_AccessPublic_UnknownTokenRefusalIsCheap pins the
// unauthenticated surface's anti-amplification property: refusing a token
// that names no share at all must cost a rate-limit check plus one
// token-index lookup, NOT a full argon2id verification. Burning that
// check on every unknown token would give a scanner that sprays random
// tokens (each hashing differently, so per-token rate limits cannot bind
// it) a memory- and CPU-amplification primitive capped only by the
// per-IP budget. The unknown refusal must therefore take a small
// fraction of the time a real verification against a known
// password-protected share takes. Timing test: skipped under -short,
// min-of-samples like its sibling
// TestService_Access_RefusalPathsPayEqualPasswordCheckCost.
func TestService_AccessPublic_UnknownTokenRefusalIsCheap(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement is slow under -short")
	}

	svc, _ := newTestService(t, nil)
	password := "correct horse"
	wrong := "wrong guess"
	protected, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", Password: &password})
	if err != nil {
		t.Fatalf("Create(password-protected): %v", err)
	}
	if _, err := svc.AccessPublic(context.Background(), "no-such-token", AccessParams{}); !apperr.HasCode(err, ErrNotAccessible.Code) {
		t.Fatalf("AccessPublic(unknown token) error = %v, want ErrNotAccessible", err)
	}

	const samples = 5
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

	// The reference cost the unknown-token refusal must be cheap against:
	// one argon2id verification on a known password-protected share.
	realCheck := minDuration(func() {
		_, _ = svc.AccessPublic(context.Background(), protected.Token, AccessParams{Password: &wrong})
	})
	unknownRefusal := minDuration(func() {
		_, _ = svc.AccessPublic(context.Background(), "no-such-token", AccessParams{})
	})

	if float64(unknownRefusal) > float64(realCheck)*0.5 {
		t.Errorf("unknown-token refusal took %v, want well under half the real check's %v -- the unauthenticated surface must not burn an argon2id verification on tokens that cannot exist", unknownRefusal, realCheck)
	}
}
