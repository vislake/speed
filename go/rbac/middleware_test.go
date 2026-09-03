package rbac

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubAuthorizer is an Authorizer whose answers are dictated by the test,
// for the cases a real Service cannot produce on demand -- a storage
// failure, or a decision that must be proven NOT to have been asked for.
type stubAuthorizer struct {
	allow bool
	err   error

	// calls records every (action, resource) pair Can was asked about, so
	// a test can assert both what was asked and that nothing was asked.
	calls [][2]string
}

func (a *stubAuthorizer) Can(_ context.Context, _ Subject, action, resource string) (bool, error) {
	a.calls = append(a.calls, [2]string{action, resource})
	return a.allow, a.err
}

func (a *stubAuthorizer) ListPermissions(context.Context, Subject) ([]string, error) {
	return nil, nil
}

func (a *stubAuthorizer) AssignRole(context.Context, Subject, string, Scope) error { return nil }

func (a *stubAuthorizer) RevokeRole(context.Context, Subject, string, Scope) error { return nil }

func (a *stubAuthorizer) DataScope(context.Context, Subject, string, string) (DataScope, error) {
	return DataScope{}, nil
}

var _ Authorizer = (*stubAuthorizer)(nil)

// recordingHandler is the protected handler. It records whether it ran and
// what Subject the middleware left on the context for it.
type recordingHandler struct {
	served  bool
	subject Subject
	present bool
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.served = true
	h.subject, h.present = SubjectFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
}

// serve runs one request through mw wrapped around a fresh
// recordingHandler and returns the response recorder and that handler.
func serve(mw func(http.Handler) http.Handler, r *http.Request) (*httptest.ResponseRecorder, *recordingHandler) {
	next := &recordingHandler{}
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, r)
	return rec, next
}

// decodeErrorBody reads the {code, params} body the middleware writes.
func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) authzErrorBody {
	t.Helper()
	var body authzErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the error body %q: %v", rec.Body.String(), err)
	}
	return body
}

// requestWithSubject returns a GET request carrying sub the way the
// authenticating side installs it.
func requestWithSubject(sub Subject) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	return r.WithContext(WithSubject(r.Context(), sub))
}

