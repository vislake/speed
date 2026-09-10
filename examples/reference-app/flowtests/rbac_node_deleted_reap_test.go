package flowtests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/rbac"
)

// This file is the real cross-module regression: a role binding scoped to
// an org node, that node deleted through
// org's own real TreeService, delivered to rbac's own real Service through
// the actual event bus -- no mock, no fake payload, no direct call into
// either module's internals.
//
// It lives here, in the reference app, rather than inside go/rbac's own
// test package, because it is the one place in this repository that can
// import both go/org and go/rbac concrete packages without adding a new
// dependency edge between them: this app already requires both (see
// go.mod), and the mandatory-first-consumer discipline makes it the
// intended home for exactly this kind of real, composed proof. rbac itself
// must keep importing neither org nor anything shaped like it -- see
// go/rbac/reap.go's own header comment -- so a test that needs BOTH real
// services on one real bus cannot live inside either module's own test
// package. This file adds no production wiring to internal/app/server.go; it builds its
// own minimal two-module kernel, independent of BuildServer's much larger
// composition.
//
// The property: a binding scoped to a deleted node must not stay live.
// org.node.deleted is delivered to rbac's own onNodeDeleted subscriber on
// the real bus; a rbac with no such subscriber would leave the binding
// granted after the delete forever.

// newOrgRBACReapHarness boots a minimal, self-contained kernel of exactly
// two modules -- org and rbac -- over a fresh SQLite file, migrated and
// bootstrapped the same way BuildServer composes the full app, just without
// every other module the full app also wires. It returns the real
// TreeService, the real MemberService and the real rbac Service, all backed
// by the same pkgcore.Registry and its real event bus, so a delete or a
// removal published by one is delivered to the other exactly as it is in
// production.
func newOrgRBACReapHarness(t *testing.T) (*org.TreeService, *org.MemberService, *rbac.Service) {
	t.Helper()
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     filepath.Join(t.TempDir(), "org-rbac-reap-test.db"),
	})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			t.Errorf("test database handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("closing the test database: %v", closeErr)
		}
	})

	orgIndexer, err := dbkit.NewBlindIndexer(org.EmailIndexColumn, testOrgRBACIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("building org's email indexer: %v", err)
	}
	// Invitation email delivery is switched off: this harness never creates
	// an invitation, so it needs neither WithMailFrom nor
	// WithInvitationLinkBuilder, both otherwise mandatory per
	// org.Module.Register's own doc comment.
	orgModule := org.NewModule(db, org.WithEmailIndexer(orgIndexer), org.WithInvitationEmailDisabled())
	// WithSubtreeResolver is wired onto org's real Scope -- the
	// OrgSubtreeResolver adapter BuildServer itself uses (internal/app/server.go) -- so
	// the seam answers node liveness against the very tree this harness
	// mutates. That matters only to the restore side of the reap pair: the
	// member-restored re-instatement re-verifies every node-scoped row's
	// node through this seam before un-marking it (rbac's
	// bindingNodeLivesAtMemberRestore, the b52b64d gate), and a harness
	// without the resolver fails that gate closed, leaving node-scoped
	// bindings revoked after a membership restore no matter what the tree
	// actually holds. rbac's own pre-b52b64d uses of this seam -- DataScope
	// narrowing -- are still not exercised by this file's assertions, which
	// read Can and the binding rows directly; the seam is wired for the
	// restore-side consumer, not for them.
	rbacModule := rbac.NewModule(db, rbac.WithSubtreeResolver(app.OrgSubtreeResolverFor(orgModule.Scope())))

	migrationRegistry := dbkit.NewMigrationRegistry()
	if regErr := migrationRegistry.Register(orgModule); regErr != nil {
		t.Fatalf("registering org's migrations: %v", regErr)
	}
	if regErr := migrationRegistry.Register(rbacModule); regErr != nil {
		t.Fatalf("registering rbac's migrations: %v", regErr)
	}
	if applyErr := migrationRegistry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		t.Fatalf("applying migrations: %v", applyErr)
	}

	reg, err := pkgcore.NewKernel().Bootstrap(ctx, orgModule, rbacModule)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	rbacService, err := rbacModule.Attach(reg)
	if err != nil {
		t.Fatalf("rbacModule.Attach: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := rbacService.Close(); closeErr != nil {
			t.Errorf("closing the rbac service: %v", closeErr)
		}
	})

	return orgModule.Tree(), orgModule.Members(), rbacService
}

