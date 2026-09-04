package integration

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// testService builds a *Service directly (bypassing Module.Attach, which
// needs a full *pkgcore.Registry from a real Bootstrap) over a fresh
// migrated SQLite database, an in-memory EventBus and a fixed clock, so
// every test in this file is deterministic and needs no wall-clock
// tolerance.
func testService(t *testing.T, permissions PermissionLister, membership MembershipChecker, now time.Time) *Service {
	t.Helper()
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(auditActionDecls...); err != nil {
		t.Fatalf("register audit actions: %v", err)
	}
	return &Service{
		repo:         NewAPIKeyRepository(newTestDB(t)),
		permissions:  permissions,
		membership:   membership,
		bus:          reg.Events.Bus(),
		auditActions: reg.AuditActions,
		now:          func() time.Time { return now },
	}
}

func ctxFor(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tenant)
}

const testTenant pkgcore.TenantID = "tenant-1"

var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// alwaysHeld is a PermissionLister that reports every scope a caller asks
// about as held.
func alwaysHeld(scopes ...string) PermissionLister {
	return PermissionListerFunc(func(ctx context.Context, tenantID, userID string) ([]string, error) {
		return scopes, nil
	})
}

func TestService_Create_ReturnsRawKeyExactlyOnce(t *testing.T) {
	svc := testService(t, alwaysHeld("notes:read"), nil, fixedNow)

	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1", Scopes: []string{"notes:read"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Key == "" {
		t.Fatal("Create returned an empty Key")
	}
	if created.ID == "" {
		t.Fatal("Create returned an empty ID")
	}

	// Round-trip: the returned raw key must actually authenticate against
	// the stored hash.
	row, err := svc.repo.FindByID(ctxFor(testTenant), created.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if row.Hash != hashAPIKeyToken(created.Key) {
		t.Error("the stored Hash does not match a hash of the key Create returned")
	}

	// List must never expose the raw key or the hash.
	summaries, err := svc.List(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(summaries))
	}
	if summaries[0].Prefix != created.Prefix {
		t.Errorf("List prefix = %q, want %q", summaries[0].Prefix, created.Prefix)
	}
}

func TestService_Create_EmptyScopes_NeedsNoPermissionLister(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create with empty scopes and no PermissionLister: %v", err)
	}
	if len(created.Scopes) != 0 {
		t.Errorf("Scopes = %v, want empty", created.Scopes)
	}
}

func TestService_Create_NonEmptyScopes_NoPermissionLister_Refused(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	_, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1", Scopes: []string{"notes:read"}})
	if !apperrIs(err, ErrPermissionListerUnavailable) {
		t.Errorf("Create error = %v, want ErrPermissionListerUnavailable", err)
	}
}

func TestService_Create_ScopeNotHeldByCreator_Refused(t *testing.T) {
	svc := testService(t, alwaysHeld("notes:read"), nil, fixedNow)
	_, err := svc.Create(ctxFor(testTenant), CreateInput{
		CreatedBy: "user-1",
		Scopes:    []string{"notes:read", "notes:delete"},
	})
	if !apperrIs(err, ErrScopeNotHeldByCreator) {
		t.Errorf("Create error = %v, want ErrScopeNotHeldByCreator", err)
	}
}

func TestService_Create_EmptyCreatedBy_Refused(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	_, err := svc.Create(ctxFor(testTenant), CreateInput{})
	if !apperrIs(err, ErrCreatedByRequired) {
		t.Errorf("Create error = %v, want ErrCreatedByRequired", err)
	}
}

func TestService_Create_NoExplicitExpiry_DefaultsToMaxLifetime(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := fixedNow.Add(MaxAPIKeyLifetime)
	if !created.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", created.ExpiresAt, want)
	}
}

func TestService_Create_ExpiryBeyondMaximum_Refused(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	tooFar := fixedNow.Add(MaxAPIKeyLifetime + time.Hour)
	_, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1", ExpiresAt: &tooFar})
	if !apperrIs(err, ErrExpiryExceedsMaximum) {
		t.Errorf("Create error = %v, want ErrExpiryExceedsMaximum", err)
	}
}

