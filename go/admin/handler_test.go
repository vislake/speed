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
	"time"

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

// TestHandler_MalformedRequestBody_ReportsRequestBodyInvalid pins the
// decode-failure error code: a genuinely malformed JSON body against
// AdminUpdateTenant must be reported as admin.request_body_invalid, never
// admin.tenant_id_required -- a caller who sent syntactically broken JSON
// was not "missing the tenant id". Proven against the real composed HTTP
// handler, not a bare service call.
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
// pins the same code on the impersonation surface: the start-impersonation
// decode failure must be reported as admin.request_body_invalid, never
// admin.impersonation_target_required (a decode failure is not "you
// forgot the target").
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

// TestPaginate_ExtremeOffsetAndLimit_DoesNotPanic pins the overflow-proof
// clamp: an extreme caller-supplied limit (math.MaxInt, as an HTTP query
// string could carry) combined with a small in-range offset would
// otherwise overflow the offset+limit addition inside paginate, wrap the
// computed end negative, sail through both clamp conditions, and panic
// slicing events[offset:negative]. The clamp must bound the limit by the
// remaining tail BEFORE the addition so the addition itself can never
// overflow.
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

// TestHandler_UpdateTenant_InvalidStatus_RefusedAndNotPersisted pins the
// status vocabulary boundary: a status string outside the API enum's
// closed vocabulary must be refused at the handler boundary (400,
// admin.tenant_status_invalid), never persisted -- tenancy's status gate
// (only tenancy.TenantStatusActive is servable) would otherwise refuse
// every request for that tenant, silently taking it offline until an
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

