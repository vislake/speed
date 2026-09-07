package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/vislake/speed/go/admin/api"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy"
)

// decodedErrorCode decodes w's body as api.AdminError's own {"code": ...}
// envelope shape and returns the code, failing the test if the body does
// not decode.
func decodedErrorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope %q: %v", w.Body.String(), err)
	}
	return body.Code
}

// TestHandler_MalformedRequestBody_ReportsRequestBodyInvalid is Finding
// P3-5's regression test: a genuinely malformed JSON body against
// AdminUpdateTenant (one of the five sites that used to wrap a decode
// failure in the semantically wrong ErrTenantIDRequired) must now be
// reported as admin.request_body_invalid, never admin.tenant_id_required
// -- proven against the real composed HTTP handler, not a bare service call.
func TestHandler_MalformedRequestBody_ReportsRequestBodyInvalid(t *testing.T) {
	env := buildTestAdminModule(t)

	const tenant = pkgcore.TenantID("tenant-malformed-body")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Malformed Body Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	body := strings.NewReader(`{"displayName": 12345}`) // displayName must be a string, not a number
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/tenants/"+string(tenant), body)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: "operator-malformed-1"}))
	w := httptest.NewRecorder()

	env.Admin.handler.ServeHTTP(w, req)

	if got := decodedErrorCode(t, w); got != ErrRequestBodyInvalid.Code {
		t.Fatalf("code = %q, want %q (NOT %q, the pre-fix semantically-wrong code)",
			got, ErrRequestBodyInvalid.Code, ErrTenantIDRequired.Code)
	}
}

// TestHandler_StartImpersonation_MalformedBody_ReportsRequestBodyInvalid
// covers the sixth site, which wrapped its decode failure in
// ErrImpersonationTargetRequired rather than ErrTenantIDRequired -- a
// different wrong code, but the identical defect (a decode failure
// reported as a semantically unrelated validation refusal).
func TestHandler_StartImpersonation_MalformedBody_ReportsRequestBodyInvalid(t *testing.T) {
	env := buildTestAdminModule(t)

	body := strings.NewReader(`{"targetUserId": true}`) // targetUserId must be a string, not a bool
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/impersonation", body)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: "operator-malformed-2"}))
	w := httptest.NewRecorder()

	env.Admin.handler.ServeHTTP(w, req)

	if got := decodedErrorCode(t, w); got != ErrRequestBodyInvalid.Code {
		t.Fatalf("code = %q, want %q (NOT %q, the pre-fix semantically-wrong code)",
			got, ErrRequestBodyInvalid.Code, ErrImpersonationTargetRequired.Code)
	}
}

// TestPaginate_ExtremeOffsetAndLimit_DoesNotPanic is P2-4's regression
// test: an extreme caller-supplied limit (math.MaxInt, as an HTTP query
// string could carry) combined with a small in-range offset used to
// overflow the offset+limit addition inside paginate, wrap the computed
// end negative, sail through both clamp conditions, and panic slicing
// events[offset:negative]. The clamp must bound the limit by the remaining
// tail BEFORE the addition so the addition itself can never overflow.
func TestPaginate_ExtremeOffsetAndLimit_DoesNotPanic(t *testing.T) {
	events := []audit.AuditEvent{
		{ID: "evt-0"},
		{ID: "evt-1"},
		{ID: "evt-2"},
	}

	got := paginate(events, 1, math.MaxInt)
	if len(got) != 2 || got[0].ID != "evt-1" || got[1].ID != "evt-2" {
		t.Fatalf("paginate(3 events, offset=1, limit=MaxInt) = %+v, want the whole tail evt-1..evt-2 (no panic, no truncation)", got)
	}
}