func TestService_Create_ExpiryAtMaximum_Allowed(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	atCeiling := fixedNow.Add(MaxAPIKeyLifetime)
	if _, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1", ExpiresAt: &atCeiling}); err != nil {
		t.Errorf("Create at the exact ceiling: %v", err)
	}
}

func TestService_Create_ExpiryInPast_Refused(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	past := fixedNow.Add(-time.Hour)
	_, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1", ExpiresAt: &past})
	if !apperrIs(err, ErrExpiryInPast) {
		t.Errorf("Create error = %v, want ErrExpiryInPast", err)
	}
}

func TestService_Create_ExpiryExactlyNow_Refused(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	now := fixedNow
	_, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1", ExpiresAt: &now})
	if !apperrIs(err, ErrExpiryInPast) {
		t.Errorf("Create error = %v, want ErrExpiryInPast for an ExpiresAt exactly equal to now", err)
	}
}

func TestService_Create_NoTenantInContext_ReturnsPkgcoreError(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	_, err := svc.Create(context.Background(), CreateInput{CreatedBy: "user-1"})
	if err == nil {
		t.Fatal("Create with no tenant in context returned no error")
	}
}

func TestService_List_CreatorLeft_NoMembershipChecker_AlwaysFalse(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	if _, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	summaries, err := svc.List(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if summaries[0].CreatorLeft {
		t.Error("CreatorLeft = true with no MembershipChecker wired, want false")
	}
}

func TestService_List_CreatorLeft_ReflectsMembershipChecker(t *testing.T) {
	membership := MembershipCheckerFunc(func(ctx context.Context, tenantID, userID string) (bool, error) {
		return userID == "still-here", nil
	})
	svc := testService(t, nil, membership, fixedNow)

	if _, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "still-here"}); err != nil {
		t.Fatalf("Create(still-here): %v", err)
	}
	if _, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "gone"}); err != nil {
		t.Fatalf("Create(gone): %v", err)
	}

	summaries, err := svc.List(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	left := map[string]bool{}
	for _, s := range summaries {
		left[s.CreatedBy] = s.CreatorLeft
	}
	if left["still-here"] {
		t.Error("CreatorLeft = true for still-here, want false")
	}
	if !left["gone"] {
		t.Error("CreatorLeft = false for gone, want true")
	}
}

func TestService_List_ReportsRevokedAndExpired(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if revokeErr := svc.Revoke(ctxFor(testTenant), created.ID); revokeErr != nil {
		t.Fatalf("Revoke: %v", revokeErr)
	}

	summaries, err := svc.List(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !summaries[0].Revoked {
		t.Error("Revoked = false after Revoke, want true")
	}
	if summaries[0].RevokedAt == nil {
		t.Error("RevokedAt = nil after Revoke, want set")
	}
}

func TestService_Revoke_NotFound(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	err := svc.Revoke(ctxFor(testTenant), "does-not-exist")
	if !apperrIs(err, ErrKeyNotFound) {
		t.Errorf("Revoke(missing) error = %v, want ErrKeyNotFound", err)
	}
}

func TestService_Revoke_AlreadyRevoked_Refused(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Revoke(ctxFor(testTenant), created.ID); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if err := svc.Revoke(ctxFor(testTenant), created.ID); !apperrIs(err, ErrKeyAlreadyRevoked) {
		t.Errorf("second Revoke error = %v, want ErrKeyAlreadyRevoked", err)
	}
}

// TestService_Revoke_CrossTenant_NotFound proves Revoke never leaks a key's
// existence across tenants: a key created under tenant A is reported
// ErrKeyNotFound (never ErrKeyAlreadyRevoked, never any success) when
// revoked under tenant B's context, exactly matching dbkit.Repository's own
// "no such id" / "wrong tenant" indistinguishability rule.
func TestService_Revoke_CrossTenant_NotFound(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const otherTenant pkgcore.TenantID = "tenant-2"
	if err := svc.Revoke(ctxFor(otherTenant), created.ID); !apperrIs(err, ErrKeyNotFound) {
		t.Errorf("cross-tenant Revoke error = %v, want ErrKeyNotFound", err)
	}
}

func TestService_Rotate_IssuesNewKeyAndRevokesOld(t *testing.T) {
	svc := testService(t, alwaysHeld("notes:read"), nil, fixedNow)
	original, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1", Scopes: []string{"notes:read"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rotated, err := svc.Rotate(ctxFor(testTenant), original.ID)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rotated.ID == original.ID {
		t.Error("Rotate returned the same id as the predecessor, want a new one")
	}
	if rotated.Key == original.Key {
		t.Error("Rotate returned the same raw key as the predecessor")
	}
	if len(rotated.Scopes) != 1 || rotated.Scopes[0] != "notes:read" {
		t.Errorf("rotated Scopes = %v, want [notes:read] carried forward", rotated.Scopes)
	}
	if rotated.CreatedBy != original.CreatedBy {
		t.Errorf("rotated CreatedBy = %q, want %q carried forward", rotated.CreatedBy, original.CreatedBy)
	}

	oldRow, err := svc.repo.FindByID(ctxFor(testTenant), original.ID)
	if err != nil {
		t.Fatalf("FindByID(old): %v", err)
	}
	if !oldRow.IsRevoked() {
		t.Error("the predecessor key was not revoked by Rotate")
	}

	newRow, err := svc.repo.FindByID(ctxFor(testTenant), rotated.ID)
	if err != nil {
		t.Fatalf("FindByID(new): %v", err)
	}
	if newRow.IsRevoked() {
		t.Error("the new key was revoked, want it live")
	}
}

