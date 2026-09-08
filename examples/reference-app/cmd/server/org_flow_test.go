package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sync"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// capturingMailer is a test double for pkgcore.Mailer that records every
// message instead of sending it, so this test can pull the invitation token
// back out of the rendered body: the token is a bearer credential that
// deliberately never appears on org_createInvitation's HTTP response (see
// go/org/handler.go's own comment on OrgCreateInvitation -- "result.Token is
// deliberately never read here"), so the only place a caller can observe it
// is the message the invitee actually receives, exactly as a real invitee
// would read it out of their inbox.
type capturingMailer struct {
	mu   sync.Mutex
	sent []pkgcore.Mail
}

// Send implements pkgcore.Mailer.
func (m *capturingMailer) Send(ctx context.Context, mail pkgcore.Mail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, mail)
	return nil
}

// last returns the most recently captured message, failing the test if none
// was ever sent.
func (m *capturingMailer) last(t *testing.T) pkgcore.Mail {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("capturingMailer: no mail was sent")
	}
	return m.sent[len(m.sent)-1]
}

// compile-time check that *capturingMailer satisfies pkgcore.Mailer.
var _ pkgcore.Mailer = (*capturingMailer)(nil)

// acceptURLPattern finds the invitation accept link inside a rendered
// plain-text mail body (go/org/locales/en-US.toml's
// "org.invitation.body_text" prints it on its own line).
var acceptURLPattern = regexp.MustCompile(`https://\S+`)

// tokenFromMail extracts the invitation token carried on mail's accept URL --
// reference-app's own org.WithInvitationLinkBuilder wiring (server.go) embeds
// it as a "token" query parameter, exactly as a real invitee's browser would
// receive it in the link they click.
func tokenFromMail(t *testing.T, mail pkgcore.Mail) string {
	t.Helper()
	match := acceptURLPattern.FindString(mail.Text)
	if match == "" {
		t.Fatalf("no accept URL found in mail body: %q", mail.Text)
	}
	parsed, err := url.Parse(match)
	if err != nil {
		t.Fatalf("parse accept URL %q: %v", match, err)
	}
	token := parsed.Query().Get("token")
	if token == "" {
		t.Fatalf("accept URL %q carries no token query parameter", match)
	}
	return token
}