// TestHandler_RoleWritePaths_BindingAndRoleEventsCarryActor pins the
// actor at the real event boundary: both role-management write paths
// (POST /api/v1/admin/roles and POST /api/v1/admin/roles/{id}/bindings)
// must resolve the calling operator and install them as the rbac Subject
// on the ctx they hand to RoleService, so rbac's own role-binding and
// role-changed events carry ActorUserID -- without it a role-management
// write would publish events no audit trail could ever attribute.
func TestHandler_RoleWritePaths_BindingAndRoleEventsCarryActor(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)

	const operator = "operator-rbac-1"
	const tenant = "tenant-rbac-actor"

	var bindingActors []string
	env.Registry.EventBus().Subscribe(rbac.EventRoleBindingAssigned, func(_ context.Context, evt pkgcore.Event) error {
		var p rbac.RoleBindingChangedEvent
		if err := pkgcore.DecodeEventPayload(evt.Payload, &p); err != nil {
			return err
		}
		bindingActors = append(bindingActors, p.ActorUserID)
		return nil
	})
	var roleActors []string
	env.Registry.EventBus().Subscribe(rbac.EventRoleChanged, func(_ context.Context, evt pkgcore.Event) error {
		var p rbac.RoleChangedEvent
		if err := pkgcore.DecodeEventPayload(evt.Payload, &p); err != nil {
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

// TestHandler_AdminSearchUsers_LeavesAuditedTrailNamingOperator pins the
// search's audited trail at the real composed-HTTP boundary: a
// cross-tenant user search (GET /api/v1/admin/users) must resolve the
// calling operator from the verified Principal and leave exactly one
// tenancy.system_context.entered audit record naming that operator as
// Actor under SystemPurposeAdminCrossTenant -- identically to the
// membership half (AdminListUserMemberships/MembershipsOf) and to every
// other audited cross-tenant read. Without it, a search that returns
// plaintext email and phone from identity data (encrypted at rest) would
// leave no trace of which operator searched the user directory.
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
		if err := pkgcore.DecodeEventPayload(evt.Payload, &e); err != nil {
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

// TestHandler_AdminListAuditEvents_FiltersByOnBehalfOf pins the
// on-behalf-of read dimension end to end: an investigator asking "what did
// THIS administrator do during their impersonation sessions?" must get
// exactly the audit rows written while that administrator impersonated a
// user -- and no others. The endpoint's onBehalfOf query parameter exists
// because an attribute that can only be written and never queried does not
// exist for accountability: an administrator never appears as actor on an
// impersonation-era row, so without the filter the answer would look
// complete (200, no error) while silently omitting everything asked
// for.
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

// TestHandler_NoPrincipal_EveryGatedOperationRefused pins the fail-closed
// gate at the whole-surface level: every principal-gated admin operation
// -- the tenant CRUD writes, the user search and membership halves, the
// impersonation writes, the audit query and export, the role writes and
// the usage and send-record dashboards -- must answer 401
// admin.principal_required when no verified Principal rides the request
// context, whatever the request otherwise says. An anonymous caller is
// nobody an operations console may act as.
func TestHandler_NoPrincipal_EveryGatedOperationRefused(t *testing.T) {
	env := buildTestAdminModule(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"create tenant", http.MethodPost, "/api/v1/admin/tenants", `{"tenantId":"t-anon-1"}`},
		{"update tenant", http.MethodPatch, "/api/v1/admin/tenants/t-anon-1", `{"displayName":"x"}`},
		{"search users", http.MethodGet, "/api/v1/admin/users?email=anon@example.com", ""},
		{"list memberships", http.MethodGet, "/api/v1/admin/users/u-anon-1/memberships", ""},
		{"start impersonation", http.MethodPost, "/api/v1/admin/impersonation", `{"targetUserId":"u"}`},
		{"end impersonation", http.MethodDelete, "/api/v1/admin/impersonation/g-anon-1", ""},
		{"list audit events", http.MethodGet, "/api/v1/admin/audit-events", ""},
		{"export audit events", http.MethodPost, "/api/v1/admin/audit-events/export", `{"tenantId":"t-anon-1"}`},
		{"define role", http.MethodPost, "/api/v1/admin/roles", `{"key":"x"}`},
		{"create role binding", http.MethodPost, "/api/v1/admin/roles/x/bindings", `{"userId":"u"}`},
		{"usage summary", http.MethodGet, "/api/v1/admin/usage-summary", ""},
		{"list send records", http.MethodGet, "/api/v1/admin/notifications/send-records", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			env.Admin.handler.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, body = %s, want 401 for an unauthenticated request", w.Code, w.Body.String())
			}
			if got := decodedErrorCode(t, w); got != ErrPrincipalRequired.Code {
				t.Fatalf("code = %q, want %q", got, ErrPrincipalRequired.Code)
			}
		})
	}
}

// TestHandler_TenantLedger_CreateGetList_OverHTTP drives the tenant
// ledger's operator-facing surface end to end: POST registers a ledger row
// (with display name and notes), a duplicate POST is refused as a conflict
// (admin.tenant_already_exists) rather than silently overwriting, GET
// serves the row back, GET of an unknown id answers
// admin.tenant_not_found, and the list endpoint serves every matching row
// through its status/cursor/limit parameters.
func TestHandler_TenantLedger_CreateGetList_OverHTTP(t *testing.T) {
	env := buildTestAdminModule(t)
	const operator = "operator-ledger-1"
	principalReq := func(method, path, body string) *http.Request {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		return req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: operator}))
	}

	// Create: 201 with the full wire shape.
	createBody := `{"tenantId":"tenant-crud-1","displayName":"Crud Co","notes":"ops note"}`
	w := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodPost, "/api/v1/admin/tenants", createBody))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s, want 201", w.Code, w.Body.String())
	}
	var created api.AdminTenant
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created tenant: %v", err)
	}
	if created.TenantID != "tenant-crud-1" || created.DisplayName != "Crud Co" || created.Notes != "ops note" {
		t.Fatalf("created tenant = %+v, want the submitted id/displayName/notes", created)
	}
	if created.CreatedBy != operator || created.Status != api.AdminTenantStatus(tenancy.TenantStatusActive) {
		t.Fatalf("created tenant = %+v, want operator-attributed and active", created)
	}

	// A duplicate registration is a conflict, never a silent overwrite.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodPost, "/api/v1/admin/tenants", createBody))
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409", w.Code)
	}
	if got := decodedErrorCode(t, w); got != ErrTenantAlreadyExists.Code {
		t.Fatalf("duplicate create code = %q, want %q", got, ErrTenantAlreadyExists.Code)
	}

	// A syntactically broken body is a request-body error, never a
	// tenant-id error -- a caller who sent invalid JSON was not "missing
	// the tenant id".
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodPost, "/api/v1/admin/tenants", `{"tenantId": 42}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed create status = %d, want 400", w.Code)
	}
	if got := decodedErrorCode(t, w); got != ErrRequestBodyInvalid.Code {
		t.Fatalf("malformed create code = %q, want %q", got, ErrRequestBodyInvalid.Code)
	}

	// Get serves the row back; an unknown id is a coded 404.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodGet, "/api/v1/admin/tenants/tenant-crud-1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", w.Code)
	}
	var got api.AdminTenant
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.TenantID != "tenant-crud-1" {
		t.Fatalf("get = %+v, want tenant-crud-1", got)
	}

	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodGet, "/api/v1/admin/tenants/no-such-tenant", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("get-unknown status = %d, want 404", w.Code)
	}
	if got := decodedErrorCode(t, w); got != ErrTenantNotFound.Code {
		t.Fatalf("get-unknown code = %q, want %q", got, ErrTenantNotFound.Code)
	}

	// List: a second tenant, then ?status/&cursor/&limit all answered.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodPost, "/api/v1/admin/tenants", `{"tenantId":"tenant-crud-2","displayName":"Crud Two Co"}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("second create status = %d, want 201", w.Code)
	}

	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodGet, "/api/v1/admin/tenants?status=active&cursor=&limit=10", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var list api.AdminListTenantsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	ids := make(map[string]bool, len(list.Tenants))
	for _, row := range list.Tenants {
		ids[row.TenantID] = true
	}
	if !ids["tenant-crud-1"] || !ids["tenant-crud-2"] {
		t.Fatalf("list = %+v, want both created tenants under status=active", list.Tenants)
	}
	if len(list.Tenants) != 2 {
		t.Fatalf("list = %+v, want exactly the two created tenants", list.Tenants)
	}
}

