package flowtests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
)

// org_invitation_signin_test.go is the end-to-end regression for the
// invited-user sign-in path: org invitation acceptance creates a
// real, persistent org Membership row, and sign-in must keep honoring that
// row -- in the process that accepted it and in every later process booted
// against the same database. The membership answers come from org's own
// rows, read live through the org-backed internal/app/sign_in_memberships.go store; an
// in-process roster alone would lose an accepted invitation's membership
// when the accepting process exited, leaving the invited user unable to
// sign in to the invited tenant: 403 authn.tenant_membership_required
// forever, exactly the failure a real invited user hits on a deployed
// instance that stopped and came back between their acceptance and their
// first sign-in.
//
// The shape: the invitee is
// registered through authn's real register route and NEVER granted a
// membership by hand -- its only path to a membership in the INVITING
// tenant is really accepting the invitation below. (Its registration does
// provision its own clinic under this app's self-service signup --
// internal/app/self_service.go -- which is a DIFFERENT tenant: the sign-ins this test
// drives all name tenant-acme explicitly, so the clinic never answers
// them, and the pre-invitation control below remains a genuine control
// for the tenant that matters: an invitee cannot enter the inviting
// tenant until the acceptance creates the membership.) And the acceptance
// itself is performed the way a real memberless invitee must perform it:
// the accept request carries NO bearer token at all (the invitee holds no
// token for the inviting tenant -- sign-in into it is impossible until
// the acceptance below creates one) and names the REAL registered invitee
// as the acting subject through the demo identity header.
// org_acceptInvitation resolves the invitation's own tenant from the
// token, server-side, and this app's tenancy allowlist lets the accept
// path through unresolved -- a memberless invitee can therefore accept
// with no bearer of its own, and the acceptance grants a real
// sign-in-able membership. Boot one proves the accept
// grants sign-in in process; the server shuts down completely, and boot
// two against the same database proves the sign-in survives the restart.

// registerOnlyRealAccount registers email through authn's real register
// route and returns the user id authn assigned, WITHOUT ever granting the
// account a membership anywhere by hand -- this account must reach its
// membership in the inviting tenant through org's own real
// invitation-accept flow alone. (Its registration's self-service clinic,
// internal/app/self_service.go, is a different tenant and plays no part in this test's
// explicit-tenant sign-ins.)
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