// testOrgRBACIndexKey is a fixed 32-byte HMAC key for this file's blind
// indexer. It is a test fixture, not a secret -- see org's own tests for
// the identical pattern (testIndexerKey in go/org/events_test.go).
var testOrgRBACIndexKey = []byte("org-rbac-reap-test-blind-index32")

// TestOrgRBACReap_NodeDeleted_ReapsDanglingBinding is the single-node-delete
// leg of the cross-module regression: a role binding scoped to a node with
// no org member ever bound to it (so org's own TreeService.Delete member
// refusal never blocks the delete),
// deleted through org's real, non-cascading Delete, must leave rbac showing
// that binding reaped once the real org.node.deleted event has travelled
// the real bus.
func TestOrgRBACReap_NodeDeleted_ReapsDanglingBinding(t *testing.T) {
	tree, _, rbacService := newOrgRBACReapHarness(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	root, err := tree.CreateRoot(ctx, "root", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	leaf, err := tree.CreateChild(ctx, root.ID, "team-with-no-members", "team")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	// A second node, kept alive, whose own binding must survive untouched --
	// proving the reap is scoped to the deleted node, not the whole tenant.
	sibling, err := tree.CreateChild(ctx, root.ID, "sibling", "team")
	if err != nil {
		t.Fatalf("CreateChild(sibling): %v", err)
	}

	if _, err = rbacService.DefineRole(ctx, rbac.RoleDefinition{
		Key: "reader", Permissions: []string{"org:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if err = rbacService.AssignRole(ctx, sub, "reader", rbac.Scope{NodeID: leaf.ID}); err != nil {
		t.Fatalf("AssignRole at the leaf: %v", err)
	}
	if err = rbacService.AssignRole(ctx, sub, "reader", rbac.Scope{NodeID: sibling.ID}); err != nil {
		t.Fatalf("AssignRole at the sibling: %v", err)
	}

	ok, err := rbacService.Can(ctx, sub, "read", "org")
	if err != nil || !ok {
		t.Fatalf("the grant was not live before the delete: Can = %v, %v", ok, err)
	}

	// The real delete, through org's real TreeService -- not a fake event,
	// not a direct call into rbac's reap machinery.
	if err = tree.Delete(ctx, leaf.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The in-memory bus runs every subscriber synchronously inside Publish,
	// so by the time Delete above returned, rbac's onNodeDeleted subscriber
	// -- if wired -- has already run to completion. Checking the leaf and
	// the sibling separately, by node (rather than through Can, whose
	// tenant-wide aggregation cannot distinguish "reaped at the leaf" from
	// "still granted elsewhere"), is what proves the reap is scoped
	// correctly rather than merely having happened somewhere.
	leafLive, err := isReaderLiveAtNode(ctx, rbacService, sub, leaf.ID)
	if err != nil {
		t.Fatalf("checking the leaf binding: %v", err)
	}
	if leafLive {
		t.Fatal("the leaf's binding is still live after its node was deleted -- the reap did not run")
	}
	siblingLive, err := isReaderLiveAtNode(ctx, rbacService, sub, sibling.ID)
	if err != nil {
		t.Fatalf("checking the sibling binding: %v", err)
	}
	if !siblingLive {
		t.Fatal("the sibling's binding was wrongly reaped too -- the reap over-reached beyond the deleted node")
	}
}

// TestOrgRBACReap_NodeDeleted_CascadeReapsEveryBinding is the cascade leg:
// a subtree with no members anywhere in it, cascade-deleted through org's
// real Delete, must leave rbac showing every binding across that subtree
// reaped in the one pass the reap runs.
func TestOrgRBACReap_NodeDeleted_CascadeReapsEveryBinding(t *testing.T) {
	tree, _, rbacService := newOrgRBACReapHarness(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	root, err := tree.CreateRoot(ctx, "root", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	parent, err := tree.CreateChild(ctx, root.ID, "parent", "team")
	if err != nil {
		t.Fatalf("CreateChild(parent): %v", err)
	}
	childA, err := tree.CreateChild(ctx, parent.ID, "child-a", "team")
	if err != nil {
		t.Fatalf("CreateChild(child-a): %v", err)
	}
	childB, err := tree.CreateChild(ctx, parent.ID, "child-b", "team")
	if err != nil {
		t.Fatalf("CreateChild(child-b): %v", err)
	}
	sibling, err := tree.CreateChild(ctx, root.ID, "sibling", "team")
	if err != nil {
		t.Fatalf("CreateChild(sibling): %v", err)
	}

	if _, err = rbacService.DefineRole(ctx, rbac.RoleDefinition{
		Key: "reader", Permissions: []string{"org:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	for _, nodeID := range []string{parent.ID, childA.ID, childB.ID, sibling.ID} {
		if err = rbacService.AssignRole(ctx, sub, "reader", rbac.Scope{NodeID: nodeID}); err != nil {
			t.Fatalf("AssignRole at %s: %v", nodeID, err)
		}
	}

	if err = tree.Delete(ctx, parent.ID, true); err != nil {
		t.Fatalf("Delete(cascade): %v", err)
	}

	for _, deletedID := range []string{parent.ID, childA.ID, childB.ID} {
		var live bool
		live, err = isReaderLiveAtNode(ctx, rbacService, sub, deletedID)
		if err != nil {
			t.Fatalf("checking node %s: %v", deletedID, err)
		}
		if live {
			t.Fatalf("the binding at %s is still live after the cascade delete -- the reap did not cover every removed node in one pass", deletedID)
		}
	}
	siblingLive, err := isReaderLiveAtNode(ctx, rbacService, sub, sibling.ID)
	if err != nil {
		t.Fatalf("checking the sibling binding: %v", err)
	}
	if !siblingLive {
		t.Fatal("the sibling's binding was wrongly reaped too -- the cascade reap over-reached beyond the deleted subtree")
	}
}

// isReaderLiveAtNode answers whether sub still holds a live "reader"
// binding scoped exactly at nodeID -- read through rbac's own public
// Authorizer surface rather than any package-internal field, since this
// file sits outside package rbac and Can's own tenant-wide aggregation
// (service.go's own doc comment) cannot distinguish "reaped at this one
// node" from "still granted at some other scope entirely", which is
// exactly the distinction every assertion in this file needs.
//
// The probe is RevokeRole itself, deliberately: RevokeRole implements
// Authorizer and is STRICT (rbac/assign.go's own doc comment) --
// ErrBindingNotFound when nothing lives at that exact (user, role, node)
// tuple, success when something does. A binding this reap already
// soft-deleted answers not-found (Find only ever sees a live row), which
// is exactly the "not live" signal this helper wants with no restore
// needed. A binding still live is genuinely revoked by the call, so it is
// immediately put back with RestoreRole -- idempotent, per RestoreRole's
// own doc comment -- restoring the exact state the caller had before the
// probe, so checking one node never disturbs the assertion this file is
// about to make about a different one.
func isReaderLiveAtNode(ctx context.Context, svc *rbac.Service, sub rbac.Subject, nodeID string) (bool, error) {
	switch err := svc.RevokeRole(ctx, sub, "reader", rbac.Scope{NodeID: nodeID}); {
	case err == nil:
		if restoreErr := svc.RestoreRole(ctx, sub, "reader", rbac.Scope{NodeID: nodeID}); restoreErr != nil {
			return false, restoreErr
		}
		return true, nil
	case apperr.HasCode(err, rbac.ErrBindingNotFound.Code):
		return false, nil
	default:
		return false, err
	}
}
