package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
)

// TestOrgAuditCapture_ImpersonatedMemberRemovalAndNodeDelete_LeaveDualIdentityRows
// is the regression: an administrator removing a member or deleting
// an org node through the real composed stack leaves an audit row carrying
// the operator identity -- and, performed during an impersonation session,
// the row carries BOTH identities, per the hard rule: Actor
// (the impersonated user) and OnBehalfOf (the real administrator). Before
// org's declared audit actions had zero
// emission, no dbkit.Auditable opt-in and no wired capture, so these two
// operations produced NO audit row at all -- the same class the rbac
// zero-audit gap graded, the easiest accountability accident
// in the ops console.
//
// The fix this test pins is doc 10's own "automatic first, declaration
// second" route, not hand-written audit.Emit at each write path: org's
// OrgNode, Membership and Invitation models implement dbkit.Auditable,
// buildServer wires dbkit.Options.AuditBus (with Options.AuditModels set
// to org's own exported org.AuditableModels() -- notes.Note is outside
// that scope by construction, since notes records its own trail through
// audit.Emit), and go/dbkit/audit's persister module turns the captured
// writes into rows under the capture-derived actions org declares
// ("org.node.create"/"org.node.update", "org.member.create"/
// "org.member.update", "org.invitation.create"/"org.invitation.update").
// Note what the assertion means by "removal" and
// "delete" in that vocabulary: a member removal and a node delete are
// mark-delete UPDATEs underneath, so the rows they leave are
// "org.member.update" and "org.node.update" respectively, distinguishable
// from an ordinary update by the written deleted_at/deleted_by columns in
// the changes diff (see go/org's own audit-trail doc comments).
//
// The test drives the REAL impersonation pipeline (admin's
// ImpersonationMiddleware, exactly as admin_flow_test.go's
// TestAdminFlow_Impersonation_EndToEnd does): the platform staff starts a
// grant naming demo-owner in tenant-acme, then every org request below is
// made with the staff's own bearer token plus the grant header and no demo
// headers, so both the rbac gate and org's subject resolution see the
// substituted principal. The rows are read back through a real
// go/dbkit/audit Repository over a second connection to the same SQLite
// file -- the same reach server_test.go's note-audit test uses -- never a
// mock or an in-process event assertion.
//
// Determinism: each test builds its own server on its own fresh SQLite
// file, performs a fixed sequence of operations and asserts exact row
// counts per action, so -count=1 runs are repeatable without any timing
// element -- the in-process bus delivers captured events synchronously
// before the triggering HTTP response returns.
func TestOrgAuditCapture_ImpersonatedMemberRemovalAndNodeDelete_LeaveDualIdentityRows(t *testing.T) {
	srv, cfg, _ := buildAdminTestServer(t)
	staffToken := platformStaffToken(t, srv)
	staffID := searchStaffID(t, srv, staffToken)

	// The impersonation target is the seeded demo-owner account: it holds
	// every permission (org:manage, org:remove_member and friends) in
	// every configured tenant, which is what lets the org requests below
	// succeed rather than merely proving the gate closes.
	var searched adminSearchUsersResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/admin/users?email="+demoOwnerEmail, staffToken, nil, http.StatusOK, &searched, nil)
	if len(searched.Users) != 1 {
		t.Fatalf("search for %q = %+v, want exactly one seeded account", demoOwnerEmail, searched.Users)
	}
	ownerID := searched.Users[0].ID

	var grant adminGrant
	adminRequest(t, srv, http.MethodPost, "/api/v1/admin/impersonation", staffToken,
		map[string]string{
			"targetUserId":   ownerID,
			"targetTenantId": "tenant-acme",
			"reason":         "org P1 audit regression: verify removals and deletes are recorded",
		}, http.StatusCreated, &grant, nil)
	if grant.ID == "" || grant.TargetUserID != ownerID || grant.TargetTenantID != "tenant-acme" {
		t.Fatalf("start-impersonation response = %+v, want a grant naming %q in tenant-acme", grant, ownerID)
	}
	// Every org request below rides on the staff's own bearer token plus
	// the impersonation header, with no demo headers.
	impersonationHeaders := map[string]string{"X-Admin-Impersonation": grant.ID}

	// --- Leg A: an impersonated member removal. ---
	// The victim is a seeded non-owner member of tenant-acme (the roster
	// holds demo-owner, demo-reader and demo-acme-only, all active at the
	// tenant's root -- demo_users.go's addDemoOrgMembership seed), so the
	// removal passes Remove's not-the-last-active-member guard.
	var roster orgListMembersResponse
	adminRequest(t, srv, http.MethodGet, "/api/v1/org/members", staffToken, nil, http.StatusOK, &roster, impersonationHeaders)
	if len(roster.Members) < 2 {
		t.Fatalf("tenant-acme roster = %+v, want at least two seeded members (the removal guard needs a non-last victim)", roster.Members)
	}
	var victim orgMembership
	for _, m := range roster.Members {
		if m.UserID != ownerID && m.Status == "active" {
			victim = m
			break
		}
	}
	if victim.MembershipID == "" {
		t.Fatalf("no removable non-owner member found in roster %+v", roster.Members)
	}
	adminRequest(t, srv, http.MethodDelete, "/api/v1/org/members/"+victim.UserID, staffToken, nil, http.StatusNoContent, nil, impersonationHeaders)

	memberRows := auditRowsForTenant(t, cfg, "tenant-acme")
	// The removal records exactly two org.member.update rows in this
	// fresh tenant: the row that IS the removal (resource id = the
	// membership row removed, diff carrying the mark-delete columns) and
	// the bulk row-lock write removeIfNotLastActive issues over the
	// tenant's active memberships before the deletion (an empty
	// {deleted_by: ""} diff -- see go/org's own lock-touch note).
	// Exact counts are asserted so a double record (capture plus a
	// hand-written Emit) or a missed write both fail this test.
	var memberUpdates, memberLockTouches []auditRow
	for _, row := range memberRows {
		if row.Action != orgActionMemberUpdate {
			continue
		}
		if row.ResourceID == victim.MembershipID {
			memberUpdates = append(memberUpdates, row)
		} else {
			memberLockTouches = append(memberLockTouches, row)
		}
	}
	if len(memberUpdates) != 1 {
		t.Fatalf("org.member.update rows naming membership %q = %+v, want exactly 1 (the removal); all tenant rows = %+v", victim.MembershipID, memberUpdates, memberRows)
	}
	if len(memberLockTouches) != 1 {
		t.Fatalf("org.member.update rows not naming the removed membership = %+v, want exactly 1 (Remove's own active-member lock write); all tenant rows = %+v", memberLockTouches, memberRows)
	}
	removal := memberUpdates[0]
	if removal.ActorID != ownerID {
		t.Errorf("removal ActorID = %q, want the impersonated owner %q", removal.ActorID, ownerID)
	}
	if removal.OnBehalfOfID == nil || *removal.OnBehalfOfID != staffID {
		t.Errorf("removal OnBehalfOfID = %v, want the real administrator %q", removal.OnBehalfOfID, staffID)
	}
	if after := afterDiff(t, removal.Changes); after["deleted_by"] != ownerID || after["deleted_at"] == nil {
		t.Errorf("removal changes after = %v, want deleted_by=%q and deleted_at written (the mark-delete's own attribution)", after, ownerID)
	}

	// --- Leg B: an impersonated node create and delete. ---
	// tenant-acme holds a single seeded root node; a fresh leaf is created
	// beneath it and then deleted, so the delete is a clean leaf
	// mark-delete with no cascade.
	var nodes struct {
		Nodes []orgNode `json:"nodes"`
	}
	adminRequest(t, srv, http.MethodGet, "/api/v1/org/nodes", staffToken, nil, http.StatusOK, &nodes, impersonationHeaders)
	if len(nodes.Nodes) == 0 {
		t.Fatalf("tenant-acme has no org nodes; the seeded root is missing")
	}
	var root orgNode
	for _, n := range nodes.Nodes {
		if n.ParentID == "" {
			root = n
			break
		}
	}
	if root.ID == "" {
		t.Fatalf("no root node found in %+v", nodes.Nodes)
	}

	var created orgNode
	adminRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", staffToken,
		map[string]string{
			"parentId": root.ID,
			"name":     "p1-audit-leaf",
			"kind":     "store",
		}, http.StatusCreated, &created, impersonationHeaders)
	if created.ID == "" {
		t.Fatal("POST /api/v1/org/nodes while impersonating returned no node id")
	}
	adminRequest(t, srv, http.MethodDelete, "/api/v1/org/nodes/"+created.ID, staffToken, nil, http.StatusNoContent, nil, impersonationHeaders)

	nodeRows := auditRowsForTenant(t, cfg, "tenant-acme")
	// The node create records one org.node.create row naming the new node.
	var creates []auditRow
	for _, row := range nodeRows {
		if row.Action == orgActionNodeCreate && row.ResourceID == created.ID {
			creates = append(creates, row)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("org.node.create rows naming node %q = %+v, want exactly 1; all tenant rows = %+v", created.ID, creates, nodeRows)
	}
	createRow := creates[0]
	if createRow.ActorID != ownerID {
		t.Errorf("create ActorID = %q, want the impersonated owner %q", createRow.ActorID, ownerID)
	}
	if createRow.OnBehalfOfID == nil || *createRow.OnBehalfOfID != staffID {
		t.Errorf("create OnBehalfOfID = %v, want the real administrator %q", createRow.OnBehalfOfID, staffID)
	}

	// The delete records exactly two org.node.update rows in this tenant
	// beyond the create-leg's own parent-lock touch (which names the
	// root): the delete-leg's own row-lock touch (resource id = the
	// deleted node) and the mark-delete itself (a path-swept bulk UPDATE
	// whose resource id is empty, carrying the deleted_at/deleted_by
	// columns in its diff -- see WriteCapturedEvent.ResourceID's
	// best-effort contract). The mark row is the load-bearing assertion:
	// it is the row that IS the deletion.
	var nodeTouches, markRows []auditRow
	for _, row := range nodeRows {
		if row.Action != orgActionNodeUpdate {
			continue
		}
		if row.ResourceID == created.ID {
			nodeTouches = append(nodeTouches, row)
			continue
		}
		if row.ResourceID == "" {
			markRows = append(markRows, row)
		}
	}
	if len(nodeTouches) != 1 {
		t.Fatalf("org.node.update rows naming node %q = %+v, want exactly 1 (the delete-leg's lock touch); all tenant rows = %+v", created.ID, nodeTouches, nodeRows)
	}
	touch := nodeTouches[0]
	if touch.ActorID != ownerID {
		t.Errorf("lock-touch ActorID = %q, want the impersonated owner %q", touch.ActorID, ownerID)
	}
	if touch.OnBehalfOfID == nil || *touch.OnBehalfOfID != staffID {
		t.Errorf("lock-touch OnBehalfOfID = %v, want the real administrator %q", touch.OnBehalfOfID, staffID)
	}
	var deletion auditRow
	for _, row := range markRows {
		after := afterDiff(t, row.Changes)
		if after["deleted_at"] != nil {
			deletion = row
			break
		}
	}
	if deletion.Action == "" {
		t.Fatalf("no org.node.update row with a deleted_at diff found (the deletion's mark-delete row is missing); mark rows = %+v; all tenant rows = %+v", markRows, nodeRows)
	}
	if deletion.ActorID != ownerID {
		t.Errorf("deletion ActorID = %q, want the impersonated owner %q", deletion.ActorID, ownerID)
	}
	if deletion.OnBehalfOfID == nil || *deletion.OnBehalfOfID != staffID {
		t.Errorf("deletion OnBehalfOfID = %v, want the real administrator %q", deletion.OnBehalfOfID, staffID)
	}
	if after := afterDiff(t, deletion.Changes); after["deleted_by"] != ownerID {
		t.Errorf("deletion changes after = %v, want deleted_by=%q (the mark-delete's own attribution)", after, ownerID)
	}
}

