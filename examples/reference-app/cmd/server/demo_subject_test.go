package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
)

// requestInTenant returns a request whose context carries tenantID the way
// tenancy.Middleware leaves it after resolving the tenant from the caller's
// verified access token (see server.go's middleware-chain doc comment on
// authn.NewPrincipalResolver).
func requestInTenant(method string, tenantID pkgcore.TenantID) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/notes", nil)
	if tenantID == "" {
		return r
	}
	return r.WithContext(pkgcore.WithTenant(r.Context(), tenantID))
}

// TestDemoSubjectResolver_TakesTheTenantFromTheContextNotTheRequest is the
// isolation property of the seam. The user id is a placeholder read from a
// header; the TENANT never is, under any header a caller can set.
func TestDemoSubjectResolver_TakesTheTenantFromTheContextNotTheRequest(t *testing.T) {
	r := requestInTenant(http.MethodGet, "tenant-acme")
	r.Header.Set(demoUserHeader, demoOwnerUserID)
	// Everything a caller might try in order to steer the tenant.
	r.Header.Set("X-Tenant-ID", "tenant-globex")
	r.Header.Set("Tenant-ID", "tenant-globex")

	sub, ok := demoSubjectResolver(r)
	if !ok {
		t.Fatal("the resolver reported no subject for a request with both a resolved tenant and a user")
	}
	if sub.TenantID != "tenant-acme" {
		t.Fatalf("subject tenant = %q, want %q -- a client-supplied header must never steer the tenant", sub.TenantID, "tenant-acme")
	}
	if sub.UserID != demoOwnerUserID {
		t.Fatalf("subject user = %q, want %q", sub.UserID, demoOwnerUserID)
	}
}

// TestDemoSubjectResolver_FailsClosed pins every incomplete case as "no
// subject", which rbac's gate turns into a 403.
func TestDemoSubjectResolver_FailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		tenant pkgcore.TenantID
		user   string
	}{
		{name: "no tenant resolved", user: demoOwnerUserID},
		{name: "no user header", tenant: "tenant-acme"},
		{name: "neither"},
		{name: "empty user header", tenant: "tenant-acme", user: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := requestInTenant(http.MethodGet, tc.tenant)
			if tc.user != "" {
				r.Header.Set(demoUserHeader, tc.user)
			}
			if _, ok := demoSubjectResolver(r); ok {
				t.Fatal("the resolver produced a subject; every incomplete identity must report none")
			}
		})
	}
}

// TestDemoSubjectResolver_HeaderPrecedenceAndPrincipalFallback pins the
// two-source semantics demoUserHeader's own comment describes. The demo
// header, when present, still names the acting user exactly as it always
// did -- the pre-auth flows send both a token and a header, and the header
// is what they mean, so it must keep winning; only without a header does a
// verified Principal in the request context supply the user, which is the
// path the real accounts demo_users.go seeds take.
func TestDemoSubjectResolver_HeaderPrecedenceAndPrincipalFallback(t *testing.T) {
	principal := authn.Principal{UserID: "real-seeded-user", TenantID: "tenant-acme", SessionID: "session-1"}

	t.Run("the demo header wins when both are present", func(t *testing.T) {
		r := requestInTenant(http.MethodGet, "tenant-acme")
		r = r.WithContext(authn.WithPrincipal(r.Context(), principal))
		r.Header.Set(demoUserHeader, demoReaderUserID)

		sub, ok := demoSubjectResolver(r)
		if !ok {
			t.Fatal("no subject for a request with a tenant, a verified principal and a demo header")
		}
		if sub.UserID != demoReaderUserID {
			t.Fatalf("subject user = %q, want the header's %q -- the header names who acts", sub.UserID, demoReaderUserID)
		}
	})

	t.Run("without a header the verified principal supplies the user", func(t *testing.T) {
		r := requestInTenant(http.MethodGet, "tenant-acme")
		r = r.WithContext(authn.WithPrincipal(r.Context(), principal))

		sub, ok := demoSubjectResolver(r)
		if !ok {
			t.Fatal("no subject for a request with a tenant and a verified principal")
		}
		if sub.UserID != principal.UserID {
			t.Fatalf("subject user = %q, want the principal's %q", sub.UserID, principal.UserID)
		}
		if sub.TenantID != "tenant-acme" {
			t.Fatalf("subject tenant = %q, want %q", sub.TenantID, "tenant-acme")
		}
	})

	t.Run("the principal's own tenant claim never steers the subject", func(t *testing.T) {
		// The tenant comes from the request context, where tenancy.Middleware
		// put it -- never from the Principal, which is what the resolver
		// consumes user identity from only.
		r := requestInTenant(http.MethodGet, "tenant-acme")
		r = r.WithContext(authn.WithPrincipal(r.Context(), authn.Principal{
			UserID: "real-seeded-user", TenantID: "tenant-globex", SessionID: "session-1",
		}))

		sub, ok := demoSubjectResolver(r)
		if !ok {
			t.Fatal("no subject for a request with a tenant and a verified principal")
		}
		if sub.TenantID != "tenant-acme" {
			t.Fatalf("subject tenant = %q, want the context's %q -- never the principal's claim", sub.TenantID, "tenant-acme")
		}
	})
}