// buildOrgTestServer wires buildServer's real output exactly like
// buildTestServer (server_test.go), except with a capturingMailer standing in
// for the deployment mode's default console mailer: org's invitation flow
// needs to observe the sent message to recover the token that never appears
// on any HTTP response (see capturingMailer's own doc comment above).
//
// It returns the serverConfig alongside the server and mailer, for the same
// reason buildTestServer does: org's flow test authenticates its callers as
// real authn users, and registerAndAuthenticate reaches cfg.Memberships to
// grant each one membership in the tenant its token must select.
func buildOrgTestServer(t *testing.T) (*httptest.Server, serverConfig, *capturingMailer) {
	t.Helper()

	cfg := testConfig(t)
	mailer := &capturingMailer{}
	cfg.Mailer = mailer

	handler, cleanup, _, err := buildServer(context.Background(), cfg)
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

// orgNode is the subset of org's OrgNode response this test reads, decoded
// by field name rather than by importing go/org/api's generated types --
// the same "assert on the wire shape, not the generator's Go types" posture
// server_test.go's testNote/testListNotesResponse already take for notes.
type orgNode struct {
	ID       string `json:"id"`
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
}

type orgInvitation struct {
	ID     string `json:"id"`
	NodeID string `json:"nodeId"`
	Status string `json:"status"`
}

type orgMembership struct {
	MembershipID string `json:"membershipId"`
	UserID       string `json:"userId"`
	NodeID       string `json:"nodeId"`
	Status       string `json:"status"`
}

type orgListMembersResponse struct {
	Members []orgMembership `json:"members"`
}

// orgRequest issues method against srv.URL+path, authenticated as token --
// the bearer access token is the ONLY thing that selects the tenant an org
// operation runs in: org's routes sit behind tenancy.Middleware
// (authn.NewPrincipalResolver) like every other protected route in this app,
// and Host plays no part in resolving their tenant (see server.go's
// middleware-chain doc comment). token empty omits the Authorization header
// entirely.
//
// subjectUserID is a separate thing from the token, the same split
// createNoteAs's own doc comment (server_test.go) explains for notes: it is
// the X-Demo-User-Id header org's own SubjectResolver (demoOrgSubjectResolver
// in server.go) reads to name WHO is acting, for the two operations that
// resolve a caller identity (creating and accepting an invitation). Empty
// omits the header entirely, which the operations that resolve no caller
// identity must do (org_createNode).
//
// Every call also sends the rbac demo header (demoUserHeader) naming
// demoOwnerUserID, the seeded identity seedDemoGrants grants BuiltinRoleOwner
// in every configured tenant -- since the org-route-guards round, org's
// route is gated per operation on its own declared permissions like every
// other module's (go/org/AGENTS.md's own permission table), and this
// helper's callers register throwaway accounts through registerAndAuthenticate
// that hold no rbac grant of their own. Riding on the pre-seeded owner
// identity here is a SETUP choice -- which demo identity rbac evaluates the
// request for -- not a weakening of the gate itself: a caller who does NOT
// hold org's permissions is refused exactly the same way regardless (see
// org_route_guards_test.go's own THE-scenario test, which exercises that
// refusal directly). It is harmless on org_acceptInvitation, the one
// operation the gate never checks a permission for at all.
//
// It fails the test outright on anything outside 2xx, and otherwise decodes
// the response into out (nil to skip decoding, for 204 No Content
// responses). It returns the raw response bytes either way, so a caller can
// assert on the wire shape itself -- the shape a real client's code would
// see -- rather than only on the decoded subset in out (callers that do not
// need the bytes simply ignore the return value).
func orgRequest(t *testing.T, srv *httptest.Server, method, path, token, subjectUserID string, body, out any) []byte {
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
	req.Header.Set(demoUserHeader, demoOwnerUserID)
	if subjectUserID != "" {
		req.Header.Set(demoOrgUserHeader, subjectUserID)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body for %s %s: %v", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s %s status = %d, want 2xx; body = %s",
			method, path, resp.StatusCode, respBody)
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			t.Fatalf("decode response for %s %s: %v", method, path, err)
		}
	}
	return respBody
}