// TestHandler_UpdateTenant_SuspendResume_OverHTTP drives a full
// ledger-edit cycle through the real PATCH surface: a display-name/notes/
// status/suspendedReason patch lands on the row (the suspended reason is
// served back on the wire and the tenant's Status gate would refuse
// traffic), and a later resume PATCH clears only the suspension -- the
// historical reason stays on the row, per Tenant's own model contract.
// The missing-tenant PATCH answers admin.tenant_not_found, so a typo in
// the id can never silently edit nothing.
func TestHandler_UpdateTenant_SuspendResume_OverHTTP(t *testing.T) {
	env := buildTestAdminModule(t)
	ctx := context.Background()
	const operator = "operator-ledger-2"
	const tenant = "tenant-suspend-http"

	if err := env.Admin.Tenants().Create(ctx, &Tenant{TenantID: tenant, DisplayName: "Suspend Co"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	req := func(body string) *http.Request {
		r := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/tenants/"+tenant, strings.NewReader(body))
		return r.WithContext(authn.WithPrincipal(r.Context(), authn.Principal{UserID: operator}))
	}

	// Suspend with the full patch: rename, note, status and reason.
	w := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req(`{"displayName":"Suspended Co","notes":"under review","status":"suspended","suspendedReason":"abuse report #12"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("suspend status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var suspended api.AdminTenant
	if err := json.Unmarshal(w.Body.Bytes(), &suspended); err != nil {
		t.Fatalf("decode suspend response: %v", err)
	}
	if suspended.DisplayName != "Suspended Co" || suspended.Notes != "under review" {
		t.Fatalf("suspended tenant = %+v, want the renamed/annotated row", suspended)
	}
	if suspended.Status != api.AdminTenantStatus(tenancy.TenantStatusSuspended) {
		t.Fatalf("suspended status = %q, want suspended", suspended.Status)
	}
	if suspended.SuspendedReason == nil || *suspended.SuspendedReason != "abuse report #12" {
		t.Fatalf("suspended reason = %v, want abuse report #12 on the wire", suspended.SuspendedReason)
	}
	if suspended.SuspendedAt == nil {
		t.Fatal("SuspendedAt is nil on a suspended tenant, want the suspension timestamp")
	}

	// The stored row agrees, and tenancy's own resolver now reports
	// suspended -- the real teeth of the ledger edit.
	row, err := env.Admin.Tenants().Get(ctx, tenant)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if row.Status != tenancy.TenantStatusSuspended {
		t.Fatalf("stored status = %q, want suspended", row.Status)
	}
	if status, err := env.Admin.Tenants().Status(ctx, pkgcore.TenantID(tenant)); err != nil || status != tenancy.TenantStatusSuspended {
		t.Fatalf("Status() = %q, %v, want suspended", status, err)
	}

	// Resume: status flips back; the reason stays as a historical note.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req(`{"status":"active"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var resumed api.AdminTenant
	if err := json.Unmarshal(w.Body.Bytes(), &resumed); err != nil {
		t.Fatalf("decode resume response: %v", err)
	}
	if resumed.Status != api.AdminTenantStatus(tenancy.TenantStatusActive) {
		t.Fatalf("resumed status = %q, want active", resumed.Status)
	}
	if resumed.SuspendedAt != nil {
		t.Fatalf("resumed SuspendedAt = %v, want nil -- the tenant is not currently suspended", resumed.SuspendedAt)
	}
	if resumed.SuspendedReason == nil || *resumed.SuspendedReason != "abuse report #12" {
		t.Fatalf("resumed SuspendedReason = %v, want the historical reason retained", resumed.SuspendedReason)
	}

	// A PATCH naming a tenant the ledger does not know is a coded 404.
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/tenants/no-such-tenant", strings.NewReader(`{"displayName":"x"}`))
	r = r.WithContext(authn.WithPrincipal(r.Context(), authn.Principal{UserID: operator}))
	env.Admin.handler.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("update-unknown status = %d, want 404", w.Code)
	}
	if got := decodedErrorCode(t, w); got != ErrTenantNotFound.Code {
		t.Fatalf("update-unknown code = %q, want %q", got, ErrTenantNotFound.Code)
	}
}

// TestHandler_ImpersonationLifecycle_StartListEnd_OverHTTP drives the
// impersonation pipeline's operator surface over the real composed stack:
// a POST starts a grant for a real member user (201 with the grant's
// shape on the wire), the list endpoint serves the live grant, DELETE
// ends it (the ended-by operator and timestamp ride the wire), and a
// second DELETE answers admin.impersonation_grant_ended -- ending an
// already-ended grant is a conflict, never a silent second success.
func TestHandler_ImpersonationLifecycle_StartListEnd_OverHTTP(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)

	const operator = "operator-impersonation-http"
	const tenant = pkgcore.TenantID("tenant-impersonation-http")
	root, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Impersonation HTTP Co", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	targetID := registerTestUser(t, env, "impersonation-http-target@example.com", "")
	if _, addErr := env.Org.Members().Add(pkgcore.WithTenant(context.Background(), tenant), targetID, root.ID); addErr != nil {
		t.Fatalf("Members().Add() error = %v", addErr)
	}

	principalReq := func(method, path, body string) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		return r.WithContext(authn.WithPrincipal(r.Context(), authn.Principal{UserID: operator}))
	}

	// Start with an explicit locale.
	w := httptest.NewRecorder()
	body := fmt.Sprintf(`{"targetUserId":%q,"targetTenantId":%q,"reason":"support ticket #7","locale":"zh-CN"}`, targetID, tenant)
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodPost, "/api/v1/admin/impersonation", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("start status = %d, body = %s, want 201", w.Code, w.Body.String())
	}
	var grant api.AdminImpersonationGrant
	if err := json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if grant.ID == "" || grant.AdminUserID != operator || grant.TargetUserID != targetID || grant.TargetTenantID != string(tenant) || grant.Reason != "support ticket #7" {
		t.Fatalf("grant = %+v, want the full submitted shape attributed to the operator", grant)
	}
	if grant.EndedAt != nil || grant.EndedBy != nil {
		t.Fatalf("grant = %+v, want a live grant with no end recorded", grant)
	}

	// List serves the live grant back.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodGet, "/api/v1/admin/impersonation", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", w.Code)
	}
	var list api.AdminListImpersonationGrantsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, g := range list.Grants {
		if g.ID == grant.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("list = %+v, want the just-started grant %s among the live grants", list.Grants, grant.ID)
	}

	// End it, then prove ending it again is refused as a conflict.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodDelete, "/api/v1/admin/impersonation/"+grant.ID, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("end status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var ended api.AdminImpersonationGrant
	if err := json.Unmarshal(w.Body.Bytes(), &ended); err != nil {
		t.Fatalf("decode ended grant: %v", err)
	}
	if ended.EndedAt == nil || ended.EndedBy == nil || *ended.EndedBy != operator {
		t.Fatalf("ended grant = %+v, want EndedAt set and EndedBy naming the operator", ended)
	}

	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodDelete, "/api/v1/admin/impersonation/"+grant.ID, ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("second end status = %d, want 409", w.Code)
	}
	if got := decodedErrorCode(t, w); got != ErrImpersonationGrantEnded.Code {
		t.Fatalf("second end code = %q, want %q", got, ErrImpersonationGrantEnded.Code)
	}
}