// TestOrgAuditCapture_InvitationCreate_AuditRowCarriesNoAddressIndex extends the
// regression to the third opted-in model -- Invitation -- and to the
// one org write whose captured diff carries the invitee's address-derived
// values. The invitation create must leave exactly one
// "org.invitation.create" row (the forward half of the capture-scope
// contract: Invitation sits inside the host's Options.AuditModels scope,
// which is org's own org.AuditableModels() export, so a change that dropped
// the model from that export would fail here, in the composed app, not in
// a compliance query years later). And that row's changes diff must carry
// NEITHER the address's blind index NOR the address itself. The index is
// the case that matters: it is a stable linkable identifier of a guessable
// address (see Invitation.EmailIndex's own doc comment in go/org), and the
// audit trail is the most permanent and widest-audience exit it could
// reach -- append-only with no delete by design, and tenant-readable
// (compliance's audit query runs under an ordinary tenant context) where
// the org rows themselves are subtree-readable -- so a raw index in the
// diff would let a tenant member with audit read invite a guessed address,
// read its index from their own row, and compare it against every other
// invitation row of the tenant, confirming the address across subtrees
// they cannot see. Invitation.EmailIndex therefore carries dbkit's
// audit:"redact" capture opt-out, and the diff records the "[redacted]"
// marker, never the digest; the plaintext address column is
// serializer-redacted the same way and asserted here as the control.
// Before this fix the create row carried the raw 64-hex index.
func TestOrgAuditCapture_InvitationCreate_AuditRowCarriesNoAddressIndex(t *testing.T) {
	srv, cfg, mailer := buildOrgTestServer(t)
	inviterToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "p1-invite-audit-owner")

	// Invite into a freshly created node of the tenant's tree (created
	// through the real route, exactly as org_flow_test's own invite leg
	// does -- buildOrgTestServer's fresh database carries no pre-seeded
	// org tree). The subject header names the acting identity org's
	// invitation create resolves; that identity is not what this
	// regression is about, so no Actor assertion follows.
	var root orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", inviterToken, "",
		map[string]string{"name": "P1 Invite Audit Group", "kind": "group"}, &root)
	if root.ID == "" {
		t.Fatalf("created root = %+v, want a non-empty id", root)
	}

	const inviteeEmail = "p1-invite-audit@example.com"
	var invitation orgInvitation
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations", inviterToken, "p1-invite-audit-owner-actor",
		map[string]string{"email": inviteeEmail, "nodeId": root.ID}, &invitation)
	if invitation.ID == "" || invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want a pending invitation carrying an id", invitation)
	}
	mail := mailer.last(t)
	if len(mail.To) != 1 || mail.To[0] != inviteeEmail {
		t.Fatalf("mail.To = %v, want exactly [%q]", mail.To, inviteeEmail)
	}

	// The composed trail holds exactly one org.invitation.create row for
	// this invitation -- the F3 forward direction at the host: the model
	// is genuinely inside the capture scope, whatever org.AuditableModels()
	// currently declares.
	rows := auditRowsForTenant(t, cfg, "tenant-acme")
	var creates []auditRow
	for _, row := range rows {
		if row.Action == orgActionInvitationCreate && row.ResourceID == invitation.ID {
			creates = append(creates, row)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("org.invitation.create rows naming invitation %q = %+v, want exactly 1; all tenant rows = %+v",
			invitation.ID, creates, rows)
	}

	// The F1 assertion: the create row's diff carries no usable
	// address-derived value. The email_index column is audit-redacted
	// (go/org's Invitation model), so the key may appear only with the
	// "[redacted]" marker -- before this fix the raw blind index was
	// recorded under it, and the failing assertion below read the raw
	// 64-hex digest out of the row.
	after := afterDiff(t, creates[0].Changes)
	if v, ok := after["email_index"]; ok && v != "[redacted]" {
		t.Fatalf("org.invitation.create changes carry the invitee's blind index as %v: the index must never reach the audit trail", v)
	}
	if v, ok := after["email"]; ok && v != "[redacted]" {
		t.Fatalf("org.invitation.create changes carry the invitee's plaintext address as %v", v)
	}
}

