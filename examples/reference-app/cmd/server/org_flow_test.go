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

	handler, cleanup, err := buildServer(context.Background(), cfg)
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
	ID         string `json:"id"`
	NodeID     string `json:"nodeId"`
	EmailIndex string `json:"emailIndex"`
	Status     string `json:"status"`
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
// It fails the test outright on anything outside 2xx, and otherwise decodes
// the response into out (nil to skip decoding, for 204 No Content
// responses).
func orgRequest(t *testing.T, srv *httptest.Server, method, path, token, subjectUserID string, body, out any) {
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
	if subjectUserID != "" {
		req.Header.Set(demoOrgUserHeader, subjectUserID)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s status = %d, want 2xx; body = %s",
			method, path, resp.StatusCode, respBody)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response for %s %s: %v", method, path, err)
		}
	}
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
	// tenant now: org's routes sit behind the authn+tenancy middleware
	// chain like every other route this app protects, and Host no longer
	// resolves a tenant for them (see server.go's middleware-chain doc
	// comment). The invitee authenticates as a DIFFERENT account than the
	// inviter, exactly as a real acceptance would be: the person accepting
	// is never the same HTTP caller who sent the invite. The two accounts'
	// emails are authn-internal placeholders -- only the invitation's own
	// email (inviteeEmail below) is what org's token binds to.
	const inviterUserID = "user-owner-1"
	const inviteeUserID = "user-new-hire-1"
	const inviteeEmail = "new-hire@example.com"

	inviterToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "org-owner")
	inviteeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "org-invitee")

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
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations", inviterToken, inviterUserID,
		map[string]string{"email": inviteeEmail, "nodeId": storeA.ID}, &invitation)
	if invitation.NodeID != storeA.ID || invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want nodeId %q and status \"pending\"", invitation, storeA.ID)
	}
	if invitation.EmailIndex == "" || invitation.EmailIndex == inviteeEmail {
		t.Fatalf("invitation.EmailIndex = %q, want a non-empty blind index distinct from the plaintext address "+
			"(the response must never echo the address itself -- see toInvitationResponse's own doc comment)",
			invitation.EmailIndex)
	}

	// The invitation response carries no token (see capturingMailer's own
	// doc comment on why): recover it from the message org actually sent,
	// exactly as the invitee would from their inbox.
	mail := mailer.last(t)
	if len(mail.To) != 1 || mail.To[0] != inviteeEmail {
		t.Fatalf("mail.To = %v, want exactly [%q]", mail.To, inviteeEmail)
	}
	token := tokenFromMail(t, mail)

	// Step 4: the invitee accepts, authenticated as themselves -- a
	// different subject than the inviter, exactly as a real acceptance
	// would be: the person accepting is never the same HTTP caller who sent
	// the invite.
	var membership orgMembership
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations/accept", inviteeToken, inviteeUserID,
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
