package rbac

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// contains is a local shorthand for the message assertions below.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// newRoute builds the mounted route GuardRoutes is applied to, returning
// the recording handler alongside -- a guarded route wraps (or keeps) it as
// its own Handler, so a test must hold the inner reference to observe
// whether a request reached the route's handler at all.
func newRoute(path string) (pkgcore.MountedRoute, *recordingHandler) {
	next := &recordingHandler{}
	return pkgcore.MountedRoute{Path: path, Handler: next}, next
}

// guardedRoute returns the route GuardRoutes produced for path, failing the
// test when the table did not cover it. The returned route's own Access is
// the recorded decision.
func guardedRoute(t *testing.T, guarded []pkgcore.MountedRoute, path string) pkgcore.MountedRoute {
	t.Helper()
	for _, route := range guarded {
		if route.Path == path {
			return route
		}
	}
	t.Fatalf("GuardRoutes returned no route for %q", path)
	return pkgcore.MountedRoute{}
}

// serveGuarded runs r through the guarded route for path.
func serveGuarded(t *testing.T, guarded []pkgcore.MountedRoute, path string, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	guardedRoute(t, guarded, path).Handler.ServeHTTP(rec, r)
	return rec
}

// requestFor builds a request to path carrying sub the way the
// authenticating side installs it.
func requestFor(method, path string, sub Subject) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	return r.WithContext(WithSubject(r.Context(), sub))
}

// demoSubject is the ordinary complete subject every gated-route test
// decides for.
var demoSubject = Subject{TenantID: "tenant-a", UserID: "user-1"}

// TestGuardRoutes_MountedRouteWithNoRule_FailsStartup is the coverage
// contract's failure half: a module route the table does not name must fail
// the assembly, by path, rather than be served with no authorization
// decision.
func TestGuardRoutes_MountedRouteWithNoRule_FailsStartup(t *testing.T) {
	route, _ := newRoute("/api/v1/invoices")
	guarded, err := GuardRoutes(&stubAuthorizer{allow: true}, []pkgcore.MountedRoute{route}, nil)
	if err == nil {
		t.Fatal("a mounted route with no declared decision was accepted")
	}
	if !errors.Is(err, ErrRouteUndecided) {
		t.Fatalf("error = %v, want errors.Is(err, ErrRouteUndecided)", err)
	}
	if guarded != nil {
		t.Fatalf("routes were returned alongside the error: %v", guarded)
	}
	if got := err.Error(); !contains(got, "/api/v1/invoices") {
		t.Fatalf("the error does not name the undecided route: %v", err)
	}
}

// TestGuardRoutes_RuleDeclaringNoDecision_FailsStartup pins the zero-value
// declaration as refused: the empty pkgcore.RouteAccess declares nothing,
// so a table entry built from it is an omission, never an implicit public
// route.
func TestGuardRoutes_RuleDeclaringNoDecision_FailsStartup(t *testing.T) {
	route, _ := newRoute("/api/v1/notes")
	_, err := GuardRoutes(&stubAuthorizer{allow: true},
		[]pkgcore.MountedRoute{route},
		[]RouteRule{{Path: "/api/v1/notes"}})
	if !errors.Is(err, ErrRouteUndecided) {
		t.Fatalf("error = %v, want errors.Is(err, ErrRouteUndecided)", err)
	}
}

// TestGuardRoutes_ContradictoryDecisions_FailStartup pins the two
// self-contradicting declarations: public-and-permission-gated at once, and
// a public route carrying options only a permission check would use.
func TestGuardRoutes_ContradictoryDecisions_FailStartup(t *testing.T) {
	tests := []struct {
		name string
		rule RouteRule
	}{
		{
			name: "public and permission-gated",
			rule: RouteRule{Path: "/api/v1/notes", Access: pkgcore.RouteAccess{
				Public:     true,
				Permission: func(*http.Request) string { return "notes:read" },
			}},
		},
		{
			name: "public with a subject resolver",
			rule: RouteRule{
				Path:            "/api/v1/notes",
				Access:          pkgcore.RouteAccess{Public: true},
				SubjectResolver: func(*http.Request) (Subject, bool) { return Subject{}, false },
			},
		},
		{
			name: "public with an exemption",
			rule: RouteRule{
				Path:   "/api/v1/notes",
				Access: pkgcore.RouteAccess{Public: true},
				Exempt: func(*http.Request) bool { return true },
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			route, _ := newRoute("/api/v1/notes")
			_, err := GuardRoutes(&stubAuthorizer{allow: true},
				[]pkgcore.MountedRoute{route},
				[]RouteRule{tc.rule})
			if !errors.Is(err, ErrRouteUndecided) {
				t.Fatalf("error = %v, want errors.Is(err, ErrRouteUndecided)", err)
			}
		})
	}
}

