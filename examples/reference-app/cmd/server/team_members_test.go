package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// team_members_test.go is the consumer proof of the reference-app's own
// roster-with-identity answer (team_members.go): GET
// /api/reference-app/team-members answers the caller's tenant's org
// roster, every row enriched with the identity of the person behind it
// from authn's users table -- the display name the account registered
// with, or its email when no name was given. These tests drive the real
// composed stack (buildServer's own output) with the demo-user seed
// switched on, and authenticate their callers as the real seeded
// accounts -- bearer token, NO demo header -- so the rbac gate evaluates
// the Principal's own grants, the shape a browser request has.
//
// The failure these tests pin is the defect the team surface's walk-
// through found: the roster answered "who works here" with raw user ids,
// because go/org's member rows carry opaque ids only by its own module-
// boundary rule and nothing composed the name from elsewhere. The route
// under test IS the composition, so its assertions are that every roster
// row names the account behind it -- seeded accounts by the email they
// registered (they registered without display names), an invited-and-
// joined colleague by the display name their registration carried -- and
// that a caller without org:read is refused exactly like the org module
// route refuses one, and that a tenant's roster names only that tenant's
// own members. The web surface's own rendering is pinned by the web
// suites (src/views/team-view.test.tsx); this suite pins the answer
// those render.
//
// The answers are decoded by field name rather than through any
// generated type -- the same "assert on the wire shape" posture
// server_test.go's testNote takes -- because this host route belongs to
// no module fragment: its shape is hand-kept in step with src/team-api.ts
// and the demo server the web suites ride. The decode targets are the
// route's own wire types (team_members.go's teamMemberRow and
// teamMembersResponse, reachable here because this test lives in the
// same package): what the test asserts on is the JSON shape those types
// marshal, never a separately-typed copy that could drift from them.

// buildSeededTeamTestServer composes buildServer's real output with the
// demo-user seed switched on AND a capturingMailer standing in for the
// console mailer, so a test can both act as the seeded demo accounts
// (bearer tokens from demoLogin) and pull an invitation token out of the
// mail org actually sent (capturingMailer, org_flow_test.go).
func buildSeededTeamTestServer(t *testing.T) (*httptest.Server, *capturingMailer) {
	t.Helper()

	cfg := testConfig(t)
	cfg.DemoUsersPassword = demoSeedPassword
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
	return srv, mailer
}

// rosterRequest GETs teamMembersPath as token -- the bearer access token
// is the ONLY thing that selects a tenant and names the caller; no demo
// header rides along, exactly the shape a browser request has -- and
// returns the HTTP status plus either the decoded answer (200) or the
// decoded error envelope (anything else).
func rosterRequest(t *testing.T, srv *httptest.Server, token string) (int, teamMembersResponse, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+teamMembersPath, nil)
	if err != nil {
		t.Fatalf("build roster request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", teamMembersPath, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read roster response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		var envelope map[string]any
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("decode roster error body %q: %v", raw, err)
		}
		return resp.StatusCode, teamMembersResponse{}, envelope
	}
	var answer teamMembersResponse
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("decode roster body %q: %v", raw, err)
	}
	return resp.StatusCode, answer, nil
}

// signInAsDemo signs the demo account identified by email into tenant and
// returns its bearer token, failing the test on a refused sign-in.
func signInAsDemo(t *testing.T, srv *httptest.Server, email string, tenant pkgcore.TenantID) string {
	t.Helper()
	status, code, token := demoLogin(t, srv, email, demoSeedPassword, tenant)
	if status != http.StatusOK || code != "" {
		t.Fatalf("sign in as %s into %q: status %d code %q, want 200", email, tenant, status, code)
	}
	if token == "" {
		t.Fatalf("sign in as %s into %q: no access token answered", email, tenant)
	}
	return token
}

// expectEveryRowNamed asserts the roster's whole point: every member row
// carries a human identity (a display name or an email), never an empty
// pair -- a row nobody can name is exactly the raw-id defect this route
// exists to close, one member at a time.
func expectEveryRowNamed(t *testing.T, answer teamMembersResponse) {
	t.Helper()
	for _, row := range answer.Members {
		if row.DisplayName == "" && row.Email == "" {
			t.Errorf("member row %s (%s) carries no display identity: displayName %q email %q",
				row.MembershipID, row.UserID, row.DisplayName, row.Email)
		}
	}
}