// orgAction* are the capture-derived audit actions this test asserts on.
// They are spelled out (rather than importing go/org's constants) because
// this file asserts on the WIRE-visible audit rows the composed app
// produced, exactly as admin_flow_test.go asserts "notes.note.create" as a
// literal -- the same reason org_flow_test.go decodes responses by field
// name instead of importing go/org/api's generated types.
const (
	orgActionNodeCreate       = "org.node.create"
	orgActionNodeUpdate       = "org.node.update"
	orgActionMemberUpdate     = "org.member.update"
	orgActionInvitationCreate = "org.invitation.create"
)

// auditRow is the subset of go/dbkit/audit's AuditEvent this test reads.
type auditRow struct {
	Action       string
	TenantID     string
	ResourceType string
	ResourceID   string
	ActorID      string
	OnBehalfOfID *string
	Changes      []byte
}

// auditRowsForTenant opens a second connection to the same SQLite file the
// test's server writes and returns every audit row of the tenant -- the
// identical reach server_test.go's note-audit persistence test uses.
func auditRowsForTenant(t *testing.T, cfg serverConfig, tenantID string) []auditRow {
	t.Helper()
	auditDB, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		t.Fatalf("open second connection to %q: %v", cfg.SQLitePath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := auditDB.DB()
		if dbErr != nil {
			t.Errorf("second connection handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("close second connection: %v", closeErr)
		}
	})

	events, err := audit.NewRepository(auditDB).ListByTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("ListByTenant(%q): %v", tenantID, err)
	}
	rows := make([]auditRow, 0, len(events))
	for _, evt := range events {
		rows = append(rows, auditRow{
			Action:       evt.Action,
			TenantID:     evt.TenantID,
			ResourceType: evt.Resource().Type,
			ResourceID:   evt.Resource().ID,
			ActorID:      evt.Actor().ID,
			OnBehalfOfID: evt.OnBehalfOfID,
			Changes:      evt.Changes,
		})
	}
	return rows
}

// afterDiff decodes a row's Changes JSON (go/dbkit/audit's Diff shape,
// marshaled without tags, so the keys are "Before"/"After") and returns
// the After map.
func afterDiff(t *testing.T, changes []byte) map[string]any {
	t.Helper()
	var d struct {
		Before map[string]any
		After  map[string]any
	}
	if len(changes) == 0 {
		return nil
	}
	if err := json.Unmarshal(changes, &d); err != nil {
		t.Fatalf("decode changes JSON %q: %v", changes, err)
	}
	return d.After
}
