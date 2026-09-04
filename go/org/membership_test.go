package org

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// seedMembership inserts one membership directly through the repository, for
// tests that need a specific roster rather than MemberService's validation.
func seedMembership(t *testing.T, repo *MembershipRepository, ctx context.Context, m Membership) Membership {
	t.Helper()
	if err := repo.Create(ctx, &m); err != nil {
		t.Fatalf("seed membership %q: %v", m.ID, err)
	}
	return m
}

// TestMembershipRepository_AssertIsolated runs the mandatory tenant-isolation
// suite against memberships.
//
// AssertIsolated, not AssertNotTenantScoped, and the distinction is worth
// stating because it is the one a reviewer is most likely to "correct":
// memberships are LINK data, and docs/internal/04-data-and-tenancy.md
// classifies link data as tenant-scoped and makes this suite mandatory for
// it. The neighbouring users table is identity data and is deliberately NOT
// tenant-scoped -- one person belongs to several tenants -- which is exactly
// why the bridging row must be: it is the per-tenant half of that
// relationship, and a membership readable across tenants would expose one
// tenant's roster to another.
func TestMembershipRepository_AssertIsolated(t *testing.T) {
	repo := NewMembershipRepository(newTestDB(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Membership {
		n++
		return &Membership{
			ID:     fmt.Sprintf("10000000-0000-4000-8000-%012d", n),
			UserID: fmt.Sprintf("user-%d", n),
			NodeID: fmt.Sprintf("node-%d", n),
			Status: MembershipStatusActive,
		}
	})
}

func TestMembership_IsActive(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{MembershipStatusActive, true},
		{MembershipStatusInvited, false},
		{MembershipStatusSuspended, false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			if got := (Membership{Status: tc.status}).IsActive(); got != tc.want {
				t.Errorf("Membership{Status: %q}.IsActive() = %t, want %t", tc.status, got, tc.want)
			}
		})
	}
}

// TestMembership_GetDeletedAt_ReturnsFieldValue is a no-database sanity
// check, matching examples/reference-app/internal/notes' identical
// precedent, that Membership's GetDeletedAt method returns exactly the
// DeletedAt field it reads.
func TestMembership_GetDeletedAt_ReturnsFieldValue(t *testing.T) {
	if got := (Membership{}).GetDeletedAt(); got != nil {
		t.Fatalf("GetDeletedAt() on a zero-valued Membership = %v, want nil", got)
	}
	now := time.Now()
	m := Membership{DeletedAt: &now}
	got := m.GetDeletedAt()
	if got != &now {
		t.Fatalf("GetDeletedAt() = %v, want %v", got, &now)
	}
}

func TestMembership_TableNameAndTenantScoping(t *testing.T) {
	if got := (Membership{}).TableName(); got != tableMemberships {
		t.Errorf("TableName() = %q, want %q", got, tableMemberships)
	}
	m := Membership{}
	m.TenantID = "tenant-a"
	if got := m.GetTenantID(); got != "tenant-a" {
		t.Errorf("GetTenantID() = %q, want %q", got, "tenant-a")
	}
}

func TestMembershipRepository_byUser(t *testing.T) {
	repo := NewMembershipRepository(newTestDB(t))
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	seedMembership(t, repo, ctxA, Membership{ID: "m-1", UserID: "u-1", NodeID: "n-1", Status: MembershipStatusActive})

	got, err := repo.byUser(ctxA, "u-1")
	if err != nil {
		t.Fatalf("byUser: %v", err)
	}
	if got.ID != "m-1" || got.NodeID != "n-1" {
		t.Errorf("byUser returned %+v, want membership m-1 at n-1", got)
	}

	if _, err := repo.byUser(ctxA, "u-missing"); !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("byUser(unknown user) error = %v, want org.membership_not_found", err)
	}

	// The same user id in another tenant is invisible: this is the isolation
	// property that makes a roster private.
	if _, err := repo.byUser(ctxB, "u-1"); !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("byUser(other tenant) error = %v, want org.membership_not_found", err)
	}
}