// TestOrgFlow_MultiLevelTree_InviteAcceptAndSubtreeScopedListing_EndToEnd is
// the round's own acceptance criterion (see the frozen plan's B3 block): the
// roadmap M1 exit path, driven end to end through the real composed HTTP
// stack this app serves -- the authn+tenancy middleware chain, org's real
// Handler, and real dbkit.Repository-backed SQLite storage, none of it
// mocked -- proving org is a genuine consumed dependency of the reference
// app, not merely a module that compiles alongside it.
//
// The shape mirrors docs/internal/14's dental-SaaS DSO scenario: a group
// with two stores beneath it. A member invited into one store must be
// visible when the roster is read from the group (their subtree) and
// invisible when it is read from the sibling store -- the property
// go/org/scope.go's Scope.MemberNodeIDs exists to guarantee, and which
// org_listMembers (handler.go's OrgListMembers -> MemberService.List)
// resolves through that exact seam. This is the seam actually being
// exercised, not merely declared: see org.FeatureGate's own wiring in
// server.go (orgFeatureGate) for the parallel no-import technique used for
// config, proven the same way by TestSystemFeatures_EnabledFlagChain_ResolvesDependencies
// in public_config_test.go.
func TestOrgFlow_MultiLevelTree_InviteAcceptAndSubtreeScopedListing_EndToEnd(t *testing.T) {
	srv, cfg, mailer := buildOrgTestServer(t)

	// The whole flow runs in one tenant, tenant-acme -- the tenant the demo
	// host "acme.demo.localhost" used to select before authn landed. The
	// bearer token registerAndAuthenticate returns is what selects that
	// tenant for the INVITER: org's routes sit behind the authn+tenancy
	// middleware chain like every other route this app protects, and Host no
	// longer resolves a tenant for them (see server.go's middleware-chain
	// doc comment). The INVITEE deliberately authenticates as nobody: since
	// the tenantless-accept round, org_acceptInvitation resolves the
	// invitation's own tenant from the token, server-side, and the app's
	// tenancy allowlist lets the accept path through unresolved -- exactly
	// the situation a real invitee is in, holding no membership in and no
	// bearer token for the inviting tenant yet. The accept request below
	// therefore carries NO Authorization header at all; only the subject
	// header names who is accepting (the invitee's user id, never the
	// inviter's), and only the invitation's own email (inviteeEmail below)
	// is what org's token binds to.
	const inviterUserID = "user-owner-1"
	const inviteeUserID = "user-new-hire-1"
	const inviteeEmail = "new-hire@example.com"

	inviterToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "org-owner")

	// Step 1: create the tenant's root -- the DSO's top-level group. No
	// subject header: org_createNode never resolves a caller identity (only
	// invitation create/accept do -- see handler.go), so this must succeed
	// with none set.
	var root orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", inviterToken, "",
		map[string]string{"name": "Acme Dental Group", "kind": "group"}, &root)
	if root.ID == "" || root.ParentID != "" {
		t.Fatalf("created root = %+v, want a non-empty id and empty parentId", root)
	}

	// Step 2: two stores beneath the group -- the "multi-level" shape the
	// round's acceptance criterion names explicitly.
	var storeA, storeB orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", inviterToken, "",
		map[string]string{"name": "Downtown Store", "kind": "store", "parentId": root.ID}, &storeA)
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", inviterToken, "",
		map[string]string{"name": "Uptown Store", "kind": "store", "parentId": root.ID}, &storeB)
	if storeA.ID == "" || storeB.ID == "" || storeA.ID == storeB.ID {
		t.Fatalf("stores = %+v, %+v, want two distinct non-empty ids", storeA, storeB)
	}

	// Step 3: invite a member into storeA. The inviter is the authenticated
	// caller org_createInvitation resolves through SubjectResolver (the demo
	// header here), never a value the request body could forge.
	var invitation orgInvitation
	invitationBody := orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations", inviterToken, inviterUserID,
		map[string]string{"email": inviteeEmail, "nodeId": storeA.ID}, &invitation)
	if invitation.NodeID != storeA.ID || invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want nodeId %q and status \"pending\"", invitation, storeA.ID)
	}
	// The response must echo neither the address nor its blind index, on
	// the raw wire: org's own convention never echoes the address, and
	// since org P1-1 it never echoes the index either -- HMAC
	// non-invertibility is no defense against an online oracle when
	// invitation creation itself yields (address, index) pairs (see
	// toInvitationResponse's own doc comment in go/org/handler.go).
	if bytes.Contains(invitationBody, []byte(inviteeEmail)) {
		t.Fatal("the invitation response echoes the invitee's plaintext address")
	}
	if bytes.Contains(invitationBody, []byte("emailIndex")) {
		t.Fatal("the invitation response carries the emailIndex key")
	}

	// The invitation response carries no token (see capturingMailer's own
	// doc comment on why): recover it from the message org actually sent,
	// exactly as the invitee would from their inbox.
	mail := mailer.last(t)
	if len(mail.To) != 1 || mail.To[0] != inviteeEmail {
		t.Fatalf("mail.To = %v, want exactly [%q]", mail.To, inviteeEmail)
	}
	token := tokenFromMail(t, mail)

	// Step 4: the invitee accepts -- a different subject than the inviter
	// (the person accepting is never the same HTTP caller who sent the
	// invite) AND, since the tenantless-accept round, a caller holding no
	// bearer token at all: the token in the body resolves the tenant
	// server-side, which is the only way a real invitee, who has no
	// membership in the inviting tenant yet, could ever reach an acceptance.
	// The subject header names the invitee's user id; the request's
	// Authorization header is deliberately absent.
	var membership orgMembership
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations/accept", "", inviteeUserID,
		map[string]string{"token": token}, &membership)
	if membership.UserID != inviteeUserID || membership.NodeID != storeA.ID || membership.Status != "active" {
		t.Fatalf("membership after accept = %+v, want userId %q, nodeId %q, status \"active\"",
			membership, inviteeUserID, storeA.ID)
	}

	// Step 5: listing from the group node returns the group's whole
	// subtree -- both stores -- so the new member, bound at storeA, must be
	// visible from the root.
	var fromRoot orgListMembersResponse
	orgRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/org/members?nodeId=%s", url.QueryEscape(root.ID)), inviterToken, "", nil, &fromRoot)
	if !containsUserID(fromRoot.Members, inviteeUserID) {
		t.Fatalf("members listed from the group root = %+v, want it to include %q", fromRoot.Members, inviteeUserID)
	}

	// Step 6: listing from storeA directly must show the same member --
	// standing at the exact node they are bound to is the smallest subtree
	// that still contains them.
	var fromStoreA orgListMembersResponse
	orgRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/org/members?nodeId=%s", url.QueryEscape(storeA.ID)), inviterToken, "", nil, &fromStoreA)
	if len(fromStoreA.Members) != 1 || !containsUserID(fromStoreA.Members, inviteeUserID) {
		t.Fatalf("members listed from storeA = %+v, want exactly one member, %q", fromStoreA.Members, inviteeUserID)
	}

	// Step 7 -- the property the whole test exists to prove: listing from
	// storeB, a SIBLING of storeA under the same group, must NOT see a
	// member bound only to storeA. This is Scope.MemberNodeIDs' subtree
	// boundary (go/org/scope.go), exercised here through the real HTTP
	// listing path rather than a direct unit call.
	var fromStoreB orgListMembersResponse
	orgRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/org/members?nodeId=%s", url.QueryEscape(storeB.ID)), inviterToken, "", nil, &fromStoreB)
	if len(fromStoreB.Members) != 0 {
		t.Fatalf("members listed from the sibling storeB = %+v, want none -- "+
			"a member bound to storeA must never leak into a sibling subtree's roster", fromStoreB.Members)
	}
}

