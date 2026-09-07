package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// register_tenant_attest_flow_test.go is the P1-authn-15 regression in the
// reference-app shape: an authenticated tenant member registering a new
// account through the app's own register form used to stamp their tenant
// onto the new account's authn.user.created event.
//
// The chain that made it a common case is this host's own composition:
// authn's pre-auth routes sit inside the authn+tenancy middleware chain
// (server.go's authnPreAuthAllowlist), and go/tenancy.WithAllowlist only
// exempts a route from the 403 when tenant RESOLUTION FAILS -- it never
// skips resolution, so a register request carrying a valid bearer still
// gets the caller's tenant injected into its context. The api-client
// attaches the held token to every request by default, so a signed-in
// caller's register POST reaches authn's handler with their tenant in the
// context, and (before this round) Service.publish stamped it onto the
// event. Org's own handleUserCreated (go/org/events.go) then seated the
// fresh account in the CALLER's tenant, while this host's tenant-less
// self-service provisioning (self_service.go) skipped it -- the new
// account got a membership it was never granted and no clinic of its own.
//
// Failing before the fix: the fresh account lands on tenant-acme's org
// roster and its sign-in resolves into tenant-acme. Passing after: the
// account is absent from the roster, its sign-in naming tenant-acme is
// refused with the unified 401 authn.invalid_credentials answer a wrong
// password also gets (the fold of no-membership logins into
// ErrInvalidCredentials: the login endpoint discloses nothing about
// whether the password verified), and the signup provisioning path gives
// it its own clinic (the browser-shaped sign-in lands in the
// deterministic clinicTenantOf tenant).
func TestRegister_AuthenticatedCallersBearer_NeverSeatsTheAccountInTheirTenant(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)

	const freshEmail = "p1-authn-fresh@example.com"
	const freshPassword = "the fresh account's own passphrase"

	// The caller: a real, signed-in member of tenant-acme -- the
	// reviewer's demo-owner stand-in, an account that genuinely holds a
	// tenant-acme bearer (registerAndAuthenticate drives the real register
	// and login routes and grants the tenant membership the sign-in
	// verifies).
	callerToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "p1-authn-caller")

	// tenant-acme's org root is created BEFORE the register, through org's
	// real HTTP surface -- the state a real tenant already has, which is
	// what makes the roster question meaningful in both the pre-fix and
	// the post-fix shape (a tenant's own members are the accounts org's
	// handleUserCreated subscriber would seat there).
	var acmeRoot orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", callerToken, "",
		map[string]string{"name": "Acme Dental Group", "kind": "group"}, &acmeRoot)
	if acmeRoot.ID == "" || acmeRoot.ParentID != "" {
		t.Fatalf("created root = %+v, want a non-empty id and empty parentId", acmeRoot)
	}

	// The register itself: a fresh account created THROUGH THE APP'S OWN
	// REGISTER FORM SHAPE, with the caller's valid bearer attached exactly
	// as the api-client attaches it to every request. Registration is a
	// public pre-tenant operation, so this must succeed -- the account is
	// an independent one, never a creation inside the caller's tenant.
	body, err := json.Marshal(map[string]string{"email": freshEmail, "password": freshPassword})
	if err != nil {
		t.Fatalf("marshal register body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/authn/register", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build register request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+callerToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("register %s with the caller's bearer: %v", freshEmail, err)
	}
	freshID := func() string {
		defer resp.Body.Close()
		raw, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatalf("read register response: %v", readErr)
		}
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("register %s with the caller's bearer: status = %d, want %d; body = %s",
				freshEmail, resp.StatusCode, http.StatusCreated, raw)
		}
		var user struct {
			ID string `json:"id"`
		}
		if decodeErr := json.Unmarshal(raw, &user); decodeErr != nil {
			t.Fatalf("decode register response: %v", decodeErr)
		}
		if user.ID == "" {
			t.Fatalf("register %s: response carried no id", freshEmail)
		}
		return user.ID
	}()

	// Leg 1, the roster: tenant-acme's members, read from the tenant's own
	// root as the caller -- the fresh account must NOT be among them. A
	// user-created event carrying the caller's tenant would have put it
	// there (org's handleUserCreated); a tenant-less event leaves the
	// roster untouched.
	var roster orgListMembersResponse
	orgRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/org/members?nodeId=%s", url.QueryEscape(acmeRoot.ID)),
		callerToken, "", nil, &roster)
	if containsUserID(roster.Members, freshID) {
		t.Fatalf("tenant-acme's member roster = %+v, must NOT contain the freshly registered account %q: "+
			"registration under an authenticated caller's bearer used to stamp the caller's tenant onto "+
			"authn.user.created, and org's own subscriber seated the account in the caller's tenant",
			roster.Members, freshID)
	}

	// Leg 2, the refusal: a sign-in of the fresh account naming
	// tenant-acme is refused -- the account holds no membership there. The
	// refusal is the unified 401 authn.invalid_credentials answer a wrong
	// password also gets (this leg used to pin the distinguishable 403
	// authn.tenant_membership_required; the answer was deliberately
	// unified, the specific reason now recorded in the login history,
	// never the response).
	status, code, _ := demoLogin(t, srv, freshEmail, freshPassword, "tenant-acme")
	if status != http.StatusUnauthorized || code != "authn.invalid_credentials" {
		t.Fatalf("sign-in of the freshly registered account into tenant-acme: status = %d, code = %q, want 401 %q "+
			"(the account must hold no membership in the registering caller's tenant)",
			status, code, "authn.invalid_credentials")
	}

	// Leg 3, the account's own clinic: the signup provisioning path (this
	// host's tenant-less self-service subscriber) runs for the register,
	// so the browser-shaped sign-in lands the account in its OWN clinic --
	// the deterministic tenant derived from its own user id, never
	// tenant-acme.
	clinic := pkgcore.TenantID("tenant-" + freshID)
	status, code, freshToken, tenant := browserSignIn(t, srv, freshEmail, freshPassword)
	if status != http.StatusOK {
		t.Fatalf("browser-shaped sign-in of the freshly registered account: status = %d, code = %q, want %d "+
			"(registration must provision the clinic its account can sign into)",
			status, code, http.StatusOK)
	}
	if tenant != clinic {
		t.Fatalf("browser-shaped sign-in of an account registered under an authenticated caller's bearer landed in "+
			"tenant %q, want its own clinic %q: the caller's tenant must never steer the account's provisioning",
			tenant, clinic)
	}

	// Leg 2's refusal was the unified 401 authn.invalid_credentials a
	// wrong password also gets, so its real reason must be read from the
	// login history -- the one place authn writes it: this very sign-in
	// proved the password right, and the account's newest failed attempt
	// (leg 2's) must record the no-membership refusal rather than a bad
	// password, so the leg-2 401 stays evidence of "no seat in
	// tenant-acme" instead of a credential failure proving nothing.
	assertNoMembershipRefusal(t, srv, freshToken, "sign-in of the freshly registered account into tenant-acme")
}