// expectNoRawIDAsIdentity asserts that no row's identity fields carry the
// member's raw user id where a name belongs -- the exact defect this
// route exists to fix, at the wire level.
func expectNoRawIDAsIdentity(t *testing.T, answer teamMembersResponse) {
	t.Helper()
	for _, row := range answer.Members {
		if row.DisplayName == row.UserID || row.Email == row.UserID {
			t.Errorf("member row %s names the user by its raw id (%q)", row.MembershipID, row.UserID)
		}
	}
}

// rosterEmails returns the answer rows' emails as a set -- the seeded
// accounts all registered without a display name, so their rows' identity
// is their email, and member order is not load-bearing (org lists by
// node, then user id).
func rosterEmails(answer teamMembersResponse) map[string]struct{} {
	set := make(map[string]struct{}, len(answer.Members))
	for _, row := range answer.Members {
		set[row.Email] = struct{}{}
	}
	return set
}

func TestTeamMembersEndpoint_NamesEveryMemberOfTheTenantFromAuthn(t *testing.T) {
	srv, _ := buildSeededTeamTestServer(t)

	// The demo owner in the tenant-acme clinic: its roster holds the three
	// demo accounts seeded into that tenant (owner, reader and acme-only),
	// all registered without a display name -- so their rows' identity is
	// the email each account registered with, read from authn's users
	// table, never a raw user id.
	ownerToken := signInAsDemo(t, srv, demoOwnerEmail, demoSingleTenantID)
	status, answer, _ := rosterRequest(t, srv, ownerToken)
	if status != http.StatusOK {
		t.Fatalf("GET %s as the demo owner: status %d, want 200", teamMembersPath, status)
	}
	if len(answer.Members) != 3 {
		t.Fatalf("tenant-acme roster answered %d members, want the three seeded accounts", len(answer.Members))
	}
	expectEveryRowNamed(t, answer)
	expectNoRawIDAsIdentity(t, answer)

	emails := rosterEmails(answer)
	for _, email := range []string{demoOwnerEmail, demoReaderEmail, demoAcmeOnlyEmail} {
		if _, ok := emails[email]; !ok {
			t.Errorf("tenant-acme roster names no row %q (rows: %v)", email, emails)
		}
	}

	// The same owner in the other demo clinic sees only ITS OWN members:
	// the enrichment can never reach past the tenant whose roster the org
	// rows named -- demo-acme-only holds no seat in tenant-globex, so
	// tenant-globex's roster must not name it.
	globexToken := signInAsDemo(t, srv, demoOwnerEmail, pkgcore.TenantID("tenant-globex"))
	status, answer, _ = rosterRequest(t, srv, globexToken)
	if status != http.StatusOK {
		t.Fatalf("GET %s as the demo owner in tenant-globex: status %d, want 200", teamMembersPath, status)
	}
	emails = rosterEmails(answer)
	if _, ok := emails[demoAcmeOnlyEmail]; ok {
		t.Errorf("tenant-globex roster names demo-acme-only (%v), which holds no seat there", emails)
	}
	for _, email := range []string{demoOwnerEmail, demoReaderEmail} {
		if _, ok := emails[email]; !ok {
			t.Errorf("tenant-globex roster names no row %q (rows: %v)", email, emails)
		}
	}
}

func TestTeamMembersEndpoint_RefusesACallerWithoutOrgRead(t *testing.T) {
	srv, _ := buildSeededTeamTestServer(t)

	// The demo reader holds the note-reader role (notes:read and nothing
	// else) in every tenant -- the shape whose roster read the org module
	// route refuses with the rbac gate's 403, which is what closes the web
	// team surface's gate to it. This answer must refuse it identically.
	readerToken := signInAsDemo(t, srv, demoReaderEmail, demoSingleTenantID)
	status, _, envelope := rosterRequest(t, srv, readerToken)
	if status != http.StatusForbidden {
		t.Fatalf("GET %s as the demo reader: status %d, want the rbac gate's 403", teamMembersPath, status)
	}
	code, _ := envelope["code"].(string)
	if code != "rbac.permission_denied" {
		t.Fatalf("GET %s as the demo reader: code %q, want %q", teamMembersPath, code, "rbac.permission_denied")
	}
}

