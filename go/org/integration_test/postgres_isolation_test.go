//go:build integration

// Package org_test holds go/org's integration tier: the module's tenant-
// isolation proof (this file) and its materialized-path dialect-identity
// proof (postgres_tree_test.go) re-run against a real PostgreSQL server --
// mirroring go/dbkit, go/tenancy,
// go/jobs, go/pkgcore and go/config's own integration_test/ directories. It
// is physically separate from go/org's unit tests (which live in package
// org, one file per source file) and carries the "integration" build tag: a
// plain "go test ./..." never compiles or runs anything in this directory;
// it is invoked explicitly with "go test -tags=integration ./...".
//
// Every test here starts its own disposable PostgreSQL container via
// testcontainers and requires a working Docker (or Docker-API-compatible)
// daemon; there is no fallback or skip-on-missing-Docker path, matching the
// other modules' tiers -- go/org/internal/testutil.NewPostgres is what
// starts the container and applies org's real migrations from zero.
package org_test

import (
	"fmt"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/testkit"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/org/internal/testutil"
	"github.com/vislake/speed/go/org/migrations"
)

// pgEmailCipherKey and pgIndexKey are DIFFERENT 32-byte secrets,
// deliberately: dbkit takes the encryption key (for Invitation.Email) and
// the blind-index key through two separate constructors precisely because
// reusing one secret for both weakens both -- the same rule go/org's own
// unit tier holds itself to (invitation_test.go's testEncryptionKey /
// testIndexerKey pair), restated here because this package cannot import
// org's unexported test helpers.
var (
	pgEmailCipherKey = []byte("org-pg-integration-email-cipher-")
	pgIndexKey       = []byte("org-pg-integration-blind-index-x")
)

// newPostgres returns a migrated PostgreSQL *gorm.DB with org's encrypted
// email serializer registered under org.EmailSerializerName, skipping the
// test when no Docker daemon is reachable (testutil.NewPostgres's own
// contract).
func newPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	cipher, err := dbkit.NewCipher(pgEmailCipherKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	dbkit.RegisterEncryptedSerializer(org.EmailSerializerName, cipher)
	return testutil.NewPostgres(t, "org", migrations.FS)
}

// newIndexer returns a blind indexer over pgIndexKey, the same construction
// InvitationRepository's own callers use.
func newIndexer(t *testing.T) *dbkit.BlindIndexer {
	t.Helper()
	indexer, err := dbkit.NewBlindIndexer("email_index", pgIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("NewBlindIndexer: %v", err)
	}
	return indexer
}

// TestOrgNodeRepository_AssertIsolated_Postgres re-runs go/org's own
// TestRepository_AssertIsolated against a real PostgreSQL server. org_nodes
// is tenant data; AssertIsolated is the mandatory suite for it.
//
// The fixture seeds each tenant ONE root row (the first record requested for
// that tenant) and every further record beneath it, mirroring the unit-tier
// fixture's shape: uq_org_nodes_single_root (migrations/0007_single_root.sql)
// admits exactly one root row per tenant, and the suite's records must be
// rows the schema admits.
func TestOrgNodeRepository_AssertIsolated_Postgres(t *testing.T) {
	repo := org.NewRepository(newPostgres(t))

	n := 0
	roots := map[pkgcore.TenantID]string{}
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *org.OrgNode {
		n++
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
		if rootID, hasRoot := roots[tenant]; hasRoot {
			return &org.OrgNode{
				ID:       id,
				ParentID: rootID,
				Path:     "/" + rootID + "/" + id + "/",
				Depth:    1,
				Name:     fmt.Sprintf("node-%d", n),
				Kind:     "group",
			}
		}
		roots[tenant] = id
		return &org.OrgNode{
			ID:       id,
			ParentID: "",
			Path:     "/" + id + "/",
			Depth:    0,
			Name:     fmt.Sprintf("node-%d", n),
			Kind:     "group",
		}
	})
}

// TestMembershipRepository_AssertIsolated_Postgres re-runs go/org's own
// TestMembershipRepository_AssertIsolated against a real PostgreSQL server.
// memberships is LINK data, and link data is tenant-scoped: AssertIsolated
// is the correct half of the pair, never
// AssertNotTenantScoped -- see membership.go's own extensive doc comment on
// why this is not a mistake.
func TestMembershipRepository_AssertIsolated_Postgres(t *testing.T) {
	db := newPostgres(t)
	// A membership references a node of the same tenant; AssertIsolated's
	// fixture factory needs one to exist first.
	nodes := org.NewRepository(db)
	repo := org.NewMembershipRepository(db)

	n := 0
	roots := map[pkgcore.TenantID]string{}
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *org.Membership {
		n++
		nodeID := fmt.Sprintf("10000000-0000-4000-8000-%012d", n)
		var node *org.OrgNode
		if rootID, hasRoot := roots[tenant]; hasRoot {
			node = &org.OrgNode{
				ID: nodeID, ParentID: rootID, Path: "/" + rootID + "/" + nodeID + "/", Depth: 1,
				Name: fmt.Sprintf("node-%d", n), Kind: "group",
			}
		} else {
			roots[tenant] = nodeID
			node = &org.OrgNode{
				ID: nodeID, ParentID: "", Path: "/" + nodeID + "/", Depth: 0,
				Name: fmt.Sprintf("node-%d", n), Kind: "group",
			}
		}
		if err := nodes.Create(testkit.TenantCtx(tenant), node); err != nil {
			t.Fatalf("seed node for membership fixture %d: %v", n, err)
		}
		return &org.Membership{
			ID:     fmt.Sprintf("20000000-0000-4000-8000-%012d", n),
			UserID: fmt.Sprintf("user-%d", n),
			NodeID: nodeID,
			Status: org.MembershipStatusActive,
		}
	})
}

// TestInvitationRepository_AssertIsolated_Postgres re-runs go/org's own
// TestInvitationRepository_AssertIsolated against a real PostgreSQL server:
// org_invitations is tenant data, its Email column is encrypted at rest
// (org.EmailSerializerName) and made queryable by the HMAC blind index this
// test computes exactly as InviteService does.
func TestInvitationRepository_AssertIsolated_Postgres(t *testing.T) {
	repo := org.NewInvitationRepository(newPostgres(t))
	indexer := newIndexer(t)

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *org.Invitation {
		n++
		email := fmt.Sprintf("invitee-%d@example.test", n)
		index, err := indexer.Index(email)
		if err != nil {
			t.Fatalf("Index: %v", err)
		}
		return &org.Invitation{
			ID:            fmt.Sprintf("30000000-0000-4000-8000-%012d", n),
			NodeID:        fmt.Sprintf("40000000-0000-4000-8000-%012d", n),
			Email:         email,
			EmailIndex:    index,
			InviterUserID: fmt.Sprintf("inviter-%d", n),
			Locale:        "zh-CN",
			TokenHash:     fmt.Sprintf("%064d", n),
			Status:        org.InvitationStatusPending,
		}
	})
}
