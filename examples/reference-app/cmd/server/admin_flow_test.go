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

	"github.com/vislake/speed/go/admin"
	"github.com/vislake/speed/go/pkgcore"
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
// opts, applied in order after the shared defaults above, let a caller
// customize the config buildServer boots from -- the org-route-guards
// round's TestAdminFlow_SuspendTenant_... test uses it to add its own
// ad hoc tenant to cfg.HostTenants (never demoHostTenants directly, a
// shared package-level map every other test relies on unmodified), which
// is what makes seedDemoGrants seed demoOwnerUserID's rbac grant there too.
func buildAdminTestServer(t *testing.T, opts ...func(*serverConfig)) (*httptest.Server, serverConfig, *capturingMailer) {
	t.Helper()

	cfg := testConfig(t)
	cfg.DemoUsersPassword = demoSeedPassword
	// The platform-staff account is seeded from its OWN variable, never the
	// demo users' one (demo_admin.go's demoPlatformStaffPasswordEnv), so a
	// suite that signs it in sets its own field -- and signs it in with its
	// own passphrase below.
	cfg.DemoPlatformStaffPassword = demoPlatformStaffSeedPassword
	mailer := &capturingMailer{}
	cfg.Mailer = mailer
	for _, opt := range opts {
		opt(&cfg)
	}

	handler, cleanup, _, err := buildServer(t.Context(), cfg)
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

// platformStaffToken signs the seeded demo platform-staff account in, with
// the account's OWN passphrase (demoPlatformStaffSeedPassword -- the
// platform-staff seed reads its own variable, never the demo users' one,
// per demo_admin.go; signing it in with the demo users' passphrase must
// never work). Its only membership is rbac.SystemDomain
// (seedDemoPlatformStaff's own contract), so no tenant_id request is even
// needed for it to resolve there -- but naming it explicitly keeps this
// test readable regardless.
func platformStaffToken(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	status, code, token := demoLogin(t, srv, demoPlatformStaffEmail, demoPlatformStaffSeedPassword, rbac.SystemDomain)
	if status != http.StatusOK || token == "" {
		t.Fatalf("platform-staff login status = %d code = %q, want 200 with a token", status, code)
	}
	return token
}

// existingOrgRoot returns the caller tenant's org root node, reusing one
// that already exists (org_listNodes with no parentId, which answers the
// root together with everything beneath it, or an empty list when the
// tenant has no root yet) rather than assuming this call is the first
// ever root-creation request for the tenant -- which it is NOT for
// tenant-acme in this file's tests, since buildAdminTestServer's demo
// seed (seedDemoUsers' addDemoOrgMembership, demo_users.go) already
// created that tenant's root at boot, idempotently, the identical
// Root-then-CreateRoot shape this helper mirrors. Only a tenant no demo
// account reaches -- none, today -- would still need the create branch.
func existingOrgRoot(t *testing.T, srv *httptest.Server, token string) orgNode {
	t.Helper()
	var listed struct {
		Nodes []orgNode `json:"nodes"`
	}
	orgRequest(t, srv, http.MethodGet, "/api/v1/org/nodes", token, "", nil, &listed)
	for _, n := range listed.Nodes {
		if n.ParentID == "" {
			return n
		}
	}
	var root orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Admin Flow Co", "kind": "group"}, &root)
	return root
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

	// tenant-acme is demoSingleTenantID -- the one tenant demoHostTenants
	// configures -- so buildAdminTestServer's own demo seed (seedDemoUsers'
	// addDemoOrgMembership, demo_users.go) has already created its org
	// root and bound the demo accounts to it before this test's first
	// request. existingOrgRoot reuses that root instead of racing it with
	// a second org_createNode call, which org's own one-root-per-tenant
	// invariant would refuse with 409 org.root_already_exists.
	root := existingOrgRoot(t, srv, inviterToken)

	// D3: the ledger picks up tenant-acme the moment its root is created,
	// with no operator action at all -- by the demo seed above in THIS
	// test's case, rather than by a root-creation request this test makes
	// itself.
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
	// calling org's existing, unmodified per-tenant membership method. The
	// account was registered at runtime, so under this app's self-service
	// signup (self_service.go) its registration provisioned its own clinic
	// -- tenant-<targetID>, the deterministic derivation -- whose root's
	// org.node.created lazily registered the clinic in the very same D3
	// ledger this query loops; the answer must name BOTH the clinic the
	// account owns and the tenant-acme seat its invitation acceptance
	// created.
	var memberships adminListMembershipsResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/users/"+targetID+"/memberships", staffToken, nil, http.StatusOK, &memberships, nil)
	wantTenants := map[string]bool{"tenant-acme": true, "tenant-" + targetID: true}
	if len(memberships.TenantIds) != len(wantTenants) {
		t.Fatalf("memberships of %q = %+v, want the account's own clinic plus tenant-acme", targetID, memberships.TenantIds)
	}
	for _, tenant := range memberships.TenantIds {
		if !wantTenants[tenant] {
			t.Fatalf("memberships of %q = %+v, want exactly its own clinic (tenant-%s) plus tenant-acme",
				targetID, memberships.TenantIds, targetID)
		}
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
			// locale is optional (StartInput.Locale's own doc comment):
			// Start resolves the target's own authn.User.Locale itself
			// when this is omitted (falling back to authn.DefaultLocale
			// when the user has never chosen one), so the mandatory
			// notification below is guaranteed to be attempted either
			// way -- P1-1's fix. Passed explicitly here only to exercise
			// that leg of the contract too ("a request WITH a locale
			// keeps working exactly as today").
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

// tenancyErrorBody decodes the {code, params} envelope
// tenancy.Middleware's own writeError writes on every refusal -- D4's
// coded-error discipline (root CLAUDE.md: "a coded error, never a raw
// HTTP status with no code").
type tenancyErrorBody struct {
	Code string `json:"code"`
}

// rawStatusRequest issues method against srv.URL+path, authenticated as
// token, and returns the raw status code plus the decoded {code, ...}
// error envelope body (empty Code on a non-error response) -- unlike
// adminRequest/orgRequest, it never fatals on an unexpected status, since
// this suite's whole point is asserting a REFUSAL happens, not just a
// success.
//
// demoUser, when non-empty, is sent as the rbac demo header (demoUserHeader)
// naming which seeded identity org-route-guards' per-operation gate
// evaluates -- empty omits it entirely, which is right for a path (admin's)
// whose own subject resolver never reads it in the first place
// (adminSubjectResolver's own doc comment).
func rawStatusRequest(t *testing.T, srv *httptest.Server, method, path, token, demoUser string) (int, tenancyErrorBody) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if demoUser != "" {
		req.Header.Set(demoUserHeader, demoUser)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	var body tenancyErrorBody
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// TestAdminFlow_SuspendTenant_BlocksThenResumeAllows_EndToEnd is D4's
// genuinely observable, live-request-pipeline proof (docs/internal/23-
// admin.md's D4, go/tenancy/tenant_status.go's WithTenantStatusResolver
// seam): suspending a tenant through admin's own PATCH
// /api/v1/admin/tenants/{id} makes every OTHER route serving that tenant
// -- org's, here -- actually start refusing requests on the very next
// one, with no restart, no cache to invalidate and no code path of its
// own beyond tenancy.Middleware consulting the resolver it was wired
// with; resuming the tenant makes the exact same route succeed again
// immediately.
//
// This is the one round-2 deliverable this suite proves through the real
// composed HTTP stack end to end, per this round's own brief: D4 is the
// only round-2 item with a genuinely observable behavior change in a live
// request pipeline (D8/D9/D10/D7-export are proven at go/admin's own
// module level, mirroring how go/pki's X.509 layer and go/billing/
// go/metering carry the identical "real, tested, but not yet a reference-
// app HTTP consumer" exception elsewhere in this codebase).
func TestAdminFlow_SuspendTenant_BlocksThenResumeAllows_EndToEnd(t *testing.T) {
	const tenant = pkgcore.TenantID("tenant-suspend-flow")

	// This test's own tenant is not one of demoHostTenants' two entries, so
	// it must add itself to cfg.HostTenants before buildServer boots --
	// otherwise seedDemoGrants never seeds demoOwnerUserID's rbac grant
	// there, and the org_createNode call below (which rides on that seeded
	// identity like every other orgRequest caller, org-route-guards round)
	// would be refused before D4's own tenant-suspension check ever runs.
	srv, cfg, _ := buildAdminTestServer(t, func(c *serverConfig) {
		c.HostTenants = map[string]pkgcore.TenantID{"tenant-suspend-flow.demo.localhost": tenant}
	})
	staffToken := platformStaffToken(t, srv)

	ownerToken := registerAndAuthenticate(t, srv, cfg, tenant, "suspend-flow-owner")

	// The tenant's root node is what lazily registers "tenant-suspend-flow"
	// in admin's own D3 ledger (org.node.created -> TenantService.
	// handleOrgNodeCreated), so no separate manual ledger-registration call
	// is needed before D4's PATCH below -- whichever caller's request
	// happens to create it. That creator is NOT reliably this test's own
	// explicit call: reusing existingOrgRoot (like
	// TestAdminFlow_SearchMembershipsAndAudit_EndToEnd above) rather than
	// assuming a bare org_createNode is this tenant's first-ever root
	// request avoids racing addDemoOrgMembership's own idempotent
	// Root-then-CreateRoot boot-time seeding (demo_users.go), which reaches
	// this same tenant the moment this test's HostTenants override above
	// makes it one of the inEveryTenant demo owner's configured tenants --
	// whichever of the two runs first wins the create and the other reuses
	// it, and the ledger lands either way. orgRequest itself sends the demo
	// rbac header naming demoOwnerUserID (org-route-guards round: org's
	// route is gated per operation like every other module's, so ownerToken's
	// freshly-registered account -- which holds no rbac grant of its own --
	// rides on the pre-seeded owner identity's grant for this call. This
	// test's whole point is D4's tenant-suspension gate, not org's
	// permission gate, so borrowing the seeded identity here is a setup
	// choice, not a weakening of what either gate enforces).
	root := existingOrgRoot(t, srv, ownerToken)
	nodePath := "/api/v1/org/nodes/" + root.ID

	// Before suspension: the tenant is active (either its own explicit
	// TenantStatusActive, or a WithTenantStatusResolver-wired but
	// not-yet-suspended ledger row), so the ordinary request succeeds. The
	// demo owner header rides along here too, for the same reason as the
	// create call above -- D4's tenant-suspension check runs in
	// tenancy.Middleware, upstream of org's own rbac permission gate, so
	// the caller must already clear THAT gate for D4's own refusal (below)
	// to be the one this test is actually proving.
	if status, _ := rawStatusRequest(t, srv, http.MethodGet, nodePath, ownerToken, demoOwnerUserID); status != http.StatusOK {
		t.Fatalf("GET %s before suspension: status = %d, want %d", nodePath, status, http.StatusOK)
	}

	// D3 + D4: suspend the tenant through admin's own console.
	var patched adminTenant
	adminRequest(t, srv, http.MethodPatch, "/api/v1/admin/tenants/"+string(tenant), staffToken,
		map[string]string{"status": "suspended", "suspendedReason": "reproducing a billing dispute"},
		http.StatusOK, &patched, nil)
	if patched.Status != "suspended" {
		t.Fatalf("PATCH tenants/%s response Status = %q, want %q", tenant, patched.Status, "suspended")
	}

	// D4's whole point: the VERY NEXT request against this tenant --
	// through an entirely different module's route, org's, not admin's
	// own -- is refused, with the coded error tenancy.Middleware writes,
	// never a bare status with no code.
	status, body := rawStatusRequest(t, srv, http.MethodGet, nodePath, ownerToken, demoOwnerUserID)
	if status != http.StatusForbidden {
		t.Fatalf("GET %s after suspension: status = %d, want %d", nodePath, status, http.StatusForbidden)
	}
	if body.Code != "tenancy.tenant_suspended" {
		t.Fatalf("GET %s after suspension: error code = %q, want %q", nodePath, body.Code, "tenancy.tenant_suspended")
	}

	// Resume: PATCH status back to active.
	adminRequest(t, srv, http.MethodPatch, "/api/v1/admin/tenants/"+string(tenant), staffToken,
		map[string]string{"status": "active"}, http.StatusOK, &patched, nil)
	if patched.Status != "active" {
		t.Fatalf("PATCH tenants/%s (resume) response Status = %q, want %q", tenant, patched.Status, "active")
	}

	// The same request that was refused a moment ago now succeeds again,
	// with no restart and nothing else changed.
	if status, _ := rawStatusRequest(t, srv, http.MethodGet, nodePath, ownerToken, demoOwnerUserID); status != http.StatusOK {
		t.Fatalf("GET %s after resume: status = %d, want %d", nodePath, status, http.StatusOK)
	}
}

// searchStaffID resolves the platform-staff account's own user id, for
// asserting an audit event's OnBehalfOf against it -- found the same way
// any operator would, through D6's own search endpoint, rather than a
// second identity channel this test would otherwise have to invent.
func searchStaffID(t *testing.T, srv *httptest.Server, staffToken string) string {
	t.Helper()
	return searchUserID(t, srv, staffToken, demoPlatformStaffEmail)
}

// searchUserID resolves one account's user id by email through D6's own
// cross-tenant search endpoint, generalizing searchStaffID for the probe
// accounts the round-2 permission tests below register.
func searchUserID(t *testing.T, srv *httptest.Server, staffToken, email string) string {
	t.Helper()
	var searched adminSearchUsersResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/users?email="+email, staffToken, nil, http.StatusOK, &searched, nil)
	if len(searched.Users) != 1 {
		t.Fatalf("search for %q = %+v, want exactly one", email, searched.Users)
	}
	return searched.Users[0].ID
}

// adminRole/adminRoleBinding mirror admin's own AdminRole/AdminRoleBinding
// wire shapes (go/admin/api/openapi.yaml), the same field-named-not-
// imported posture every wire shape in this file takes.
type (
	adminRole struct {
		ID             string   `json:"id"`
		TenantID       string   `json:"tenantId"`
		Key            string   `json:"key"`
		DescriptionKey string   `json:"descriptionKey"`
		Permissions    []string `json:"permissions"`
	}
	adminRoleBinding struct {
		TenantID string `json:"tenantId"`
		UserID   string `json:"userId"`
		Role     string `json:"role"`
	}
	adminListDeclaredPermissionsResponse struct {
		Permissions []string `json:"permissions"`
	}
	adminListSendRecordsResponse struct {
		Records []map[string]any `json:"records"`
	}
	adminExportAuditEventsResponse struct {
		JobID string `json:"jobId"`
	}
)

// defineAdminRole calls staffToken's admin:roles_manage-gated POST
// /api/v1/admin/roles to create a role scoped to a CUSTOMER tenant
// carrying exactly permissions -- the write half of the pair bindAdminRole
// completes -- the HTTP-shape reachability proof
// TestAdminFlow_Round2Routes_ReachableForPlatformStaff below drives, since
// registerAndAuthenticate itself grants no rbac role at all
// (server_test.go's own doc comment). These helpers never name
// rbac.SystemDomain: admin's role-management surface refuses the system
// pseudo-tenant with admin.roles_system_domain_forbidden (go/admin/role.go's
// checkTenantWritable), so a test that needs system-domain probes seeds
// them out of band, directly against rbac.Service under a system-tenant
// context -- the shape TestAdminFlow_AuditExport_RequiresExportPermission's
// own doc comment records.
func defineAdminRole(t *testing.T, srv *httptest.Server, staffToken, tenant, key string, permissions []string) adminRole {
	t.Helper()
	var role adminRole
	adminRequest(t, srv, http.MethodPost, "/api/v1/admin/roles", staffToken,
		map[string]any{"tenantId": tenant, "key": key, "permissions": permissions},
		http.StatusCreated, &role, nil)
	if role.Key != key {
		t.Fatalf("defineAdminRole(%q) response = %+v, want Key %q", key, role, key)
	}
	return role
}

// bindAdminRole binds roleKey to (tenant, userID) through staffToken's
// admin:roles_manage-gated POST /api/v1/admin/roles/{key}/bindings -- the
// bind half of defineAdminRole's pair, carrying the same
// customer-tenant-only restriction that helper's own doc comment records.
func bindAdminRole(t *testing.T, srv *httptest.Server, staffToken, roleKey, tenant, userID string) {
	t.Helper()
	var binding adminRoleBinding
	adminRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/"+roleKey+"/bindings", staffToken,
		map[string]string{"tenantId": tenant, "userId": userID},
		http.StatusCreated, &binding, nil)
	if binding.Role != roleKey || binding.UserID != userID {
		t.Fatalf("bindAdminRole(%q, %q) response = %+v, want Role %q UserID %q", roleKey, userID, binding, roleKey, userID)
	}
}

// TestAdminFlow_Round2Routes_ReachableForPlatformStaff reproduces the
// review-flagged blocker directly: adminPermissionFor (demo_admin.go) was
// never updated for round 2, so its switch fell through to the default
// case for every one of D8's and D10's new sub-paths, returning "" --
// which rbac.RequirePermissionFunc's own doc comment says unconditionally
// denies the request, even for the platform-staff account holding
// rbac.BuiltinRoleOwner (every permission any module declared). Before
// the fix, every request below answered 403; after it, each reaches
// admin's own Handler (a non-403 status, whatever that handler itself
// then answers).
func TestAdminFlow_Round2Routes_ReachableForPlatformStaff(t *testing.T) {
	srv, _, _ := buildAdminTestServer(t)
	staffToken := platformStaffToken(t, srv)

	// D8, read half: the declared-permission catalog.
	var perms adminListDeclaredPermissionsResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/roles", staffToken, nil, http.StatusOK, &perms, nil)
	found := false
	for _, p := range perms.Permissions {
		if p == admin.PermissionRolesManage {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("declared permissions = %v, want it to include %q", perms.Permissions, admin.PermissionRolesManage)
	}

	// D8, write half: define a role and bind it to the staff account
	// itself (a real, already-known user id -- what it grants doesn't
	// matter for this test, only that the two requests are reached at
	// all rather than refused by an empty permission string).
	defineAdminRole(t, srv, staffToken, "tenant-acme", "flow-test-role-reachability", []string{"notes:read"})
	staffID := searchStaffID(t, srv, staffToken)
	bindAdminRole(t, srv, staffToken, "flow-test-role-reachability", "tenant-acme", staffID)

	// D10: cross-tenant notification send-record search.
	var sendRecords adminListSendRecordsResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/notifications/send-records", staffToken, nil, http.StatusOK, &sendRecords, nil)

	// D9: the usage/billing dashboard. Deliberately NOT wired into this
	// app (go/admin/AGENTS.md's Known limitations), so the platform-staff
	// account -- which holds admin:usage_read via BuiltinRoleOwner --
	// still answers 500 (ErrUsageModulesNotWired) rather than 200; the
	// property this test actually needs is that it is no longer the
	// fail-closed 403 an empty adminPermissionFor entry would have
	// produced.
	status, body := rawStatusRequest(t, srv, http.MethodGet, "/api/v1/admin/usage-summary", staffToken, "")
	if status == http.StatusForbidden {
		t.Fatalf("GET /api/v1/admin/usage-summary for platform-staff = 403 %+v, want it to reach admin's Handler (not be refused by an unmapped permission)", body)
	}
}

// TestAdminFlow_AuditExport_RequiresExportPermission reproduces the
// second review-flagged blocker directly: adminPermissionFor's switch
// matched "/api/v1/admin/audit-events/export" against the existing
// adminAuditEventsPath prefix case (both share that prefix), gating the
// round-2 export leg on the weaker admin:audit_read instead of the
// newly-declared, deliberately-stronger admin:audit_export
// (module.go's own PermissionAuditExport doc comment: exporting a
// tenant's complete audit trail is a materially stronger action than
// merely reading it). Two probe accounts, each holding exactly one of
// the two permissions under rbac.SystemDomain and nothing else, prove
// both directions: audit_read alone must NOT reach the export route, and
// audit_export alone must.
//
// The probes' system-domain role scaffolding is seeded directly against
// rbac.Service under a system-tenant context -- the out-of-band shape
// seedDemoPlatformStaff sanctions (demo_admin.go) -- rather than through
// admin's own role-management HTTP surface, which the admin P1-2 fix
// refuses for rbac.SystemDomain with admin.roles_system_domain_forbidden
// (go/admin/role.go's checkTenantWritable: no admin:roles_manage-gated
// permission is fine-grained enough to separate "may manage a customer
// tenant's roles" from "may delegate platform-operator authority", so
// hosts seed system-domain grants out of band, directly against
// rbac.Service). That choice leaves the gate this test exists to prove
// untouched: the two probes still reach the audit-events surface as real
// HTTP callers, and every assertion below runs through it.
func TestAdminFlow_AuditExport_RequiresExportPermission(t *testing.T) {
	var rbacService *rbac.Service
	srv, cfg, _ := buildAdminTestServer(t, func(cfg *serverConfig) {
		cfg.OnRBACReady = func(svc *rbac.Service) { rbacService = svc }
	})
	if rbacService == nil {
		t.Fatal("cfg.OnRBACReady was never called by buildServer")
	}
	staffToken := platformStaffToken(t, srv)

	readOnlyToken := registerAndAuthenticate(t, srv, cfg, rbac.SystemDomain, "flow-audit-read-only-probe")
	exportToken := registerAndAuthenticate(t, srv, cfg, rbac.SystemDomain, "flow-audit-export-probe")
	readOnlyID := searchUserID(t, srv, staffToken, "flow-audit-read-only-probe@example.com")
	exportID := searchUserID(t, srv, staffToken, "flow-audit-export-probe@example.com")

	// The system-domain role seed itself -- see this test's own doc
	// comment for why it is out-of-band rbac.Service calls under a
	// system-tenant context rather than the role-management HTTP surface.
	systemCtx := pkgcore.WithTenant(t.Context(), rbac.SystemDomain)
	for _, probe := range []struct {
		key   string
		perms []string
		user  string
	}{
		{key: "flow-audit-read-only", perms: []string{admin.PermissionAuditRead}, user: readOnlyID},
		{key: "flow-audit-export-only", perms: []string{admin.PermissionAuditExport}, user: exportID},
	} {
		if _, err := rbacService.DefineRole(systemCtx, rbac.RoleDefinition{Key: probe.key, Permissions: probe.perms}); err != nil {
			t.Fatalf("seeding role %q: %v", probe.key, err)
		}
		if err := rbacService.AssignRole(systemCtx, rbac.Subject{TenantID: rbac.SystemDomain, UserID: probe.user}, probe.key, rbac.Scope{}); err != nil {
			t.Fatalf("seeding binding of %q to %q: %v", probe.key, probe.user, err)
		}
	}

	// The audit_read-only probe reads the audit trail fine...
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/audit-events", readOnlyToken, nil, http.StatusOK, nil, nil)
	// ...but MUST NOT reach the export leg: this is the exact regression
	// the review flagged (before the fix, this answered 202).
	adminRequest(t, srv, http.MethodPost, "/api/v1/admin/audit-events/export", readOnlyToken,
		map[string]string{"tenantId": "tenant-acme"}, http.StatusForbidden, nil, nil)

	// The audit_export-only probe is refused the plain read...
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/audit-events", exportToken, nil, http.StatusForbidden, nil, nil)
	// ...but its own export request is genuinely accepted.
	var exportResp adminExportAuditEventsResponse
	adminRequest(t, srv, http.MethodPost, "/api/v1/admin/audit-events/export", exportToken,
		map[string]string{"tenantId": "tenant-acme"}, http.StatusAccepted, &exportResp, nil)
	if exportResp.JobID == "" {
		t.Fatal("POST /api/v1/admin/audit-events/export for the audit_export-only probe returned no jobId")
	}
}