// TestGuardRoutes_TableAndMountedSet_MustLineUp pins the reverse direction:
// a rule for a path no module mounted, and the same path declared twice,
// are wiring errors rather than silently ignored entries -- so neither side
// of the table can stop describing the other while still looking complete.
func TestGuardRoutes_TableAndMountedSet_MustLineUp(t *testing.T) {
	t.Run("rule for an unmounted path", func(t *testing.T) {
		route, _ := newRoute("/api/v1/notes")
		_, err := GuardRoutes(&stubAuthorizer{allow: true},
			[]pkgcore.MountedRoute{route},
			[]RouteRule{
				{Path: "/api/v1/notes", Access: pkgcore.RouteAccess{Public: true}},
				{Path: "/api/v1/invoices", Access: pkgcore.RouteAccess{Public: true}},
			})
		if !errors.Is(err, ErrRouteTableMismatch) {
			t.Fatalf("error = %v, want errors.Is(err, ErrRouteTableMismatch)", err)
		}
		if got := err.Error(); !contains(got, "/api/v1/invoices") {
			t.Fatalf("the error does not name the stale rule's path: %v", err)
		}
	})

	t.Run("duplicate rule for one path", func(t *testing.T) {
		route, _ := newRoute("/api/v1/notes")
		_, err := GuardRoutes(&stubAuthorizer{allow: true},
			[]pkgcore.MountedRoute{route},
			[]RouteRule{
				{Path: "/api/v1/notes", Access: pkgcore.RouteAccess{Public: true}},
				{Path: "/api/v1/notes", Access: pkgcore.RouteAccess{Public: true}},
			})
		if !errors.Is(err, ErrRouteTableMismatch) {
			t.Fatalf("error = %v, want errors.Is(err, ErrRouteTableMismatch)", err)
		}
	})
}

// TestGuardRoutes_PublicRoute_IsAdmittedWithoutACheck pins the explicit
// public declaration as the one form that serves with no check at all: the
// nil Authorizer would make any gated route answer 500, so reaching the
// handler proves no gate was applied rather than that a gate allowed.
func TestGuardRoutes_PublicRoute_IsAdmittedWithoutACheck(t *testing.T) {
	route, next := newRoute("/api/v1/config/public")
	guarded, err := GuardRoutes(nil,
		[]pkgcore.MountedRoute{route},
		[]RouteRule{{Path: "/api/v1/config/public", Access: pkgcore.RouteAccess{Public: true}}})
	if err != nil {
		t.Fatalf("GuardRoutes: %v", err)
	}

	rec := serveGuarded(t, guarded, "/api/v1/config/public", httptest.NewRequest(http.MethodGet, "/api/v1/config/public", nil))
	if !next.served || rec.Code != http.StatusOK {
		t.Fatalf("the public route answered %d with served=%v, want the handler reached", rec.Code, next.served)
	}
	returned := guardedRoute(t, guarded, "/api/v1/config/public")
	if !returned.Access.Public || returned.Access.Permission != nil {
		t.Fatalf("the returned route records %+v, want the public decision", returned.Access)
	}
}

// TestGuardRoutes_GatedRoute_RefusesAndAdmitsThroughTheOneGate pins the
// request-time check: the same refusal shape rbac's gate writes everywhere
// (403 rbac.permission_denied), the Authorizer asked with the SPLIT halves
// of the declared permission, and the handler reached only on an allow.
func TestGuardRoutes_GatedRoute_RefusesAndAdmitsThroughTheOneGate(t *testing.T) {
	selector := func(*http.Request) string { return "notes:read" }

	for _, tc := range []struct {
		name     string
		allow    bool
		wantCode int
	}{
		{name: "holder admitted", allow: true, wantCode: http.StatusOK},
		{name: "non-holder refused", allow: false, wantCode: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route, next := newRoute("/api/v1/notes")
			az := &stubAuthorizer{allow: tc.allow}
			guarded, err := GuardRoutes(az,
				[]pkgcore.MountedRoute{route},
				[]RouteRule{{Path: "/api/v1/notes", Access: pkgcore.RouteAccess{Permission: selector}}})
			if err != nil {
				t.Fatalf("GuardRoutes: %v", err)
			}

			rec := serveGuarded(t, guarded, "/api/v1/notes", requestFor(http.MethodGet, "/api/v1/notes", demoSubject))

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if next.served != tc.allow {
				t.Fatalf("handler served = %v, want %v", next.served, tc.allow)
			}
			if len(az.calls) != 1 || az.calls[0] != [2]string{"read", "notes"} {
				t.Fatalf("Can calls = %v, want one [read notes]", az.calls)
			}
			if tc.allow {
				return
			}
			if code := decodeErrorBody(t, rec).Code; code != ErrPermissionDenied.Code {
				t.Fatalf("code = %q, want %q", code, ErrPermissionDenied.Code)
			}
			if guardedRoute(t, guarded, "/api/v1/notes").Access.Permission == nil {
				t.Fatal("the returned route records no permission decision")
			}
		})
	}
}