func TestMembershipRepository_byNodeIDs(t *testing.T) {
	repo := NewMembershipRepository(newTestDB(t))
	ctx := tenantCtx("tenant-a")
	other := tenantCtx("tenant-b")

	seedMembership(t, repo, ctx, Membership{ID: "m-1", UserID: "u-b", NodeID: "n-1", Status: MembershipStatusActive})
	seedMembership(t, repo, ctx, Membership{ID: "m-2", UserID: "u-a", NodeID: "n-1", Status: MembershipStatusActive})
	seedMembership(t, repo, ctx, Membership{ID: "m-3", UserID: "u-c", NodeID: "n-2", Status: MembershipStatusActive})
	seedMembership(t, repo, other, Membership{ID: "m-4", UserID: "u-d", NodeID: "n-1", Status: MembershipStatusActive})

	got, err := repo.byNodeIDs(ctx, []string{"n-1", "n-2"})
	if err != nil {
		t.Fatalf("byNodeIDs: %v", err)
	}
	// Ordered by (node_id, user_id), so the order is total and stable.
	wantIDs := []string{"m-2", "m-1", "m-3"}
	if len(got) != len(wantIDs) {
		t.Fatalf("byNodeIDs returned %d rows, want %d", len(got), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("row %d = %q, want %q", i, got[i].ID, want)
		}
	}

	empty, err := repo.byNodeIDs(ctx, nil)
	if err != nil || empty != nil {
		t.Errorf("byNodeIDs(nil) = (%v, %v), want (nil, nil) without touching the database", empty, err)
	}
}

func TestMembershipRepository_activeSample(t *testing.T) {
	repo := NewMembershipRepository(newTestDB(t))
	ctx := tenantCtx("tenant-a")

	seedMembership(t, repo, ctx, Membership{ID: "m-1", UserID: "u-1", NodeID: "n-1", Status: MembershipStatusActive})
	seedMembership(t, repo, ctx, Membership{ID: "m-2", UserID: "u-2", NodeID: "n-1", Status: MembershipStatusSuspended})

	got, err := repo.activeSample(ctx, 2)
	if err != nil {
		t.Fatalf("activeSample: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m-1" {
		t.Errorf("activeSample returned %+v, want only the active m-1", got)
	}

	seedMembership(t, repo, ctx, Membership{ID: "m-3", UserID: "u-3", NodeID: "n-1", Status: MembershipStatusActive})
	got, err = repo.activeSample(ctx, 2)
	if err != nil {
		t.Fatalf("activeSample: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("activeSample(2) returned %d rows, want 2", len(got))
	}
}

func TestMembershipRepository_anyInNodes(t *testing.T) {
	repo := NewMembershipRepository(newTestDB(t))
	ctx := tenantCtx("tenant-a")
	seedMembership(t, repo, ctx, Membership{ID: "m-1", UserID: "u-1", NodeID: "n-1", Status: MembershipStatusActive})

	for _, tc := range []struct {
		name  string
		nodes []string
		want  bool
	}{
		{"occupied node", []string{"n-1"}, true},
		{"empty node", []string{"n-2"}, false},
		{"one occupied among several", []string{"n-2", "n-1"}, true},
		{"no nodes at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.anyInNodes(ctx, tc.nodes)
			if err != nil {
				t.Fatalf("anyInNodes: %v", err)
			}
			if got != tc.want {
				t.Errorf("anyInNodes(%v) = %t, want %t", tc.nodes, got, tc.want)
			}
		})
	}

	// Another tenant's membership does not make a node look occupied.
	got, err := repo.anyInNodes(tenantCtx("tenant-b"), []string{"n-1"})
	if err != nil {
		t.Fatalf("anyInNodes(other tenant): %v", err)
	}
	if got {
		t.Error("anyInNodes saw another tenant's membership")
	}
}

func TestMemberService_Add(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, _ := seedTree(t, m.Tree(), ctx)

	membership, err := m.Members().Add(ctx, "u-1", left.ID)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if membership.NodeID != left.ID || !membership.IsActive() {
		t.Errorf("Add produced %+v, want an active membership at %q", membership, left.ID)
	}
	if err := validateNodeID(membership.ID); err != nil {
		t.Errorf("membership id %q is outside the id alphabet: %v", membership.ID, err)
	}

	// One seat per person per tenant, whichever node the second attempt names.
	if _, err := m.Members().Add(ctx, "u-1", root.ID); !hasCode(err, ErrMembershipExists.Code) {
		t.Errorf("second Add error = %v, want org.membership_exists", err)
	}
}

func TestMemberService_Add_UnknownNode(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	seedTree(t, m.Tree(), ctx)

	if _, err := m.Members().Add(ctx, "u-1", "00000000-0000-4000-8000-000000000000"); !hasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("Add(unknown node) error = %v, want org.node_not_found", err)
	}
}

// TestMemberService_Add_CrossTenantNode_ReportsNodeNotFound pins that a node
// id from another tenant is indistinguishable from one that does not exist.
func TestMemberService_Add_CrossTenantNode_ReportsNodeNotFound(t *testing.T) {
	m, _ := newTestModule(t)
	foreign, err := m.Tree().CreateRoot(tenantCtx("tenant-b"), "their group", "group")
	if err != nil {
		t.Fatalf("CreateRoot(tenant-b): %v", err)
	}

	ctxA := tenantCtx("tenant-a")
	seedTree(t, m.Tree(), ctxA)
	if _, err := m.Members().Add(ctxA, "u-1", foreign.ID); !hasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("Add(another tenant's node) error = %v, want org.node_not_found", err)
	}
}