// TestHandler_SearchUsers_FilterParameters_ShapeTheResponse drives the
// search surface's remaining filter parameters over HTTP: a phone query
// goes out as an exact-match search (answering empty for a number no
// account holds), and a displayNamePrefix query with a limit returns
// exactly that many matches with the accounts' shape -- email present
// when the account has one, phone absent from the wire when it has none.
func TestHandler_SearchUsers_FilterParameters_ShapeTheResponse(t *testing.T) {
	env := buildTestAdminModule(t)
	const operator = "operator-search-http"

	ctx := context.Background()
	for _, email := range []string{"beta-founder@example.com", "beta-boss@example.com"} {
		if _, err := env.Authn.Service().Register(ctx, authn.RegisterInput{
			Email: email, Password: "a perfectly fine passphrase", DisplayName: "Beta " + email,
		}); err != nil {
			t.Fatalf("Register(%s) error = %v", email, err)
		}
	}

	principalReq := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		return r.WithContext(authn.WithPrincipal(r.Context(), authn.Principal{UserID: operator}))
	}

	// Phone: an exact-match search that no account satisfies is an empty
	// 200, never an error.
	w := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq("/api/v1/admin/users?phone="+url.QueryEscape("+15551239876")))
	if w.Code != http.StatusOK {
		t.Fatalf("phone search status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var byPhone api.AdminSearchUsersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &byPhone); err != nil {
		t.Fatalf("decode phone search: %v", err)
	}
	if len(byPhone.Users) != 0 {
		t.Fatalf("phone search = %+v, want no account holding that number", byPhone.Users)
	}

	// Prefix + limit: exactly the asked-for number of matches, newest
	// accounts excluded, and each match shapes email/phone independently.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq("/api/v1/admin/users?displayNamePrefix="+url.QueryEscape("Beta ")+"&limit=1"))
	if w.Code != http.StatusOK {
		t.Fatalf("prefix search status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var byPrefix api.AdminSearchUsersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &byPrefix); err != nil {
		t.Fatalf("decode prefix search: %v", err)
	}
	if len(byPrefix.Users) != 1 {
		t.Fatalf("prefix search with limit=1 = %+v, want exactly one account", byPrefix.Users)
	}
	u := byPrefix.Users[0]
	if u.Email == nil || !strings.HasPrefix(*u.Email, "beta-") {
		t.Fatalf("prefix search user = %+v, want an account with its email present", u)
	}
	if u.Phone != nil {
		t.Fatalf("prefix search user = %+v, want Phone absent -- no account holds a phone", u)
	}
}

// TestHandler_ListUserMemberships_OverHTTP drives the membership
// composition through its real route: a user holding memberships in two
// of three ledger-registered tenants is answered with exactly those two
// tenant ids -- the third tenant's ledger row alone never qualifies.
func TestHandler_ListUserMemberships_OverHTTP(t *testing.T) {
	env := buildTestAdminModule(t)
	ctx := context.Background()
	const operator = "operator-memberships-http"

	user, err := env.Authn.Service().Register(ctx, authn.RegisterInput{
		Email: "memberships-http-user@example.com", Password: "a perfectly fine passphrase", DisplayName: "Membership HTTP User",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	var memberTenants []pkgcore.TenantID
	for _, id := range []string{"tenant-membership-a", "tenant-membership-b"} {
		tenant := pkgcore.TenantID(id)
		root, createErr := env.Org.Tree().CreateRoot(pkgcore.WithTenant(ctx, tenant), "Membership Co "+id, "workspace")
		if createErr != nil {
			t.Fatalf("CreateRoot(%s) error = %v", id, createErr)
		}
		if _, addErr := env.Org.Members().Add(pkgcore.WithTenant(ctx, tenant), user.ID, root.ID); addErr != nil {
			t.Fatalf("Members().Add(%s) error = %v", id, addErr)
		}
		memberTenants = append(memberTenants, tenant)
	}
	// A third ledger tenant the user never joined.
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(ctx, pkgcore.TenantID("tenant-membership-c")), "Membership Co C", "workspace"); err != nil {
		t.Fatalf("CreateRoot(c) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users/"+user.ID+"/memberships", nil)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: operator}))
	w := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var resp api.AdminListUserMembershipsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.TenantIds) != 2 {
		t.Fatalf("memberships = %v, want exactly the two joined tenants", resp.TenantIds)
	}
	got := make(map[string]bool, len(resp.TenantIds))
	for _, id := range resp.TenantIds {
		got[id] = true
	}
	for _, want := range memberTenants {
		if !got[string(want)] {
			t.Fatalf("memberships = %v, want %s among them", resp.TenantIds, want)
		}
	}
	if got["tenant-membership-c"] {
		t.Fatalf("memberships = %v, want tenant-membership-c excluded -- the user never joined it", resp.TenantIds)
	}
}