// TestGuardRoutes_ThreeSegmentPermission_SplitsAtTheLastSeparator pins the
// multi-entity convention end to end: a rule naming
// "integration:apikey:read" asks the Authorizer about resource
// "integration:apikey" and action "read", the halves
// Permission("integration:apikey", "read") composes back into.
func TestGuardRoutes_ThreeSegmentPermission_SplitsAtTheLastSeparator(t *testing.T) {
	route, next := newRoute("/api/v1/integration")
	az := &stubAuthorizer{allow: true}
	guarded, err := GuardRoutes(az,
		[]pkgcore.MountedRoute{route},
		[]RouteRule{{
			Path: "/api/v1/integration",
			Access: pkgcore.RouteAccess{
				Permission: func(r *http.Request) string {
					if r.Method == http.MethodGet {
						return "integration:apikey:read"
					}
					return "integration:apikey:manage"
				},
			},
		}})
	if err != nil {
		t.Fatalf("GuardRoutes: %v", err)
	}

	rec := serveGuarded(t, guarded, "/api/v1/integration", requestFor(http.MethodGet, "/api/v1/integration", demoSubject))
	if !next.served || rec.Code != http.StatusOK {
		t.Fatalf("status = %d served=%v, want the handler reached", rec.Code, next.served)
	}
	if len(az.calls) != 1 || az.calls[0] != [2]string{"read", "integration:apikey"} {
		t.Fatalf("Can calls = %v, want one [read integration:apikey]", az.calls)
	}
}

// TestGuardRoutes_AuthorizerError_KeepsTheGatesServerErrorShape pins that a
// gated route's undecidable check is reported exactly as the standalone
// gate reports it: 500 rbac.storage_error, request blocked.
func TestGuardRoutes_AuthorizerError_KeepsTheGatesServerErrorShape(t *testing.T) {
	route, next := newRoute("/api/v1/notes")
	az := &stubAuthorizer{allow: true, err: errors.New("storage unreachable")}
	guarded, err := GuardRoutes(az,
		[]pkgcore.MountedRoute{route},
		[]RouteRule{{
			Path: "/api/v1/notes",
			Access: pkgcore.RouteAccess{
				Permission: func(*http.Request) string { return "notes:read" },
			},
		}})
	if err != nil {
		t.Fatalf("GuardRoutes: %v", err)
	}

	rec := serveGuarded(t, guarded, "/api/v1/notes", requestFor(http.MethodGet, "/api/v1/notes", demoSubject))
	if next.served {
		t.Fatal("the handler ran although the decision failed")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if code := decodeErrorBody(t, rec).Code; code != ErrStorage.Code {
		t.Fatalf("code = %q, want %q", code, ErrStorage.Code)
	}
}

// TestGuardRoutes_Exempt_ShortCircuitsToTheHandler pins the exemption: a
// request the predicate names reaches the handler with the Authorizer never
// consulted, while every other request still passes the gate.
func TestGuardRoutes_Exempt_ShortCircuitsToTheHandler(t *testing.T) {
	route, next := newRoute("/api/v1/org")
	az := &stubAuthorizer{allow: false}
	guarded, err := GuardRoutes(az,
		[]pkgcore.MountedRoute{route},
		[]RouteRule{{
			Path: "/api/v1/org",
			Access: pkgcore.RouteAccess{
				Permission: func(*http.Request) string { return "org:read" },
			},
			Exempt: func(r *http.Request) bool { return r.URL.Path == "/api/v1/org/invitations/accept" },
		}})
	if err != nil {
		t.Fatalf("GuardRoutes: %v", err)
	}

	rec := serveGuarded(t, guarded, "/api/v1/org", requestFor(http.MethodPost, "/api/v1/org/invitations/accept", demoSubject))
	if !next.served || rec.Code != http.StatusOK {
		t.Fatalf("the exempt request answered %d served=%v, want the handler reached", rec.Code, next.served)
	}
	if len(az.calls) != 0 {
		t.Fatalf("the Authorizer was consulted for an exempt request: %v", az.calls)
	}

	next.served = false
	rec2 := serveGuarded(t, guarded, "/api/v1/org", requestFor(http.MethodGet, "/api/v1/org/nodes", demoSubject))
	if next.served {
		t.Fatal("a non-exempt request reached the handler despite the denial")
	}
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec2.Code, http.StatusForbidden)
	}
}

