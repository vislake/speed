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

// TestImpersonationMiddleware_ValidGrant_SubstitutesIdentity is properties
// (b) and (d): the target user's identity is what downstream reads, and
// both Actor (target) and OnBehalfOf (admin) are set for audit capture.
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

	adminPrincipal := authn.Principal{UserID: "admin-1", TenantID: "system", SessionID: "sess-1"}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	req.Header.Set(ImpersonationHeader, "grant-1")
	req = req.WithContext(authn.WithPrincipal(req.Context(), adminPrincipal))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !gotPrincipalOK || gotPrincipal.UserID != "user-1" || gotPrincipal.TenantID != "tenant-1" {
		t.Fatalf("Principal = %+v, ok=%v, want the TARGET user's substituted identity", gotPrincipal, gotPrincipalOK)
	}
	// Property (a): the session id (proof of which real credential
	// verified) is carried over unchanged -- this is still the
	// administrator's own verified session, never a forged one.
	if gotPrincipal.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q, want the admin's own real session id sess-1 (property (a))", gotPrincipal.SessionID)
	}
	if !gotActorOK || gotActor.Type != pkgcore.ActorTypeUser || gotActor.ID != "user-1" {
		t.Fatalf("Actor = %+v, ok=%v, want {user, user-1}", gotActor, gotActorOK)
	}
	if !gotOnBehalfOfOK || gotOnBehalfOf.Type != pkgcore.ActorTypePlatformAdmin || gotOnBehalfOf.ID != "admin-1" {
		t.Fatalf("OnBehalfOf = %+v, ok=%v, want {platform_admin, admin-1}", gotOnBehalfOf, gotOnBehalfOfOK)
	}
}

// TestImpersonationMiddleware_ExpiredGrant_FallsBackToAdmin is property
// (c): an expired grant id must NEVER impersonate -- it must fall back to
// the administrator's own real identity, not fail the request either.
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

// TestImpersonationMiddleware_UnknownGrantID_FallsBackToAdmin is property
// (c) again, for a grant id that never existed at all.
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

// TestImpersonationMiddleware_NoVerifiedPrincipal_PassesThrough is
// property (a) from the other direction: with no verified admin identity
// at all, the header is simply ignored -- there is no administrator to
// substitute FOR.
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