func TestMemberService_Add_EmptyUserID(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.Tree(), ctx)

	if _, err := m.Members().Add(ctx, "", root.ID); !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("Add(empty user) error = %v, want org.membership_not_found", err)
	}
}

// TestMemberService_ensure_IsIdempotentAndNeverRebinds pins the property the
// whole event-subscriber contract rests on: repeating the call changes
// nothing, and in particular does NOT move an existing member to the node the
// repeated call named.
func TestMemberService_ensure_IsIdempotentAndNeverRebinds(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, _ := seedTree(t, m.Tree(), ctx)

	first, created, err := m.Members().ensure(ctx, "u-1", left.ID)
	if err != nil || !created {
		t.Fatalf("first ensure = (%v, %t, %v), want a created membership", first, created, err)
	}
	second, created, err := m.Members().ensure(ctx, "u-1", root.ID)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if created {
		t.Error("second ensure reported a creation; it must be idempotent")
	}
	if second.ID != first.ID || second.NodeID != left.ID {
		t.Errorf("second ensure returned %+v, want the original membership still at %q", second, left.ID)
	}
}

func TestMemberService_List_SubtreeRoster(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, right := seedTree(t, m.Tree(), ctx)
	chair, err := m.Tree().CreateChild(ctx, left.ID, "chair 1", "room")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}

	for user, node := range map[string]string{"u-root": root.ID, "u-left": left.ID, "u-chair": chair.ID, "u-right": right.ID} {
		if _, err := m.Members().Add(ctx, user, node); err != nil {
			t.Fatalf("Add(%s): %v", user, err)
		}
	}

	tests := []struct {
		name string
		node string
		want []string
	}{
		{"from the root, everybody", root.ID, []string{"u-root", "u-left", "u-chair", "u-right"}},
		{"from a mid-tree node, its own subtree", left.ID, []string{"u-left", "u-chair"}},
		{"from a leaf, only itself", chair.ID, []string{"u-chair"}},
		{"from the sibling branch, no crossover", right.ID, []string{"u-right"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.Members().List(ctx, tc.node)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			users := map[string]bool{}
			for _, membership := range got {
				users[membership.UserID] = true
			}
			if len(users) != len(tc.want) {
				t.Fatalf("List returned %d members (%v), want %v", len(users), users, tc.want)
			}
			for _, want := range tc.want {
				if !users[want] {
					t.Errorf("List is missing %q; got %v", want, users)
				}
			}
		})
	}
}

func TestMemberService_List_UnknownNode(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	seedTree(t, m.Tree(), ctx)

	if _, err := m.Members().List(ctx, "00000000-0000-4000-8000-000000000000"); !hasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("List(unknown node) error = %v, want org.node_not_found", err)
	}
}

func TestMemberService_Remove_PublishesTheEvent(t *testing.T) {
	m, host := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, _ := seedTree(t, m.Tree(), ctx)

	if _, err := m.Members().Add(ctx, "u-owner", root.ID); err != nil {
		t.Fatalf("Add(owner): %v", err)
	}
	leaving, err := m.Members().Add(ctx, "u-leaving", left.ID)
	if err != nil {
		t.Fatalf("Add(leaving): %v", err)
	}

	if err := m.Members().Remove(ctx, "u-leaving"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := m.Members().Get(ctx, "u-leaving"); !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("Get after Remove error = %v, want org.membership_not_found", err)
	}

	removed := host.bus.events(EventMemberRemoved)
	if len(removed) != 1 {
		t.Fatalf("published %d member-removed events, want 1", len(removed))
	}
	payload, ok := removed[0].Payload.(MemberRemoved)
	if !ok {
		t.Fatalf("payload is %T, want org.MemberRemoved", removed[0].Payload)
	}
	if payload.UserID != "u-leaving" || payload.MembershipID != leaving.ID || payload.NodeID != left.ID {
		t.Errorf("payload = %+v, want the removed membership %q", payload, leaving.ID)
	}
	if removed[0].TenantID != "tenant-a" {
		t.Errorf("event tenant = %q, want tenant-a", removed[0].TenantID)
	}
}