// TestGuardRoutes_Layer_RunsBetweenGateAndHandler pins the rule Layer's
// position: the permission check decides first, the layer runs only for an
// admitted request, and an exempt request reaches the handler with neither
// the check nor the layer run -- the narrowing layer reads the Subject the
// gate installed, so it must never see a request the gate did not admit.
func TestGuardRoutes_Layer_RunsBetweenGateAndHandler(t *testing.T) {
	ruleFor := func(layerRan *[]string) RouteRule {
		return RouteRule{
			Path: "/api/v1/org",
			Access: pkgcore.RouteAccess{
				Permission: func(*http.Request) string { return "org:read" },
			},
			Exempt: func(r *http.Request) bool { return r.URL.Path == "/api/v1/org/invitations/accept" },
			Layer: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					*layerRan = append(*layerRan, "layer")
					next.ServeHTTP(w, r)
				})
			},
		}
	}

	t.Run("admitted request runs the layer", func(t *testing.T) {
		route, next := newRoute("/api/v1/org")
		var layerRan []string
		az := &stubAuthorizer{allow: true}
		guarded, err := GuardRoutes(az, []pkgcore.MountedRoute{route}, []RouteRule{ruleFor(&layerRan)})
		if err != nil {
			t.Fatalf("GuardRoutes: %v", err)
		}
		rec := serveGuarded(t, guarded, "/api/v1/org", requestFor(http.MethodGet, "/api/v1/org/nodes", demoSubject))
		if !next.served || rec.Code != http.StatusOK {
			t.Fatalf("status = %d served=%v, want the handler reached", rec.Code, next.served)
		}
		if len(layerRan) != 1 {
			t.Fatalf("layer runs = %v, want exactly one run for an admitted request", layerRan)
		}
	})

	t.Run("denied request never runs the layer", func(t *testing.T) {
		route, next := newRoute("/api/v1/org")
		var layerRan []string
		az := &stubAuthorizer{allow: false}
		guarded, err := GuardRoutes(az, []pkgcore.MountedRoute{route}, []RouteRule{ruleFor(&layerRan)})
		if err != nil {
			t.Fatalf("GuardRoutes: %v", err)
		}
		rec := serveGuarded(t, guarded, "/api/v1/org", requestFor(http.MethodGet, "/api/v1/org/nodes", demoSubject))
		if next.served {
			t.Fatal("the handler ran despite the denial")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
		}
		if len(layerRan) != 0 {
			t.Fatalf("the layer ran behind a denied request: %v", layerRan)
		}
	})

	t.Run("exempt request runs neither the check nor the layer", func(t *testing.T) {
		route, next := newRoute("/api/v1/org")
		var layerRan []string
		az := &stubAuthorizer{allow: false}
		guarded, err := GuardRoutes(az, []pkgcore.MountedRoute{route}, []RouteRule{ruleFor(&layerRan)})
		if err != nil {
			t.Fatalf("GuardRoutes: %v", err)
		}
		rec := serveGuarded(t, guarded, "/api/v1/org", requestFor(http.MethodPost, "/api/v1/org/invitations/accept", demoSubject))
		if !next.served || rec.Code != http.StatusOK {
			t.Fatalf("status = %d served=%v, want the exempt request to reach the handler", rec.Code, next.served)
		}
		if len(az.calls) != 0 {
			t.Fatalf("the Authorizer was consulted for an exempt request: %v", az.calls)
		}
		if len(layerRan) != 0 {
			t.Fatalf("the layer ran for an exempt request: %v", layerRan)
		}
	})
}