func TestTeamMembersEndpoint_NamesAnInvitedColleagueByTheirRegisteredDisplayName(t *testing.T) {
	srv, mailer := buildSeededTeamTestServer(t)
	ownerToken := signInAsDemo(t, srv, demoOwnerEmail, demoSingleTenantID)

	// The clinic's second employee: a self-registered account that typed a
	// display name at registration -- the shape a colleague invited by a
	// real clinic has. Registration goes through the real register route
	// with the display-name field the register form carries; authn stores
	// the name on the user row this route enriches from.
	const inviteeEmail = "team-flow-invitee@example.com"
	const inviteeDisplayName = "Lin Chen"
	registerBody, err := json.Marshal(map[string]string{
		"email":        inviteeEmail,
		"password":     testPassword,
		"display_name": inviteeDisplayName,
	})
	if err != nil {
		t.Fatalf("marshal register body: %v", err)
	}
	registerResp, err := srv.Client().Post(srv.URL+"/api/v1/authn/register", "application/json", bytes.NewReader(registerBody))
	if err != nil {
		t.Fatalf("register %s: %v", inviteeEmail, err)
	}
	defer registerResp.Body.Close()
	if registerResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(registerResp.Body)
		t.Fatalf("register %s status = %d, want 201; body = %s", inviteeEmail, registerResp.StatusCode, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(registerResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode register response for %s: %v", inviteeEmail, err)
	}
	if created.ID == "" {
		t.Fatalf("register %s: response carried no id", inviteeEmail)
	}

	// The owner invites the colleague into the clinic's root node -- the
	// only node the demo clinics have -- and the colleague accepts through
	// org's real accept route, the token recovered from the mail org sent
	// (exactly as org_flow_test.go's own invitation journey does). The
	// accept names the acceptor by the user id authn assigned at
	// registration, so the membership row this route enriches is the real
	// account's own.
	var nodes struct {
		Nodes []struct {
			ID    string `json:"id"`
			Depth int    `json:"depth"`
		} `json:"nodes"`
	}
	orgRequest(t, srv, http.MethodGet, "/api/v1/org/nodes", ownerToken, "", nil, &nodes)
	rootID := ""
	for _, node := range nodes.Nodes {
		if node.Depth == 0 {
			rootID = node.ID
		}
	}
	if rootID == "" {
		t.Fatal("tenant-acme has no root node to invite into")
	}

	var invitation struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations", ownerToken, "",
		map[string]string{"email": inviteeEmail, "nodeId": rootID}, &invitation)
	if invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want status \"pending\"", invitation)
	}
	token := tokenFromMail(t, mailer.last(t))

	var membership struct {
		UserID string `json:"userId"`
		Status string `json:"status"`
	}
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations/accept", "", created.ID,
		map[string]string{"token": token}, &membership)
	if membership.UserID != created.ID || membership.Status != "active" {
		t.Fatalf("membership after accept = %+v, want userId %q, status \"active\"", membership, created.ID)
	}

	// The clinic's roster now names the colleague by the display name they
	// registered with -- the row the walk-through would previously have
	// answered as another raw user id.
	status, answer, _ := rosterRequest(t, srv, ownerToken)
	if status != http.StatusOK {
		t.Fatalf("GET %s as the demo owner: status %d, want 200", teamMembersPath, status)
	}
	var inviteeRow *teamMemberRow
	for i := range answer.Members {
		if answer.Members[i].UserID == created.ID {
			inviteeRow = &answer.Members[i]
		}
	}
	if inviteeRow == nil {
		t.Fatalf("roster answers no row for the accepted colleague (rows: %+v)", answer.Members)
	}
	if inviteeRow.DisplayName != inviteeDisplayName {
		t.Errorf("the colleague's row is named %q, want the display name %q their registration carried",
			inviteeRow.DisplayName, inviteeDisplayName)
	}
	if inviteeRow.Email != inviteeEmail {
		t.Errorf("the colleague's row carries email %q, want %q", inviteeRow.Email, inviteeEmail)
	}
	expectEveryRowNamed(t, answer)
	expectNoRawIDAsIdentity(t, answer)
}
