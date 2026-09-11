package admin

import (
	"context"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy"
)

// SearchService is the cross-tenant user search, composed with the
// audited system-context mechanism (tenancy.WithSystemContext) to answer
// both halves of the surface: the platform-wide identity lookup ("which
// account does this identifier belong to", Users) and the membership
// composition ("which tenants does this person belong to",
// MembershipsOf).
//
// Both halves take the operator as an explicit actorUserID-style argument
// so every call leaves an audit trail naming WHO performed the
// cross-tenant read -- Users under one platform-wide grant (authn's search
// has no tenant in scope at all), MembershipsOf under one grant per
// candidate tenant. It never adds a bypass method to org: MembershipsOf
// calls *org.MemberService's existing, ordinary per-tenant Get method once
// per candidate tenant. The candidate tenant list comes from admin's own
// tenant ledger -- the one list of "every tenant the platform knows
// about" that exists anywhere in this codebase.
type SearchService struct {
	authnSvc *authn.Service
	members  *org.MemberService
	tenants  *TenantService

	// emitter carries the bus the Users half's one platform-wide
	// tenancy.WithSystemContext grant audits onto. MembershipsOf takes its
	// per-tenant grants through the tenants service's own emitter
	// (TenantService.forEachLedgerTenant). Unattached until
	// Module.Register calls attach.
	emitter auditEmitter
}

// NewSearchService returns a SearchService reading users through authnSvc
// and memberships through members, with candidate tenants drawn from
// tenants (admin's own ledger).
func NewSearchService(authnSvc *authn.Service, members *org.MemberService, tenants *TenantService) *SearchService {
	return &SearchService{authnSvc: authnSvc, members: members, tenants: tenants}
}

// attach gives the service the bus it needs for tenancy.WithSystemContext
// -- this service's only use of the audit seam.
func (s *SearchService) attach(bus pkgcore.EventBus) { s.emitter.attachBus(bus) }

// Users is the search surface's identity-lookup half: a passthrough to
// authn.Service.SearchUsers, authn's platform-operator search entry point,
// which has no tenant in scope at all (users is identity data, not tenant
// data).
//
// actorUserID identifies the platform operator making this cross-tenant
// search, for pkgcore.SystemReason.Actor -- never the searched-for user.
// The single SearchUsers call runs under one audited
// tenancy.WithSystemContext grant (the identical shape MembershipsOf uses
// per candidate tenant): authn's search needs no tenant-isolation escape
// hatch -- it is platform-wide by design -- but the wrapper's audit record
// is the trail of "which operator searched the user directory, under which
// declared purpose", and a search whose answers carry PII such as email
// and phone (fields authn encrypts at rest) is exactly the
// platform-operator cross-tenant read the audited mechanism exists to
// cover. The authorization half -- the admin:search_users permission check
// -- is the HTTP handler's own job, at admin's handler layer; authn.Service
// itself knows nothing about rbac.
func (s *SearchService) Users(ctx context.Context, actorUserID string, q authn.UserSearchQuery) ([]authn.User, error) {
	sysCtx, err := tenancy.WithSystemContext(ctx, s.emitter.bus, pkgcore.SystemReason{
		Actor:   actorUserID,
		Purpose: SystemPurposeAdminCrossTenant,
	})
	if err != nil {
		return nil, err
	}
	return s.authnSvc.SearchUsers(sysCtx, q)
}

// MembershipsOf is the search surface's membership half: every tenant
// (from admin's own tenant ledger) that userID currently has an active
// membership in.
//
// actorUserID identifies the platform operator making this cross-tenant
// read, for pkgcore.SystemReason.Actor -- never the searched-for userID.
// The walk runs through TenantService.forEachLedgerTenant, which pages
// through the FULL ledger rather than one capped List call and enters
// each tenant's own audited system-context grant around its lookup. A
// per-tenant membership lookup failing for a reason OTHER than "no
// membership" aborts the whole call rather than silently omitting that
// tenant from the answer, so a storage outage is reported as an error
// instead of masquerading as "this person belongs to fewer tenants than
// they actually do" -- the same no-silent-omission contract the helper
// documents, which also keeps a ledger larger than one List page from
// making this answer quietly incomplete.
func (s *SearchService) MembershipsOf(ctx context.Context, userID, actorUserID string) ([]pkgcore.TenantID, error) {
	var found []pkgcore.TenantID
	err := s.tenants.forEachLedgerTenant(ctx, actorUserID, func(tenantCtx context.Context, row Tenant) error {
		if _, err := s.members.Get(tenantCtx, userID); err != nil {
			if apperr.HasCode(err, org.ErrMembershipNotFound.Code) {
				// No membership here: skip this tenant, not the walk.
				return nil
			}
			return err
		}
		found = append(found, pkgcore.TenantID(row.TenantID))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}