// TestGuardRoutes_SubjectResolverOverride_IsHonored pins the per-route
// resolver: a request carrying no Subject in its context still reaches the
// handler when the rule's resolver supplies one (a request whose decisions
// must be evaluated under another tenant domain), and the default
// context-reading resolver refuses the identical request when the rule
// wires none.
func TestGuardRoutes_SubjectResolverOverride_IsHonored(t *testing.T) {
	route, next := newRoute("/api/v1/admin")
	az := &stubAuthorizer{allow: true}
	pinned := Subject{TenantID: SystemDomain, UserID: "staff-1"}
	rule := RouteRule{
		Path: "/api/v1/admin",
		Access: pkgcore.RouteAccess{
			Permission: func(*http.Request) string { return "admin:impersonate" },
		},
		SubjectResolver: func(*http.Request) (Subject, bool) { return pinned, true },
	}
	guarded, err := GuardRoutes(az, []pkgcore.MountedRoute{route}, []RouteRule{rule})
	if err != nil {
		t.Fatalf("GuardRoutes: %v", err)
	}

	rec := serveGuarded(t, guarded, "/api/v1/admin", httptest.NewRequest(http.MethodPost, "/api/v1/admin/impersonation", nil))
	if !next.served || rec.Code != http.StatusOK {
		t.Fatalf("status = %d served=%v, want the resolver-supplied subject admitted", rec.Code, next.served)
	}

	route2, next2 := newRoute("/api/v1/admin")
	noResolver, err := GuardRoutes(az,
		[]pkgcore.MountedRoute{route2},
		[]RouteRule{{
			Path: "/api/v1/admin",
			Access: pkgcore.RouteAccess{
				Permission: func(*http.Request) string { return "admin:impersonate" },
			},
		}})
	if err != nil {
		t.Fatalf("GuardRoutes: %v", err)
	}
	rec2 := serveGuarded(t, noResolver, "/api/v1/admin", httptest.NewRequest(http.MethodPost, "/api/v1/admin/impersonation", nil))
	if next2.served {
		t.Fatal("a request with no Subject reached the handler without a resolver supplying one")
	}
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec2.Code, http.StatusForbidden)
	}
}

// TestGuardRoutes_InexactTable_ReturnsNothingAndMutatesNothing pins two
// wholesale properties: a refused table returns no routes at all (never a
// partial, silently-served set), and an accepted call leaves the caller's
// mounted slice untouched -- GuardRoutes annotates and wraps the routes it
// RETURNS.
func TestGuardRoutes_InexactTable_ReturnsNothingAndMutatesNothing(t *testing.T) {
	notes, _ := newRoute("/api/v1/notes")
	publicRoute, publicHandler := newRoute("/api/v1/config/public")
	mounted := []pkgcore.MountedRoute{notes, publicRoute}
	original := mounted[0].Handler

	t.Run("refused table returns nothing", func(t *testing.T) {
		guarded, err := GuardRoutes(&stubAuthorizer{allow: true}, mounted,
			[]RouteRule{{Path: "/api/v1/notes", Access: pkgcore.RouteAccess{Public: true}}})
		if err == nil {
			t.Fatal("an incomplete table was accepted")
		}
		if guarded != nil {
			t.Fatalf("a partial route set was returned: %v", guarded)
		}
	})

	guarded, err := GuardRoutes(&stubAuthorizer{allow: true}, mounted, []RouteRule{
		{
			Path: "/api/v1/notes",
			Access: pkgcore.RouteAccess{
				Permission: func(*http.Request) string { return "notes:read" },
			},
		},
		{Path: "/api/v1/config/public", Access: pkgcore.RouteAccess{Public: true}},
	})
	if err != nil {
		t.Fatalf("GuardRoutes: %v", err)
	}
	if len(guarded) != 2 || guarded[0].Path != "/api/v1/notes" || guarded[1].Path != "/api/v1/config/public" {
		t.Fatalf("guarded routes = %+v, want both inputs in mount order", guarded)
	}
	if mounted[0].Access.Public || mounted[0].Access.Permission != nil {
		t.Fatalf("the input slice's first route was annotated: %+v", mounted[0].Access)
	}
	if mounted[0].Handler != original {
		t.Fatal("the input slice's first route was wrapped in place")
	}
	if guarded[0].Handler == original {
		t.Fatal("the returned gated route was not wrapped")
	}
	if guarded[1].Handler != publicHandler {
		t.Fatal("the returned public route's handler changed; a public route passes through untouched")
	}
	if !guarded[1].Access.Public {
		t.Fatalf("the returned public route records %+v, want the public decision", guarded[1].Access)
	}
}
