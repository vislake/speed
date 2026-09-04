package main

// admin_flow_test.go drives go/admin round 1's mandatory-first-consumer
// proof end to end, through the exact composed HTTP stack every other
// module's own flow test drives (authn+tenancy(+impersonation)
// middleware, real handlers, real dbkit-backed SQLite storage, none of it
// mocked). It follows org_flow_test.go's and notification_flow_test.go's
// wire-shape discipline: responses decode into structs mirroring the JSON
// on the wire, never the spec-generated api.Admin* types this app must not
// import.
//
// Two scenarios, matching the round's own acceptance criteria:
//
//   - TestAdminFlow_SearchMembershipsAndAudit_EndToEnd: D6 (cross-tenant
//     user search), D6+D2 (membership composition) and D7 (audit query),
//     driven by an operator looking up a real user and reading back which
//     tenant they belong to and what happened there.
//   - TestAdminFlow_Impersonation_EndToEnd: D5's full pipeline -- start,
//     one request made AS the impersonated identity, the dual-identity
//     audit trail, the mandatory security notification landing in the
//     target's own inbox, and the grant no longer working once ended.
//
// D5's five mandatory properties are already pinned exhaustively at the
// unit level in go/admin/impersonation_service_test.go and
// go/admin/pipeline_test.go; this file's job is the end-to-end WIRING
// proof -- that a real operator token, a real grant and a real subsequent
// request compose correctly through this app's own middleware chain and
// demo identity layer, not a re-proof of the mechanism itself.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/go/rbac"
)

// The wire shapes this suite decodes, field-named after admin's own
// generated JSON (go/admin/api/openapi.yaml) rather than importing its
// generated types -- the same posture every other flow test in this
// package takes toward the module it drives.
type (
	adminTenant struct {
		TenantID string `json:"tenantId"`
		Status   string `json:"status"`
	}
	adminUser struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
		Email       string `json:"email"`
	}
	adminSearchUsersResponse struct {
		Users []adminUser `json:"users"`
	}
	adminListMembershipsResponse struct {
		TenantIds []string `json:"tenantIds"`
	}
	adminGrant struct {
		ID             string     `json:"id"`
		AdminUserID    string     `json:"adminUserId"`
		TargetUserID   string     `json:"targetUserId"`
		TargetTenantID string     `json:"targetTenantId"`
		ExpiresAt      time.Time  `json:"expiresAt"`
		EndedAt        *time.Time `json:"endedAt"`
	}
	adminListGrantsResponse struct {
		Grants []adminGrant `json:"grants"`
	}
	adminAuditEvent struct {
		ID           string `json:"id"`
		ActorID      string `json:"actorId"`
		OnBehalfOfID string `json:"onBehalfOfId"`
		Action       string `json:"action"`
		ResourceID   string `json:"resourceId"`
		TenantID     string `json:"tenantId"`
	}
	adminListAuditEventsResponse struct {
		Events []adminAuditEvent `json:"events"`
	}
)

// adminRequest issues method against srv.URL+path, authenticated as token,
// with extraHeaders applied after the standard ones (X-Admin-Impersonation
// in particular -- there is no dedicated subjectUserID parameter here the
// way orgRequest/notifRequest have, because every admin operation resolves
// its caller from the verified Principal alone, never a demo header).
func adminRequest(t *testing.T, srv *httptest.Server, method, path, token string, body any, wantStatus int, out any, extraHeaders map[string]string) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, resp.StatusCode, wantStatus, respBody)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response for %s %s: %v", method, path, err)
		}
	}
}

// buildAdminTestServer composes buildServer's real output with the demo
// accounts (and admin's own demo platform-staff account) seeded, and a
// capturing mailer so org invitation tokens can be recovered the same way
// org_flow_test.go's buildOrgTestServer does.
func buildAdminTestServer(t *testing.T) (*httptest.Server, serverConfig, *capturingMailer) {
	t.Helper()

	cfg := testConfig(t)
	cfg.DemoUsersPassword = demoSeedPassword
	mailer := &capturingMailer{}
	cfg.Mailer = mailer

	handler, cleanup, err := buildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, cfg, mailer
}