// insertAuditEventAt inserts one audit event whose OccurredAt is exactly
// occurredAt -- the shared helpers' now-stamped rows cannot express the
// deterministic time ordering the pagination and window filters below
// assert.
func insertAuditEventAt(t *testing.T, repo *audit.Repository, evt *audit.AuditEvent) {
	t.Helper()
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
}

// TestHandler_AuditEvents_WindowPaginationAndMapping_OverHTTP drives the
// audit-query shell's remaining parameters through the real route: the
// from/to/success window narrows the answer to exactly the matching
// events, offset/limit page a newest-first list (an offset past the end is
// an empty 200, never an error), and a failed event inside the window
// carries its failure reason and the actor's display name on the wire --
// the fields an investigator reading the trail actually needs.
func TestHandler_AuditEvents_WindowPaginationAndMapping_OverHTTP(t *testing.T) {
	env := buildTestAdminModule(t)
	auditRepo := audit.NewRepository(env.DB)

	base := time.Now().UTC().Truncate(time.Millisecond)
	mk := func(id, action string, at time.Time, success bool, failureReason string) *audit.AuditEvent {
		evt := &audit.AuditEvent{TenantID: "tenant-audit-window", Action: action, OccurredAt: at}
		evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "audit-actor-" + id, DisplayName: "Actor " + id})
		evt.SetResource(audit.Resource{Type: "note", ID: "note-1"})
		evt.SetResult(audit.Result{Success: success, FailureReason: failureReason})
		return evt
	}
	// Six rows at one-second spacing, newest last; C and E failed.
	at := func(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }
	insertAuditEventAt(t, auditRepo, mk("a", "notes.note.read", at(-5), true, ""))
	insertAuditEventAt(t, auditRepo, mk("b", "notes.note.read", at(-4), true, ""))
	insertAuditEventAt(t, auditRepo, mk("c", "notes.note.update", at(-3), false, "rate limited"))
	insertAuditEventAt(t, auditRepo, mk("d", "notes.note.create", at(-2), true, ""))
	insertAuditEventAt(t, auditRepo, mk("e", "notes.note.delete", at(-1), false, "no such note"))
	insertAuditEventAt(t, auditRepo, mk("f", "notes.note.read", at(0), true, ""))

	req := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		return r.WithContext(authn.WithPrincipal(r.Context(), authn.Principal{UserID: "operator-audit-window"}))
	}

	// Window: strictly after from, strictly before to, failures only --
	// rows B(-4s), C(-3s), D(-2s) and E(-1s) sit inside, of which C and E
	// failed.
	from := url.QueryEscape(at(-4).Add(500 * time.Millisecond).Format(time.RFC3339Nano))
	to := url.QueryEscape(at(0).Add(-500 * time.Millisecond).Format(time.RFC3339Nano))
	w := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req(fmt.Sprintf(
		"/api/v1/admin/audit-events?tenantId=tenant-audit-window&from=%s&to=%s&success=false", from, to)))
	if w.Code != http.StatusOK {
		t.Fatalf("window status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var filtered api.AdminListAuditEventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("decode window response: %v", err)
	}
	actorIDs := make(map[string]bool, len(filtered.Events))
	failureReasons := make(map[string]string, len(filtered.Events))
	for _, evt := range filtered.Events {
		actorIDs[evt.ActorID] = true
		if evt.FailureReason != nil {
			failureReasons[evt.ActorID] = *evt.FailureReason
		}
		if evt.ActorDisplayName == nil {
			t.Fatalf("event %s carries no actor display name on the wire, want the resolved name", evt.ActorID)
		}
	}
	if len(filtered.Events) != 2 || !actorIDs["audit-actor-c"] || !actorIDs["audit-actor-e"] {
		t.Fatalf("window+success=false = %+v, want exactly the failed C and E events in the window", filtered.Events)
	}
	if failureReasons["audit-actor-c"] != "rate limited" || failureReasons["audit-actor-e"] != "no such note" {
		t.Fatalf("window failure reasons = %v, want each event's own reason on the wire", failureReasons)
	}

	// Actor narrowing: only that operator's own rows answer.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req("/api/v1/admin/audit-events?tenantId=tenant-audit-window&actor=audit-actor-d"))
	if w.Code != http.StatusOK {
		t.Fatalf("actor status = %d, want 200", w.Code)
	}
	var byActor api.AdminListAuditEventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &byActor); err != nil {
		t.Fatalf("decode actor response: %v", err)
	}
	if len(byActor.Events) != 1 || byActor.Events[0].ActorID != "audit-actor-d" {
		t.Fatalf("actor=audit-actor-d = %+v, want exactly that actor's own event", byActor.Events)
	}

	// Pagination: limit=1 is the newest event; offset=1&limit=1 the next;
	// offset past the end is an empty 200.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req("/api/v1/admin/audit-events?tenantId=tenant-audit-window&limit=1"))
	if w.Code != http.StatusOK {
		t.Fatalf("limit status = %d, want 200", w.Code)
	}
	var page1 api.AdminListAuditEventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode limit response: %v", err)
	}
	if len(page1.Events) != 1 || page1.Events[0].ActorID != "audit-actor-f" {
		t.Fatalf("limit=1 = %+v, want exactly the newest (f) event", page1.Events)
	}

	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req("/api/v1/admin/audit-events?tenantId=tenant-audit-window&offset=1&limit=1"))
	if w.Code != http.StatusOK {
		t.Fatalf("offset status = %d, want 200", w.Code)
	}
	var page2 api.AdminListAuditEventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode offset response: %v", err)
	}
	if len(page2.Events) != 1 || page2.Events[0].ActorID != "audit-actor-e" {
		t.Fatalf("offset=1&limit=1 = %+v, want exactly the e event (second newest)", page2.Events)
	}

	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req("/api/v1/admin/audit-events?tenantId=tenant-audit-window&offset=99"))
	if w.Code != http.StatusOK {
		t.Fatalf("offset-past-end status = %d, want 200", w.Code)
	}
	var pastEnd api.AdminListAuditEventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pastEnd); err != nil {
		t.Fatalf("decode past-end response: %v", err)
	}
	if len(pastEnd.Events) != 0 {
		t.Fatalf("offset=99 = %+v, want an empty page -- never a panic", pastEnd.Events)
	}
}