// TestInvitationAccept_SignInSurvivesTheAcceptingProcess tests the invited
// user's whole journey across a real two-boot restart against one database
// file: invite through org's real HTTP routes, accept through org's real
// accept route naming the real registered invitee, sign in -- and then,
// after the accepting server has shut down completely and a fresh one has
// booted against the same database, sign in again. The membership org
// wrote is a database row and must answer for itself; nothing the
// accepting process held in memory may be load-bearing.
func TestInvitationAccept_SignInSurvivesTheAcceptingProcess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reference-app-invite-restart.db")

	// boot composes a server against the shared dbPath with its own fresh
	// mailer and membership store (the honest image of a restart), without
	// any test cleanup: the caller closes and cleans up each boot
	// explicitly, in order. cfg comes back too: registerAndAuthenticate
	// reaches the membership store through it, exactly as every other flow
	// test in this package does.
	boot := func() (*httptest.Server, app.ServerConfig, *capturingMailer, func() error) {
		cfg := testConfig(t)
		cfg.SQLitePath = dbPath
		mailer := &capturingMailer{}
		cfg.Mailer = mailer
		handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
		if err != nil {
			t.Fatalf("BuildServer: %v", err)
		}
		return httptest.NewServer(handler), cfg, mailer, cleanup
	}

	const inviteeEmail = "restart-invitee@example.com"
	const inviteePassword = "a perfectly fine invitee passphrase"

	// ---- Boot one: invite, register, accept, sign in ----
	srv1, cfg1, mailer1, cleanup1 := boot()

	// The inviter's own membership goes through the test shortcut, same as
	// every org flow test in this package -- that is not what this defect
	// is about (registering an inviter needs SOME membership to even obtain
	// a bearer token that resolves a tenant). What must NEVER go through
	// the shortcut is the INVITEE, below.
	inviterToken := registerAndAuthenticate(t, srv1, cfg1, "tenant-acme", "restart-invite-flow-owner")

	var root orgNode
	orgRequest(t, srv1, http.MethodPost, "/api/v1/org/nodes", inviterToken, "",
		map[string]string{"name": "Restart Invite Flow Group", "kind": "group"}, &root)
	if root.ID == "" {
		t.Fatalf("created root = %+v, want a non-empty id", root)
	}

	// The invitee is registered through the real register route ONLY --
	// its only path to a membership must be really accepting the
	// invitation below.
	inviteeUserID := registerOnlyRealAccount(t, srv1, inviteeEmail, inviteePassword)

	// The invitee's registration provisioned its own self-service clinic
	// (a different tenant, internal/app/self_service.go), and signing into it succeeds
	// -- the account is real and the password right, and the clinic bearer
	// is what the login-history read after the control below needs
	// (history is the one place a refusal's real reason survives).
	status, code, inviteeClinicToken, _ := browserSignIn(t, srv1, inviteeEmail, inviteePassword)
	if status != http.StatusOK {
		t.Fatalf("sign-in of the freshly registered invitee into its own clinic: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}

	// Control: before any invitation exists, the freshly registered
	// invitee cannot sign into the inviting tenant -- its only membership
	// so far is the self-service clinic its registration provisioned
	// (internal/app/self_service.go), which is a different tenant, so this
	// explicit-tenant sign-in proves the invitee starts with no seat in the
	// tenant the invitation will open. The refusal is the unified 401
	// authn.invalid_credentials answer a wrong password also gets -- the
	// account is real and the password right, but the login endpoint must
	// not say so (the specific reason lives in
	// the login history, never the response). The account's reality is
	// proven from both sides: the clinic sign-in above, and the same
	// credentials answering 200 below, once the acceptance creates the
	// membership.
	status, code, _ = demoLogin(t, srv1, inviteeEmail, inviteePassword, "tenant-acme")
	if status != http.StatusUnauthorized || code != "authn.invalid_credentials" {
		t.Fatalf("pre-invitation login: status = %d, code = %q, want 401 %q", status, code, "authn.invalid_credentials")
	}
	// The refusal's real reason: the 401 above is also a wrong password's
	// answer, so the login history -- the one place authn writes the
	// specific reason -- must show this attempt as the no-membership
	// refusal it is (the clinic sign-in just above proved the password;
	// history proves the tenant-acme refusal was the missing membership,
	// the exact pre-state this control exists to establish).
	assertNoMembershipRefusal(t, srv1, inviteeClinicToken, "the pre-invitation sign-in of the real invitee into tenant-acme")

	// Invite the real invitee's email into the new root node, through
	// org's real HTTP invite route, and recover the token from the mail
	// org actually sent.
	var invitation orgInvitation
	orgRequest(t, srv1, http.MethodPost, "/api/v1/org/invitations", inviterToken, "restart-invite-flow-owner-actor",
		map[string]string{"email": inviteeEmail, "nodeId": root.ID}, &invitation)
	if invitation.NodeID != root.ID || invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want nodeId %q and status \"pending\"", invitation, root.ID)
	}
	mail := mailer1.last(t)
	if len(mail.To) != 1 || mail.To[0] != inviteeEmail {
		t.Fatalf("mail.To = %v, want exactly [%q]", mail.To, inviteeEmail)
	}
	token := tokenFromMail(t, mail)

	// Accept through org's real HTTP accept route, naming the REAL
	// registered invitee's id as the acting subject -- never a
	// cfg.Memberships.Grant on its behalf -- and carrying NO bearer token,
	// because this invitee genuinely has no token for the inviting tenant:
	// authn's own sign-in into it refused the account above (the control
	// leg), so the only credential the invitee holds for this tenant is the
	// invitation token itself, and org's tenantless accept resolves the
	// inviting tenant from it -- borrowing the inviter's target-tenant
	// bearer would be impossible for a real memberless invitee, since
	// org's accept path is what creates the membership that makes sign-in
	// possible at all. The membership this call creates is org's own real
	// row, and whether that row reaches authn's sign-in path is exactly
	// what this test measures.
	var membership orgMembership
	orgRequest(t, srv1, http.MethodPost, "/api/v1/org/invitations/accept", "", inviteeUserID,
		map[string]string{"token": token}, &membership)
	if membership.UserID != inviteeUserID || membership.NodeID != root.ID || membership.Status != "active" {
		t.Fatalf("membership after accept = %+v, want userId %q, nodeId %q, status \"active\"",
			membership, inviteeUserID, root.ID)
	}

	// The really-invited user can now sign in -- in the accepting process.
	status, code, _ = demoLogin(t, srv1, inviteeEmail, inviteePassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("post-accept login in the accepting process: status = %d, code = %q, want %d",
			status, code, http.StatusOK)
	}

	// Shut boot one down completely -- its cleanup also releases the
	// database file -- before booting again against the same path. The
	// membership org wrote must not need boot one for anything.
	srv1.Close()
	if err := cleanup1(); err != nil {
		t.Fatalf("boot-one cleanup: %v", err)
	}

	// ---- Boot two: the invited user signs in against the same database ----
	srv2, _, _, cleanup2 := boot()
	defer func() {
		srv2.Close()
		if err := cleanup2(); err != nil {
			t.Errorf("boot-two cleanup: %v", err)
		}
	}()

	// THE property this regression exists to protect: the invited user's
	// sign-in survives the process that accepted the invitation. The org
	// row is real and persistent; an in-process roster would be empty on
	// boot two, and this login would answer
	// 403 authn.tenant_membership_required forever.
	status, code, _ = demoLogin(t, srv2, inviteeEmail, inviteePassword, "tenant-acme")
	if status != http.StatusOK {
		t.Fatalf("post-restart login as the invited user: status = %d, code = %q, want %d "+
			"(a real accepted org invitation must grant sign-in to every process booted against its database)",
			status, code, http.StatusOK)
	}
}