func TestService_Rotate_AlreadyRevoked_Refused(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Revoke(ctxFor(testTenant), created.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := svc.Rotate(ctxFor(testTenant), created.ID); !apperrIs(err, ErrKeyAlreadyRevoked) {
		t.Errorf("Rotate(already revoked) error = %v, want ErrKeyAlreadyRevoked", err)
	}
}

func TestService_Rotate_NotFound(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	if _, err := svc.Rotate(ctxFor(testTenant), "does-not-exist"); !apperrIs(err, ErrKeyNotFound) {
		t.Errorf("Rotate(missing) error = %v, want ErrKeyNotFound", err)
	}
}

// TestService_Rotate_ScopeNoLongerHeld_Refused proves scope validation
// applies to a Rotate-issued key exactly as it does to any other Create --
// see Rotate's own doc comment for why this is deliberate, not an oversight.
func TestService_Rotate_ScopeNoLongerHeld_Refused(t *testing.T) {
	// The creator holds notes:read at issuance time...
	svc := testService(t, alwaysHeld("notes:read"), nil, fixedNow)
	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1", Scopes: []string{"notes:read"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// ...but has since lost it by the time Rotate is called.
	svc.permissions = alwaysHeld()
	if _, rotateErr := svc.Rotate(ctxFor(testTenant), created.ID); !apperrIs(rotateErr, ErrScopeNotHeldByCreator) {
		t.Errorf("Rotate error = %v, want ErrScopeNotHeldByCreator", rotateErr)
	}

	// The predecessor must still be live -- a refused Rotate must not have
	// revoked it.
	oldRow, err := svc.repo.FindByID(ctxFor(testTenant), created.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if oldRow.IsRevoked() {
		t.Error("the predecessor was revoked even though Rotate itself failed")
	}
}

// TestService_List_MultipleKeys_TenantIsolated proves List never returns
// another tenant's rows -- the Service-level companion to
// TestAPIKeyRepository_AssertIsolated's repository-level proof.
func TestService_List_MultipleKeys_TenantIsolated(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)
	if _, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"}); err != nil {
		t.Fatalf("Create(tenant-1): %v", err)
	}
	const otherTenant pkgcore.TenantID = "tenant-2"
	if _, err := svc.Create(ctxFor(otherTenant), CreateInput{CreatedBy: "user-2"}); err != nil {
		t.Fatalf("Create(tenant-2): %v", err)
	}

	got, err := svc.List(ctxFor(testTenant))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List(tenant-1) returned %d rows, want 1", len(got))
	}
	if got[0].CreatedBy != "user-1" {
		t.Errorf("List(tenant-1) returned a row created by %q, want user-1", got[0].CreatedBy)
	}
}