// TestDemoPermissionFor_DependsOnlyOnTheMethod pins the read/write split
// and, more importantly, its strict direction: a method this app never
// considered demands the WRITE permission, not the read one.
func TestDemoPermissionFor_DependsOnlyOnTheMethod(t *testing.T) {
	permissionFor := demoPermissionFor(notesResource)
	tests := []struct {
		method string
		want   string
	}{
		{method: http.MethodGet, want: notes.PermissionRead},
		{method: http.MethodHead, want: notes.PermissionRead},
		{method: http.MethodPost, want: notes.PermissionWrite},
		{method: http.MethodPut, want: notes.PermissionWrite},
		{method: http.MethodPatch, want: notes.PermissionWrite},
		{method: http.MethodDelete, want: notes.PermissionWrite},
		{method: http.MethodOptions, want: notes.PermissionWrite},
	}
	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			if got := permissionFor(requestInTenant(tc.method, "tenant-acme")); got != tc.want {
				t.Fatalf("demoPermissionFor(%s) = %q, want %q", tc.method, got, tc.want)
			}
		})
	}
}

// TestGuardModuleRoute_UnknownPath_RefusesToMount is the fail-closed
// default that makes demoRouteGuards worth having. A module that starts
// mounting a new route must break the build here, loudly and by path,
// rather than have that route served with no permission check.
func TestGuardModuleRoute_UnknownPath_RefusesToMount(t *testing.T) {
	const unlisted = "/api/v1/invoices"

	handler, err := guardModuleRoute(nil, unlisted, http.NotFoundHandler())
	if err == nil {
		t.Fatal("an unlisted mounted path was accepted; it must fail the server build")
	}
	if handler != nil {
		t.Fatal("a handler was returned alongside the error")
	}
	if !strings.Contains(err.Error(), unlisted) {
		t.Fatalf("the error does not name the offending path: %v", err)
	}
}

// TestGuardModuleRoute_PublicPath_IsNotGated protects config's two
// pre-auth endpoints from acquiring a permission check by accident. They
// must serve with no subject at all -- a login page's brand has to render
// before anyone has signed in.
func TestGuardModuleRoute_PublicPath_IsNotGated(t *testing.T) {
	for _, path := range []string{config.PathPublic, config.PathSystemFeatures} {
		t.Run(path, func(t *testing.T) {
			served := false
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served = true
				w.WriteHeader(http.StatusOK)
			})

			// A nil Authorizer would make any gated route fail with
			// rbac.service_not_attached, so reaching the handler proves no
			// gate was applied rather than that a gate happened to allow.
			handler, err := guardModuleRoute(nil, path, inner)
			if err != nil {
				t.Fatalf("guardModuleRoute(%q): %v", path, err)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if !served || rec.Code != http.StatusOK {
				t.Fatalf("served %v status %d, want the handler reached with %d", served, rec.Code, http.StatusOK)
			}
		})
	}
}

// TestNotesResource_MatchesTheModulesOwnPermissions pins the derivation
// that keeps this example from drifting: the resource this app gates on is
// computed from notes' exported permission constants, so renaming one
// breaks here rather than silently gating on a permission the module no
// longer declares.
func TestNotesResource_MatchesTheModulesOwnPermissions(t *testing.T) {
	if got := rbac.Permission(notesResource, demoActionRead); got != notes.PermissionRead {
		t.Fatalf("read permission = %q, want %q", got, notes.PermissionRead)
	}
	if got := rbac.Permission(notesResource, demoActionWrite); got != notes.PermissionWrite {
		t.Fatalf("write permission = %q, want %q", got, notes.PermissionWrite)
	}
}

func TestSplitDemoPermission(t *testing.T) {
	tests := []struct {
		permission string
		resource   string
		action     string
		ok         bool
	}{
		{permission: "notes:read", resource: "notes", action: "read", ok: true},
		{permission: "notes", ok: false},
		{permission: "notes:", ok: false},
		{permission: ":read", ok: false},
		{permission: "", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.permission, func(t *testing.T) {
			resource, action, ok := splitDemoPermission(tc.permission)
			if ok != tc.ok {
				t.Fatalf("splitDemoPermission(%q) ok = %v, want %v", tc.permission, ok, tc.ok)
			}
			if ok && (resource != tc.resource || action != tc.action) {
				t.Fatalf("splitDemoPermission(%q) = (%q, %q), want (%q, %q)",
					tc.permission, resource, action, tc.resource, tc.action)
			}
		})
	}
}

// TestMustResourceOf_DisagreeingPermissions_Panics pins the startup guard.
// The panic is the point: it fires at package initialization, long before
// a request exists, and what it reports is that the constants this file
// gates on are not the ones it believes they are.
func TestMustResourceOf_DisagreeingPermissions_Panics(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions []string
	}{
		{name: "two resources", permissions: []string{"notes:read", "billing:manage"}},
		{name: "malformed", permissions: []string{"notes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("mustResourceOf(%v) returned instead of panicking", tc.permissions)
				}
			}()
			_ = mustResourceOf(tc.permissions...)
		})
	}
}
