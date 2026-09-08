package org

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// membershipTenantRow is the scan destination of TenantsOf's read: one
// column of one memberships row, its tenant.
//
// It deliberately does NOT implement dbkit.TenantScoped, and the reason is
// the query itself, not an accident of modelling. dbkit's isolation plugin
// scopes a statement by its destination model: a TenantScoped destination
// gets "WHERE tenant_id = ?" injected (or fails closed when the context
// carries no tenant). TenantsOf's statement is the one read in this module
// whose whole purpose is to span tenants -- a person's memberships across
// every organization -- so it must be a non-tenant-scoped statement by
// construction, exactly as dbkit's own tenant_scope.go anticipates: routing
// an authorized cross-tenant read around the tenant filter "is left to a
// higher layer ... that can make that decision deliberately and audit it,
// rather than this plugin guessing at it". This repository -- org's own
// data-access layer, sitting above the plugin the way dbkit.Repository[T]
// does -- is that higher layer for the memberships table, and
// MemberService.TenantsOf is the deliberate, gated decision point in front
// of it. No db.Table / db.Model / db.Raw call exists anywhere in this
// package (go/org carries no allowlist entry in either semgrep rule), and
// no hand-written tenant_id filter does either: the statement is built from
// ordinary Where clauses on the column being matched, and the absence of a
// tenant predicate is the read's own defining property, gated on the system
// context TenantsOf requires.
type membershipTenantRow struct {
	TenantID string `gorm:"column:tenant_id"`
}

// TableName names the memberships table this projection reads. It is the
// only place the table is named for the cross-tenant read; the row type
// itself carries no tenant scoping marker, for the reason documented above.
func (membershipTenantRow) TableName() string { return tableMemberships }

// tenantsOfUser returns every tenant in which userID currently holds an
// active membership, ordered by tenant_id so the answer is stable across
// engines and runs.
//
// It is the repository half of MemberService.TenantsOf, and it inherits
// that method's defining property: the statement carries no tenant filter
// at all, because the question spans tenants by definition. The context
// MUST therefore carry a system context -- MemberService.TenantsOf refuses
// otherwise, before this method is reached -- and every row the statement
// may return is the answer the system-context holder asked for: active
// memberships are the one fact org publishes about a person across
// organizations, and "which organizations does this person belong to" is
// exactly the question only a system-context holder may ask.
//
// Three properties make the read safe to run as one unfiltered statement
// rather than a per-tenant loop:
//
//   - The destination is the non-TenantScoped projection above, so the
//     isolation plugin neither injects a tenant filter nor fails the
//     statement closed for lacking one -- a tenant-scoped destination
//     would do one of those two things, and neither is this read's
//     meaning. A tenant (if any) already carried by ctx is irrelevant to
//     the answer: the statement is unscoped regardless, and the answer is
//     the same whatever tenant a caller was acting in when it took its
//     system context.
//   - The WHERE clauses filter on user_id, status and the soft-delete
//     marker only: live, active membership rows of one user. The partial
//     index migration 0009_memberships_user_lookup.sql adds makes the
//     user_id half of that an index lookup rather than a scan.
//   - The service-layer gate makes the whole statement unreachable
//     without pkgcore.SystemReasonFromContext finding a grant.
//
// What this deliberately does NOT do is set the PostgreSQL RLS session
// variable (dbkit.WithTenantSession's GUC step): that mechanism exists to
// scope a statement to ONE tenant, and this statement is scoped to none by
// design. A production PostgreSQL deployment whose RLS policies admit only
// per-tenant reads must give the platform role its own admission for
// system-context reads of the memberships table -- a policy shape no
// distributed-mode deployment exists yet to prove.
func (r *MembershipRepository) tenantsOfUser(ctx context.Context, userID string) ([]pkgcore.TenantID, error) {
	if userID == "" {
		// An empty user id belongs to no tenants. Asked of the database the
		// answer is the same, but the question is answered without one: no
		// row can match, and the empty answer needs no I/O to be true.
		return nil, nil
	}

	var rows []membershipTenantRow
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Where("status = ?", MembershipStatusActive).
		Where("deleted_at IS NULL").
		Order("tenant_id").
		Find(&rows).Error
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	tenants := make([]pkgcore.TenantID, 0, len(rows))
	for _, row := range rows {
		tenants = append(tenants, pkgcore.TenantID(row.TenantID))
	}
	return tenants, nil
}

// TenantsOf returns every tenant in which userID holds an active
// membership, ordered by tenant_id. It is the membership half of the
// "which tenants does this user belong to" question, and the one
// cross-tenant read this module offers: every other MemberService method
// operates inside the tenant the context names, while a person's memberships
// span organizations by definition, so no single tenant context could ever
// answer it.
//
// # System context is mandatory
//
// The answer is a platform-operator-shaped fact about a person -- "this
// account may act in these organizations" -- and org will not serve it to
// a caller holding only an organization's own context. TenantsOf therefore
// refuses with ErrSystemContextRequired unless ctx carries a system
// context (pkgcore.WithSystemContext; the audited wrapper that business
// code should use is tenancy.WithSystemContext). Like dbkit's own
// system-context gate on HardDelete, org checks only the grant's presence,
// never who holds it: which modules may hold a system context at all is
// the caller-side whitelist's business, and org's gate exists so a
// tenant-scoped caller cannot accidentally or deliberately widen its own
// read. A tenant (if any) already carried by ctx neither satisfies nor
// defeats the gate, and does not change the answer.
//
// # What the answer contains
//
// Only active memberships count -- the same MembershipStatusActive that
// Scope.MemberNodeIDs grants data visibility for -- and a soft-deleted
// membership (MemberService.Remove) is gone from the answer exactly as it
// is from every tenant-scoped read. The list is ordered by tenant_id,
// ascending, so two replicas and two engines answer identically; the
// partial index migration 0009_memberships_user_lookup.sql exists for this
// lookup. Callers that need a preferred-first ordering (authn's
// MembershipReader contract asks the host to prefer the first tenant on a
// no-tenant sign-in) reorder the answer themselves.
//
// The natural consumer is a host's authn.MembershipReader.TenantsOf
// (go/authn/service.go declares the seam): authn may not import org, so
// the assembling application wires this method behind the interface, the
// same shape this module's own Get serves for ActiveMembership.
func (s *MemberService) TenantsOf(ctx context.Context, userID string) ([]pkgcore.TenantID, error) {
	if _, ok := pkgcore.SystemReasonFromContext(ctx); !ok {
		return nil, ErrSystemContextRequired.WithParam("user_id", userID)
	}
	return s.repo.tenantsOfUser(ctx, userID)
}