// TestPaginate_NegativeOffsetAndLimit_ClampToAValidSlice pins the three
// clamp branches the extreme-limit test's in-range inputs cannot reach:
// a negative offset is clamped to 0, a negative limit ("no bound") to the
// slice length, and an offset at or past the end yields an empty result.
func TestPaginate_NegativeOffsetAndLimit_ClampToAValidSlice(t *testing.T) {
	events := []audit.AuditEvent{{ID: "evt-0"}, {ID: "evt-1"}}

	got := paginate(events, -1, 1)
	if len(got) != 1 || got[0].ID != "evt-0" {
		t.Fatalf("paginate(offset=-1) = %+v, want the slice from its start", got)
	}
	got = paginate(events, 1, -1)
	if len(got) != 1 || got[0].ID != "evt-1" {
		t.Fatalf("paginate(limit=-1) = %+v, want the whole tail from offset 1", got)
	}
	got = paginate(events, 2, 5)
	if len(got) != 0 {
		t.Fatalf("paginate(offset=len) = %+v, want an empty result", got)
	}
	got = paginate(events, 5, 0)
	if len(got) != 0 {
		t.Fatalf("paginate(offset beyond len) = %+v, want an empty result", got)
	}
}

// TestHandler_RoleSurface_CatalogDescriptionAndNodeScopedBinding_OverHTTP
// drives the role-management surface's remaining branches through its
// real routes: the declared-permission catalog serves the frozen list
// every module declared, a role definition carrying a descriptionKey
// round-trips that key onto the wire, and a binding naming an org node
// carries the node id back in its 201 answer -- while an empty tenant id
// on either write is refused up front with admin.tenant_id_required.
func TestHandler_RoleSurface_CatalogDescriptionAndNodeScopedBinding_OverHTTP(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)

	const operator = "operator-roles-http"
	const tenant = "tenant-roles-http"
	principalReq := func(method, path, body string) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		return r.WithContext(authn.WithPrincipal(r.Context(), authn.Principal{UserID: operator}))
	}

	// The declared-permission catalog is served as-is.
	w := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodGet, "/api/v1/admin/roles", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("catalog status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var catalog api.AdminListDeclaredPermissionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	foundAccess := false
	for _, p := range catalog.Permissions {
		if p == PermissionAccess {
			foundAccess = true
		}
	}
	if !foundAccess {
		t.Fatalf("catalog = %v, want admin:access among the declared permissions", catalog.Permissions)
	}

	// Define a role with a descriptionKey; the key rides the 201 body.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodPost, "/api/v1/admin/roles",
		fmt.Sprintf(`{"tenantId":%q,"key":"auditor-http","permissions":["%s"],"descriptionKey":"roles.auditor.description"}`, tenant, PermissionAuditRead)))
	if w.Code != http.StatusCreated {
		t.Fatalf("define status = %d, body = %s, want 201", w.Code, w.Body.String())
	}
	var role api.AdminRole
	if err := json.Unmarshal(w.Body.Bytes(), &role); err != nil {
		t.Fatalf("decode role: %v", err)
	}
	if role.Key != "auditor-http" || role.TenantID != tenant || role.DescriptionKey != "roles.auditor.description" {
		t.Fatalf("role = %+v, want the submitted key/tenant/descriptionKey", role)
	}
	if len(role.Permissions) != 1 || role.Permissions[0] != PermissionAuditRead {
		t.Fatalf("role permissions = %v, want exactly admin:audit_read", role.Permissions)
	}

	// Bind with an org-node scope; the node id rides the 201 body back.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodPost, "/api/v1/admin/roles/auditor-http/bindings",
		fmt.Sprintf(`{"tenantId":%q,"userId":"grantee-http-1","nodeId":"node-http-1"}`, tenant)))
	if w.Code != http.StatusCreated {
		t.Fatalf("bind status = %d, body = %s, want 201", w.Code, w.Body.String())
	}
	var binding api.AdminRoleBinding
	if err := json.Unmarshal(w.Body.Bytes(), &binding); err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	if binding.TenantID != tenant || binding.UserID != "grantee-http-1" || binding.Role != "auditor-http" {
		t.Fatalf("binding = %+v, want the submitted tenant/user/role", binding)
	}
	if binding.NodeID == nil || *binding.NodeID != "node-http-1" {
		t.Fatalf("binding.NodeID = %v, want node-http-1 on the wire", binding.NodeID)
	}

	// The up-front tenant refusal: an empty tenant names no catalog to
	// write into.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodPost, "/api/v1/admin/roles", `{"key":"x","permissions":[]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("define-no-tenant status = %d, want 400", w.Code)
	}
	if got := decodedErrorCode(t, w); got != ErrTenantIDRequired.Code {
		t.Fatalf("define-no-tenant code = %q, want %q", got, ErrTenantIDRequired.Code)
	}

	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, principalReq(http.MethodPost, "/api/v1/admin/roles/auditor-http/bindings", `{"userId":"grantee-http-2"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bind-no-tenant status = %d, want 400", w.Code)
	}
	if got := decodedErrorCode(t, w); got != ErrTenantIDRequired.Code {
		t.Fatalf("bind-no-tenant code = %q, want %q", got, ErrTenantIDRequired.Code)
	}
}