func TestSplitPermission(t *testing.T) {
	tests := []struct {
		name       string
		permission string
		resource   string
		action     string
		ok         bool
	}{
		{name: "well formed", permission: "notes:read", resource: "notes", action: "read", ok: true},
		{name: "underscored halves", permission: "role_binding:manage_all", resource: "role_binding", action: "manage_all", ok: true},
		{name: "no separator", permission: "notes"},
		{name: "empty", permission: ""},
		{name: "empty action", permission: "notes:"},
		{name: "empty resource", permission: ":read"},
		{name: "only a separator", permission: ":"},
		{name: "three parts", permission: "notes:sub:read"},
		// Structurally well-formed, semantically impossible: no module
		// could have declared it, so it is split successfully and then
		// denied by the catalog miss. splitPermission guards SHAPE, not
		// membership -- adding a trim here would buy nothing the frozen
		// catalog does not already refuse, on the hot path of every
		// request. TestRequirePermission_UndeclaredButWellFormed_Denies
		// below pins that end of it.
		{name: "whitespace halves are shaped correctly", permission: " : ", resource: " ", action: " ", ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resource, action, ok := splitPermission(tc.permission)
			if ok != tc.ok {
				t.Fatalf("splitPermission(%q) ok = %v, want %v", tc.permission, ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if resource != tc.resource || action != tc.action {
				t.Fatalf("splitPermission(%q) = (%q, %q), want (%q, %q)",
					tc.permission, resource, action, tc.resource, tc.action)
			}
		})
	}
}

// TestSplitPermission_IsTheInverseOfPermission pins the two halves of the
// naming convention against each other: whatever Permission composes,
// splitPermission must take apart again, or the middleware would refuse a
// permission a module legitimately declared.
func TestSplitPermission_IsTheInverseOfPermission(t *testing.T) {
	for _, pair := range [][2]string{{"notes", "read"}, {"billing", "manage"}, {"rbac", "read"}} {
		composed := Permission(pair[0], pair[1])
		resource, action, ok := splitPermission(composed)
		if !ok || resource != pair[0] || action != pair[1] {
			t.Fatalf("Permission(%q, %q) = %q, which splits to (%q, %q, %v)",
				pair[0], pair[1], composed, resource, action, ok)
		}
	}
}

func TestRequirePermission_SubjectHoldsIt_ReachesTheHandler(t *testing.T) {
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	rec, next := serve(RequirePermission(svc, "notes:read"), requestWithSubject(sub))

	if !next.served {
		t.Fatalf("the handler was not reached; status %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !next.present || next.subject != sub {
		t.Fatalf("the handler saw subject %+v (present %v), want %+v", next.subject, next.present, sub)
	}
}

func TestRequirePermission_SubjectLacksIt_IsForbidden(t *testing.T) {
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	rec, next := serve(RequirePermission(svc, "notes:write"), requestWithSubject(sub))

	if next.served {
		t.Fatal("the handler ran for a subject that does not hold the permission")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	body := decodeErrorBody(t, rec)
	if body.Code != ErrPermissionDenied.Code {
		t.Fatalf("code = %q, want %q", body.Code, ErrPermissionDenied.Code)
	}
	if got := body.Params["permission"]; got != "notes:write" {
		t.Fatalf("params[permission] = %v, want %q", got, "notes:write")
	}
	if ct := rec.Header().Get("Content-Type"); ct != authzErrorContentType {
		t.Fatalf("Content-Type = %q, want %q", ct, authzErrorContentType)
	}
}

// TestRequirePermission_NoSubject_IsForbiddenNotAServerError pins the
// fail-closed shape of an unauthenticated request: the gate refuses it
// with the same 403 and the same code a denied request gets, never a 500
// and never a distinguishable "you are not signed in" answer.
func TestRequirePermission_NoSubject_IsForbiddenNotAServerError(t *testing.T) {
	svc := newTestService(t)

	rec, next := serve(
		RequirePermission(svc, "notes:read"),
		httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil),
	)

	if next.served {
		t.Fatal("the handler ran for a request carrying no subject")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if code := decodeErrorBody(t, rec).Code; code != ErrPermissionDenied.Code {
		t.Fatalf("code = %q, want %q -- a missing subject must not be distinguishable from a denial", code, ErrPermissionDenied.Code)
	}
}

// TestRequirePermission_IncompleteSubject_IsForbidden covers the half-built
// Subject an authenticating layer could install after a partial token
// parse. SubjectFromContext reports it as no subject at all, so the gate
// must refuse it rather than evaluate an empty tenant.
func TestRequirePermission_IncompleteSubject_IsForbidden(t *testing.T) {
	az := &stubAuthorizer{allow: true}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	r = r.WithContext(WithSubject(r.Context(), Subject{TenantID: "tenant-a"}))

	rec, next := serve(RequirePermission(az, "notes:read"), r)

	if next.served {
		t.Fatal("the handler ran for an incomplete subject")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if len(az.calls) != 0 {
		t.Fatalf("the Authorizer was consulted %d time(s) for an incomplete subject; it must be refused before any decision", len(az.calls))
	}
}

// TestRequirePermission_MalformedPermission_DeniesWithoutDeciding is the
// route-table typo case. A permission string no module could have declared
// must close the route, not open it, and must not even reach the
// Authorizer -- an engine asked about a name that cannot exist has nothing
// useful to say.
func TestRequirePermission_MalformedPermission_DeniesWithoutDeciding(t *testing.T) {
	for _, permission := range []string{"", "notes", "notes:", ":read", "notes:sub:read"} {
		t.Run(permission, func(t *testing.T) {
			az := &stubAuthorizer{allow: true}
			sub := Subject{TenantID: "tenant-a", UserID: "user-1"}

			rec, next := serve(RequirePermission(az, permission), requestWithSubject(sub))

			if next.served {
				t.Fatalf("the handler ran behind the malformed permission %q", permission)
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
			if len(az.calls) != 0 {
				t.Fatalf("the Authorizer was consulted about %q", permission)
			}
		})
	}
}

// TestRequirePermission_UndeclaredButWellFormed_Denies is the other half
// of splitPermission's shape-only contract: a permission string that parses
// but that no module ever declared reaches the engine and is refused there,
// because nobody can hold what nobody declared. It denies rather than
// erroring -- a check must never turn a request into a failure just because
// the caller asked about a name that does not exist.
func TestRequirePermission_UndeclaredButWellFormed_Denies(t *testing.T) {
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "owner-ish", Scope{}, "notes:read", "notes:write", "billing:manage")

	rec, next := serve(RequirePermission(svc, "nosuch:permission"), requestWithSubject(sub))

	if next.served {
		t.Fatal("a permission no module declared opened the gate")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d -- an undeclared permission must deny, not error", rec.Code, http.StatusForbidden)
	}
	if code := decodeErrorBody(t, rec).Code; code != ErrPermissionDenied.Code {
		t.Fatalf("code = %q, want %q", code, ErrPermissionDenied.Code)
	}
}

// TestRequirePermission_AuthorizerError_IsAServerErrorAndStillBlocks pins
// the one distinction the middleware does draw: a check that could not be
// PERFORMED is a 500, not a 403 -- but the request is blocked either way.
func TestRequirePermission_AuthorizerError_IsAServerErrorAndStillBlocks(t *testing.T) {
	az := &stubAuthorizer{allow: true, err: errors.New("database unreachable")}
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}

	rec, next := serve(RequirePermission(az, "notes:read"), requestWithSubject(sub))

	if next.served {
		t.Fatal("the handler ran even though the decision failed -- an undecidable check must never pass")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	body := decodeErrorBody(t, rec)
	if body.Code != ErrStorage.Code {
		t.Fatalf("code = %q, want %q", body.Code, ErrStorage.Code)
	}
	// The cause must never reach the body: it can carry SQL fragments.
	if got := rec.Body.String(); strings.Contains(got, "database unreachable") {
		t.Fatalf("the response body leaked the underlying cause: %s", got)
	}
}

// TestRequirePermission_NilAuthorizer_ReportsWiringNotDenial makes the one
// case that must NOT look like a denial visible: a host that forgot to
// pass the Service would otherwise see a permanently 403ing route that is
// indistinguishable from a correctly configured one.
func TestRequirePermission_NilAuthorizer_ReportsWiringNotDenial(t *testing.T) {
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}

	rec, next := serve(RequirePermission(nil, "notes:read"), requestWithSubject(sub))

	if next.served {
		t.Fatal("the handler ran behind a nil Authorizer")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if code := decodeErrorBody(t, rec).Code; code != ErrServiceNotAttached.Code {
		t.Fatalf("code = %q, want %q", code, ErrServiceNotAttached.Code)
	}
}

func TestRequirePermissionFunc_NilPermissionFunc_ReportsWiringNotDenial(t *testing.T) {
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}

	rec, _ := serve(RequirePermissionFunc(&stubAuthorizer{allow: true}, nil), requestWithSubject(sub))

	if code := decodeErrorBody(t, rec).Code; code != ErrServiceNotAttached.Code {
		t.Fatalf("code = %q, want %q", code, ErrServiceNotAttached.Code)
	}
}

// TestRequirePermissionFunc_ChoosesThePermissionPerMethod is the shape the
// reference app mounts: one path, a read permission on GET and a write
// permission on POST.
func TestRequirePermissionFunc_ChoosesThePermissionPerMethod(t *testing.T) {
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	mw := RequirePermissionFunc(svc, func(r *http.Request) string {
		if r.Method == http.MethodGet {
			return "notes:read"
		}
		return "notes:write"
	})

	get := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	rec, next := serve(mw, get.WithContext(WithSubject(get.Context(), sub)))
	if !next.served || rec.Code != http.StatusOK {
		t.Fatalf("GET: served %v status %d, want served with %d", next.served, rec.Code, http.StatusOK)
	}

	post := httptest.NewRequest(http.MethodPost, "/api/v1/notes", nil)
	rec, next = serve(mw, post.WithContext(WithSubject(post.Context(), sub)))
	if next.served {
		t.Fatal("POST reached the handler on a read-only grant")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestRequirePermissionFunc_UnknownMethod_FailsClosed covers the method a
// route table forgot. An empty permission must close the route rather than
// fall through unguarded, which is why there is no "nothing required"
// return value.
func TestRequirePermissionFunc_UnknownMethod_FailsClosed(t *testing.T) {
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "everything", Scope{}, "notes:read", "notes:write")

	mw := RequirePermissionFunc(svc, func(r *http.Request) string {
		if r.Method == http.MethodGet {
			return "notes:read"
		}
		return "" // the table has no entry for this method
	})

	del := httptest.NewRequest(http.MethodDelete, "/api/v1/notes/n1", nil)
	rec, next := serve(mw, del.WithContext(WithSubject(del.Context(), sub)))

	if next.served {
		t.Fatal("DELETE fell through the gate unguarded")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestRequirePermission_GrantInAnotherTenant_DoesNotOpenTheGate is the
// isolation case at the HTTP boundary. The same user id holding the same
// role in tenant A must not pass a gate for tenant B, and the tenant comes
// from the Subject the authenticating side vouched for -- nothing in the
// request can steer it.
func TestRequirePermission_GrantInAnotherTenant_DoesNotOpenTheGate(t *testing.T) {
	svc := newTestService(t)
	inA := Subject{TenantID: "tenant-a", UserID: "user-1"}
	inB := Subject{TenantID: "tenant-b", UserID: "user-1"}
	grant(t, svc, inA, "reader", Scope{}, "notes:read")

	mw := RequirePermission(svc, "notes:read")

	if rec, next := serve(mw, requestWithSubject(inA)); !next.served || rec.Code != http.StatusOK {
		t.Fatalf("tenant-a: served %v status %d, want served with %d", next.served, rec.Code, http.StatusOK)
	}
	rec, next := serve(mw, requestWithSubject(inB))
	if next.served {
		t.Fatal("a grant written in tenant-a opened the gate for the same user id in tenant-b")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tenant-b: status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestWithSubjectResolver_AdaptsAHostIdentity proves the second no-import
// seam: a host whose authenticating layer carries identity its own way
// wires an adapter, and neither module learns about the other. The
// resolved Subject must also reach the handler, so a handler needing
// DataScope reads back the identity this gate decided on.
func TestWithSubjectResolver_AdaptsAHostIdentity(t *testing.T) {
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	mw := RequirePermission(svc, "notes:read", WithSubjectResolver(func(r *http.Request) (Subject, bool) {
		// A host-shaped identity source: its own context key, its own
		// framework value, a demo header. rbac never sees it.
		if r.Header.Get("X-Demo-User") == "" {
			return Subject{}, false
		}
		return Subject{TenantID: "tenant-a", UserID: r.Header.Get("X-Demo-User")}, true
	}))

	// The request carries NO rbac.Subject on its context at all.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil)
	r.Header.Set("X-Demo-User", "user-1")
	rec, next := serve(mw, r)
	if !next.served || rec.Code != http.StatusOK {
		t.Fatalf("served %v status %d, want served with %d", next.served, rec.Code, http.StatusOK)
	}
	if !next.present || next.subject != sub {
		t.Fatalf("the handler saw subject %+v (present %v), want the resolved %+v", next.subject, next.present, sub)
	}

	// A resolver that reports no identity denies, exactly like an absent
	// context subject.
	rec, next = serve(mw, httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil))
	if next.served || rec.Code != http.StatusForbidden {
		t.Fatalf("unresolved identity: served %v status %d, want denied with %d", next.served, rec.Code, http.StatusForbidden)
	}
}

// TestWithSubjectResolver_OverridesTheContextSubject pins which of the two
// sources wins when both are present: the explicitly wired resolver, since
// a host that wired one has said that is where identity comes from.
func TestWithSubjectResolver_OverridesTheContextSubject(t *testing.T) {
	svc := newTestService(t)
	fromResolver := Subject{TenantID: "tenant-a", UserID: "user-resolver"}
	fromContext := Subject{TenantID: "tenant-a", UserID: "user-context"}
	grant(t, svc, fromResolver, "reader", Scope{}, "notes:read")

	mw := RequirePermission(svc, "notes:read", WithSubjectResolver(func(*http.Request) (Subject, bool) {
		return fromResolver, true
	}))

	_, next := serve(mw, requestWithSubject(fromContext))
	if !next.served {
		t.Fatal("the resolver's subject holds the permission, so the handler should have run")
	}
	if next.subject != fromResolver {
		t.Fatalf("the handler saw %+v, want the resolver's %+v", next.subject, fromResolver)
	}
}

// TestWithSubjectResolver_Nil_KeepsTheDefault guards the accidental-nil
// case: a middleware that denied everything because an option was built
// from a nil function would be a silent outage.
func TestWithSubjectResolver_Nil_KeepsTheDefault(t *testing.T) {
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	rec, next := serve(RequirePermission(svc, "notes:read", WithSubjectResolver(nil)), requestWithSubject(sub))
	if !next.served || rec.Code != http.StatusOK {
		t.Fatalf("served %v status %d, want served with %d", next.served, rec.Code, http.StatusOK)
	}
}

// TestRequirePermission_AsksTheAuthorizerForTheSplitHalves pins the wiring
// between the composed string a route table holds and the two-argument Can
// the Authorizer interface exposes.
func TestRequirePermission_AsksTheAuthorizerForTheSplitHalves(t *testing.T) {
	az := &stubAuthorizer{allow: true}
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}

	if _, next := serve(RequirePermission(az, "billing:manage"), requestWithSubject(sub)); !next.served {
		t.Fatal("the handler should have run")
	}
	if len(az.calls) != 1 || az.calls[0] != [2]string{"manage", "billing"} {
		t.Fatalf("Can was called with %v, want one call with (action %q, resource %q)", az.calls, "manage", "billing")
	}
}
