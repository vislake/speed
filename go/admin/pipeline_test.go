package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeGrantLookup is a GrantLookup test double: a plain map from grant id
// to grant, standing in for a real *ImpersonationService so these tests
// exercise the middleware's own logic in isolation.
type fakeGrantLookup map[string]ImpersonationGrant

func (f fakeGrantLookup) Lookup(_ context.Context, id string) (*ImpersonationGrant, bool) {
	g, ok := f[id]
	if !ok {
		return nil, false
	}
	if !g.Active(time.Now()) {
		return nil, false
	}
	return &g, true
}

// echoHandler is the terminal handler every pipeline test wraps: it reports
// back exactly what the request context carries, so the test can assert on
// the identity substitution the middleware performed (or didn't).
func echoHandler(gotPrincipal *authn.Principal, gotPrincipalOK *bool, gotActor *pkgcore.Actor, gotActorOK *bool, gotOnBehalfOf *pkgcore.Actor, gotOnBehalfOfOK *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPrincipal, *gotPrincipalOK = authn.PrincipalFromContext(r.Context())
		*gotActor, *gotActorOK = pkgcore.ActorFromContext(r.Context())
		*gotOnBehalfOf, *gotOnBehalfOfOK = pkgcore.OnBehalfOfFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func TestImpersonationMiddleware_NoHeader_PassesThroughUnmodified(t *testing.T) {
	var gotPrincipal authn.Principal
	var gotPrincipalOK bool
	var gotActor, gotOnBehalfOf pkgcore.Actor
	var gotActorOK, gotOnBehalfOfOK bool

	handler := ImpersonationMiddleware(fakeGrantLookup{})(
		echoHandler(&gotPrincipal, &gotPrincipalOK, &gotActor, &gotActorOK, &gotOnBehalfOf, &gotOnBehalfOfOK),
	)

	adminPrincipal := authn.Principal{UserID: "admin-1", TenantID: "system", SessionID: "sess-1"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	req = req.WithContext(authn.WithPrincipal(req.Context(), adminPrincipal))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !gotPrincipalOK || gotPrincipal.UserID != "admin-1" {
		t.Fatalf("Principal = %+v, ok=%v, want the admin's own untouched principal", gotPrincipal, gotPrincipalOK)
	}
	if gotOnBehalfOfOK {
		t.Fatalf("OnBehalfOf present (%+v) with no impersonation header at all", gotOnBehalfOf)
	}
}

// TestImpersonationMiddleware_ValidGrant_SubstitutesIdentity pins the
// substitution: the target user's identity is what downstream reads, and
// both Actor (target) and OnBehalfOf (admin) are set for audit capture.
//
// It also pins the boundaries the substituted principal must NOT cross:
// no inherited SessionID and no inherited AMR -- authn's session lifecycle
// keys on a session id alone, so an inherited one would let an
// impersonated request sign the administrator out of their own console
// session, and an inherited AMR would let the administrator's own second
// factor satisfy an MFA gate as the target. The impersonation's own
// session identity is the grant (SessionID = grant id), and no
// authentication method was ever established for the target (AMR empty).
func TestImpersonationMiddleware_ValidGrant_SubstitutesIdentity(t *testing.T) {
	var gotPrincipal authn.Principal
	var gotPrincipalOK bool
	var gotActor, gotOnBehalfOf pkgcore.Actor
	var gotActorOK, gotOnBehalfOfOK bool

	lookup := fakeGrantLookup{
		"grant-1": {ID: "grant-1", AdminUserID: "admin-1", TargetUserID: "user-1", TargetTenantID: "tenant-1", ExpiresAt: time.Now().Add(time.Hour)},
	}
	handler := ImpersonationMiddleware(lookup)(
		echoHandler(&gotPrincipal, &gotPrincipalOK, &gotActor, &gotActorOK, &gotOnBehalfOf, &gotOnBehalfOfOK),
	)

	// The administrator's own session is TOTP-elevated -- precisely the
	// posture that must never cross the impersonation boundary.
	adminPrincipal := authn.Principal{
		UserID:    "admin-1",
		TenantID:  "system",
		SessionID: "sess-1",
		AMR:       []string{"password", "mfa:totp"},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	req.Header.Set(ImpersonationHeader, "grant-1")
	req = req.WithContext(authn.WithPrincipal(req.Context(), adminPrincipal))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !gotPrincipalOK || gotPrincipal.UserID != "user-1" || gotPrincipal.TenantID != "tenant-1" {
		t.Fatalf("Principal = %+v, ok=%v, want the TARGET user's substituted identity", gotPrincipal, gotPrincipalOK)
	}
	// The session identity during impersonation is the GRANT's, never the
	// administrator's real session id -- a session-scoped operation as the
	// target must pair (target user, grant), not (target user, admin
	// session), and authn's logout must never be able to revoke the
	// administrator's own session through this principal.
	if gotPrincipal.SessionID != "grant-1" {
		t.Fatalf("SessionID = %q, want the grant's own id grant-1 (the impersonated session), never the admin's sess-1", gotPrincipal.SessionID)
	}
	// No administrator authentication method survives the substitution --
	// an empty AMR cannot satisfy any "requires a second factor" policy,
	// so an admin with TOTP cannot cross an MFA gate as the target.
	if len(gotPrincipal.AMR) != 0 {
		t.Fatalf("AMR = %v, want empty (the target proved no authentication method; the admin's %v must not cross over)", gotPrincipal.AMR, adminPrincipal.AMR)
	}
	if !gotActorOK || gotActor.Type != pkgcore.ActorTypeUser || gotActor.ID != "user-1" {
		t.Fatalf("Actor = %+v, ok=%v, want {user, user-1}", gotActor, gotActorOK)
	}
	if !gotOnBehalfOfOK || gotOnBehalfOf.Type != pkgcore.ActorTypePlatformAdmin || gotOnBehalfOf.ID != "admin-1" {
		t.Fatalf("OnBehalfOf = %+v, ok=%v, want {platform_admin, admin-1}", gotOnBehalfOf, gotOnBehalfOfOK)
	}
}

// TestImpersonationMiddleware_LogoutUnderImpersonation_DoesNotKillAdminsSession
// pins the grant-as-session boundary at the real-authn edge: the
// administrator holds a REAL session row (authn's own SessionRepository),
// and a logout issued from inside the impersonated context -- authn's
// Service.Logout, the very call its logout handler makes with the calling
// principal's SessionID -- must not revoke that row. Logout keys on the
// session id alone, so a substituted SessionID naming the grant instead of
// the administrator's real session id is what keeps the administrator's
// own row active.
func TestImpersonationMiddleware_LogoutUnderImpersonation_DoesNotKillAdminsSession(t *testing.T) {
	env := buildTestAdminModule(t)

	const adminSession = "sess-admin-logout-1"
	sessionRepo, err := authn.NewSessionRepository(env.DB)
	if err != nil {
		t.Fatalf("NewSessionRepository() error = %v", err)
	}
	ctx := context.Background()
	if err = sessionRepo.Create(ctx, &authn.Session{
		ID:              adminSession,
		UserID:          "admin-logout-1",
		CurrentTenantID: "tenant-admin-console",
		ExpiresAt:       time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create admin session fixture: %v", err)
	}

	lookup := fakeGrantLookup{
		"grant-logout-1": {ID: "grant-logout-1", AdminUserID: "admin-logout-1", TargetUserID: "user-target-1", TargetTenantID: "tenant-target-1", ExpiresAt: time.Now().Add(time.Hour)},
	}
	var gotPrincipal authn.Principal
	var gotPrincipalOK bool
	var gotActor, gotOnBehalfOf pkgcore.Actor
	var gotActorOK, gotOnBehalfOfOK bool
	handler := ImpersonationMiddleware(lookup)(
		echoHandler(&gotPrincipal, &gotPrincipalOK, &gotActor, &gotActorOK, &gotOnBehalfOf, &gotOnBehalfOfOK),
	)

	adminPrincipal := authn.Principal{UserID: "admin-logout-1", TenantID: "system", SessionID: adminSession}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/logout", nil)
	req.Header.Set(ImpersonationHeader, "grant-logout-1")
	req = req.WithContext(authn.WithPrincipal(req.Context(), adminPrincipal))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !gotPrincipalOK {
		t.Fatal("no principal reached the downstream handler")
	}

	// The logout the authn handler would perform for this request's
	// principal -- the impersonated principal's SessionID must never be
	// the admin's real session row, which this call would revoke.
	if err = env.Authn.Service().Logout(context.Background(), gotPrincipal.SessionID); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}

	row, err := sessionRepo.FindByID(context.Background(), adminSession)
	if err != nil {
		t.Fatalf("FindByID(admin session) error = %v", err)
	}
	if row.Status != authn.SessionStatusActive {
		t.Fatalf("admin session status after an impersonated logout = %q, want %q -- the impersonated request must not kill the administrator's own session",
			row.Status, authn.SessionStatusActive)
	}
}

// TestImpersonationMiddleware_ExpiredGrant_FallsBackToAdmin pins the
// fail-closed rule: an expired grant id must NEVER impersonate -- it must
// fall back to the administrator's own real identity, not fail the
// request either.
func TestImpersonationMiddleware_ExpiredGrant_FallsBackToAdmin(t *testing.T) {
	var gotPrincipal authn.Principal
	var gotPrincipalOK bool
	var gotActor, gotOnBehalfOf pkgcore.Actor
	var gotActorOK, gotOnBehalfOfOK bool

	lookup := fakeGrantLookup{
		"grant-expired": {ID: "grant-expired", AdminUserID: "admin-1", TargetUserID: "user-1", TargetTenantID: "tenant-1", ExpiresAt: time.Now().Add(-time.Hour)},
	}
	handler := ImpersonationMiddleware(lookup)(
		echoHandler(&gotPrincipal, &gotPrincipalOK, &gotActor, &gotActorOK, &gotOnBehalfOf, &gotOnBehalfOfOK),
	)

	adminPrincipal := authn.Principal{UserID: "admin-1", TenantID: "system"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	req.Header.Set(ImpersonationHeader, "grant-expired")
	req = req.WithContext(authn.WithPrincipal(req.Context(), adminPrincipal))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the request must proceed, not fail)", rec.Code)
	}
	if !gotPrincipalOK || gotPrincipal.UserID != "admin-1" {
		t.Fatalf("Principal = %+v, ok=%v, want the ADMIN's own identity (expired grant must not impersonate)", gotPrincipal, gotPrincipalOK)
	}
	if gotOnBehalfOfOK {
		t.Fatalf("OnBehalfOf present (%+v) even though the grant was expired -- must not fall open", gotOnBehalfOf)
	}
}

// TestImpersonationMiddleware_UnknownGrantID_FallsBackToAdmin pins the
// same fail-closed rule for a grant id that never existed at all.
func TestImpersonationMiddleware_UnknownGrantID_FallsBackToAdmin(t *testing.T) {
	var gotPrincipal authn.Principal
	var gotPrincipalOK bool
	var gotActor, gotOnBehalfOf pkgcore.Actor
	var gotActorOK, gotOnBehalfOfOK bool

	handler := ImpersonationMiddleware(fakeGrantLookup{})(
		echoHandler(&gotPrincipal, &gotPrincipalOK, &gotActor, &gotActorOK, &gotOnBehalfOf, &gotOnBehalfOfOK),
	)

	adminPrincipal := authn.Principal{UserID: "admin-1", TenantID: "system"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	req.Header.Set(ImpersonationHeader, "grant-never-existed")
	req = req.WithContext(authn.WithPrincipal(req.Context(), adminPrincipal))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !gotPrincipalOK || gotPrincipal.UserID != "admin-1" {
		t.Fatalf("Principal = %+v, ok=%v, want the admin's own identity", gotPrincipal, gotPrincipalOK)
	}
}

// TestImpersonationMiddleware_GrantBelongsToDifferentAdmin_FallsBack is the
// conservative tightening beyond the document's own text (see
// ImpersonationMiddleware's doc comment): a grant started by one
// administrator must not be usable by a different administrator's own
// verified token, even if the grant is otherwise still active.
func TestImpersonationMiddleware_GrantBelongsToDifferentAdmin_FallsBack(t *testing.T) {
	var gotPrincipal authn.Principal
	var gotPrincipalOK bool
	var gotActor, gotOnBehalfOf pkgcore.Actor
	var gotActorOK, gotOnBehalfOfOK bool

	lookup := fakeGrantLookup{
		"grant-1": {ID: "grant-1", AdminUserID: "admin-OTHER", TargetUserID: "user-1", TargetTenantID: "tenant-1", ExpiresAt: time.Now().Add(time.Hour)},
	}
	handler := ImpersonationMiddleware(lookup)(
		echoHandler(&gotPrincipal, &gotPrincipalOK, &gotActor, &gotActorOK, &gotOnBehalfOf, &gotOnBehalfOfOK),
	)

	adminPrincipal := authn.Principal{UserID: "admin-1", TenantID: "system"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	req.Header.Set(ImpersonationHeader, "grant-1")
	req = req.WithContext(authn.WithPrincipal(req.Context(), adminPrincipal))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !gotPrincipalOK || gotPrincipal.UserID != "admin-1" {
		t.Fatalf("Principal = %+v, ok=%v, want admin-1's own identity, never user-1's", gotPrincipal, gotPrincipalOK)
	}
}

// TestImpersonationMiddleware_NoVerifiedPrincipal_PassesThrough pins the
// unchanged-credential rule from the other direction: with no verified
// admin identity at all, the header is simply ignored -- there is no
// administrator to substitute FOR.
func TestImpersonationMiddleware_NoVerifiedPrincipal_PassesThrough(t *testing.T) {
	var gotPrincipal authn.Principal
	var gotPrincipalOK bool
	var gotActor, gotOnBehalfOf pkgcore.Actor
	var gotActorOK, gotOnBehalfOfOK bool

	lookup := fakeGrantLookup{
		"grant-1": {ID: "grant-1", AdminUserID: "admin-1", TargetUserID: "user-1", TargetTenantID: "tenant-1", ExpiresAt: time.Now().Add(time.Hour)},
	}
	handler := ImpersonationMiddleware(lookup)(
		echoHandler(&gotPrincipal, &gotPrincipalOK, &gotActor, &gotActorOK, &gotOnBehalfOf, &gotOnBehalfOfOK),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	req.Header.Set(ImpersonationHeader, "grant-1")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotPrincipalOK {
		t.Fatalf("Principal present (%+v) with no verified admin identity on the request at all", gotPrincipal)
	}
}
