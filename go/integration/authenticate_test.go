package integration

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestService_Authenticate_ValidKey_Success proves the whole happy path:
// Create under one tenant, then Authenticate the raw key with NO tenant in
// ctx at all (context.Background()), and get back the same tenant, key id,
// creator and scopes Create persisted.
func TestService_Authenticate_ValidKey_Success(t *testing.T) {
	svc := testService(t, alwaysHeld("notes:read", "notes:write"), nil, fixedNow)

	created, err := svc.Create(ctxFor(testTenant), CreateInput{
		CreatedBy: "user-1",
		Scopes:    []string{"notes:read"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	authenticated, err := svc.Authenticate(context.Background(), created.Key)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authenticated.KeyID != created.ID {
		t.Errorf("KeyID = %q, want %q", authenticated.KeyID, created.ID)
	}
	if authenticated.TenantID != string(testTenant) {
		t.Errorf("TenantID = %q, want %q", authenticated.TenantID, testTenant)
	}
	if authenticated.CreatedBy != "user-1" {
		t.Errorf("CreatedBy = %q, want %q", authenticated.CreatedBy, "user-1")
	}
	if len(authenticated.Scopes) != 1 || authenticated.Scopes[0] != "notes:read" {
		t.Errorf("Scopes = %v, want [notes:read]", authenticated.Scopes)
	}
}

// TestService_Authenticate_DoesNotRequireTenantInContext is the direct proof
// of Authenticate's whole reason to differ from Create/List/Rotate/Revoke: a
// genuinely anonymous caller (context.Background(), nothing attached at all)
// can still authenticate, because the tenant is resolved from the key
// itself, not read from ctx. A regression that silently made this method
// call pkgcore.MustTenantFromContext would fail this test with
// pkgcore.ErrNoTenant instead of a real answer.
func TestService_Authenticate_DoesNotRequireTenantInContext(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)

	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Deliberately context.Background(), not ctxFor(testTenant) or any other
	// tenant-carrying context.
	authenticated, err := svc.Authenticate(context.Background(), created.Key)
	if err != nil {
		t.Fatalf("Authenticate with no tenant in ctx: %v", err)
	}
	if authenticated.TenantID != string(testTenant) {
		t.Errorf("TenantID = %q, want %q", authenticated.TenantID, testTenant)
	}
}

// TestService_Authenticate_WrongKey_ReportsAuthenticationFailed proves a key
// that was never issued at all is refused with the same sentinel a revoked
// or expired key gets -- errors.go's own no-enumeration contract.
func TestService_Authenticate_WrongKey_ReportsAuthenticationFailed(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)

	if _, err := svc.Authenticate(context.Background(), "sk_this-was-never-issued"); !errors.Is(err, ErrAuthenticationFailed) {
		t.Errorf("Authenticate(unknown key) error = %v, want ErrAuthenticationFailed", err)
	}
}

// TestService_Authenticate_EmptyKey_ReportsAuthenticationFailed proves a
// missing/empty credential is refused identically to a wrong one -- no
// observable "you didn't even try" signal.
func TestService_Authenticate_EmptyKey_ReportsAuthenticationFailed(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)

	if _, err := svc.Authenticate(context.Background(), ""); !errors.Is(err, ErrAuthenticationFailed) {
		t.Errorf("Authenticate(\"\") error = %v, want ErrAuthenticationFailed", err)
	}
}

// TestService_Authenticate_RevokedKey_ReportsAuthenticationFailed proves a
// genuinely-issued, correctly-hashed key that has since been revoked is
// refused with the exact same ErrAuthenticationFailed a wrong key gets --
// this is the property go/integration/AGENTS.md's round-5 section named as
// unverifiable until this round: "a request bearing a rotated-away or
// revoked key is refused".
func TestService_Authenticate_RevokedKey_ReportsAuthenticationFailed(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)

	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Revoke(ctxFor(testTenant), created.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, authErr := svc.Authenticate(context.Background(), created.Key); !errors.Is(authErr, ErrAuthenticationFailed) {
		t.Errorf("Authenticate(revoked key) error = %v, want ErrAuthenticationFailed", authErr)
	}
}

// TestService_Authenticate_RotatedAwayKey_ReportsAuthenticationFailed drives
// the exact rotate-then-old-key-refused shape AGENTS.md's round-5 section
// named as the follow-up work this round discharges: the OLD raw key value
// is refused after Rotate, and the NEW one authenticates successfully.
func TestService_Authenticate_RotatedAwayKey_ReportsAuthenticationFailed(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)

	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rotated, err := svc.Rotate(ctxFor(testTenant), created.ID)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if _, authErr := svc.Authenticate(context.Background(), created.Key); !errors.Is(authErr, ErrAuthenticationFailed) {
		t.Errorf("Authenticate(old, rotated-away key) error = %v, want ErrAuthenticationFailed", authErr)
	}
	authenticated, err := svc.Authenticate(context.Background(), rotated.Key)
	if err != nil {
		t.Fatalf("Authenticate(new, rotated key): %v", err)
	}
	if authenticated.KeyID != rotated.ID {
		t.Errorf("KeyID = %q, want %q", authenticated.KeyID, rotated.ID)
	}
}