// containsUserID reports whether members includes one bound to userID.
func containsUserID(members []orgMembership, userID string) bool {
	for _, m := range members {
		if m.UserID == userID {
			return true
		}
	}
	return false
}

// TestOrgInvitation_BrowserShapedInviter_CreateInvitationFromPrincipalAlone
// pins the org-web round's resolver change end to end: a signed-in owner
// whose browser requests carry a bearer token and NEITHER demo header --
// the exact shape the team surface's invite flow produces -- can create an
// invitation. Before the round, org's caller-scoped endpoints resolved the
// caller from the X-Demo-User-Id header alone (demoOrgSubjectResolver's
// header-only contract), so a header-less request was refused with
// org.subject_unresolved even though the rbac gate ahead of it had already
// let the same principal through.
//
// The caller must genuinely hold the permission its bearer proves: this
// test grants the registered account the owner role through the live rbac
// service (the demo seeding grants only the header actors), so the whole
// composed gate -- rbac's demoSubjectResolver falling back to the verified
// Principal, then org's own demoOrgSubjectResolver with principalFallback
// -- is exercised with no demo header anywhere on the wire.
func TestOrgInvitation_BrowserShapedInviter_CreateInvitationFromPrincipalAlone(t *testing.T) {
	// The server is built by hand rather than through buildOrgTestServer
	// because the rbac hook must be armed on the config BEFORE buildServer
	// runs (the hook fires inside it, right after seedDemoGrants) -- the
	// same shape TestOrgRouteGuards_SubtreeScopedGrant_ManagesOwnSubtreeOnly
	// uses. The capturingMailer stands in for the console mailer exactly as
	// buildOrgTestServer wires it, so the sent invitation can be observed.
	cfg := testConfig(t)
	var rbacService *rbac.Service
	cfg.OnRBACReady = func(svc *rbac.Service) { rbacService = svc }
	mailer := &capturingMailer{}
	cfg.Mailer = mailer

	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	if rbacService == nil {
		t.Fatal("cfg.OnRBACReady was never called by buildServer")
	}

	const tenant = pkgcore.TenantID("tenant-acme")
	ownerToken := registerAndAuthenticate(t, srv, cfg, tenant, "browser-shaped-owner")

	// The registered account's own user id, read from its own /me answer --
	// the identity the bearer token proves.
	meReq, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/authn/me", nil)
	if err != nil {
		t.Fatalf("build /me request: %v", err)
	}
	meReq.Header.Set("Authorization", "Bearer "+ownerToken)
	meResp, err := srv.Client().Do(meReq)
	if err != nil {
		t.Fatalf("GET /me with bearer: %v", err)
	}
	defer meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(meResp.Body)
		t.Fatalf("GET /me status = %d, want 200; body = %s", meResp.StatusCode, body)
	}
	var me struct {
		UserID string `json:"user_id"`
	}
	if decodeErr := json.NewDecoder(meResp.Body).Decode(&me); decodeErr != nil {
		t.Fatalf("decode /me: %v", decodeErr)
	}
	if me.UserID == "" {
		t.Fatal("/me answered without a user_id")
	}

	// Grant the account the owner role in the tenant, under the tenant's
	// own context exactly like seedDemoGrants grants its actors -- the
	// mirror of the demo seeding that gives the real demo-owner account its
	// grants in a boot with APP_DEMO_USERS_PASSWORD set.
	tenantCtx := pkgcore.WithTenant(context.Background(), tenant)
	ownerSubject := rbac.Subject{TenantID: tenant, UserID: me.UserID}
	if assignErr := rbacService.AssignRole(tenantCtx, ownerSubject, rbac.BuiltinRoleOwner, rbac.Scope{}); assignErr != nil {
		t.Fatalf("AssignRole(owner, %q): %v", me.UserID, assignErr)
	}

	// The tenant's root node, created as the seeded demo owner (the rbac
	// header actor) -- the setup this suite's other flows use. The invite
	// below binds the invitee to it.
	var root orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", ownerToken, "",
		map[string]string{"name": "Browser Shaped Dental", "kind": "group"}, &root)
	if root.ID == "" {
		t.Fatalf("created root = %+v, want a non-empty id", root)
	}

	// THE browser-shaped call: POST /invitations carrying ONLY the bearer
	// token -- no X-Demo-User, no X-Demo-User-Id. Both the rbac gate and
	// org's own SubjectResolver must resolve the caller from the verified
	// Principal.
	const inviteeEmail = "browser-invitee@example.com"
	inviteBody, err := json.Marshal(map[string]string{"email": inviteeEmail, "nodeId": root.ID})
	if err != nil {
		t.Fatalf("marshal invite body: %v", err)
	}
	inviteReq, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/org/invitations", bytes.NewReader(inviteBody))
	if err != nil {
		t.Fatalf("build invite request: %v", err)
	}
	inviteReq.Header.Set("Authorization", "Bearer "+ownerToken)
	inviteReq.Header.Set("Content-Type", "application/json")
	inviteResp, err := srv.Client().Do(inviteReq)
	if err != nil {
		t.Fatalf("POST /api/v1/org/invitations: %v", err)
	}
	defer inviteResp.Body.Close()
	if inviteResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(inviteResp.Body)
		t.Fatalf("principal-only invite status = %d, want 201; body = %s", inviteResp.StatusCode, body)
	}
	var invitation orgInvitation
	if decodeErr := json.NewDecoder(inviteResp.Body).Decode(&invitation); decodeErr != nil {
		t.Fatalf("decode invite response: %v", decodeErr)
	}
	if invitation.NodeID != root.ID || invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want nodeId %q and status \"pending\"", invitation, root.ID)
	}

	// The invitation really went out to the address, not just that the
	// endpoint answered 201.
	mail := mailer.last(t)
	if len(mail.To) != 1 || mail.To[0] != inviteeEmail {
		t.Fatalf("mail.To = %v, want exactly [%q]", mail.To, inviteeEmail)
	}

	// The pending invitation is listed back -- the roster read a team
	// surface would make after the invite.
	listReq, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/org/invitations", nil)
	if err != nil {
		t.Fatalf("build invitations list request: %v", err)
	}
	listReq.Header.Set("Authorization", "Bearer "+ownerToken)
	listResp, err := srv.Client().Do(listReq)
	if err != nil {
		t.Fatalf("GET /api/v1/org/invitations: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("invitations list status = %d, want 200; body = %s", listResp.StatusCode, body)
	}
	var listed struct {
		Invitations []orgInvitation `json:"invitations"`
	}
	if decodeErr := json.NewDecoder(listResp.Body).Decode(&listed); decodeErr != nil {
		t.Fatalf("decode invitations list: %v", decodeErr)
	}
	found := false
	for _, inv := range listed.Invitations {
		if inv.ID == invitation.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("invitations list = %+v, want it to include the created invitation %q", listed.Invitations, invitation.ID)
	}
}