// platformStaffToken signs the seeded demo platform-staff account in. Its
// only membership is rbac.SystemDomain (seedDemoPlatformStaff's own
// contract), so no tenant_id request is even needed for it to resolve
// there -- but naming it explicitly keeps this test readable regardless.
func platformStaffToken(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	status, code, token := demoLogin(t, srv, demoPlatformStaffEmail, demoSeedPassword, rbac.SystemDomain)
	if status != http.StatusOK || token == "" {
		t.Fatalf("platform-staff login status = %d code = %q, want 200 with a token", status, code)
	}
	return token
}

// TestAdminFlow_SearchMembershipsAndAudit_EndToEnd is D6+D2+D7's
// acceptance shape: an operator finds a real user by email, reads back
// which tenant they actually belong to (a genuine org.memberships row,
// created through org's real invite/accept flow -- not the separate
// authn-level "membership" seedDemoUsers grants, which D6's
// MembershipsOf never consults), and queries that tenant's audit trail.
func TestAdminFlow_SearchMembershipsAndAudit_EndToEnd(t *testing.T) {
	srv, cfg, mailer := buildAdminTestServer(t)
	staffToken := platformStaffToken(t, srv)

	// Build a real tenant with a real member: an owner creates the root,
	// invites a fresh account, and that account accepts -- the exact
	// sequence org_flow_test.go proves creates a genuine org.memberships
	// row. The root's creation is also what lazily registers "tenant-acme"
	// in admin's own D3 ledger (org.node.created -> handleOrgNodeCreated),
	// which D7's cross-tenant query path (used implicitly below via the
	// single-tenant path) and D6's MembershipsOf both depend on.
	const inviterUserID = "user-admin-flow-owner"
	const targetEmail = "admin-flow-target@example.com"
	inviterToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "admin-flow-owner")
	targetToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "admin-flow-target")

	var root orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", inviterToken, "",
		map[string]string{"name": "Admin Flow Co", "kind": "group"}, &root)

	// D3: the ledger picks up tenant-acme the moment the root is created,
	// with no operator action at all.
	var tenants adminTenant
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/tenants/tenant-acme", staffToken, nil, http.StatusOK, &tenants, nil)
	if tenants.TenantID != "tenant-acme" || tenants.Status != "active" {
		t.Fatalf("GET tenant ledger row = %+v, want tenant-acme/active (lazily registered by org.node.created)", tenants)
	}

	// D6, first half: cross-tenant search by email -- resolved BEFORE the
	// invitation is accepted, because org's SubjectResolver (demoOrgSubjectResolver)
	// identifies the accepting caller ONLY from the X-Demo-User-Id header
	// it is given, never from the verified Principal (its own doc comment
	// says so explicitly); the membership org creates is therefore bound
	// to whatever id that header names, and it must be this account's REAL
	// authn id for D6's later membership lookup (which is keyed on that
	// same real id) to find it.
	var searched adminSearchUsersResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/users?email="+targetEmail, staffToken, nil, http.StatusOK, &searched, nil)
	if len(searched.Users) != 1 || searched.Users[0].Email != targetEmail {
		t.Fatalf("search by email = %+v, want exactly one user matching %q", searched.Users, targetEmail)
	}
	targetID := searched.Users[0].ID

	var invitation orgInvitation
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations", inviterToken, inviterUserID,
		map[string]string{"email": targetEmail, "nodeId": root.ID}, &invitation)

	mail := mailer.last(t)
	inviteToken := tokenFromMail(t, mail)
	var membership orgMembership
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations/accept", targetToken, targetID,
		map[string]string{"token": inviteToken}, &membership)
	if membership.NodeID != root.ID || membership.Status != "active" || membership.UserID != targetID {
		t.Fatalf("membership after accept = %+v, want an active membership at %q for %q", membership, root.ID, targetID)
	}

	// A real, tenant-scoped audit trail entry to find later: creating a
	// note under tenant-acme publishes AuditActionNoteCreate through the
	// SAME shared bus/persister every other flow test relies on.
	createNoteAs(t, srv, inviterToken, "an admin-flow note")

	// D6 + D2, second half: which tenants this user belongs to, composed
	// by looping admin's own D3 ledger under tenancy.WithSystemContext and
	// calling org's existing, unmodified per-tenant membership method.
	var memberships adminListMembershipsResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/users/"+targetID+"/memberships", staffToken, nil, http.StatusOK, &memberships, nil)
	if len(memberships.TenantIds) != 1 || memberships.TenantIds[0] != "tenant-acme" {
		t.Fatalf("memberships of %q = %+v, want exactly [tenant-acme]", targetID, memberships.TenantIds)
	}

	// D7: query that tenant's audit trail and find the note-create event.
	var events adminListAuditEventsResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/audit-events?tenantId=tenant-acme&action=notes.note.create", staffToken, nil, http.StatusOK, &events, nil)
	if len(events.Events) == 0 {
		t.Fatal("audit query for tenant-acme's notes.note.create found no events")
	}
	for _, evt := range events.Events {
		if evt.TenantID != "tenant-acme" {
			t.Fatalf("audit event %+v carries a foreign tenant id, want only tenant-acme", evt)
		}
	}
}

