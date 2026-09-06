package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// demo_org_membership_sync_test.go is the mandatory end-to-end regression
// for Finding 3 of the reference-app-go.md audit: an org membership created
// by REALLY accepting an invitation (through org's own real HTTP flow) used
// to be permanently disconnected from authn's sign-in path, because nothing
// told demoMemberships the new membership existed -- the invited user could
// accept for real and still never sign in, forever refused with authn's
// tenant_membership_required. org_flow_test.go's own existing invite/accept
// test never caught this: it authenticates its "invitee" through
// registerAndAuthenticate, which calls cfg.Memberships.Grant directly and
// binds the accepted membership to a wholly separate, synthetic user id
// (inviteeUserID = "user-new-hire-1") that no real authn account holds --
// so it never actually exercises "can THIS real, invited account sign in
// after accepting", which is exactly what this test does instead.

// registerOnlyRealAccount registers email through authn's real register
// route and returns the user id authn assigned, WITHOUT ever touching
// cfg.Memberships -- the one difference from registerAndAuthenticate
// (server_test.go) that matters here: this account must reach its first
// membership through org's own real invitation-accept flow alone, never
// through the test-only shortcut.
func registerOnlyRealAccount(t *testing.T, srv *httptest.Server, email, password string) (userID string) {
	t.Helper()

	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("marshal register body: %v", err)
	}
	resp, err := srv.Client().Post(srv.URL+"/api/v1/authn/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("register %s: status = %d, want %d; body = %s", email, resp.StatusCode, http.StatusCreated, raw)
	}
	var user struct {
		ID string `json:"id"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&user); decodeErr != nil {
		t.Fatalf("decode register response for %s: %v", email, decodeErr)
	}
	if user.ID == "" {
		t.Fatalf("register %s: response carried no id", email)
	}
	return user.ID
}

// TestOrgInvitationAccept_RealHTTPFlow_GrantsSignIn drives the full,
// real-HTTP shape Finding 3 requires: invite a real registered user through
// org's real HTTP invite route, accept the invitation through org's real
// HTTP accept route (naming the REAL registered user id as the accepting
// subject, never a synthetic one), then attempt to sign in as that exact
// user through authn's real HTTP login route.
//
// Before the fix (demo_org_membership_sync.go's subscription), accepting
// changed nothing about whether the invitee could sign in: the membership
// existed in org's own real table, but demoMemberships -- the store authn's
// sign-in path actually reads -- never heard about it, so the post-accept
// login stayed refused exactly like the pre-accept one. After the fix, the
// subscription grants demoMemberships the moment org's own
// org.member.joined event lands, and the post-accept login succeeds.
func TestOrgInvitationAccept_RealHTTPFlow_GrantsSignIn(t *testing.T) {
	srv, cfg, mailer := buildOrgTestServer(t)

	const inviteeEmail = "real-invitee@example.com"
	const inviteePassword = "a perfectly fine invitee passphrase"

	// The inviter's own membership is granted through the test shortcut,
	// same as org_flow_test.go's existing test -- that is not what this
	// finding is about (registering an inviter needs SOME membership to
	// even obtain a bearer token that resolves a tenant, a chicken-and-egg
	// problem every org test in this package solves the identical way).
	// What must NOT go through the shortcut is the INVITEE, below.
	inviterToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "invite-flow-owner")

	var root orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", inviterToken, "",
		map[string]string{"name": "Real Invite Flow Group", "kind": "group"}, &root)
	if root.ID == "" {
		t.Fatalf("created root = %+v, want a non-empty id", root)
	}

	// The invitee is registered through the real register route ONLY --
	// deliberately never granted a membership directly, which is the whole
	// point: its only path to a membership must be really accepting the
	// invitation below.
	inviteeUserID := registerOnlyRealAccount(t, srv, inviteeEmail, inviteePassword)

	// Control: before any invitation exists, the freshly registered,
	// memberless invitee cannot sign in at all -- confirms the account is
	// real and genuinely starts with no membership anywhere (the same
	// refusal TestDemoUsers_RegisteredButMemberless_BrowserShapedSignInRefused
	// pins for the general case).
	status, code, _ := demoLogin(t, srv, inviteeEmail, inviteePassword, "tenant-acme")
	if status != http.StatusForbidden || code != "authn.tenant_membership_required" {
		t.Fatalf("pre-invitation login: status = %d, code = %q, want 403 %q", status, code, "authn.tenant_membership_required")
	}

	// Invite the real invitee's email into the new root node, through
	// org's real HTTP invite route.
	var invitation orgInvitation
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations", inviterToken, "invite-flow-inviter-actor",
		map[string]string{"email": inviteeEmail, "nodeId": root.ID}, &invitation)
	if invitation.NodeID != root.ID || invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want nodeId %q and status \"pending\"", invitation, root.ID)
	}

	mail := mailer.last(t)
	if len(mail.To) != 1 || mail.To[0] != inviteeEmail {
		t.Fatalf("mail.To = %v, want exactly [%q]", mail.To, inviteeEmail)
	}
	token := tokenFromMail(t, mail)

	// Accept the invitation through org's real HTTP accept route -- naming
	// inviteeUserID (the REAL registered account's own id) as the acting
	// subject. This is the one line that must NEVER be
	// cfg.Memberships.Grant(inviteeUserID, "tenant-acme"): the membership
	// this call creates is org's own real row, and whether it reaches
	// authn's sign-in path is exactly what this test measures.
	var membership orgMembership
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations/accept", inviterToken, inviteeUserID,
		map[string]string{"token": token}, &membership)
	if membership.UserID != inviteeUserID || membership.NodeID != root.ID || membership.Status != "active" {
		t.Fatalf("membership after accept = %+v, want userId %q, nodeId %q, status \"active\"",
			membership, inviteeUserID, root.ID)
	}

	// THE property Finding 3 exists to fix: the real, really-invited user
	// can now sign in through authn's real HTTP login route, with no
	// cfg.Memberships.Grant call ever made on its behalf.
	status, code, accessToken := demoLogin(t, srv, inviteeEmail, inviteePassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("post-accept login: status = %d, code = %q, want %d "+
			"(a really-accepted org invitation must grant real sign-in, not just an internal Membership row)",
			status, code, http.StatusOK)
	}
	if accessToken == "" {
		t.Fatal("post-accept login: status 200 but no access_token")
	}
}