// TestHandler_UpdateTenant_InvalidStatus_RefusedAndNotPersisted is P2-5's
// regression test: a status string outside the API enum's closed
// vocabulary must be refused at the handler boundary (400,
// admin.tenant_status_invalid), never persisted. On unfixed main the wire
// value was written verbatim into admin_tenants.status -- and tenancy's
// status gate (tenant_status.go: only TenantStatusActive is servable, an
// out-of-vocabulary answer refused with ErrTenantSuspended) would then
// refuse every request for that tenant, silently taking it offline until an
// operator noticed.
func TestHandler_UpdateTenant_InvalidStatus_RefusedAndNotPersisted(t *testing.T) {
	env := buildTestAdminModule(t)
	ctx := context.Background()
	const tenant = "tenant-invalid-status"
	if err := env.Admin.Tenants().Create(ctx, &Tenant{TenantID: tenant}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	body := strings.NewReader(`{"status": "bogus"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/tenants/"+tenant, body)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: "operator-status-1"}))
	w := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an out-of-vocabulary status", w.Code)
	}
	if got := decodedErrorCode(t, w); got != ErrTenantStatusInvalid.Code {
		t.Fatalf("code = %q, want %q", got, ErrTenantStatusInvalid.Code)
	}

	row, err := env.Admin.Tenants().Get(ctx, tenant)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if row.Status != tenancy.TenantStatusActive {
		t.Fatalf("stored status = %q, want %q -- the invalid status must never be persisted", row.Status, tenancy.TenantStatusActive)
	}
}

// TestHandler_RoleWritePaths_BindingAndRoleEventsCarryActor is P2-6's
// regression test, at the real event boundary: both role-management write
// paths (POST /api/v1/admin/roles and POST
// /api/v1/admin/roles/{id}/bindings) must resolve the calling operator and
// install them as the rbac Subject on the ctx they hand to RoleService, so
// rbac's own role-binding and role-changed events carry ActorUserID. On
// unfixed main neither handler resolved the caller at all, so every event
// those writes published carried an empty ActorUserID -- a role-management
// write no audit trail could ever attribute.
func TestHandler_RoleWritePaths_BindingAndRoleEventsCarryActor(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)

	const operator = "operator-rbac-1"
	const tenant = "tenant-rbac-actor"

	var bindingActors []string
	env.Registry.EventBus().Subscribe(rbac.EventRoleBindingAssigned, func(_ context.Context, evt pkgcore.Event) error {
		var p rbac.RoleBindingChangedEvent
		if err := decodeEventPayload(evt.Payload, &p); err != nil {
			return err
		}
		bindingActors = append(bindingActors, p.ActorUserID)
		return nil
	})
	var roleActors []string
	env.Registry.EventBus().Subscribe(rbac.EventRoleChanged, func(_ context.Context, evt pkgcore.Event) error {
		var p rbac.RoleChangedEvent
		if err := decodeEventPayload(evt.Payload, &p); err != nil {
			return err
		}
		roleActors = append(roleActors, p.ActorUserID)
		return nil
	})

	// Write path 1: define a role.
	defineBody := strings.NewReader(fmt.Sprintf(`{"tenantId": %q, "key": "auditor", "permissions": ["%s"]}`, tenant, PermissionAccess))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/roles", defineBody)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: operator}))
	w := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("define-role status = %d, want 201", w.Code)
	}
	if len(roleActors) != 1 || roleActors[0] != operator {
		t.Fatalf("role-changed event ActorUserID = %v, want exactly [%s] -- the defining operator must be on the event", roleActors, operator)
	}

	// Write path 2: bind the role to a user.
	bindBody := strings.NewReader(fmt.Sprintf(`{"tenantId": %q, "userId": "grantee-rbac-1"}`, tenant))
	bindReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/roles/auditor/bindings", bindBody)
	bindReq = bindReq.WithContext(authn.WithPrincipal(bindReq.Context(), authn.Principal{UserID: operator}))
	bindW := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(bindW, bindReq)
	if bindW.Code != http.StatusCreated {
		t.Fatalf("bind-role status = %d, want 201", bindW.Code)
	}
	if len(bindingActors) != 1 || bindingActors[0] != operator {
		t.Fatalf("role-binding event ActorUserID = %v, want exactly [%s] -- the binding operator must be on the event", bindingActors, operator)
	}
}

// TestHandler_AdminSearchUsers_LeavesAuditedTrailNamingOperator is the
// D6-search P1's regression test, at the real composed-HTTP boundary: a
// cross-tenant user search (GET /api/v1/admin/users) must resolve the
// calling operator from the verified Principal and leave exactly one
// tenancy.system_context.entered audit record naming that operator as
// Actor under SystemPurposeAdminCrossTenant -- identically to the D6
// second half (AdminListUserMemberships/MembershipsOf) and to every other
// D2 audited read. On unfixed main AdminSearchUsers never read the
// caller's Principal and SearchService.Users was a wrapper-less
// passthrough to authn.Service.SearchUsers, so a search that returns
// plaintext email and phone from identity data (encrypted at rest) left no
// audit trace of which operator searched the user directory at all.
func TestHandler_AdminSearchUsers_LeavesAuditedTrailNamingOperator(t *testing.T) {
	env := buildTestAdminModule(t)
	ctx := context.Background()

	const operatorID = "operator-search-audited-7"
	const targetEmail = "search-audit-target@example.com"
	if _, err := env.Authn.Service().Register(ctx, authn.RegisterInput{
		Email: targetEmail, Password: "a perfectly fine passphrase", DisplayName: "Search Audit Target",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	var entered []tenancy.SystemContextEnteredEvent
	env.Registry.EventBus().Subscribe(tenancy.EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
		var e tenancy.SystemContextEnteredEvent
		if err := decodeEventPayload(evt.Payload, &e); err != nil {
			return err
		}
		entered = append(entered, e)
		return nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users?email="+url.QueryEscape(targetEmail), nil)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: operatorID}))
	w := httptest.NewRecorder()

	env.Admin.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want %d", w.Code, w.Body.String(), http.StatusOK)
	}
	var resp api.AdminSearchUsersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Users) != 1 || resp.Users[0].Email == nil || *resp.Users[0].Email != targetEmail {
		t.Fatalf("search = %+v, want exactly the registered account for %q", resp.Users, targetEmail)
	}
	if len(entered) != 1 {
		t.Fatalf("AdminSearchUsers published %d tenancy.system_context.entered events, want exactly 1 -- the search must take the audited D2 wrapper, never a direct untrailed read", len(entered))
	}
	if entered[0].Actor != operatorID || entered[0].Purpose != SystemPurposeAdminCrossTenant {
		t.Fatalf("system-context event = %+v, want Actor %q under %s -- the trail must name the OPERATOR, never the searched-for user", entered[0], operatorID, SystemPurposeAdminCrossTenant)
	}
}

// TestHandler_AdminListAuditEvents_FiltersByOnBehalfOf is P2-1's
// end-to-end regression test: an investigator asking "what did THIS
// administrator do during their impersonation sessions?" must get exactly
// the audit rows written while that administrator impersonated a user --
// and no others. Pre-fix the endpoint had no on_behalf_of dimension at
// all: the request below answered 200 with every row in the tenant (the
// silent-failure signature this finding is about -- a plausible-looking
// list with no error), because the on_behalf_of fields the response
// already carried could be read but never queried, and an administrator
// never appears as actor on an impersonation-era row.
func TestHandler_AdminListAuditEvents_FiltersByOnBehalfOf(t *testing.T) {
	env := buildTestAdminModule(t)
	auditRepo := audit.NewRepository(env.DB)

	// Four rows in one tenant: two written during admin-1's impersonation
	// sessions (actor = the impersonated user, on_behalf_of = admin-1),
	// one during admin-2's session, and one ordinary row written directly
	// with no impersonation at all (no on_behalf_of identity).
	insertImpersonationTestEvent(t, auditRepo, "tenant-a", "impersonated-user-a", "admin-1", "notes.note.update")
	insertImpersonationTestEvent(t, auditRepo, "tenant-a", "impersonated-user-b", "admin-1", "notes.note.create")
	insertImpersonationTestEvent(t, auditRepo, "tenant-a", "impersonated-user-c", "admin-2", "notes.note.delete")
	insertTestEvent(t, auditRepo, "tenant-a", "notes.note.read")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit-events?tenantId=tenant-a&onBehalfOf=admin-1", nil)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: "operator-audit-query-1"}))
	w := httptest.NewRecorder()

	env.Admin.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want %d", w.Code, w.Body.String(), http.StatusOK)
	}
	var resp api.AdminListAuditEventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("onBehalfOf=admin-1 returned %d events, want exactly 2 -- admin-1's impersonation-era rows and no others (the pre-fix code ignores the parameter and returns all %d rows, silently)", len(resp.Events), 4)
	}
	actors := make(map[string]bool, len(resp.Events))
	for _, evt := range resp.Events {
		if evt.OnBehalfOfID == nil || *evt.OnBehalfOfID != "admin-1" {
			t.Fatalf("event %s returned for onBehalfOf=admin-1 carries on_behalf_of_id %v, want admin-1", evt.ID, evt.OnBehalfOfID)
		}
		actors[evt.ActorID] = true
	}
	if !actors["impersonated-user-a"] || !actors["impersonated-user-b"] {
		t.Fatalf("onBehalfOf=admin-1 events' actors = %v, want impersonated-user-a and impersonated-user-b -- admin-2's impersonation row and the direct row must be excluded", actors)
	}
}