// TestAdminFlow_Impersonation_EndToEnd is D5's full pipeline, exercised
// through the real composed stack: start, one impersonated request that
// actually creates data attributed to the target, the dual-identity audit
// row, the mandatory notification landing in the target's own inbox, and
// the ended grant no longer taking effect.
func TestAdminFlow_Impersonation_EndToEnd(t *testing.T) {
	srv, _, _ := buildAdminTestServer(t)
	staffToken := platformStaffToken(t, srv)

	// The impersonation target is the seeded demo-owner account: it holds
	// every permission (including notes:write) in every configured
	// tenant, which is what lets step 2 below actually create a note
	// rather than merely proving the gate closes.
	var searched adminSearchUsersResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/users?email="+demoOwnerEmail, staffToken, nil, http.StatusOK, &searched, nil)
	if len(searched.Users) != 1 {
		t.Fatalf("search for %q = %+v, want exactly one seeded account", demoOwnerEmail, searched.Users)
	}
	targetID := searched.Users[0].ID

	// Step 1: start the impersonation session.
	var grant adminGrant
	adminRequest(t, srv, http.MethodPost, "/api/v1/admin/impersonation", staffToken,
		map[string]string{
			"targetUserId":   targetID,
			"targetTenantId": "tenant-acme",
			"reason":         "reproduce a customer-reported bug",
			// Locale is required for a user-recipient Dispatch
			// (notification.Dispatch's own doc comment) -- Start passes
			// it straight through with no default of its own, so the
			// mandatory notification below would otherwise silently fail
			// to send.
			"locale": "zh-CN",
		}, http.StatusCreated, &grant, nil)
	if grant.ID == "" || grant.TargetUserID != targetID || grant.TargetTenantID != "tenant-acme" {
		t.Fatalf("start-impersonation response = %+v, want a grant naming %q in tenant-acme", grant, targetID)
	}

	// Step 2: one request made AS the impersonated identity. The
	// credential is still the ADMINISTRATOR's own bearer token (property
	// (a)) -- only the impersonation header names the grant; no demo
	// headers ride along, so both the rbac gate's subject and notes'
	// creator resolver fall back to the substituted Principal
	// (ImpersonationMiddleware's own doc comment).
	impersonationHeaders := map[string]string{"X-Admin-Impersonation": grant.ID}
	var created testNote
	adminRequest(t, srv, http.MethodPost, "/api/v1/notes", staffToken,
		map[string]string{"text": "a note created while impersonating"},
		http.StatusCreated, &created, impersonationHeaders)
	if created.ID == "" {
		t.Fatal("POST /api/v1/notes while impersonating returned no note id")
	}

	// Step 3: the resulting audit event carries the dual identity --
	// Actor is the impersonated target, OnBehalfOf is the real
	// administrator -- confirmed by reading it back through admin's OWN
	// D7 audit query, closing the loop between D5 and D7.
	// AuditFilter.Resource matches an event's ResourceTYPE ("note"), not
	// its ResourceID -- there is no per-id filter on this endpoint, so the
	// matching event is found by scanning the (small, action-filtered)
	// result below rather than by over-narrowing the query itself.
	var events adminListAuditEventsResponse
	adminRequest(t, srv, http.MethodGet,
		"/api/v1/admin/audit-events?tenantId=tenant-acme&action=notes.note.create",
		staffToken, nil, http.StatusOK, &events, nil)
	found := false
	for _, evt := range events.Events {
		if evt.ResourceID != created.ID {
			continue
		}
		found = true
		if evt.ActorID != targetID {
			t.Errorf("audit event Actor = %q, want the impersonated target %q", evt.ActorID, targetID)
		}
		if evt.OnBehalfOfID != searchStaffID(t, srv, staffToken) {
			t.Errorf("audit event OnBehalfOf = %q, want the real administrator", evt.OnBehalfOfID)
		}
	}
	if !found {
		t.Fatalf("no audit event found for the impersonated note %q", created.ID)
	}

	// Step 4: the mandatory, non-unsubscribable security notification
	// landed in the TARGET's own inbox -- read back as the target
	// themselves (their own real token and their own real user id header),
	// never through the impersonation session.
	targetLoginStatus, targetLoginCode, targetToken := demoLogin(t, srv, demoOwnerEmail, demoSeedPassword, "tenant-acme")
	if targetLoginStatus != http.StatusOK || targetToken == "" {
		t.Fatalf("login as the impersonation target failed: status=%d code=%q", targetLoginStatus, targetLoginCode)
	}
	eventually(t, 5*time.Second, "the impersonation-started inbox message", func() bool {
		var inbox notifMessages
		notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", targetToken, targetID, nil, http.StatusOK, &inbox)
		for _, msg := range inbox.Items {
			if msg.TypeKey == "admin.impersonation_started" {
				return true
			}
		}
		return false
	})

	// Step 5: end the grant early.
	adminRequest(t, srv, http.MethodDelete, "/api/v1/admin/impersonation/"+grant.ID, staffToken, nil, http.StatusOK, nil, nil)

	var active adminListGrantsResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/impersonation", staffToken, nil, http.StatusOK, &active, nil)
	for _, g := range active.Grants {
		if g.ID == grant.ID {
			t.Fatalf("ended grant %q still listed as active: %+v", grant.ID, g)
		}
	}

	// Step 6: the SAME (now-invalid) grant id no longer impersonates.
	// ImpersonationMiddleware falls back to the administrator's own real
	// identity -- staff's Principal names tenant "system", which has no
	// notes permission of its own beyond what BuiltinRoleOwner grants
	// platform-wide, so the request either lands under a completely
	// different tenant than tenant-acme or is refused outright; either
	// way, it must NOT create another note attributed to the target under
	// tenant-acme the way step 2 did. The property is already pinned
	// exhaustively at the unit level (go/admin/pipeline_test.go); this
	// assertion is the end-to-end confirmation that ending a grant through
	// the real HTTP surface genuinely disables it, not a re-proof of the
	// fallback mechanism itself.
	var afterEnd testListNotesResponse
	listReq, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/notes", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	listReq.Header.Set("Authorization", "Bearer "+staffToken)
	listReq.Header.Set("X-Admin-Impersonation", grant.ID)
	listResp, err := srv.Client().Do(listReq)
	if err != nil {
		t.Fatalf("GET /api/v1/notes with an ended grant: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode == http.StatusOK {
		if decodeErr := json.NewDecoder(listResp.Body).Decode(&afterEnd); decodeErr != nil {
			t.Fatalf("decode notes list: %v", decodeErr)
		}
		for _, n := range afterEnd.Notes {
			if n.ID == created.ID {
				t.Fatalf("the impersonated note %q is still visible after the grant ended and the caller fell back to tenant \"system\" -- the grant did not actually stop working", created.ID)
			}
		}
	}
}

// TestAdminFlow_OrdinaryTenantOwner_CannotAccessAdminConsole reproduces a
// real privilege-escalation bug found in review: rbac.BuiltinRoleOwner
// grants every permission ANY module declared, with no domain
// partitioning at all (go/rbac/builtin.go) -- so admin:* is among them --
// and demo-owner@example.com holds exactly that role in every configured
// tenant (seedDemoUsers' own demoSeedAccounts table). Before this fix,
// admin's own router-level gate (guardAdminRoute) built its rbac.Subject
// from demoSubjectResolver, which reads TenantID from whatever tenant the
// caller's OWN session happens to be scoped to -- so demo-owner's
// perfectly ordinary tenant-acme session passed admin's gate purely
// because "owner" happens to carry admin:*'s permission strings in the
// shared global catalog. admin's gate must evaluate ONLY
// Subject{rbac.SystemDomain, callerID}, which demo-owner holds no grant
// in at all, so every admin:* request from this account must be refused
// regardless of how privileged its OWN tenant's role is.
func TestAdminFlow_OrdinaryTenantOwner_CannotAccessAdminConsole(t *testing.T) {
	srv, _, _ := buildAdminTestServer(t)

	status, code, ownerToken := demoLogin(t, srv, demoOwnerEmail, demoSeedPassword, "tenant-acme")
	if status != http.StatusOK || ownerToken == "" {
		t.Fatalf("demo-owner login status = %d code = %q, want 200 with a token", status, code)
	}

	// D6: cross-tenant user search, gated on admin:search_users.
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/users?email="+demoOwnerEmail, ownerToken, nil, http.StatusForbidden, nil, nil)

	// D3: the tenant ledger, gated on admin:access.
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/tenants/tenant-acme", ownerToken, nil, http.StatusForbidden, nil, nil)

	// D5: starting an impersonation grant, gated on admin:impersonate --
	// the most consequential of the five, since a false grant here would
	// let this ordinary tenant owner act as ANY other user platform-wide.
	adminRequest(t, srv, http.MethodPost, "/api/v1/admin/impersonation", ownerToken,
		map[string]string{
			"targetUserId":   "does-not-matter",
			"targetTenantId": "tenant-acme",
			"reason":         "should never be reached",
			"locale":         "en-US",
		}, http.StatusForbidden, nil, nil)

	// D7: the audit-query shell, gated on admin:audit_read.
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/audit-events", ownerToken, nil, http.StatusForbidden, nil, nil)
}

// TestAdminFlow_AdminRoutes_IgnoreActiveImpersonation proves the other
// half of this round's fix: admin's own mounted route deliberately does
// NOT sit behind admin.ImpersonationMiddleware (go/admin/AGENTS.md's
// wiring-contract section: "admin's OWN routes ... do not sit behind
// ImpersonationMiddleware -- that decorator's effect is on the REST of
// the application's routes only"). A request to admin's own console still
// carries the operator's OWN real, verified Principal even while an
// X-Admin-Impersonation grant they themselves started is active on the
// request, never the substituted target identity.
func TestAdminFlow_AdminRoutes_IgnoreActiveImpersonation(t *testing.T) {
	srv, _, _ := buildAdminTestServer(t)
	staffToken := platformStaffToken(t, srv)

	var searched adminSearchUsersResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/users?email="+demoOwnerEmail, staffToken, nil, http.StatusOK, &searched, nil)
	if len(searched.Users) != 1 {
		t.Fatalf("search for %q = %+v, want exactly one seeded account", demoOwnerEmail, searched.Users)
	}
	targetID := searched.Users[0].ID

	var grant adminGrant
	adminRequest(t, srv, http.MethodPost, "/api/v1/admin/impersonation", staffToken,
		map[string]string{
			"targetUserId":   targetID,
			"targetTenantId": "tenant-acme",
			"reason":         "prove admin's own console ignores active impersonation",
			"locale":         "en-US",
		}, http.StatusCreated, &grant, nil)

	// The same staff token, now carrying an active grant's id in
	// X-Admin-Impersonation, still reaches admin's OWN console as the
	// real staff identity. If this middleware branch were wrongly wired
	// behind ImpersonationMiddleware, the substituted Principal (an
	// ordinary tenant-acme user holding no admin:* permission under
	// rbac.SystemDomain) would make this request fail with 403 instead.
	impersonationHeaders := map[string]string{"X-Admin-Impersonation": grant.ID}
	var searchedAgain adminSearchUsersResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/users?email="+demoPlatformStaffEmail, staffToken, nil, http.StatusOK, &searchedAgain, impersonationHeaders)
	if len(searchedAgain.Users) != 1 {
		t.Fatalf("search for the platform-staff account while impersonating = %+v, want exactly one", searchedAgain.Users)
	}
}

// searchStaffID resolves the platform-staff account's own user id, for
// asserting an audit event's OnBehalfOf against it -- found the same way
// any operator would, through D6's own search endpoint, rather than a
// second identity channel this test would otherwise have to invent.
func searchStaffID(t *testing.T, srv *httptest.Server, staffToken string) string {
	t.Helper()
	var searched adminSearchUsersResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/users?email="+demoPlatformStaffEmail, staffToken, nil, http.StatusOK, &searched, nil)
	if len(searched.Users) != 1 {
		t.Fatalf("search for the platform-staff account = %+v, want exactly one", searched.Users)
	}
	return searched.Users[0].ID
}