// TestService_Authenticate_ExpiredKey_ReportsAuthenticationFailed proves a
// key past its ExpiresAt is refused identically to a wrong one, even though
// its row is genuinely found and its hash genuinely matches.
func TestService_Authenticate_ExpiredKey_ReportsAuthenticationFailed(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)

	almostExpired := fixedNow.Add(time.Minute)
	created, err := svc.Create(ctxFor(testTenant), CreateInput{
		CreatedBy: "user-1",
		ExpiresAt: &almostExpired,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance the service's own clock past the key's ExpiresAt -- svc.now is
	// this package's own test-only override (withClock's underlying field),
	// safe to mutate directly in a white-box test.
	svc.now = func() time.Time { return fixedNow.Add(2 * time.Minute) }

	if _, err := svc.Authenticate(context.Background(), created.Key); !errors.Is(err, ErrAuthenticationFailed) {
		t.Errorf("Authenticate(expired key) error = %v, want ErrAuthenticationFailed", err)
	}
}

// TestService_Authenticate_RecordsLastUsedAt closes the round-1 gap
// APIKey.LastUsedAt's own doc comment named ("Round 1 never writes it"): a
// successful Authenticate leaves a non-nil LastUsedAt on the row, readable
// back through the ordinary tenant-scoped ListModule path.
func TestService_Authenticate_RecordsLastUsedAt(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)

	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	row, err := svc.repo.FindByID(ctxFor(testTenant), created.ID)
	if err != nil {
		t.Fatalf("FindByID before Authenticate: %v", err)
	}
	if row.LastUsedAt != nil {
		t.Fatal("LastUsedAt is already set before any Authenticate call")
	}

	if _, authErr := svc.Authenticate(context.Background(), created.Key); authErr != nil {
		t.Fatalf("Authenticate: %v", authErr)
	}

	row, err = svc.repo.FindByID(ctxFor(testTenant), created.ID)
	if err != nil {
		t.Fatalf("FindByID after Authenticate: %v", err)
	}
	if row.LastUsedAt == nil {
		t.Error("LastUsedAt is still nil after a successful Authenticate")
	}
}

// TestService_Authenticate_RevokedKey_StaleLastUsedWrite_DoesNotRevive is
// the regression test for the revival race in LastUsedAt bookkeeping: a
// concurrent revoke and a successful Authenticate can interleave so that
// Authenticate's recordLastUsed write lands AFTER the revocation committed,
// against a row copy Authenticate read while the key was still live. That
// write must never undo the revocation. The interleaving's losing half is
// exercised deterministically: read the live row exactly as Authenticate
// would, revoke, then perform the recordLastUsed write Authenticate
// performs against its now-stale copy -- the row must still be revoked and
// the key must still refuse to authenticate. Before the fix (a full-row
// Repository[APIKey].Update carrying the stale copy's nil RevokedAt) this
// write revived the key; after it (a targeted last_used_at-only UPDATE) it
// cannot.
func TestService_Authenticate_RevokedKey_StaleLastUsedWrite_DoesNotRevive(t *testing.T) {
	svc := testService(t, nil, nil, fixedNow)

	created, err := svc.Create(ctxFor(testTenant), CreateInput{CreatedBy: "user-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The row Authenticate would have read had it started before the revoke:
	// live, RevokedAt nil, LastUsedAt nil.
	stale, err := svc.repo.FindByID(ctxFor(testTenant), created.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if stale.IsRevoked() {
		t.Fatal("precondition: key must be live before the revoke")
	}

	if revokeErr := svc.Revoke(ctxFor(testTenant), created.ID); revokeErr != nil {
		t.Fatalf("Revoke: %v", revokeErr)
	}

	// Now the write Authenticate's recordLastUsed performs after its own
	// checks pass -- run against the stale, pre-revocation row copy the
	// concurrent Authenticate would still be holding.
	after := fixedNow.Add(time.Minute)
	if writeErr := svc.recordLastUsed(ctxFor(testTenant), stale, after); writeErr != nil {
		t.Fatalf("recordLastUsed: %v", writeErr)
	}

	row, err := svc.repo.FindByID(ctxFor(testTenant), created.ID)
	if err != nil {
		t.Fatalf("FindByID after the stale write: %v", err)
	}
	if !row.IsRevoked() {
		t.Fatal("the stale last-used write revived the revoked key: RevokedAt is nil again")
	}

	if _, authErr := svc.Authenticate(context.Background(), created.Key); !errors.Is(authErr, ErrAuthenticationFailed) {
		t.Errorf("Authenticate(revoked key) error = %v, want ErrAuthenticationFailed", authErr)
	}
}
