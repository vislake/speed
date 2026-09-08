package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// org_clinic_invitation_test.go is the regression for the e2e walk-through
// defect that hit exactly one population: org_createInvitation from a
// self-registered clinic answered HTTP 500 {"code":"org.internal_error"}
// every time, while the same call from a configured demo tenant
// (tenant-acme) answered 201. The Team page's "sending the invitation
// failed. Try again later." and the always-empty pending list were the
// browser's face of it.
//
// The population difference is a host-wiring gap, not an org-module bug:
// org's invitation email needs an accept link, and the host builds it
// through the WithInvitationLinkBuilder seam (go/org/mail.go), a
// tenant-scoped choice of host that is display, never acceptance --
// InviteService.Accept resolves the invitation's own tenant from the
// token, server-side, and this app allowlists the accept path through
// tenant resolution. server.go's builder drew its hosts from
// cfg.HostTenants (demoHostTenants, the two configured demo tenants)
// alone, and FAILED the whole invitation for any tenant outside the map
// -- which is every self-registered clinic: self_service.go's
// clinicTenantOf derives the clinic tenant as "tenant-" + the
// registrant's user id, by construction never a cfg.HostTenants value
// (that derivation's own doc comment says so), so a clinic owner could
// register, sign in and open the team surface, but no invitation from
// the clinic could ever be created. Every flow test that invited before
// this one invited from a configured demo tenant, which is why the
// composed defect sat invisible.
//
// The journey below is the walk-through's own shape, driven end to end
// through the real composed stack: a fresh account registers through the
// real register route (provisioning its own clinic, self_service.go),
// the browser-shaped sign-in lands in that clinic, the clinic's org tree
// answers the account's bearer token, and the invitation is created with
// the clinic's root node -- bearer token only, no demo identity header,
// the browser's own request shape. The invitation must answer 201, list
// back as pending, and the invitation email must reach the captured
// mailer carrying an accept link against the deployment's public origin
// -- never a configured demo tenant's branded host, and never a 500 that
// revokes the row the moment it is created (the pre-fix behavior: the
// link builder's "no host configured for tenant" error failed the
// delivery leg, InviteService.Invite revoked the fresh row and answered
// org.internal_error).
func TestOrgInvitation_SelfRegisteredClinicOwner_InvitationSucceedsEndToEnd(t *testing.T) {
	cfg := testConfig(t)
	mailer := &capturingMailer{}
	cfg.Mailer = mailer
	// The clinic population has no branded host in cfg.HostTenants, so its
	// accept links fall back to the deployment's own public origin
	// (server.go's org wiring); the test names one the way a deployment
	// names its own (APP_PUBLIC_ORIGIN), and asserts the mail link against
	// it below.
	const publicOrigin = "https://app.demo.localhost"
	cfg.PublicOrigin = publicOrigin

	handler, cleanup, _, buildErr := buildServer(context.Background(), cfg)
	if buildErr != nil {
		t.Fatalf("buildServer: %v", buildErr)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// A fresh clinic owner registers through the real register route; the
	// self-service provisioning chain (self_service.go) gives the account
	// its own clinic tenant with an org tree root, a membership of it and
	// the built-in owner role, synchronously with the 201.
	const ownerEmail = "clinic-owner@example.com"
	ownerUserID := registerFreshAccount(t, srv, ownerEmail, selfServicePassword)
	wantTenant := clinicTenantOf(ownerUserID)

	// The browser-shaped sign-in lands in the account's own clinic.
	status, code, token, tenant := browserSignIn(t, srv, ownerEmail, selfServicePassword)
	if status != http.StatusOK {
		t.Fatalf("browser-shaped sign-in of the clinic owner: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}
	if tenant != wantTenant {
		t.Fatalf("browser-shaped sign-in landed the owner in tenant %q, want their own clinic %q", tenant, wantTenant)
	}

	// The clinic's org tree answers the owner's bearer token -- bearer
	// only, no demo header, the browser's own shape (the same read
	// self_service_test.go's register-then-sign-in journey makes).
	var tree struct {
		Nodes []struct {
			ID    string `json:"id"`
			Depth int    `json:"depth"`
		} `json:"nodes"`
	}
	clinicOrgCall(t, srv, http.MethodGet, "/api/v1/org/nodes", token, nil, &tree)
	rootID := ""
	for _, node := range tree.Nodes {
		// The team view's own choice of root: the node at depth 0.
		if node.Depth == 0 {
			rootID = node.ID
		}
	}
	if rootID == "" {
		t.Fatalf("clinic org tree = %+v, want a depth-0 root node to invite into", tree.Nodes)
	}

	// THE walk-through call: the clinic owner sends an invitation into the
	// clinic's root, bearer token only. Pre-fix this answered HTTP 500
	// org.internal_error (the host wiring had no host for the clinic
	// tenant, so the mail leg failed and InviteService revoked the fresh
	// row); it must answer 201 with a pending invitation.
	const inviteeEmail = "clinic-invitee@example.com"
	var invitation orgInvitation
	clinicOrgCall(t, srv, http.MethodPost, "/api/v1/org/invitations", token,
		map[string]string{"email": inviteeEmail, "nodeId": rootID}, &invitation)
	if invitation.NodeID != rootID || invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want nodeId %q and status \"pending\"", invitation, rootID)
	}

	// The invitation is listed back as pending -- the pending list the
	// team surface renders stays populated, never the empty list the 500
	// left behind.
	var listed struct {
		Invitations []orgInvitation `json:"invitations"`
	}
	clinicOrgCall(t, srv, http.MethodGet, "/api/v1/org/invitations", token, nil, &listed)
	found := false
	for _, inv := range listed.Invitations {
		if inv.ID == invitation.ID && inv.Status == "pending" {
			found = true
		}
	}
	if !found {
		t.Fatalf("clinic invitations list = %+v, want it to include the pending invitation %q", listed.Invitations, invitation.ID)
	}

	// The invitation mail really went out, addressed to the invitee, and
	// its accept link points at the deployment's own public origin -- the
	// fallback server.go's link builder applies to a tenant with no
	// configured branded host. A configured demo tenant's host leaking
	// into a clinic invitation would hand the invitee a link that brands
	// someone else's tenant; the clinic's own links must carry the
	// deployment's origin.
	mail := mailer.last(t)
	if len(mail.To) != 1 || mail.To[0] != inviteeEmail {
		t.Fatalf("mail.To = %v, want exactly [%q]", mail.To, inviteeEmail)
	}
	match := acceptURLPattern.FindString(mail.Text)
	if match == "" {
		t.Fatalf("no accept URL found in the clinic invitation mail body: %q", mail.Text)
	}
	parsed, parseErr := url.Parse(match)
	if parseErr != nil {
		t.Fatalf("parse accept URL %q: %v", match, parseErr)
	}
	origin := parsed.Scheme + "://" + parsed.Host
	if origin != publicOrigin {
		t.Fatalf("clinic invitation accept link %q points at origin %q, want the deployment's own %q",
			match, origin, publicOrigin)
	}
	if parsed.Path != "/api/v1/org/invitations/accept" || parsed.Query().Get("token") == "" {
		t.Fatalf("clinic invitation accept link %q does not name org's accept endpoint with a token", match)
	}
}

// clinicOrgCall issues method against srv.URL+path authenticated ONLY as
// the bearer token -- no X-Demo-User, no X-Demo-User-Id. That is the
// browser's own request shape, and the only shape a clinic-tenant journey
// can take: orgRequest's demo identity header names the seeded demo owner,
// who holds no grant in a self-registered clinic tenant, so every
// permission-gated org route would refuse that header there. The rbac gate
// and org's SubjectResolver must resolve the caller from the verified
// Principal -- the clinic owner the registration provisioned with the
// built-in owner role in the clinic (self_service.go). It fails the test
// on anything outside 2xx -- printing the envelope, so a pre-fix run
// names the org.internal_error answer -- and otherwise decodes the
// response into out (nil to skip decoding).
func clinicOrgCall(t *testing.T, srv *httptest.Server, method, path, token string, body, out any) {
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
	req.Header.Set("Authorization", "Bearer "+token)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
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
}