// TestMemberService_Remove_LastActiveMember_IsRefused pins the rule that
// keeps a tenant reachable: inviting somebody requires an authenticated
// member, so a tenant emptied this way could never be re-entered.
func TestMemberService_Remove_LastActiveMember_IsRefused(t *testing.T) {
	m, host := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.Tree(), ctx)

	if _, err := m.Members().Add(ctx, "u-only", root.ID); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Members().Remove(ctx, "u-only"); !hasCode(err, ErrMemberNotRemovable.Code) {
		t.Errorf("Remove(last member) error = %v, want org.member_not_removable", err)
	}
	if _, err := m.Members().Get(ctx, "u-only"); err != nil {
		t.Errorf("the refused removal deleted the membership anyway: %v", err)
	}
	if events := host.bus.events(EventMemberRemoved); len(events) != 0 {
		t.Errorf("a refused removal published %d events, want 0", len(events))
	}
}

// TestMemberService_Remove_LastSuspendedMember_IsAllowed pins the other half
// of that rule: the guard counts ACTIVE members, because a suspended one
// cannot invite anybody either -- so a tenant whose only member is suspended
// is already in the state the guard exists to prevent, and refusing the
// removal would only strand the row.
func TestMemberService_Remove_LastSuspendedMember_IsAllowed(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.Tree(), ctx)

	seedMembership(t, m.Members().Repository(), ctx, Membership{
		ID: "20000000-0000-4000-8000-000000000001", UserID: "u-suspended",
		NodeID: root.ID, Status: MembershipStatusSuspended,
	})
	if err := m.Members().Remove(ctx, "u-suspended"); err != nil {
		t.Errorf("Remove(last suspended member) = %v, want success", err)
	}
}

func TestMemberService_Remove_UnknownUser(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	seedTree(t, m.Tree(), ctx)

	if err := m.Members().Remove(ctx, "u-nobody"); !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("Remove(unknown user) error = %v, want org.membership_not_found", err)
	}
}

// TestMemberService_NoTenantContext_EveryOperationFailsClosed mirrors the
// tree's own fail-closed suite: without a tenant in context nothing reads and
// nothing writes.
func TestMemberService_NoTenantContext_EveryOperationFailsClosed(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := context.Background()

	operations := map[string]func() error{
		"Get":     func() error { _, err := m.Members().Get(ctx, "u-1"); return err },
		"Add":     func() error { _, err := m.Members().Add(ctx, "u-1", "n-1"); return err },
		"List":    func() error { _, err := m.Members().List(ctx, "n-1"); return err },
		"Remove":  func() error { return m.Members().Remove(ctx, "u-1") },
		"Restore": func() error { _, err := m.Members().Restore(ctx, "m-1"); return err },
	}
	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			if err := op(); err == nil {
				t.Errorf("%s without a tenant in context succeeded; it must fail closed", name)
			}
		})
	}
}

// TestMemberService_Remove_ThenAdd_SameUser_Succeeds is the round's own proof
// that uq_memberships_tenant_user's replacement by its WHERE deleted_at IS
// NULL partial-index equivalent
// (migrations/{sqlite,postgres}/0004_add_soft_delete.sql) actually frees a
// removed member's seat for reuse. Against the pre-round full unique index
// this Add would fail with ErrMembershipExists -- a real functional
// regression the migration exists to avoid.
func TestMemberService_Remove_ThenAdd_SameUser_Succeeds(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, _ := seedTree(t, m.Tree(), ctx)

	if _, err := m.Members().Add(ctx, "u-owner", root.ID); err != nil {
		t.Fatalf("Add(owner): %v", err)
	}
	original, err := m.Members().Add(ctx, "u-returning", left.ID)
	if err != nil {
		t.Fatalf("Add(returning): %v", err)
	}
	if err = m.Members().Remove(ctx, "u-returning"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	readded, err := m.Members().Add(ctx, "u-returning", root.ID)
	if err != nil {
		t.Fatalf("Add after Remove of the same user: %v, want success", err)
	}
	if readded.ID == original.ID {
		t.Fatal("Add returned the soft-deleted row instead of creating a new one")
	}
	if readded.NodeID != root.ID {
		t.Errorf("re-added membership NodeID = %q, want %q (Add never re-binds an existing row -- this is a fresh one)",
			readded.NodeID, root.ID)
	}

	// The seat is still exclusive among LIVE rows: a second Add for the same
	// user is still refused, exactly as before this round.
	if _, err := m.Members().Add(ctx, "u-returning", left.ID); !hasCode(err, ErrMembershipExists.Code) {
		t.Errorf("second live Add error = %v, want org.membership_exists", err)
	}
}

// TestMemberService_Restore_UnknownID_ReturnsMembershipNotFound covers the id
// half of Restore's collapsed not-found signal.
func TestMemberService_Restore_UnknownID_ReturnsMembershipNotFound(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	seedTree(t, m.Tree(), ctx)

	_, err := m.Members().Restore(ctx, "nope")
	if !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("Restore(unknown id) error = %v, want org.membership_not_found", err)
	}
}

// TestMemberService_Restore_LiveMembership_ReturnsMembershipNotFound covers
// the other half: a membership that was never removed has nothing for
// Restore to undo.
func TestMemberService_Restore_LiveMembership_ReturnsMembershipNotFound(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.Tree(), ctx)
	live, err := m.Members().Add(ctx, "u-owner", root.ID)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	_, err = m.Members().Restore(ctx, live.ID)
	if !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("Restore(live membership) error = %v, want org.membership_not_found", err)
	}
}

// TestMemberService_Restore_RoundTrip proves the full lifecycle: Add,
// Remove, Restore brings the exact same row back with its original data
// intact and visible to ordinary reads again.
func TestMemberService_Restore_RoundTrip(t *testing.T) {
	m, host := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, _ := seedTree(t, m.Tree(), ctx)

	if _, err := m.Members().Add(ctx, "u-owner", root.ID); err != nil {
		t.Fatalf("Add(owner): %v", err)
	}
	original, err := m.Members().Add(ctx, "u-leaving", left.ID)
	if err != nil {
		t.Fatalf("Add(leaving): %v", err)
	}
	if err = m.Members().Remove(ctx, "u-leaving"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err = m.Members().Get(ctx, "u-leaving"); !hasCode(err, ErrMembershipNotFound.Code) {
		t.Fatalf("Get after Remove error = %v, want org.membership_not_found", err)
	}

	restored, err := m.Members().Restore(ctx, original.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.ID != original.ID || restored.UserID != original.UserID || restored.NodeID != original.NodeID {
		t.Errorf("Restore returned %+v, want the original row %+v", restored, original)
	}

	back, err := m.Members().Get(ctx, "u-leaving")
	if err != nil {
		t.Fatalf("Get after Restore: %v, want the membership visible again", err)
	}
	if back.ID != original.ID || back.NodeID != left.ID {
		t.Errorf("Get after Restore = %+v, want the original membership at %q", back, left.ID)
	}

	restoredEvents := host.bus.events(EventMemberRestored)
	if len(restoredEvents) != 1 {
		t.Fatalf("published %d member-restored events, want 1", len(restoredEvents))
	}
	payload, ok := restoredEvents[0].Payload.(MemberRestored)
	if !ok {
		t.Fatalf("payload is %T, want org.MemberRestored", restoredEvents[0].Payload)
	}
	if payload.UserID != "u-leaving" || payload.MembershipID != original.ID || payload.NodeID != left.ID {
		t.Errorf("payload = %+v, want the restored membership %q", payload, original.ID)
	}
	if restoredEvents[0].TenantID != "tenant-a" {
		t.Errorf("event tenant = %q, want tenant-a", restoredEvents[0].TenantID)
	}
}

// TestMemberService_Restore_Twice_SecondCallReturnsMembershipNotFound pins
// that Restore's WHERE deleted_at IS NOT NULL check is what it is: a second
// Restore of an already-restored row has nothing to undo.
func TestMemberService_Restore_Twice_SecondCallReturnsMembershipNotFound(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.Tree(), ctx)
	if _, err := m.Members().Add(ctx, "u-owner", root.ID); err != nil {
		t.Fatalf("Add(owner): %v", err)
	}
	original, err := m.Members().Add(ctx, "u-leaving", root.ID)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Members().Remove(ctx, "u-leaving"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := m.Members().Restore(ctx, original.ID); err != nil {
		t.Fatalf("first Restore: %v", err)
	}
	if _, err := m.Members().Restore(ctx, original.ID); !hasCode(err, ErrMembershipNotFound.Code) {
		t.Errorf("second Restore error = %v, want org.membership_not_found", err)
	}
}
